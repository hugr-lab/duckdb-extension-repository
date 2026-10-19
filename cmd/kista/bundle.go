package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream/duckdbkeys"
)

const bundleUsage = `usage: kista bundle keygen -o <key.pem>                         a bundle key; prints its fingerprint
       kista bundle keys <prefix>                              a repository's listed key fingerprints
       kista bundle sign -key <key.pem> -plan <plan.json> -in <unsigned> -o <out>   sign an exported bundle offline
       kista bundle inspect <bundle> [-plan <plan.json>]       check a bundle's consistency offline`

// bundleCmd is spec 0014's client side: no server configuration, no database.
func bundleCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, bundleUsage)
		return 2
	}
	var err error
	switch args[0] {
	case "keygen":
		err = bundleKeygen(args[1:], stdout)
	case "keys":
		err = bundleKeys(args[1:], stdout)
	case "sign":
		err = bundleSign(args[1:], stdout)
	case "inspect":
		var ok bool
		if ok, err = bundleInspect(args[1:], stdout); err == nil && !ok {
			return 1
		}
	default:
		fmt.Fprintln(stderr, bundleUsage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "kista bundle %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func bundleKeygen(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("o", "", "the private key file to write (mode 0600)")
	if err := fs.Parse(args); err != nil || *out == "" || fs.NArg() != 0 {
		return errors.New("usage: kista bundle keygen -o <key.pem>")
	}
	pemBytes, fp, err := bundle.GenerateKey()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(pemBytes); err != nil {
		f.Close()
		_ = os.Remove(*out)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(*out)
		return err
	}
	fmt.Fprintln(stdout, fp)
	return nil
}

// bundleKeys prints the fingerprints a repository's .well-known file lists, to compare with what
// its publisher published out of band before pinning them.
func bundleKeys(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: kista bundle keys <https://host/tenant/channel>")
	}
	prefix := strings.TrimRight(args[0], "/")
	if err := bundle.ValidPrefix(prefix); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, prefix+"/.well-known/duckdb-extension-repo.json", nil)
	if err != nil {
		return err
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", req.URL, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, bundle.MaxWellKnown+1))
	if err != nil {
		return err
	}
	keys, err := wellKnownKeys(data)
	if err != nil {
		return err
	}
	for _, k := range keys {
		fmt.Fprintln(stdout, extfile.Fingerprint(k))
	}
	return nil
}

// wellKnownKeys parses a .well-known file's signature_keys, at most 64 KiB.
func wellKnownKeys(data []byte) ([]*rsa.PublicKey, error) {
	if len(data) > bundle.MaxWellKnown {
		return nil, fmt.Errorf("the .well-known file is over %d bytes", bundle.MaxWellKnown)
	}
	var wk struct {
		Keys []string `json:"signature_keys"`
	}
	if err := json.Unmarshal(data, &wk); err != nil {
		return nil, fmt.Errorf("the .well-known file is not valid: %w", err)
	}
	var out []*rsa.PublicKey
	for _, k := range wk.Keys {
		if pub, err := extfile.ParsePublicKey(k); err == nil {
			out = append(out, pub)
		}
	}
	return extfile.DedupKeys(out), nil
}

// bundleSign signs an unsigned bundle: it copies it once, inserting the signature and the key
// after the manifest.
func bundleSign(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	keyFile := fs.String("key", "", "the bundle key (PKCS#8 PEM)")
	planFile := fs.String("plan", "", "the plan the bundle answers: its bundle keys must list this key")
	in := fs.String("in", "", "the unsigned bundle")
	out := fs.String("o", "", "the signed bundle to write")
	if err := fs.Parse(args); err != nil || *keyFile == "" || *planFile == "" || *in == "" || *out == "" || fs.NArg() != 0 {
		return errors.New("usage: kista bundle sign -key <key.pem> -plan <plan.json> -in <unsigned> -o <out>")
	}
	planData, err := os.ReadFile(*planFile)
	if err != nil {
		return err
	}
	var plan bundle.Plan
	if err := bundle.DecodeStrict(planData, &plan); err != nil {
		return err
	}
	if err := plan.Validate(); err != nil {
		return err
	}
	kb, err := os.ReadFile(*keyFile)
	if err != nil {
		return err
	}
	priv, err := bundle.ParsePrivateKey(kb)
	if err != nil {
		return err
	}
	fp := bundle.Fingerprint(priv.Public().(ed25519.PublicKey))
	if !slices.Contains(plan.BundleKeys, fp) {
		return fmt.Errorf("the key %s is not among the plan's bundle keys", fp)
	}
	src, err := os.Open(*in)
	if err != nil {
		return err
	}
	defer src.Close()
	b, err := bundle.Open(src, time.Time{}, nil)
	if err != nil {
		return err
	}
	if b.Signed {
		return errors.New("the bundle is already signed")
	}
	m := b.Manifest
	if m.Plan.SHA256 != sha(planData) {
		return errors.New("the bundle answers another plan")
	}
	if err := withinPlan(m, &plan); err != nil {
		return err
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s exists", *out)
	}
	cells := map[string]bundle.Cell{}
	for _, s := range m.Sources {
		for _, c := range s.Cells {
			if c.Outcome == bundle.Fetched {
				cells[c.File] = c
			}
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(*out), ".kista-bundle-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	defer tmp.Close()
	w := bundle.NewWriter(tmp)
	if err := bundle.WriteHead(w, b.ManifestBytes, priv); err != nil {
		return err
	}
	wellKnown, files, feedEntries := 0, 0, 0
	for {
		it, err := b.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if it.Feed != nil { // a feed's entry, its bytes checked against the manifest by the reader
			if it.Damaged != "" {
				return fmt.Errorf("feed %s: %s", it.Feed.URL, it.Damaged)
			}
			if err := w.Entry(it.Feed.Entries[it.Entry].Path, int64(len(it.Data)), strings.NewReader(string(it.Data))); err != nil {
				return err
			}
			feedEntries++
			continue
		}
		if s := it.WellKnown; s != nil {
			if err := w.Entry(s.WellKnown.Path, int64(len(it.Data)), strings.NewReader(string(it.Data))); err != nil {
				return err
			}
			wellKnown++
			continue
		}
		// copy the file while checking it inflates to what the manifest says: a signature vouches
		// for the bundle's contents, not only its manifest
		c := cells[it.File]
		pr, pw := io.Pipe()
		checked := make(chan error, 1)
		go func() {
			n, h, err := bundle.Inflate(pr, c.InflatedSize, io.Discard)
			if err == nil && (n != c.InflatedSize || h != c.SHA256) {
				err = errors.New("the inflated file's size or hash differs from the manifest's")
			}
			pr.CloseWithError(err)
			checked <- err
		}()
		err = w.Entry(it.File, it.Size, io.TeeReader(it.R, pw))
		pw.CloseWithError(err)
		if cerr := <-checked; cerr != nil {
			return fmt.Errorf("%s: %w", it.File, cerr)
		}
		if err != nil {
			return err
		}
		files++
	}
	wantWK := 0
	for _, s := range m.Sources {
		if s.WellKnown != nil {
			wantWK++
		}
	}
	wantFeed := 0
	for _, f := range m.Feeds {
		wantFeed += len(f.Entries)
	}
	if !b.Ended() || files != len(cells) || wellKnown != wantWK || feedEntries != wantFeed {
		return fmt.Errorf("the bundle is incomplete: %d of %d files, %d of %d .well-known files, %d of %d feed entries",
			files, len(cells), wellKnown, wantWK, feedEntries, wantFeed)
	}
	if err := w.Close(); err != nil {
		return err
	}
	// the owner's only, as the export writes it: a bundle may carry licences (spec 0021)
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), *out); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "signed with %s\nsha256 %s\n", fp, w.SHA256())
	return nil
}

// withinPlan refuses a manifest with a source or a cell its plan did not ask for, a feed it did not
// ask for, or a feed body it did not offer with that cursor (or bring, from 0, in a file of one of
// the feed's sources).
func withinPlan(m *bundle.Manifest, p *bundle.Plan) error {
	if err := feedsWithinPlan(m, p); err != nil {
		return err
	}
	if m.Plan.Licenses != p.Licenses || m.Plan.LicenseHolder != p.LicenseHolder {
		return errors.New("the bundle's licence request differs from the plan's")
	}
	if ps := licenceProblems(m); len(ps) > 0 {
		return errors.New(ps[0])
	}
	asked := map[string]map[[3]string]bool{}
	for _, s := range p.Sources {
		cs := map[[3]string]bool{}
		for _, c := range s.Cells {
			cs[[3]string{c.DuckDBVersion, c.Platform, c.Name}] = true
		}
		asked[s.Kind+"\x00"+s.Prefix] = cs
	}
	for _, s := range m.Sources {
		cs, ok := asked[s.Kind+"\x00"+s.Prefix]
		if !ok {
			return fmt.Errorf("the plan asks for no source %s %s", s.Kind, s.Prefix)
		}
		for _, c := range s.Cells {
			if !cs[[3]string{c.DuckDBVersion, c.Platform, c.Name}] {
				return fmt.Errorf("the plan asks for no cell %s %s %s of %s %s", c.DuckDBVersion, c.Platform, c.Name, s.Kind, s.Prefix)
			}
		}
	}
	return nil
}

// licenceProblems lists the licences a manifest carries that are not what they are carried as: a
// token that does not parse, of another extension, or of another holder than the plan named. The
// site checks them again, with their signatures.
func licenceProblems(m *bundle.Manifest) []string {
	var out []string
	for _, s := range m.Sources {
		for _, l := range s.Licenses {
			t, err := license.Parse(l.Token)
			switch {
			case err != nil:
				out = append(out, fmt.Sprintf("%s: the licence of %s: %v", s.Prefix, l.Name, err))
			case t.Extension != l.Name:
				out = append(out, fmt.Sprintf("%s: the licence carried for %s is of %s", s.Prefix, l.Name, t.Extension))
			case m.Plan.LicenseHolder != "" && t.HolderID != m.Plan.LicenseHolder:
				out = append(out, fmt.Sprintf("%s: the licence of %s is of holder %q, the plan expects %q", s.Prefix, l.Name, t.HolderID, m.Plan.LicenseHolder))
			}
		}
	}
	return out
}

func feedsWithinPlan(m *bundle.Manifest, p *bundle.Plan) error {
	for _, f := range m.Feeds {
		i := slices.IndexFunc(p.Feeds, func(pf bundle.PlanFeed) bool { return pf.URL == f.URL })
		if i < 0 {
			return fmt.Errorf("the plan asks for no feed %s", f.URL)
		}
		pf := p.Feeds[i]
		offered := map[string]int64{}
		for _, b := range pf.Bodies {
			offered[b.BodyHash] = b.Seq
		}
		brought := map[string]bool{} // bodies of files the bundle brings from the feed's sources
		for _, si := range pf.Sources {
			ps := p.Sources[si]
			for _, s := range m.Sources {
				if s.Kind != ps.Kind || s.Prefix != ps.Prefix {
					continue
				}
				for _, c := range s.Cells {
					if c.Outcome == bundle.Fetched {
						brought[c.BodyHash] = true
					}
				}
			}
		}
		for _, b := range f.Bodies {
			seq, ok := offered[b.BodyHash]
			switch {
			case ok && b.AfterSeq != seq:
				return fmt.Errorf("feed %s: body %s starts after %d, the plan's cursor is %d", f.URL, b.BodyHash, b.AfterSeq, seq)
			case !ok && (b.AfterSeq != 0 || !brought[b.BodyHash]):
				return fmt.Errorf("feed %s: body %s is neither offered by the plan nor brought for the feed's sources", f.URL, b.BodyHash)
			}
		}
	}
	return nil
}

// defaultMaxBody is blob.max_body's default: inspect has no configuration to read it from.
const defaultMaxBody = 1 << 30

// bundleInspect checks a bundle offline: the manifest's signature with the bundle's own key, every
// entry's hash and size, every file's source signature, footer and spec 0008's checks. With -plan
// it adds the site's pins and filters: whether the bundle key is pinned, each source's keys pinned,
// listed only or missing, and the cells' version filters. Without -plan a bundle is reported
// consistent, not trusted.
func bundleInspect(args []string, stdout io.Writer) (bool, error) {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	planFile := fs.String("plan", "", "the site's plan, for its pins")
	maxBody := fs.Int64("max-body", defaultMaxBody, "the largest file to inflate (the site's blob.max_body), in bytes")
	var file string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return false, err
		}
		if args = fs.Args(); len(args) > 0 {
			if file != "" {
				return false, errors.New("one bundle at a time")
			}
			file, args = args[0], args[1:]
		}
	}
	if file == "" {
		return false, errors.New("usage: kista bundle inspect <bundle> [-plan <plan.json>] [-max-body <bytes>]")
	}
	var plan *bundle.Plan
	var planSHA string
	if *planFile != "" {
		data, err := os.ReadFile(*planFile)
		if err != nil {
			return false, err
		}
		plan = &bundle.Plan{}
		if err := bundle.DecodeStrict(data, plan); err != nil {
			return false, err
		}
		if err := plan.Validate(); err != nil {
			return false, err
		}
		planSHA = sha(data)
	}
	f, err := os.Open(file)
	if err != nil {
		return false, err
	}
	defer f.Close()
	b, err := bundle.Open(f, time.Now(), nil)
	if err != nil {
		return false, err
	}
	m := b.Manifest
	var problems []string
	problem := func(format string, args ...any) {
		if len(problems) < 50 {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}
	switch {
	case !b.Signed:
		problem("unsigned: kista bundle sign signs it")
	case bundle.Verify(b.Pub, b.ManifestBytes, b.Sig) != nil:
		problem("the manifest's signature does not verify with the bundle's key")
	}
	by := m.Exporter.PublicURL
	if by == "" {
		by = "a kista without a public URL"
	}
	fmt.Fprintf(stdout, "bundle %s, made %s by %s (%s), for tenant %s\n", m.ID, m.CreatedAt.Format(time.RFC3339),
		by, m.Exporter.Version, m.Plan.Tenant)
	if b.Signed {
		pinned := ""
		if plan != nil {
			ok := slices.Contains(plan.BundleKeys, b.Fingerprint)
			pinned = map[bool]string{true: " (pinned by the plan)", false: " (NOT pinned by the plan)"}[ok]
			if !ok {
				problem("the bundle key %s is not pinned by the plan", b.Fingerprint)
			}
		}
		fmt.Fprintf(stdout, "bundle key %s%s\n", b.Fingerprint, pinned)
	}
	if plan != nil && planSHA != m.Plan.SHA256 {
		problem("the bundle answers another plan (sha256 %s, made %s)", m.Plan.SHA256, m.Plan.CreatedAt.Format(time.RFC3339))
	}
	planKeys := map[string][]string{}
	planVersions := map[string]map[[3]string][]string{}
	if plan != nil {
		for _, s := range plan.Sources {
			k := s.Kind + "\x00" + s.Prefix
			planKeys[k] = s.Keys
			planVersions[k] = map[[3]string][]string{}
			for _, c := range s.Cells {
				planVersions[k][[3]string{c.DuckDBVersion, c.Platform, c.Name}] = c.Versions
			}
		}
	}
	outcomes := map[string]int{}
	for _, s := range m.Sources {
		for _, c := range s.Cells {
			outcomes[c.Outcome]++
		}
		for _, l := range s.Licenses {
			if t, err := license.Parse(l.Token); err == nil {
				fmt.Fprintf(stdout, "source %s licence %s, holder %q %q, key %s, expires %s\n", s.Prefix, l.Name, t.HolderID,
					bundle.Text(t.Holder, 200), t.Key, time.Unix(t.ExpiresAt, 0).UTC().Format(time.RFC3339))
			}
		}
	}
	if m.Plan.Licenses {
		holder := "any holder"
		if m.Plan.LicenseHolder != "" {
			holder = fmt.Sprintf("holder %q", m.Plan.LicenseHolder)
		}
		fmt.Fprintf(stdout, "the plan asks for licences, of %s\n", holder)
	}
	if plan != nil && (m.Plan.Licenses != plan.Licenses || m.Plan.LicenseHolder != plan.LicenseHolder) {
		problem("the bundle's licence request differs from the plan's")
	}
	for _, p := range licenceProblems(m) {
		problem("%s", p)
	}
	// the keys each source's files verify with: DuckDB's, or the .well-known file's (the plan's
	// pins among them, with -plan)
	repoKeys := map[*bundle.Source][]*rsa.PublicKey{}
	opts := bundle.CheckOptions{FileChecks: true, Keys: func(s *bundle.Source) []*rsa.PublicKey {
		switch s.Kind {
		case store.UpstreamCore:
			return duckdbkeys.Core()
		case store.UpstreamCommunity:
			return duckdbkeys.Community()
		}
		return repoKeys[s]
	}}
	if plan != nil {
		opts.Versions = func(r bundle.Ref) []string {
			return planVersions[r.Source.Kind+"\x00"+r.Source.Prefix][[3]string{r.Cell.DuckDBVersion, r.Cell.Platform, r.Cell.Name}]
		}
	}
	byFile := m.CellsByFile()
	var files, size, inflated, largest int64
	fc := feedCheck{plan: plan, problem: problem, out: stdout, keys: map[int]map[string]*rsa.PublicKey{}, counts: map[string]int{}}
	if plan != nil {
		if err := feedsWithinPlan(m, plan); err != nil {
			problem("%v", err)
		}
	}
	for {
		it, err := b.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			problem("%v", err)
			break
		}
		if it.Feed != nil {
			fc.entry(it)
			continue
		}
		if s := it.WellKnown; s != nil {
			ks, err := wellKnownKeys(it.Data)
			if err != nil {
				problem("%s: %v", s.Prefix, err)
				continue
			}
			k := s.Kind + "\x00" + s.Prefix
			var listed []string
			for _, key := range ks {
				fp := extfile.Fingerprint(key)
				listed = append(listed, fp)
				state := ""
				if plan != nil {
					state = map[bool]string{true: " pinned", false: " listed, not pinned"}[slices.Contains(planKeys[k], fp)]
				}
				fmt.Fprintf(stdout, "source %s key %s%s\n", s.Prefix, fp, state)
			}
			if plan != nil {
				for _, fp := range planKeys[k] {
					if !slices.Contains(listed, fp) {
						fmt.Fprintf(stdout, "source %s key %s pinned, not listed\n", s.Prefix, fp)
					}
				}
				ks = slices.DeleteFunc(ks, func(key *rsa.PublicKey) bool { return !slices.Contains(planKeys[k], extfile.Fingerprint(key)) })
			}
			repoKeys[s] = ks
			continue
		}
		refs := byFile[it.File]
		files++
		size += it.Size
		c0 := refs[0].Cell
		inflated += c0.InflatedSize
		largest = max(largest, c0.InflatedSize)
		if c0.InflatedSize > *maxBody+extfile.SignatureSize {
			problem("%s: %d bytes inflated, over -max-body", it.File, c0.InflatedSize)
			continue
		}
		for _, e := range inspectFile(it, refs, opts) {
			problem("%s: %v", it.File, e)
		}
	}
	if b.Ended() {
		fc.finish()
	}
	if missing := b.Missing(); len(missing) > 0 && b.Ended() {
		problem("the manifest names entries the bundle lacks: %s", strings.Join(missing, ", "))
	}
	if !b.Ended() && len(problems) == 0 {
		problem("the bundle ends before its end of archive")
	}
	outs := make([]string, 0, len(outcomes))
	for o, n := range outcomes {
		outs = append(outs, fmt.Sprintf("%s %d", o, n))
	}
	slices.Sort(outs)
	fmt.Fprintf(stdout, "cells: %s\nfiles: %d, %d bytes, %d inflated\nthe import needs about %d bytes of spool\n",
		strings.Join(outs, ", "), files, size, inflated, largest)
	if len(m.Feeds) > 0 {
		fmt.Fprintf(stdout, "feeds: %d, statements %d (withdrawals %d, without content %d), documents %d, %d bytes\n", len(m.Feeds),
			fc.counts["statements"], fc.counts["withdrawals"], fc.counts["without content"], fc.counts["documents"], fc.counts["bytes"])
	}
	if b.Ended() {
		fmt.Fprintf(stdout, "sha256 %s\n", b.SHA256())
	}
	if b.Skipped() > 0 {
		fmt.Fprintf(stdout, "%d bytes of entries read through\n", b.Skipped())
	}
	for _, p := range problems {
		fmt.Fprintf(stdout, "problem: %s\n", p)
	}
	if len(problems) > 0 {
		return false, nil
	}
	if plan == nil {
		fmt.Fprintln(stdout, "consistent (not checked against a site's pins: give -plan)")
	} else {
		fmt.Fprintln(stdout, "consistent with the plan's pins")
	}
	return true, nil
}

// inspectFile inflates one file into a temporary file and checks it against every cell it fills.
func inspectFile(it bundle.Item, refs []bundle.Ref, opts bundle.CheckOptions) []error {
	c0 := refs[0].Cell
	tmp, err := os.CreateTemp("", "kista-inspect-*")
	if err != nil {
		return []error{err}
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	defer tmp.Close()
	n, h, err := bundle.Inflate(it.R, c0.InflatedSize, tmp)
	if err != nil {
		return []error{err}
	}
	if n != c0.InflatedSize || h != c0.SHA256 {
		return []error{errors.New("the inflated file's size or hash differs from the manifest's")}
	}
	ef, err := extfile.Open(tmp, n, n)
	if err != nil {
		return []error{err}
	}
	return bundle.CheckFile(ef, refs, opts)
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// feedCheck checks a bundle's feed entries as they come (spec 0014 phase 1b): each statement's
// signature against the bundle's keys file (with -plan, only the plan's pinned keys), its issuer, its
// body within a covered range, and a stored statement's document present.
type feedCheck struct {
	plan    *bundle.Plan
	problem func(string, ...any)
	out     io.Writer
	keys    map[int]map[string]*rsa.PublicKey // by feed index
	counts  map[string]int
	damaged map[int]bool
	// the feed being read: its bodies' ranges, and the documents its batches named so far with the
	// size their statements give
	cur    int
	ranges map[string][2]int64
	named  map[string]int64
}

// at starts reading a feed's entries: its ranges built once per feed.
func (c *feedCheck) at(fi int, f *bundle.Feed) {
	if c.ranges != nil && c.cur == fi {
		return
	}
	c.finish() // the previous feed's
	c.cur, c.ranges, c.named = fi, map[string][2]int64{}, map[string]int64{}
	for _, b := range f.Bodies {
		c.ranges[b.BodyHash] = [2]int64{b.AfterSeq, b.Seq}
	}
}

func (c *feedCheck) entry(it bundle.Item) {
	f := it.Feed
	if it.Damaged != "" {
		if c.damaged == nil {
			c.damaged = map[int]bool{}
		}
		if !c.damaged[it.FeedIndex] {
			c.damaged[it.FeedIndex] = true
			c.problem("feed %s: %s", f.URL, it.Damaged)
		}
		return
	}
	ref := f.Entries[it.Entry]
	fe, _ := bundle.ParseFeedEntry(ref.Path)
	c.at(it.FeedIndex, f)
	switch {
	case fe.Keys:
		ks, err := attachment.ParseKeysFile(it.Data)
		if err != nil {
			c.problem("feed %s: %v", f.URL, err)
			return
		}
		var pinned []string
		if c.plan != nil {
			if i := slices.IndexFunc(c.plan.Feeds, func(pf bundle.PlanFeed) bool { return pf.URL == f.URL }); i >= 0 {
				pinned = c.plan.Feeds[i].Keys
			}
		}
		fps := make([]string, 0, len(ks))
		for fp := range ks {
			fps = append(fps, fp)
		}
		slices.Sort(fps)
		for _, fp := range fps {
			state := ""
			if c.plan != nil {
				state = map[bool]string{true: " pinned", false: " listed, not pinned"}[slices.Contains(pinned, fp)]
				if !slices.Contains(pinned, fp) {
					delete(ks, fp)
				}
			}
			fmt.Fprintf(c.out, "feed %s key %s%s\n", f.URL, fp, state)
		}
		c.keys[it.FeedIndex] = ks
	case fe.Batch > 0:
		entries, err := bundle.ParseBatch(it.Data)
		if err != nil {
			c.problem("feed %s: %s: %v", f.URL, ref.Path, err)
			return
		}
		for _, e := range entries {
			stmt, _ := base64.StdEncoding.DecodeString(e.Statement)
			sig, _ := base64.StdEncoding.DecodeString(e.Signature)
			c.counts["statements"]++
			st, err := attachment.Verify(stmt, sig, c.keys[it.FeedIndex])
			if err != nil {
				c.problem("feed %s: a statement does not verify with the keys file's keys: %v", f.URL, err)
				continue
			}
			r, ok := c.ranges[st.BodyHash]
			switch {
			case st.Issuer != f.URL:
				c.problem("feed %s: statement %d names another issuer", f.URL, st.Seq)
			case !ok || st.Seq <= r[0] || st.Seq > r[1]:
				c.problem("feed %s: statement %d of body %s is outside its covered range", f.URL, st.Seq, st.BodyHash)
			}
			switch {
			case st.Withdrawn:
				c.counts["withdrawals"]++
			case e.Note != "":
				c.counts["without content"]++
			default:
				if _, ok := c.named[st.ContentHash]; !ok {
					c.named[st.ContentHash] = st.Size
				}
				c.counts["contents"]++
			}
		}
	default:
		size, ok := c.named[fe.Document]
		switch {
		case !ok:
			c.problem("feed %s: document %s comes before a batch names it", f.URL, fe.Document)
		case size != int64(len(it.Data)):
			c.problem("feed %s: document %s is %d bytes, its statement says %d", f.URL, fe.Document, len(it.Data), size)
		}
		c.named[fe.Document] = -1 // carried
		c.counts["documents"]++
		c.counts["bytes"] += len(it.Data)
	}
}

// finish reports the last feed's statements whose document never came.
func (c *feedCheck) finish() {
	if c.ranges == nil {
		return
	}
	for h, size := range c.named {
		if size >= 0 {
			c.problem("a statement's document %s is not in the bundle", h)
		}
	}
}
