package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
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

	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
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
		return err
	}
	if err := f.Close(); err != nil {
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
	b, err := bundle.Open(src, time.Time{})
	if err != nil {
		return err
	}
	if b.Signed {
		return errors.New("the bundle is already signed")
	}
	if b.Manifest.Plan.SHA256 != sha(planData) {
		return errors.New("the bundle answers another plan")
	}
	tmp, err := os.CreateTemp(filepath.Dir(*out), ".kista-bundle-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bundle.NewWriter(tmp)
	if err := bundle.WriteHead(w, b.ManifestBytes, priv); err != nil {
		tmp.Close()
		return err
	}
	for {
		it, err := b.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			tmp.Close()
			return err
		}
		name := it.File
		if it.WellKnown != nil {
			name = it.WellKnown.WellKnown.Path
		}
		if err := w.Entry(name, it.Size, it.R); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := w.Close(); err != nil {
		tmp.Close()
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

// inspectSummary is what kista bundle inspect reports.
type inspectSummary struct {
	outcomes          map[string]int
	files, bytes, inf int64
	maxInflated       int64
	problems          []string
}

func (s *inspectSummary) problem(format string, args ...any) {
	if len(s.problems) < 50 {
		s.problems = append(s.problems, fmt.Sprintf(format, args...))
	}
}

// bundleInspect checks a bundle offline: the manifest's signature with the bundle's own key, every
// entry's hash and size, every file's source signature and footer. With -plan it adds the site's
// pins: whether the bundle key is pinned, and each source's keys pinned, listed only or missing.
// Without -plan a bundle is reported consistent, not trusted.
func bundleInspect(args []string, stdout io.Writer) (bool, error) {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	planFile := fs.String("plan", "", "the site's plan, for its pins")
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
		return false, errors.New("usage: kista bundle inspect <bundle> [-plan <plan.json>]")
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
		sum := sha256.Sum256(data)
		planSHA = hex.EncodeToString(sum[:])
	}
	f, err := os.Open(file)
	if err != nil {
		return false, err
	}
	defer f.Close()
	b, err := bundle.Open(f, time.Now())
	if err != nil {
		return false, err
	}
	m := b.Manifest
	sum := &inspectSummary{outcomes: map[string]int{}}
	switch {
	case !b.Signed:
		sum.problem("unsigned: kista bundle sign signs it")
	case bundle.Verify(b.Pub, b.ManifestBytes, b.Sig) != nil:
		sum.problem("the manifest's signature does not verify with the bundle's key")
	}
	fmt.Fprintf(stdout, "bundle %s, made %s by %s (%s), for tenant %s\n", m.ID, m.CreatedAt.Format(time.RFC3339),
		m.Exporter.PublicURL, m.Exporter.Version, m.Plan.Tenant)
	if b.Signed {
		pinned := ""
		if plan != nil {
			pinned = map[bool]string{true: " (pinned by the plan)", false: " (NOT pinned by the plan)"}[slices.Contains(plan.BundleKeys, b.Fingerprint)]
			if !slices.Contains(plan.BundleKeys, b.Fingerprint) {
				sum.problem("the bundle key %s is not pinned by the plan", b.Fingerprint)
			}
		}
		fmt.Fprintf(stdout, "bundle key %s%s\n", b.Fingerprint, pinned)
	}
	if plan != nil && planSHA != m.Plan.SHA256 {
		fmt.Fprintf(stdout, "note: the bundle answers another plan (sha256 %s), made %s\n", m.Plan.SHA256, m.Plan.CreatedAt.Format(time.RFC3339))
	}
	planKeys := map[string][]string{}
	if plan != nil {
		for _, s := range plan.Sources {
			planKeys[s.Kind+"\x00"+s.Prefix] = s.Keys
		}
	}
	// the keys each source's files verify with
	keys := map[string][]*rsa.PublicKey{}
	for i := range m.Sources {
		s := &m.Sources[i]
		switch s.Kind {
		case store.UpstreamCore:
			keys[s.Kind] = duckdbkeys.Core()
		case store.UpstreamCommunity:
			keys[s.Kind] = duckdbkeys.Community()
		}
		for _, c := range s.Cells {
			sum.outcomes[c.Outcome]++
		}
	}
	// cells by file, to check each file against every cell it fills
	byFile := map[string][]bundle.Cell{}
	srcOf := map[string]*bundle.Source{}
	for i := range m.Sources {
		for _, c := range m.Sources[i].Cells {
			if c.Outcome == bundle.Fetched {
				byFile[c.File] = append(byFile[c.File], c)
				srcOf[c.File] = &m.Sources[i]
			}
		}
	}
	tmpDir := os.TempDir()
	for {
		it, err := b.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			sum.problem("%v", err)
			break
		}
		if s := it.WellKnown; s != nil {
			data, err := io.ReadAll(it.R)
			if err != nil {
				sum.problem("%s: %v", s.Prefix, err)
				continue
			}
			if sha(data) != s.WellKnown.SHA256 {
				sum.problem("%s: the .well-known file's hash differs from the manifest's", s.Prefix)
				continue
			}
			ks, err := wellKnownKeys(data)
			if err != nil {
				sum.problem("%s: %v", s.Prefix, err)
				continue
			}
			var listed []string
			for _, k := range ks {
				fp := extfile.Fingerprint(k)
				listed = append(listed, fp)
				state := ""
				if plan != nil {
					state = map[bool]string{true: " pinned", false: " listed, not pinned"}[slices.Contains(planKeys[s.Kind+"\x00"+s.Prefix], fp)]
				}
				fmt.Fprintf(stdout, "source %s key %s%s\n", s.Prefix, fp, state)
			}
			if plan != nil {
				for _, fp := range planKeys[s.Kind+"\x00"+s.Prefix] {
					if !slices.Contains(listed, fp) {
						fmt.Fprintf(stdout, "source %s key %s pinned, not listed\n", s.Prefix, fp)
					}
				}
				ks = slices.DeleteFunc(ks, func(k *rsa.PublicKey) bool {
					return !slices.Contains(planKeys[s.Kind+"\x00"+s.Prefix], extfile.Fingerprint(k))
				})
			}
			keys[s.Kind+"\x00"+s.Prefix] = ks
			continue
		}
		cells := byFile[it.File]
		src := srcOf[it.File]
		sum.files++
		sum.bytes += it.Size
		sum.inf += cells[0].InflatedSize
		sum.maxInflated = max(sum.maxInflated, cells[0].InflatedSize)
		inspectFile(it, cells, src, keys, tmpDir, sum)
	}
	if !b.Ended() && len(sum.problems) == 0 {
		sum.problem("the bundle ends before its end of archive")
	}
	if int(sum.files) != len(byFile) {
		sum.problem("%d files named in the manifest are not in the bundle", len(byFile)-int(sum.files))
	}
	outs := make([]string, 0, len(sum.outcomes))
	for o, n := range sum.outcomes {
		outs = append(outs, fmt.Sprintf("%s %d", o, n))
	}
	slices.Sort(outs)
	fmt.Fprintf(stdout, "cells: %s\nfiles: %d, %d bytes, %d inflated\nthe import needs about %d bytes of spool\n", strings.Join(outs, ", "),
		sum.files, sum.bytes, sum.inf, sum.maxInflated)
	if b.Ended() {
		fmt.Fprintf(stdout, "sha256 %s\n", b.SHA256())
	}
	if b.Skipped() > 0 {
		fmt.Fprintf(stdout, "%d bytes of entries read through\n", b.Skipped())
	}
	for _, p := range sum.problems {
		fmt.Fprintf(stdout, "problem: %s\n", p)
	}
	if len(sum.problems) > 0 {
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
func inspectFile(it bundle.Item, cells []bundle.Cell, src *bundle.Source, keys map[string][]*rsa.PublicKey, tmpDir string, sum *inspectSummary) {
	c0 := cells[0]
	tmp, err := os.CreateTemp(tmpDir, "kista-inspect-*")
	if err != nil {
		sum.problem("%s: %v", it.File, err)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, h, err := bundle.Inflate(it.R, c0.InflatedSize, tmp)
	if err != nil {
		sum.problem("%s: %v", it.File, err)
		return
	}
	if n != c0.InflatedSize || h != c0.SHA256 {
		sum.problem("%s: the inflated file's size or hash differs from the manifest's", it.File)
		return
	}
	ef, err := extfile.Open(tmp, n, n)
	if err != nil {
		sum.problem("%s: %v", it.File, err)
		return
	}
	if ef.Hash.String() != c0.BodyHash {
		sum.problem("%s: the body hash differs from the manifest's", it.File)
	}
	k := keys[src.Kind]
	if src.Kind == store.UpstreamRepo {
		k = keys[src.Kind+"\x00"+src.Prefix]
	}
	if _, ok := extfile.Verify(ef.Hash, ef.Signature, k); !ok {
		sum.problem("%s (%s): the signature does not verify with the source's keys", it.File, c0.Name)
	}
	for _, c := range cells {
		md := ef.Metadata
		exact := md.ABI == extfile.ABICPP || md.ABI == extfile.ABICStructUnstable
		if md.Platform != c.Platform || exact && md.DuckDBVersion != c.DuckDBVersion {
			sum.problem("%s: its footer (%s, %s) is not cell %s %s %s", it.File, md.Platform, md.DuckDBVersion, c.DuckDBVersion, c.Platform, c.Name)
		}
	}
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
