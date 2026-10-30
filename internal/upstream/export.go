package upstream

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// ExportOptions are a bundle export's (spec 0014).
type ExportOptions struct {
	Plan []byte             // the site's plan, as written
	Key  ed25519.PrivateKey // the bundle key; nil: an unsigned bundle, for kista bundle sign
	// Private names the plan's repository sources whose private releases may leave.
	Private []string
	Sync    bool   // run the matching mirror-mode upstreams first, and wait for them
	Holder  string // the runs' leases' holder, with Sync
	// Licenses agrees to carry the licences the plan asks for (spec 0021); without it none go.
	Licenses bool
	// PublicURL is serve.public_url: a repository source at <PublicURL>/<tenant>/<channel> is an own
	// channel. Version is the exporter's version. Both go to the manifest, for the record.
	PublicURL, Version string
}

// ExportSource is one plan source's part of an export.
type ExportSource struct {
	Kind     string         `json:"kind"`
	Prefix   string         `json:"prefix,omitempty"`
	From     string         `json:"from,omitempty"` // the upstreams or the own channel the files came from
	Counts   map[string]int `json:"counts"`
	Bytes    int64          `json:"bytes"`              // its files' gzip entries
	Keys     []string       `json:"keys,omitempty"`     // the fingerprints its .well-known file lists
	Problems []CellNote     `json:"problems,omitempty"` // the first 20 cells not exported
	Error    string         `json:"error,omitempty"`    // it failed as a whole: it is not in the bundle
	// Licenses are the licences it carries (spec 0021); LicensesLeftOut those over the bound.
	Licenses        []ExportLicense `json:"licenses,omitempty"`
	LicensesLeftOut int             `json:"licenses_left_out,omitempty"`
}

// ExportLicense is a licence a bundle carries, as the exporter is shown it.
type ExportLicense struct {
	Name      string    `json:"name"`
	HolderID  string    `json:"holder_id"`
	Holder    string    `json:"holder,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ExportResult is an export's summary.
type ExportResult struct {
	ID             string         `json:"id"`
	CreatedAt      time.Time      `json:"created_at"`
	PlanSHA256     string         `json:"plan_sha256"`
	ManifestSHA256 string         `json:"manifest_sha256"`
	BundleKey      string         `json:"bundle_key,omitempty"` // none: unsigned
	SHA256         string         `json:"sha256"`               // the bundle's, as the import computes it
	Files          int            `json:"files"`
	Bytes          int64          `json:"bytes"`
	Sources        []ExportSource `json:"sources"`
	Feeds          []ExportFeed   `json:"feeds,omitempty"` // phase 1b
	// LicenseNote says why no licences were carried although the plan or the exporter asked.
	LicenseNote string `json:"license_note,omitempty"`
}

// Failed reports whether a source or a feed failed as a whole.
func (r ExportResult) Failed() bool {
	return slices.ContainsFunc(r.Sources, func(s ExportSource) bool { return s.Error != "" }) ||
		slices.ContainsFunc(r.Feeds, func(f ExportFeed) bool { return f.Error != "" })
}

// Export errors.
var (
	// ErrExportKey is a bundle key the plan does not list: the export stops before reading anything.
	ErrExportKey = errors.New("upstream: the bundle key is not among the plan's bundle keys")
	// ErrExportPrivate is a -private prefix that names no repository source of the plan.
	ErrExportPrivate = errors.New("upstream: -private names no repository source of the plan")
)

// The export's reasons for cells it does not export.
const (
	whyWithdrawn   = "withdrawn on the exporting kista"
	whyOtherSource = "not this source's build"
	whyPullThrough = "never fetched by pull-through"
	whyMatrix      = "outside the exporting matrix"
	whyPrivate     = "private on the exporting kista"
	whyNoOrigin    = "no original signature on the exporting kista"
	whyNoBody      = "the body is not available on the exporting kista"
	whyVersions    = "no release of the plan's versions on the exporting kista"
)

// xsource is a plan source being exported.
type xsource struct {
	plan    *bundle.PlanSource
	out     ExportSource
	ups     []*xupstream // matching upstreams, by name
	ch      *xchannel    // or an own channel
	keys    []*rsa.PublicKey
	wk      []byte
	names   map[string]bool // the tenant's upstreams of this source: a release "from this source"
	private bool
	cells   []bundle.Cell
	files   map[int]*xfile         // by index into cells: fetched cells' files
	filters map[[3]string][]string // the plan cells' versions
}

// entries are the entries whose licences of a name the source carries (spec 0035: a licence is
// of its catalogue entry): its mirrors', in their order, or the one its own channel serves the name
// from.
func (src *xsource) entries(name string) []string {
	if src.ch != nil {
		if s, ok := src.ch.snap.Selected[name]; ok {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(src.ups))
	for _, u := range src.ups {
		out = append(out, store.UpstreamSource(u.u.ID))
	}
	return out
}

// revocations are the source's exporting channel's revoked rows of the plan's names (spec 0039), as
// its -/revocations route shows them, in the export's view (private rows only where -private covers
// the source), at most bundle.MaxRevocations: its own channel's, or its upstreams' home channels'.
func (src *xsource) revocations() []bundle.Revocation {
	names := map[string]bool{}
	for _, pc := range src.plan.Cells {
		names[pc.Name] = true
	}
	vis := func(c *store.Candidate) bool { return names[c.Name] && (src.private || c.Visibility == store.Public) }
	var snaps []*release.Snapshot
	if src.ch != nil {
		snaps = append(snaps, src.ch.snap)
	}
	for _, xu := range src.ups {
		snaps = append(snaps, xu.snap)
	}
	seen := map[[4]string]int{}
	var out []bundle.Revocation
	for _, snap := range snaps {
		for _, r := range snap.RevokedRows(vis) {
			x := bundle.Revocation{Name: r.Name, Version: r.Version, Platform: r.Platform, BodyHash: r.BodyHash, State: r.State,
				Reason: bundle.Text(r.Reason, bundle.MaxDetail)}
			if i, ok := seen[r.Key()]; ok {
				if store.RevocationRank(x.State) > store.RevocationRank(out[i].State) {
					out[i] = x
				}
				continue
			}
			if len(out) >= bundle.MaxRevocations {
				continue
			}
			seen[r.Key()] = len(out)
			out = append(out, x)
		}
	}
	return out
}

type xupstream struct {
	u     store.Upstream
	snap  *release.Snapshot
	cells map[[3]string]store.UpstreamCell
}

type xchannel struct {
	ch   store.Channel
	snap *release.Snapshot
	sign []store.Key // keys both pinned and listed, the serving key first
}

// xfile is a file the bundle carries: a stored body and the signature that follows it.
type xfile struct {
	rec     store.Blob
	sig     []byte
	path    string
	sha     string
	size    int64 // the gzip entry
	inflate int64 // body ‖ signature
	md      extfile.Metadata
	err     error // the stored body could not be read
	hashed  bool
}

// Export writes a bundle answering a site's plan to w (spec 0014, Exporting a bundle): for each
// plan source, the files this tenant's mirrors or own channels hold for its cells, each with the
// signature its source made, checked with the plan's pins as the import will check them. Nothing
// is re-signed: the bundle key signs only the manifest. The error is a refusal or a failure
// before the bundle was complete; a source that fails as a whole is reported in the result and
// left out. Exporting is a server administrator's act.
func (s *Service) Export(ctx context.Context, a authz.Actor, tenant string, w io.Writer, o ExportOptions) (ExportResult, error) {
	var res ExportResult
	t, err := s.serverTenant(ctx, a, tenant)
	if err != nil {
		return res, err
	}
	var plan bundle.Plan
	if err := bundle.DecodeStrict(o.Plan, &plan); err != nil {
		return res, err
	}
	if err := plan.Validate(); err != nil {
		return res, err
	}
	sum := sha256.Sum256(o.Plan)
	res.PlanSHA256 = hex.EncodeToString(sum[:])
	if o.Key != nil {
		res.BundleKey = bundle.Fingerprint(o.Key.Public().(ed25519.PublicKey))
		if !slices.Contains(plan.BundleKeys, res.BundleKey) {
			return res, fmt.Errorf("%w: %s", ErrExportKey, res.BundleKey)
		}
	}
	private := map[string]bool{}
	for _, p := range o.Private {
		p = strings.TrimSuffix(p, "/")
		if !slices.ContainsFunc(plan.Sources, func(ps bundle.PlanSource) bool { return ps.Kind == store.UpstreamRepo && ps.Prefix == p }) {
			return res, fmt.Errorf("%w: %s", ErrExportPrivate, p)
		}
		private[p] = true
	}
	if s.Blob == nil {
		return res, errors.New("upstream: the export needs a blob service")
	}
	var ups []store.Upstream
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		ups, err = tx.ListUpstreams(ctx, t.ID)
		return err
	}); err != nil {
		return res, err
	}
	if o.Sync {
		if err := s.exportSync(ctx, a, t, ups, &plan, o.Holder); err != nil {
			return res, err
		}
	}
	channels, err := s.Store.ListChannels(ctx, t.Name)
	if err != nil {
		return res, err
	}
	channels = store.KnownChannels(channels) // spec 0034: a type this binary does not know is exported by none
	now := s.Store.Now().UTC()
	snaps := &release.Snapshots{Store: s.Store}
	x := &exporter{s: s, t: t, now: now, snaps: snaps, ups: map[string]*xupstream{}, files: map[string]*xfile{}}
	own := ownPrefix(o.PublicURL, t.Name)
	var srcs []*xsource
	for i := range plan.Sources {
		ps := &plan.Sources[i]
		src := &xsource{plan: ps, private: private[ps.Prefix], names: map[string]bool{}, files: map[int]*xfile{},
			out: ExportSource{Kind: ps.Kind, Prefix: ps.Prefix, Counts: map[string]int{}}}
		srcs = append(srcs, src)
		if err := x.source(ctx, src, ups, channels, own); err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			src.out.Error = err.Error()
			continue
		}
		if err := x.cells(ctx, src); err != nil {
			return res, err
		}
	}

	// the manifest: every fetched file read once to hash and check it
	m := bundle.Manifest{Format: bundle.ManifestFormat, ID: store.NewID(), CreatedAt: now,
		Exporter: bundle.Exporter{Version: bundle.Text(o.Version, bundle.MaxExporter), PublicURL: bundle.Text(o.PublicURL, bundle.MaxExporter)},
		Plan: bundle.PlanRef{Tenant: plan.Tenant, CreatedAt: plan.CreatedAt, SHA256: res.PlanSHA256, Licenses: plan.Licenses,
			LicenseHolder: plan.LicenseHolder}}
	held, err := s.exportable(ctx, t, &plan, o, &res)
	if err != nil {
		return res, err
	}
	carried := 0
	wellKnown := map[string][]byte{}
	byPath := map[string]*xfile{}
	for _, src := range srcs {
		if src.out.Error != "" {
			continue
		}
		bs := bundle.Source{Kind: src.plan.Kind, Prefix: src.plan.Prefix, Cells: []bundle.Cell{}}
		counted := map[string]bool{}
		if src.wk != nil {
			p := fmt.Sprintf("sources/%d/well-known.json", len(wellKnown)+1)
			ws := sha256.Sum256(src.wk)
			bs.WellKnown = &bundle.FileRef{Path: p, SHA256: hex.EncodeToString(ws[:]), Size: int64(len(src.wk))}
			wellKnown[p] = src.wk
		}
		for i, c := range src.cells {
			if f := src.files[i]; f != nil {
				if err := x.hash(ctx, f); err != nil {
					return res, err
				}
				c = x.check(src, c, f)
				if c.Outcome == bundle.Fetched {
					byPath[f.path] = f
					if !counted[f.path] { // each file once per source, as the bundle holds it
						counted[f.path] = true
						src.out.Bytes += f.size
					}
				}
			}
			src.out.Counts[c.Outcome]++
			if (c.Outcome == bundle.Missing || c.Outcome == bundle.Rejected) && len(src.out.Problems) < maxProblems {
				src.out.Problems = append(src.out.Problems, CellNote{DuckDBVersion: c.DuckDBVersion, Platform: c.Platform, Name: c.Name,
					Outcome: c.Outcome, Detail: c.Detail})
			}
			bs.Cells = append(bs.Cells, c)
		}
		if held != nil && bs.Kind == store.UpstreamRepo {
			bs.Licenses, src.out.Licenses, src.out.LicensesLeftOut = carry(bs.Cells, held, src.entries)
			carried += len(bs.Licenses)
		}
		if plan.AcceptsRevocations { // spec 0039
			bs.Revocations = src.revocations()
			if len(bs.Revocations) > 0 {
				m.Format = bundle.ManifestFormatV2
				src.out.Counts["revocations"] = len(bs.Revocations)
			}
		}
		m.Sources = append(m.Sources, bs)
	}
	if m.Sources == nil {
		m.Sources = []bundle.Source{}
	}
	bundle.Canonical(m.Sources)
	// the feeds (phase 1b): their batches spooled, so the manifest can name their hashes
	var feeds []*xfeed
	var spool *os.File
	if len(plan.Feeds) > 0 {
		if s.TempDir != "" {
			if err := os.MkdirAll(s.TempDir, 0o700); err != nil {
				return res, err
			}
		}
		if spool, err = os.CreateTemp(s.TempDir, "kista-export-feeds-*"); err != nil {
			return res, err
		}
		defer func() { _ = spool.Close(); _ = os.Remove(spool.Name()) }()
		base, err := json.Marshal(m)
		if err != nil {
			return res, err
		}
		if feeds, res.Feeds, err = x.feeds(ctx, &plan, &m, spool, bundle.MaxManifest-len(base)); err != nil {
			return res, err
		}
		for _, f := range feeds {
			m.Feeds = append(m.Feeds, f.m)
		}
	}
	if err := m.Validate(time.Time{}); err != nil {
		return res, fmt.Errorf("upstream: the export made an invalid manifest: %w", err)
	}
	md, err := json.Marshal(m)
	if err != nil {
		return res, err
	}
	ms := sha256.Sum256(md)
	res.ID, res.CreatedAt, res.ManifestSHA256 = m.ID, m.CreatedAt, hex.EncodeToString(ms[:])

	// the bundle: the head, the sources' .well-known files, then the files in canonical order,
	// straight from the stored streams
	bw := bundle.NewWriter(w)
	if err := bundle.WriteHead(bw, md, o.Key); err != nil {
		return res, err
	}
	for _, src := range m.Sources {
		if src.WellKnown != nil {
			data := wellKnown[src.WellKnown.Path]
			if err := bw.Entry(src.WellKnown.Path, int64(len(data)), strings.NewReader(string(data))); err != nil {
				return res, err
			}
		}
	}
	for _, p := range m.FileOrder() {
		f := byPath[p]
		gz, err := s.Blob.OpenGzip(ctx, f.rec, f.sig)
		if err != nil {
			return res, fmt.Errorf("upstream: %s: %w", p, err)
		}
		r := gz.NewReader(ctx)
		err = bw.Entry(p, gz.Size, r)
		_ = r.Close()
		if err != nil {
			return res, fmt.Errorf("upstream: %s: %w", p, err)
		}
		res.Files++
		res.Bytes += gz.Size
	}
	if err := x.writeFeeds(ctx, bw, feeds, spool); err != nil {
		return res, err
	}
	if err := bw.Close(); err != nil {
		return res, err
	}
	res.SHA256 = bw.SHA256()
	for _, src := range srcs {
		res.Sources = append(res.Sources, src.out)
	}
	counts := map[string]int{}
	for _, src := range res.Sources {
		for k, n := range src.Counts {
			counts[k] += n
		}
		if src.Error != "" {
			counts["failed_sources"]++
		}
	}
	if carried > 0 {
		counts["licenses"] = carried
	} else if held != nil {
		res.LicenseNote = "no licence in force for the bundle's names"
		if plan.LicenseHolder != "" {
			res.LicenseNote += fmt.Sprintf(" of holder %q", plan.LicenseHolder)
		}
	}
	for _, f := range res.Feeds {
		for k, n := range f.Counts {
			counts["feed_"+k] += n
		}
		if f.Error != "" {
			counts["failed_feeds"]++
		}
	}
	err = s.Store.InTenantTx(ctx, t.ID, nil, func(tx *store.Tx) error {
		return tx.Event(ctx, t.ID, a.String(), "bundle.export", "tenant:"+t.Name, map[string]any{"plan": res.PlanSHA256,
			"manifest": res.ID, "manifest_sha256": res.ManifestSHA256, "bundle_key": res.BundleKey, "counts": counts})
	})
	return res, err
}

// exportable is the tenant's licences an export may carry (spec 0021): only when the plan asks and
// the exporter agrees, and licensing is on here; nil carries none, and the result says why.
func (s *Service) exportable(ctx context.Context, t store.Tenant, plan *bundle.Plan, o ExportOptions, res *ExportResult) ([]store.License, error) {
	switch {
	case plan.Licenses && !o.Licenses:
		res.LicenseNote = "the plan asks for licences: none carried (export -licenses carries them)"
		return nil, nil
	case !plan.Licenses && o.Licenses:
		res.LicenseNote = "the plan asks for no licences: none carried"
		return nil, nil
	case !plan.Licenses:
		return nil, nil
	case s.Licenses == nil:
		res.LicenseNote = "licensing is off here: none carried"
		return nil, nil
	}
	held, err := s.Licenses.Exportable(ctx, t.ID, pkgtype.DuckDBExtension, plan.LicenseHolder) // a bundle's sources are DuckDB's
	if errors.Is(err, licenses.ErrOff) {
		res.LicenseNote = "licensing is off here: none carried"
		return nil, nil
	}
	if held == nil {
		held = []store.License{}
	}
	return held, err
}

// carry picks a source's licences: those of names whose files it carries or the site holds, by
// name, at most bundle.MaxLicenses; left are the ones over.
func carry(cells []bundle.Cell, held []store.License, entries func(name string) []string) (out []bundle.License, shown []ExportLicense, left int) {
	names := map[string]bool{}
	for _, c := range cells {
		if c.Outcome == bundle.Fetched || c.Outcome == bundle.Unchanged {
			names[c.Name] = true
		}
	}
	// spec 0035: a name's licence of the entry the files come from, the first of them that holds one
	// (a name once)
	rank := func(l store.License) int { return slices.Index(entries(l.Name), l.Entry) }
	ls := slices.Clone(held)
	slices.SortFunc(ls, func(a, b store.License) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), cmp.Compare(rank(a), rank(b)))
	})
	for i, l := range ls {
		if !names[l.Name] || rank(l) < 0 || i > 0 && ls[i-1].Name == l.Name && rank(ls[i-1]) >= 0 {
			continue
		}
		if len(out) == bundle.MaxLicenses {
			left++
			continue
		}
		out = append(out, bundle.License{Name: l.Name, Token: l.Token})
		shown = append(shown, ExportLicense{Name: l.Name, HolderID: l.HolderID, Holder: bundle.Text(l.Holder, 200),
			ExpiresAt: time.Unix(l.ExpiresAt, 0).UTC()})
	}
	return out, shown, left
}

// ownPrefix is where the tenant's own channels are, in a plan's normalised form: <public url>/<tenant>.
func ownPrefix(publicURL, tenant string) string {
	u, err := url.Parse(strings.TrimSuffix(publicURL, "/"))
	if publicURL == "" || err != nil || u.Scheme != "https" {
		return ""
	}
	u.Host = strings.ToLower(strings.TrimSuffix(u.Host, ":443"))
	return u.String() + "/" + tenant
}

type exporter struct {
	s     *Service
	t     store.Tenant
	now   time.Time
	snaps *release.Snapshots
	ups   map[string]*xupstream // by upstream id
	files map[string]*xfile     // by body hash and signature: each read once
}

// source finds where a plan source's files come from and its keys: the tenant's active mirror or
// pull-through upstreams of the source, or its own signed channel. An error fails the source.
func (x *exporter) source(ctx context.Context, src *xsource, ups []store.Upstream, channels []store.Channel, own string) error {
	ps := src.plan
	same := func(u store.Upstream) bool {
		return u.Kind == ps.Kind && (ps.Kind != store.UpstreamRepo || u.Prefix == ps.Prefix)
	}
	var names []string
	for _, u := range ups {
		if !same(u) {
			continue
		}
		src.names[u.Name] = true
		if u.State != store.UpstreamActive || u.Mode != store.ModeMirror && u.Mode != store.ModePullThrough {
			continue
		}
		xu, err := x.upstream(ctx, u)
		if err != nil {
			return err
		}
		src.ups = append(src.ups, xu)
		names = append(names, u.Name)
	}
	slices.SortFunc(src.ups, func(a, b *xupstream) int { return strings.Compare(a.u.Name, b.u.Name) })
	slices.Sort(names)
	if len(src.ups) > 0 {
		src.out.From = "upstreams " + strings.Join(names, ",")
	} else if own != "" && ps.Kind == store.UpstreamRepo && strings.HasPrefix(ps.Prefix, own+"/") {
		name := strings.TrimPrefix(ps.Prefix, own+"/")
		i := slices.IndexFunc(channels, func(c store.Channel) bool { return c.Name == name })
		if i >= 0 && channels[i].Kind == store.ChannelPassthrough {
			return errors.New("a passthrough channel has no key of its own and is never a source")
		}
		if i >= 0 {
			snap, err := x.snaps.Get(ctx, channels[i])
			if err != nil {
				return err
			}
			src.ch = &xchannel{ch: channels[i], snap: snap}
			src.out.From = "channel " + name
		}
	}
	if len(src.ups) == 0 && src.ch == nil {
		return errors.New("not mirrored here")
	}
	switch {
	case ps.Kind != store.UpstreamRepo:
		src.keys = x.s.duckdbKeys(ps.Kind)
		return nil
	case src.ch != nil:
		wk, _, err := (&keys.Service{Store: x.s.Store}).WellKnownOf(ctx, src.ch.ch)
		if err != nil {
			return err
		}
		src.wk = wk
	default:
		// as a mirror's key check fetches it: public by design, through egress
		wk, _, err := x.s.Fetch.Get(ctx, ps.Prefix+"/.well-known/duckdb-extension-repo.json")
		if err != nil {
			return fmt.Errorf("the .well-known file: %w", err)
		}
		if len(wk) > bundle.MaxWellKnown {
			return fmt.Errorf("the .well-known file is over %d bytes", bundle.MaxWellKnown)
		}
		src.wk = wk
	}
	var doc struct {
		Keys []string `json:"signature_keys"`
	}
	if err := json.Unmarshal(src.wk, &doc); err != nil {
		return fmt.Errorf("the .well-known file is not valid: %w", err)
	}
	for _, k := range doc.Keys {
		pub, err := extfile.ParsePublicKey(k)
		if err != nil {
			continue
		}
		fp := extfile.Fingerprint(pub)
		if !slices.Contains(src.out.Keys, fp) {
			src.out.Keys = append(src.out.Keys, fp)
		}
		if slices.Contains(ps.Keys, fp) {
			src.keys = append(src.keys, pub)
		}
	}
	src.keys = extfile.DedupKeys(src.keys)
	if len(src.keys) == 0 {
		return errors.New("the .well-known file lists none of the plan's keys")
	}
	if src.ch != nil {
		// a site that still pins a key the channel rotated away from receives files it can verify
		for _, k := range src.ch.snap.Keys {
			if (k.State == store.KeyActive || k.State == store.KeyTrusted) && slices.Contains(ps.Keys, k.Fingerprint) &&
				slices.Contains(src.out.Keys, k.Fingerprint) {
				src.ch.sign = append(src.ch.sign, k)
			}
		}
		slices.SortStableFunc(src.ch.sign, func(a, b store.Key) int {
			switch {
			case a.ID == src.ch.ch.ServingKeyID:
				return -1
			case b.ID == src.ch.ch.ServingKeyID:
				return 1
			}
			return 0
		})
	}
	return nil
}

// upstream reads an exporting upstream's channel snapshot and cell records, once per export.
func (x *exporter) upstream(ctx context.Context, u store.Upstream) (*xupstream, error) {
	if xu := x.ups[u.ID]; xu != nil {
		return xu, nil
	}
	sc, err := x.s.Store.ServeChannelByID(ctx, u.ChannelID)
	if err != nil {
		return nil, err
	}
	snap, err := x.snaps.Get(ctx, sc.Channel)
	if err != nil {
		return nil, err
	}
	xu := &xupstream{u: u, snap: snap}
	if err := x.s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		xu.cells, err = tx.Cells(ctx, u.ID)
		return err
	}); err != nil {
		return nil, err
	}
	x.ups[u.ID] = xu
	return xu, nil
}

// cells decides each plan cell's outcome on the exporting side.
func (x *exporter) cells(ctx context.Context, src *xsource) error {
	src.filters = map[[3]string][]string{}
	for _, pc := range src.plan.Cells {
		src.filters[[3]string{pc.DuckDBVersion, pc.Platform, pc.Name}] = pc.Versions
		var c bundle.Cell
		var f *xfile
		var err error
		if src.ch != nil {
			c, f, err = x.fromChannel(ctx, src, pc)
		} else {
			c, f, err = x.fromUpstreams(ctx, src, pc)
		}
		if err != nil {
			return err
		}
		c.DuckDBVersion, c.Platform, c.Name = pc.DuckDBVersion, pc.Platform, pc.Name
		c.Detail = bundle.Text(c.Detail, bundle.MaxDetail)
		if f != nil {
			src.files[len(src.cells)] = f
		}
		src.cells = append(src.cells, c)
	}
	return nil
}

// view is the releases a cell may take: the plan's versions, and private ones only with -private.
func view(pc bundle.PlanCell, private bool) (all, allowed release.Visible) {
	all = func(c *store.Candidate) bool {
		return len(pc.Versions) == 0 || slices.Contains(pc.Versions, c.PackageVersion)
	}
	return all, func(c *store.Candidate) bool { return all(c) && (private || c.Visibility == store.Public) }
}

// unservable explains a cell the channel serves nothing for in the view: a private release, a
// withdrawn one, or none of the plan's versions; missing with no reason otherwise.
func unservable(snap *release.Snapshot, pc bundle.PlanCell, all release.Visible) (bundle.Cell, bool) {
	if snap.Flat(pc.DuckDBVersion, pc.Platform, pc.Name, all) != nil {
		return rejected(whyPrivate), true
	}
	for _, c := range snap.Group(pc.Name, pc.Platform) {
		if all(c) && release.Serves(c, pc.DuckDBVersion, snap.CAPIs) && (c.State == store.ReleaseYanked || c.State == store.ReleaseDeprecated) {
			return rejected(whyWithdrawn), true
		}
	}
	if len(pc.Versions) > 0 && snap.Flat(pc.DuckDBVersion, pc.Platform, pc.Name, release.Everything) != nil {
		return missing(whyVersions), true
	}
	return missing(""), false
}

func missing(why string) bundle.Cell  { return bundle.Cell{Outcome: bundle.Missing, Detail: why} }
func rejected(why string) bundle.Cell { return bundle.Cell{Outcome: bundle.Rejected, Detail: why} }

// fromUpstreams takes a cell from the first upstream that holds a servable release of this
// source for it; otherwise it reports the first upstream whose matrix holds the cell.
func (x *exporter) fromUpstreams(ctx context.Context, src *xsource, pc bundle.PlanCell) (bundle.Cell, *xfile, error) {
	var first *bundle.Cell
	for _, xu := range src.ups {
		c, f, err := x.fromUpstream(ctx, src, xu, pc)
		if err != nil || c.Outcome == bundle.Fetched || c.Outcome == bundle.Unchanged {
			return c, f, err
		}
		if first == nil && c.Detail != whyMatrix {
			first = &c
		}
	}
	if first != nil {
		return *first, nil, nil
	}
	return missing(whyMatrix), nil, nil
}

func (x *exporter) fromUpstream(ctx context.Context, src *xsource, xu *xupstream, pc bundle.PlanCell) (bundle.Cell, *xfile, error) {
	u := xu.u
	_, served := xu.snap.CAPIs[pc.DuckDBVersion]
	if !served || !slices.Contains(u.Platforms, pc.Platform) || !slices.ContainsFunc(u.Entries, func(e store.UpstreamEntry) bool {
		return e.Name == pc.Name || e.Name == "*" && u.Mode == store.ModePullThrough
	}) {
		return missing(whyMatrix), nil, nil
	}
	key := [3]string{pc.DuckDBVersion, pc.Platform, pc.Name}
	all, allowed := view(pc, src.private)
	best := xu.snap.Flat(pc.DuckDBVersion, pc.Platform, pc.Name, allowed)
	if best == nil {
		if c, ok := unservable(xu.snap, pc, all); ok {
			return c, nil, nil
		}
		rec, ok := xu.cells[key]
		switch {
		case !ok && u.Mode == store.ModePullThrough:
			return missing(whyPullThrough), nil, nil
		case !ok:
			return missing(""), nil, nil
		case slices.Contains([]string{store.CellRejected, store.CellBlocked, store.CellShadowed, store.CellConflict, store.CellOtherSource}, rec.Outcome):
			return rejected(cmpOr(rec.Detail, rec.Outcome)), nil, nil
		case rec.Outcome == store.CellFailed:
			return missing("the last fetch failed"), nil, nil // the fetch's own error stays on the exporting side
		}
		return missing(rec.Detail), nil, nil
	}
	var prov struct {
		Upstream   string `json:"upstream"`
		Credential string `json:"credential"`
		FetchedAt  string `json:"fetched_at"`
	}
	_ = json.Unmarshal([]byte(best.Provenance), &prov)
	if best.Origin != store.OriginUpstream || !src.names[prov.Upstream] {
		return rejected(whyOtherSource), nil, nil
	}
	// authenticated: it came through a credential, or it is private here (-private let it leave)
	c := bundle.Cell{BodyHash: best.BodyHash,
		Authenticated: (prov.Credential != "" || best.Visibility == store.Private) && src.plan.Kind == store.UpstreamRepo}
	// fetched_at is when the source last confirmed this body for the cell: the cell record while it
	// still holds the body (unchanged runs keep it fresh), else when the release was fetched
	rec, ok := xu.cells[key]
	if prov.Upstream != u.Name {
		if other := x.byName(prov.Upstream); other != nil {
			rec, ok = other.cells[key]
		}
	}
	// a failed fetch keeps the body the cell held (spec 0038) but confirmed nothing: only a record of
	// a body taken counts
	if ok && conditional[rec.Outcome] && rec.BodyHash == best.BodyHash && !rec.FetchedAt.IsZero() {
		c.FetchedAt, c.ETag = rec.FetchedAt, bundle.Text(rec.ETag, bundle.MaxETag)
	} else if ft, err := time.Parse(time.RFC3339, prov.FetchedAt); err == nil {
		c.FetchedAt = ft
	} else {
		c.FetchedAt = best.CreatedAt
	}
	c.FetchedAt = x.clamp(c.FetchedAt)
	if pc.BodyHash == best.BodyHash { // the plan's ETag alone never decides
		c.Outcome = bundle.Unchanged
		return c, nil, nil
	}
	sig, err := x.s.Store.OriginSignature(ctx, best.ID)
	if errors.Is(err, store.ErrNotFound) || err == nil && len(sig) != extfile.SignatureSize {
		return rejected(whyNoOrigin), nil, nil
	}
	if err != nil {
		return bundle.Cell{}, nil, err
	}
	return x.file(ctx, c, best.BodyHash, sig)
}

// byName finds an exporting upstream already read, by name.
func (x *exporter) byName(name string) *xupstream {
	for _, xu := range x.ups {
		if xu.u.Name == name {
			return xu
		}
	}
	return nil
}

// fromChannel takes a cell from an own signed channel: its servable release with a signature by a
// key the plan pins.
func (x *exporter) fromChannel(ctx context.Context, src *xsource, pc bundle.PlanCell) (bundle.Cell, *xfile, error) {
	snap := src.ch.snap
	if _, served := snap.CAPIs[pc.DuckDBVersion]; !served {
		return missing(whyMatrix), nil, nil
	}
	all, allowed := view(pc, src.private)
	best := snap.Flat(pc.DuckDBVersion, pc.Platform, pc.Name, allowed)
	if best == nil {
		c, _ := unservable(snap, pc, all)
		return c, nil, nil
	}
	c := bundle.Cell{BodyHash: best.BodyHash, FetchedAt: x.clamp(best.CreatedAt), Authenticated: best.Visibility == store.Private}
	if pc.BodyHash == best.BodyHash {
		c.Outcome = bundle.Unchanged
		return c, nil, nil
	}
	for _, k := range src.ch.sign {
		sig, err := x.s.Store.Signature(ctx, best.ID, k.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return bundle.Cell{}, nil, err
		}
		return x.file(ctx, c, best.BodyHash, sig)
	}
	return rejected("no signature by a key the plan pins"), nil, nil
}

// clamp keeps a time at or before the manifest's created_at, as validation requires.
func (x *exporter) clamp(t time.Time) time.Time {
	if t.After(x.now) {
		return x.now
	}
	return t.UTC()
}

// file names the stored body a fetched cell carries, shared by every cell with the same body and
// signature.
func (x *exporter) file(ctx context.Context, c bundle.Cell, bodyHash string, sig []byte) (bundle.Cell, *xfile, error) {
	k := bodyHash + "/" + hex.EncodeToString(sig)
	f := x.files[k]
	if f == nil {
		rec, err := x.s.Store.GetBlob(ctx, x.t.StorageDomain, bodyHash)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return rejected(whyNoBody), nil, nil
		case err != nil:
			return bundle.Cell{}, nil, err
		case !rec.CorruptAt.IsZero():
			return rejected(whyNoBody), nil, nil
		}
		f = &xfile{rec: rec, sig: sig}
		x.files[k] = f
	}
	c.Outcome = bundle.Fetched
	return c, f, nil
}

// hash reads a file once, inflated from its verified stored stream: its SHA-256 and size, and the
// footer the checks need. A body that cannot be read is the cell's refusal, not the export's.
func (x *exporter) hash(ctx context.Context, f *xfile) error {
	if f.hashed {
		return nil
	}
	f.hashed = true
	plain, err := x.s.Blob.OpenPlain(ctx, f.rec, f.sig)
	if err != nil {
		f.err = err
		return nil
	}
	gz, err := x.s.Blob.OpenGzip(ctx, f.rec, f.sig)
	if err != nil {
		f.err = err
		return nil
	}
	r := plain.NewReader(ctx)
	defer func() { _ = r.Close() }()
	h := sha256.New()
	tail := &tailWriter{n: extfile.FooterSize}
	n, err := io.Copy(io.MultiWriter(h, tail), r)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || n != plain.Size || len(tail.b) < extfile.FooterSize {
		f.err = fmt.Errorf("reading the stored body: %v", err)
		return nil
	}
	var block [extfile.MetadataSize]byte
	copy(block[:], tail.b[:extfile.MetadataSize])
	if f.md, err = extfile.ParseMetadata(block); err != nil {
		f.err = err
		return nil
	}
	f.sha = hex.EncodeToString(h.Sum(nil))
	f.path, f.size, f.inflate = "files/"+f.sha+".gz", gz.Size, n
	return nil
}

// check runs the import's checks on a fetched cell's file with the plan's pins: the signature,
// the footer against the cell, the version filter. A failure leaves the file out.
func (x *exporter) check(src *xsource, c bundle.Cell, f *xfile) bundle.Cell {
	refuse := func(why string) bundle.Cell {
		r := rejected(bundle.Text(why, bundle.MaxDetail))
		r.DuckDBVersion, r.Platform, r.Name = c.DuckDBVersion, c.Platform, c.Name
		return r
	}
	if f.err != nil {
		return refuse(whyNoBody)
	}
	var hash extfile.BodyHash
	if b, err := hex.DecodeString(f.rec.BodyHash); err == nil && len(b) == len(hash) {
		copy(hash[:], b)
	}
	ef := &extfile.File{BodySize: f.rec.BodyLen, Metadata: f.md, Hash: hash, Signature: f.sig}
	c.File, c.SHA256, c.Size, c.InflatedSize = f.path, f.sha, f.size, f.inflate
	errs := bundle.CheckFile(ef, []bundle.Ref{{Source: &bundle.Source{Kind: src.plan.Kind, Prefix: src.plan.Prefix}, Cell: c}},
		bundle.CheckOptions{
			Keys: func(*bundle.Source) []*rsa.PublicKey { return src.keys },
			Versions: func(r bundle.Ref) []string {
				return src.filters[[3]string{r.Cell.DuckDBVersion, r.Cell.Platform, r.Cell.Name}]
			},
		})
	if len(errs) > 0 {
		return refuse(errs[0].Error())
	}
	return c
}

// tailWriter keeps the last n bytes written.
type tailWriter struct {
	n int
	b []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.n {
		t.b = append(t.b[:0], t.b[len(t.b)-t.n:]...)
	}
	return len(p), nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// exportSync runs the tenant's active mirror-mode upstreams of the plan's sources on this process
// and waits for them; a run another replica holds is waited for, then requested again. A lazy
// upstream's run fetches the cells the plan asks of its source (spec 0032).
func (s *Service) exportSync(ctx context.Context, a authz.Actor, t store.Tenant, ups []store.Upstream, plan *bundle.Plan, holder string) error {
	for _, u := range ups {
		if u.Mode != store.ModeMirror || u.State != store.UpstreamActive {
			continue
		}
		var only map[[3]string]bool
		found := false
		for _, ps := range plan.Sources {
			if ps.Kind != u.Kind || u.Kind == store.UpstreamRepo && ps.Prefix != u.Prefix {
				continue
			}
			found = true
			if u.FetchMode == store.FetchLazy {
				if only == nil {
					only = map[[3]string]bool{}
				}
				for _, pc := range ps.Cells {
					only[[3]string{pc.DuckDBVersion, pc.Platform, pc.Name}] = true
				}
			}
		}
		if !found {
			continue
		}
		if err := s.runNow(ctx, a, t.Name, u, holder, only); err != nil {
			return fmt.Errorf("upstream %s: %w", u.Name, err)
		}
	}
	return nil
}

// syncPoll is how often runNow looks again while another replica runs the upstream.
var syncPoll = 2 * time.Second

// runNow requests a run of an upstream and runs it here under its lease; only, for a lazy upstream,
// is the cells it fetches besides those asked for and its eager entries. A request another replica
// takes first runs as a sync: the whole matrix.
func (s *Service) runNow(ctx context.Context, a authz.Actor, tenant string, u store.Upstream, holder string, only map[[3]string]bool) error {
	if _, err := s.Sync(ctx, a, tenant, u.Name, false); err != nil {
		return err
	}
	lease := "kista/upstream/" + u.ID
	for {
		held, err := s.Store.AcquireLease(ctx, lease, holder, leaseTTL)
		if err != nil {
			return err
		}
		if held {
			renew := func(ctx context.Context) error {
				if ok, err := s.Store.AcquireLease(ctx, lease, holder, leaseTTL); err != nil || !ok {
					return errors.New("upstream: the run's lease was lost")
				}
				return nil
			}
			res, err := s.runUpstream(ctx, u.ID, renew, only)
			_ = s.Store.ReleaseLease(context.WithoutCancel(ctx), lease, holder)
			switch {
			case errors.Is(err, ErrNotDue): // another replica took the request and finished it, or a pause
				if paused, err := s.paused(ctx, u.ID); err != nil || paused {
					return cmpErr(err, ErrPaused)
				}
				return nil
			case err != nil:
				return err
			case res.Error != "":
				return fmt.Errorf("the run stopped: %s", res.Error)
			case res.Counts[busy] > 0: // a plan's cell another fetch held is not current yet
				return fmt.Errorf("%d cells were being fetched meanwhile; export again", res.Counts[busy])
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(syncPoll):
		}
	}
}

func cmpErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
