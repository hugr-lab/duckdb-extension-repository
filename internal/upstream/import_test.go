package upstream_test

import (
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

const bundlePrefix = "https://src.example.org/acme/prod"

// bcell is a cell of a test bundle: with a file it is fetched, else its outcome.
type bcell struct {
	v, name   string
	file      []byte
	outcome   string
	fetchedAt time.Time
	auth      bool
	bodyHash  string
	detail    string
}

type bundler struct {
	kind    string // the source's kind: repository (bundlePrefix) unless set
	corrupt bool   // damage each gzip entry's trailer (its CRC): media gone bad
	t       *testing.T
	priv    ed25519.PrivateKey
	fp      string
	wk      []byte
	created time.Time
	feeds   map[string][]byte // feed entries the edit names, written after the files
}

func (en *env) bundler(t *testing.T) *bundler {
	t.Helper()
	pemKey, fp, err := bundle.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := bundle.ParsePrivateKey(pemKey)
	if err != nil {
		t.Fatal(err)
	}
	wk, _ := json.Marshal(map[string]any{"signature_keys": en.repo.keys})
	// a minute ahead (within the future skew): the plan is never older than the upstreams the test adds
	return &bundler{t: t, priv: priv, fp: fp, wk: wk, created: time.Now().UTC().Truncate(time.Second).Add(time.Minute)}
}

func hexSHA(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// make writes a signed bundle of one repository source (bundlePrefix) with the cells; edit changes
// the manifest before it is signed. It returns the bundle and its manifest's id.
func (bu *bundler) make(cells []bcell, edit func(*bundle.Manifest)) ([]byte, string) {
	t := bu.t
	t.Helper()
	id, _ := uuid.NewV7()
	bu.created = bu.created.Add(time.Second) // each bundle after the last
	m := bundle.Manifest{Format: bundle.ManifestFormat, ID: id.String(), CreatedAt: bu.created,
		Exporter: bundle.Exporter{Version: "kista test", PublicURL: "https://gw.example.org"},
		Plan:     bundle.PlanRef{Tenant: "acme", CreatedAt: bu.created, SHA256: strings.Repeat("a", 64)}}
	src := bundle.Source{Kind: store.UpstreamRepo, Prefix: bundlePrefix,
		WellKnown: &bundle.FileRef{Path: "sources/1/well-known.json", SHA256: hexSHA(bu.wk), Size: int64(len(bu.wk))}}
	if bu.kind != "" {
		src = bundle.Source{Kind: bu.kind}
	}
	files := map[string][]byte{}
	for _, c := range cells {
		bc := bundle.Cell{DuckDBVersion: c.v, Platform: "linux_amd64", Name: c.name, Outcome: c.outcome, FetchedAt: c.fetchedAt,
			Authenticated: c.auth, BodyHash: c.bodyHash, Detail: c.detail}
		if bc.FetchedAt.IsZero() && (c.file != nil || c.outcome == bundle.Unchanged) {
			bc.FetchedAt = bu.created
		}
		if c.file != nil {
			ef, err := extfile.Open(bytes.NewReader(c.file), int64(len(c.file)), 1<<30)
			if err != nil {
				t.Fatal(err)
			}
			var z bytes.Buffer
			zw := gzip.NewWriter(&z)
			_, _ = zw.Write(c.file)
			_ = zw.Close()
			bc.Outcome, bc.File, bc.SHA256 = bundle.Fetched, "files/"+hexSHA(c.file)+".gz", hexSHA(c.file)
			bc.Size, bc.InflatedSize, bc.BodyHash = int64(z.Len()), int64(len(c.file)), ef.Hash.String()
			bc.ETag = `"e-` + hexSHA(c.file)[:8] + `"`
			gz := z.Bytes()
			if bu.corrupt {
				gz[len(gz)-6] ^= 0xff
			}
			files[bc.File] = gz
		}
		src.Cells = append(src.Cells, bc)
	}
	m.Sources = []bundle.Source{src}
	bundle.Canonical(m.Sources)
	if edit != nil {
		edit(&m)
	}
	md, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w := bundle.NewWriter(&out)
	if err := bundle.WriteHead(w, md, bu.priv); err != nil {
		t.Fatal(err)
	}
	if m.Sources[0].WellKnown != nil {
		_ = w.Entry(m.Sources[0].WellKnown.Path, int64(len(bu.wk)), bytes.NewReader(bu.wk))
	}
	for _, p := range m.FileOrder() {
		if f, ok := files[p]; ok {
			_ = w.Entry(p, int64(len(f)), bytes.NewReader(f))
		}
	}
	for _, f := range m.Feeds {
		for _, e := range f.Entries {
			_ = w.Entry(e.Path, int64(len(bu.feeds[e.Path])), bytes.NewReader(bu.feeds[e.Path]))
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), m.ID
}

func (en *env) bundleSpec(name string, entries ...string) upstream.Spec {
	sp := upstream.Spec{Name: name, Kind: store.UpstreamRepo, Prefix: bundlePrefix, Channel: "prod", Mode: store.ModeBundle,
		Visibility: store.Public, Keys: []string{en.fp}, Platforms: []string{"linux_amd64"}}
	for _, e := range entries {
		sp.Entries = append(sp.Entries, store.UpstreamEntry{Name: e})
	}
	return sp
}

func (en *env) bundleUpstream(t *testing.T, name string, entries ...string) store.Upstream {
	t.Helper()
	u, err := en.up.Add(ctx, admin, "acme", en.bundleSpec(name, entries...))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (en *env) importB(t *testing.T, bu *bundler, data []byte, o upstream.ImportOptions) upstream.ImportResult {
	t.Helper()
	if o.BundleKeys == nil {
		o.BundleKeys = []string{bu.fp}
	}
	if o.Holder == "" {
		o.Holder = "test"
	}
	res, err := en.up.Import(ctx, admin, "acme", bytes.NewReader(data), o)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return res
}

func target(t *testing.T, res upstream.ImportResult, name string) upstream.TargetResult {
	t.Helper()
	for _, tr := range res.Targets {
		if tr.Upstream == name {
			return tr
		}
	}
	t.Fatalf("no target %s in %+v", name, res.Targets)
	return upstream.TargetResult{}
}

func (en *env) cellOf(t *testing.T, upstreamName, name, v string) store.UpstreamCell {
	t.Helper()
	cs, err := en.up.Cells(ctx, admin, "acme", upstreamName, "", [3]string{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Name == name && c.DuckDBVersion == v {
			return c
		}
	}
	return store.UpstreamCell{}
}

func (en *env) events(t *testing.T, kind string) []store.Event {
	t.Helper()
	tn, err := en.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := en.st.ListEvents(ctx, tn.ID, store.EventFilter{Kind: kind, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// Spec 0014: an import takes a signed bundle's files into the bundle-mode upstreams it targets,
// as a run takes in fetched files, with the bundle in provenance, events and the import record;
// the same bundle again releases nothing; freshness refuses older and altered bundles, and an
// older build for a cell holding a newer one.
func TestImport(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		bu := en.bundler(t)
		en.bundleUpstream(t, "air", "x", "y", "z", "w")
		x1 := signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "x_duckdb_cpp_init")
		cells := []bcell{{v: "v2.0.0", name: "x", file: x1}, {v: "v2.0.0", name: "y", outcome: bundle.Missing, detail: "not at the source"},
			{v: "v2.0.0", name: "z", outcome: bundle.Rejected, detail: "the signature fails"},
			{v: "v2.0.0", name: "w", outcome: bundle.Unchanged, bodyHash: strings.Repeat("c", 64)},
			{v: "v2.0.0", name: "unasked", outcome: bundle.Missing}}
		data, id := bu.make(cells, nil)
		at := bu.created

		// refusals before anything: an unpinned key, another tenant (an unsigned bundle: internal/bundle)
		if _, err := en.up.Import(ctx, admin, "acme", bytes.NewReader(data), upstream.ImportOptions{BundleKeys: []string{"sha256:" + strings.Repeat("0", 64)}, Holder: "t"}); err == nil || !strings.Contains(err.Error(), "not pinned") {
			t.Errorf("an unpinned bundle key: %v", err)
		}
		other, _ := bu.make(cells, func(m *bundle.Manifest) { m.Plan.Tenant = "beta" })
		if _, err := en.up.Import(ctx, admin, "acme", bytes.NewReader(other), upstream.ImportOptions{BundleKeys: []string{bu.fp}, Holder: "t"}); !errors.Is(err, upstream.ErrBundleTenant) {
			t.Errorf("another tenant's bundle: %v", err)
		}
		if is, _ := en.up.Imports(ctx, admin, "acme", 10); len(is) != 0 {
			t.Errorf("a refused bundle left a record: %+v", is)
		}

		// a dry run checks everything and changes nothing but the run's record
		res := en.importB(t, bu, data, upstream.ImportOptions{DryRun: true})
		if tr := target(t, res, "air"); tr.Status != upstream.TargetImported || tr.Result.Counts["would_release"] != 1 {
			t.Errorf("dry run: %+v", tr)
		}
		if len(en.releases(t, "x")) != 0 || en.cellOf(t, "air", "x", "v2.0.0").Outcome != "" {
			t.Error("a dry run released or recorded")
		}
		if u, _ := en.up.Get(ctx, admin, "acme", "air"); u.BundleID != "" || u.LastRunAt.IsZero() {
			t.Errorf("a dry run's mark and run: %q %v", u.BundleID, u.LastRunAt)
		}

		res = en.importB(t, bu, data, upstream.ImportOptions{})
		tr := target(t, res, "air")
		if tr.Status != upstream.TargetImported || tr.Result.Counts[store.CellReleased] != 1 || tr.Result.Counts[store.CellMissing] != 2 ||
			tr.Result.Counts[store.CellRejected] != 1 || res.Unplanned != 1 || res.Import.State != store.ImportDone {
			t.Fatalf("import: %+v %+v", res, tr)
		}
		if res.Import.InputSHA256 != hexSHA(data) {
			t.Errorf("input sha256 %s, want %s", res.Import.InputSHA256, hexSHA(data))
		}
		rs := en.releases(t, "x")
		if len(rs) != 1 || rs[0].Seq == 0 || rs[0].Visibility != store.Public {
			t.Fatalf("releases: %+v", rs)
		}
		var prov map[string]string
		mustJSON(t, rs[0].Provenance, &prov)
		if prov["bundle"] != id || prov["imported_at"] == "" || prov["url"] != bundlePrefix+"/v2.0.0/linux_amd64/x.duckdb_extension.gz" ||
			prov["key"] != en.fp || prov["credential"] != "" {
			t.Errorf("provenance: %v", prov)
		}
		c := en.cellOf(t, "air", "x", "v2.0.0")
		if c.Outcome != store.CellReleased || c.ETag == "" || c.BodyHash == "" || !c.FetchedAt.Equal(at) {
			t.Errorf("the cell: %+v", c)
		}
		if c := en.cellOf(t, "air", "z", "v2.0.0"); c.Outcome != store.CellRejected || c.Detail != "bundle: the signature fails" {
			t.Errorf("a rejected cell: %+v", c)
		}
		if c := en.cellOf(t, "air", "w", "v2.0.0"); c.Outcome != store.CellMissing || !strings.Contains(c.Detail, "not held") {
			t.Errorf("an unchanged cell the site does not hold: %+v", c)
		}
		if u, _ := en.up.Get(ctx, admin, "acme", "air"); u.BundleID != id {
			t.Errorf("the mark: %q", u.BundleID)
		}
		for kind, want := range map[string]int{"bundle.import": 2, "upstream.run": 2, "upstream.release": 1, "upstream.rejected": 1} {
			evs := en.events(t, kind)
			if len(evs) != want {
				t.Errorf("%s: %d events, want %d", kind, len(evs), want)
				continue
			}
			if kind != "upstream.rejected" && !strings.Contains(evs[0].Data, id) {
				t.Errorf("%s does not name the bundle: %s", kind, evs[0].Data)
			}
		}
		if ev := en.events(t, "bundle.import")[0]; !strings.Contains(ev.Data, `"state":"done"`) || !strings.Contains(ev.Data, hexSHA(data)) ||
			!strings.Contains(ev.Data, `"air":{`) {
			t.Errorf("bundle.import: %s", ev.Data)
		}
		is, err := en.up.Imports(ctx, admin, "acme", 10)
		if err != nil || len(is) != 2 || is[0].ManifestID != id || is[0].BundleKey != bu.fp || is[0].DryRun || !is[1].DryRun {
			t.Fatalf("imports: %+v %v", is, err)
		}
		if got, err := en.up.GetImport(ctx, admin, "acme", is[0].ID); err != nil || got.State != store.ImportDone || !strings.Contains(got.Counts, `"released":1`) {
			t.Errorf("show: %+v %v", got, err)
		}

		// the same bundle again (a resume): nothing new
		res = en.importB(t, bu, data, upstream.ImportOptions{})
		if tr := target(t, res, "air"); tr.Result.Counts[store.CellUnchanged] != 1 || tr.Result.Counts[store.CellReleased] != 0 {
			t.Errorf("again: %+v", tr)
		}
		// the same id with another manifest, and an older bundle: stale
		altered, _ := bu.make(cells, func(m *bundle.Manifest) { m.ID = id })
		if tr := target(t, en.importB(t, bu, altered, upstream.ImportOptions{}), "air"); tr.Status != upstream.TargetStale {
			t.Errorf("the same id altered: %+v", tr)
		}
		older, _ := bu.make(cells, func(m *bundle.Manifest) {
			m.CreatedAt = m.CreatedAt.Add(-time.Hour)
			for i, c := range m.Sources[0].Cells {
				if !c.FetchedAt.IsZero() {
					m.Sources[0].Cells[i].FetchedAt = m.CreatedAt
				}
			}
		})
		res = en.importB(t, bu, older, upstream.ImportOptions{})
		if tr := target(t, res, "air"); tr.Status != upstream.TargetStale || res.Import.State != store.ImportFailed {
			t.Errorf("an older bundle: %+v", res)
		}
		// a newer bundle bringing an older build for x: the cell keeps its body
		x0 := signed(t, en.key, cpp("0.9", "v2.0.0"), 2, "x_duckdb_cpp_init")
		lagging, _ := bu.make([]bcell{{v: "v2.0.0", name: "x", file: x0, fetchedAt: bu.created.Add(-time.Hour)}}, nil)
		if tr := target(t, en.importB(t, bu, lagging, upstream.ImportOptions{}), "air"); tr.Result.Counts["stale"] != 1 {
			t.Errorf("an older build: %+v", tr)
		}
		if len(en.releases(t, "x")) != 1 {
			t.Error("an older build was released")
		}
		// a newer build is released and made current
		x2 := signed(t, en.key, cpp("1.1", "v2.0.0"), 3, "x_duckdb_cpp_init")
		newer, _ := bu.make([]bcell{{v: "v2.0.0", name: "x", file: x2}}, nil)
		if tr := target(t, en.importB(t, bu, newer, upstream.ImportOptions{}), "air"); tr.Result.Counts[store.CellReleased] != 1 {
			t.Errorf("a newer build: %+v", tr)
		}
		rs = en.releases(t, "x")
		var top store.Candidate
		for _, r := range rs {
			if r.Seq > top.Seq {
				top = r
			}
		}
		if len(rs) != 2 || top.ExtVersion != "1.1" {
			t.Errorf("the newer build is not current: %+v", rs)
		}

		// reset clears the mark: an older bundle is taken again
		if _, err := en.up.ResetBundle(ctx, admin, "acme", "air"); err != nil {
			t.Fatal(err)
		}
		if u, _ := en.up.Get(ctx, admin, "acme", "air"); u.BundleID != "" {
			t.Errorf("reset left %q", u.BundleID)
		}
		if tr := target(t, en.importB(t, bu, older, upstream.ImportOptions{}), "air"); tr.Status != upstream.TargetImported {
			t.Errorf("after a reset: %+v", tr)
		}
		if len(en.events(t, "bundle.reset")) != 1 {
			t.Error("no bundle.reset event")
		}
	})
}

// Spec 0014: a target's keys come from the bundle's .well-known entry, filtered by its pins; a
// leased target is busy; a plan older than the upstream is stale; an authenticated cell is private
// unless upstreams.bundle_sources allows it.
func TestImportTargets(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		bu := en.bundler(t)
		en.bundleUpstream(t, "air", "x")
		x1 := signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "x_duckdb_cpp_init")

		// a plan made before the upstream
		old, _ := bu.make([]bcell{{v: "v2.0.0", name: "x", file: x1}}, func(m *bundle.Manifest) { m.Plan.CreatedAt = m.Plan.CreatedAt.Add(-time.Hour) })
		if tr := target(t, en.importB(t, bu, old, upstream.ImportOptions{}), "air"); tr.Status != upstream.TargetStale {
			t.Errorf("a plan older than the upstream: %+v", tr)
		}
		// keys that list none of the pins
		saved := bu.wk
		bu.wk = []byte(`{"signature_keys": []}`)
		nokeys, _ := bu.make([]bcell{{v: "v2.0.0", name: "x", file: x1}}, nil)
		bu.wk = saved
		if tr := target(t, en.importB(t, bu, nokeys, upstream.ImportOptions{}), "air"); tr.Status != upstream.TargetFailed ||
			!strings.Contains(tr.Detail, "upstream.keys") {
			t.Errorf("no pinned key: %+v", tr)
		}
		// busy: another holder has the lease
		u, _ := en.up.Get(ctx, admin, "acme", "air")
		if ok, err := en.st.AcquireLease(ctx, "kista/upstream/"+u.ID, "other", time.Minute); !ok || err != nil {
			t.Fatal(ok, err)
		}
		auth, _ := bu.make([]bcell{{v: "v2.0.0", name: "x", file: x1, auth: true}}, nil)
		if tr := target(t, en.importB(t, bu, auth, upstream.ImportOptions{}), "air"); tr.Status != upstream.TargetBusy {
			t.Errorf("busy: %+v", tr)
		}
		_ = en.st.ReleaseLease(ctx, "kista/upstream/"+u.ID, "other")
		// an authenticated cell: private, with the pseudo-credential
		if tr := target(t, en.importB(t, bu, auth, upstream.ImportOptions{}), "air"); tr.Result.Counts[store.CellReleased] != 1 {
			t.Fatalf("authenticated: %+v", tr)
		}
		rs := en.releases(t, "x")
		var prov map[string]string
		mustJSON(t, rs[0].Provenance, &prov)
		if len(rs) != 1 || rs[0].Visibility != store.Private || prov["credential"] != "bundle:"+bundlePrefix {
			t.Fatalf("an authenticated cell's release: %+v %v", rs, prov)
		}
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "", rs[0].ID, "public", 0); err == nil {
			t.Error("made public without bundle_sources")
		}
		en.rel.MayPublish = func(tenant, cred string) bool { return tenant == "acme" && cred == "bundle:"+bundlePrefix }
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "", rs[0].ID, "public", 0); err != nil {
			t.Errorf("bundle_sources allows it: %v", err)
		}
	})
}

// Spec 0014: a bundle cut inside a file keeps what came before, fails the cells whose files never
// came, and marks the import failed; an input that stalls ends the import.
func TestImportTruncatedAndStalled(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		bu := en.bundler(t)
		en.bundleUpstream(t, "air", "a", "b")
		a := signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "a_duckdb_cpp_init")
		b := signed(t, en.key, cpp("1.0", "v2.0.0"), 50000, "b_duckdb_cpp_init")
		data, _ := bu.make([]bcell{{v: "v2.0.0", name: "a", file: a}, {v: "v2.0.0", name: "b", file: b}}, nil)
		res := en.importB(t, bu, data[:len(data)-2048], upstream.ImportOptions{})
		tr := target(t, res, "air")
		if res.Problem == "" || res.Import.State != store.ImportFailed || res.Import.InputSHA256 != "" ||
			tr.Result.Counts[store.CellReleased] != 1 || tr.Result.Counts[store.CellFailed] != 1 {
			t.Fatalf("truncated: %+v %+v", res, tr)
		}
		if len(en.releases(t, "a")) != 1 || len(en.releases(t, "b")) != 0 {
			t.Error("truncated: the releases")
		}
		// the rest of the same bundle resumes
		res = en.importB(t, bu, data, upstream.ImportOptions{})
		if tr := target(t, res, "air"); tr.Result.Counts[store.CellReleased] != 1 || tr.Result.Counts[store.CellUnchanged] != 1 {
			t.Errorf("resumed: %+v", tr)
		}
		// an input that says nothing after the head
		next, _ := bu.make([]bcell{{v: "v2.0.0", name: "a", file: a}}, nil)
		pr, pw := io.Pipe()
		go func() { _, _ = pw.Write(next[:len(next)-1024]) }() // then nothing: no end-of-archive blocks, never closed
		start := time.Now()
		got, err := en.up.Import(ctx, admin, "acme", pr, upstream.ImportOptions{BundleKeys: []string{bu.fp}, Holder: "t",
			Idle: 200 * time.Millisecond, Floor: time.Hour})
		if err != nil || !strings.Contains(got.Problem, "stalled") || got.Import.State != store.ImportFailed || time.Since(start) > 30*time.Second ||
			target(t, got, "air").Result.Counts[store.CellUnchanged] != 1 {
			t.Errorf("stalled: %+v %v", got, err)
		}
		u, _ := en.up.Get(ctx, admin, "acme", "air")
		if ok, _ := en.st.AcquireLease(ctx, "kista/upstream/"+u.ID, "other", time.Minute); !ok {
			t.Error("a stalled import kept its lease")
		}
	})
}

// Spec 0014: the no-downgrade rule holds through missing and rejected outcomes (the cell keeps the
// newest body it held), and damaged media fail a cell without dropping its record.
func TestImportNoDowngrade(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		bu := en.bundler(t)
		en.bundleUpstream(t, "air", "x")
		x1 := signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "x_duckdb_cpp_init")
		first, _ := bu.make([]bcell{{v: "v2.0.0", name: "x", file: x1}}, nil)
		if tr := target(t, en.importB(t, bu, first, upstream.ImportOptions{}), "air"); tr.Result.Counts[store.CellReleased] != 1 {
			t.Fatalf("first: %+v", tr)
		}
		held := en.cellOf(t, "air", "x", "v2.0.0")
		// damaged media: failed, the record stays
		bu.corrupt = true
		x2 := signed(t, en.key, cpp("1.1", "v2.0.0"), 2, "x_duckdb_cpp_init")
		damaged, _ := bu.make([]bcell{{v: "v2.0.0", name: "x", file: x2}}, nil)
		bu.corrupt = false
		if tr := target(t, en.importB(t, bu, damaged, upstream.ImportOptions{}), "air"); tr.Result.Counts[store.CellFailed] != 1 {
			t.Errorf("damaged: %+v", tr)
		}
		if c := en.cellOf(t, "air", "x", "v2.0.0"); c.Outcome != store.CellReleased || c.BodyHash != held.BodyHash {
			t.Errorf("damaged media replaced the record: %+v", c)
		}
		// a bundle marks x missing, the next brings an older build: stale
		gone, _ := bu.make([]bcell{{v: "v2.0.0", name: "x", outcome: bundle.Missing}}, nil)
		en.importB(t, bu, gone, upstream.ImportOptions{})
		if c := en.cellOf(t, "air", "x", "v2.0.0"); c.Outcome != store.CellMissing || c.BodyHash != held.BodyHash {
			t.Errorf("missing dropped the high-water mark: %+v", c)
		}
		x0 := signed(t, en.key, cpp("0.9", "v2.0.0"), 3, "x_duckdb_cpp_init")
		older, _ := bu.make([]bcell{{v: "v2.0.0", name: "x", file: x0, fetchedAt: held.FetchedAt.Add(-time.Hour)}}, nil)
		if tr := target(t, en.importB(t, bu, older, upstream.ImportOptions{}), "air"); tr.Result.Counts["stale"] != 1 {
			t.Errorf("an older build after missing: %+v", tr)
		}
		if rs := en.releases(t, "x"); len(rs) != 1 {
			t.Errorf("an older build was released: %+v", rs)
		}
	})
}

// Spec 0014: the targets: a named upstream that is missing, paused, not in bundle mode or without
// a source refuses the import, as does a tenant none of whose upstreams the bundle serves; two
// upstreams of one source both take its files; a core source fills a passthrough channel with
// DuckDB's keys.
func TestImportSelection(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		bu := en.bundler(t)
		x1 := signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "x_duckdb_cpp_init")
		data, _ := bu.make([]bcell{{v: "v2.0.0", name: "x", file: x1}}, nil)
		imp := func(names ...string) error {
			_, err := en.up.Import(ctx, admin, "acme", bytes.NewReader(data), upstream.ImportOptions{Upstreams: names,
				BundleKeys: []string{bu.fp}, Holder: "t"})
			return err
		}
		if err := imp(); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("no target: %v", err)
		}
		en.bundleUpstream(t, "air", "x")
		sp := en.bundleSpec("air2", "x")
		sp.Channel = "other"
		must(t, func() error { _, err := en.up.Add(ctx, admin, "acme", sp); return err })
		paused := en.bundleSpec("air3", "y")
		paused.Channel = "other"
		u, err := en.up.Add(ctx, admin, "acme", paused)
		if err != nil {
			t.Fatal(err)
		}
		must(t, func() error {
			_, err := en.up.Set(ctx, admin, "acme", "air3", "", store.UpstreamPaused, u.Version)
			return err
		})
		elsewhere := en.bundleSpec("far", "z")
		elsewhere.Prefix = "https://elsewhere.example.org/acme/prod"
		must(t, func() error { _, err := en.up.Add(ctx, admin, "acme", elsewhere); return err })
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "w"})); err != nil {
			t.Fatal(err)
		}
		for name, want := range map[string]error{"nope": store.ErrNotFound, "air3": store.ErrInvalid, "far": store.ErrInvalid,
			"acme-repo": store.ErrInvalid} {
			if err := imp(name); !errors.Is(err, want) {
				t.Errorf("-upstream %s: %v", name, err)
			}
		}
		if is, _ := en.up.Imports(ctx, admin, "acme", 10); len(is) != 0 {
			t.Errorf("refusals left records: %+v", is)
		}
		res := en.importB(t, bu, data, upstream.ImportOptions{})
		if len(res.Targets) != 2 || target(t, res, "air").Result.Counts[store.CellReleased] != 1 ||
			target(t, res, "air2").Result.Counts[store.CellReleased] != 1 {
			t.Errorf("two upstreams of one source: %+v", res.Targets)
		}
		if len(en.releasesIn(t, "other", "x")) != 1 {
			t.Error("the second channel")
		}

		// a core source: DuckDB's keys (here the test's), a passthrough channel, public
		en.up.DuckDB.CoreKeys = []*rsa.PublicKey{&en.key.PublicKey}
		must(t, func() error {
			_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "mirror", []string{"v2.0.0"}, nil)
			return err
		})
		must(t, func() error {
			_, err := en.up.Add(ctx, admin, "acme", upstream.Spec{Name: "core", Kind: store.UpstreamCore, Channel: "mirror",
				Mode: store.ModeBundle, Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "json"}}})
			return err
		})
		bu.kind = store.UpstreamCore
		core, _ := bu.make([]bcell{{v: "v2.0.0", name: "json", file: signed(t, en.key, cpp("1.0", "v2.0.0"), 4, "json_duckdb_cpp_init")}}, nil)
		res = en.importB(t, bu, core, upstream.ImportOptions{})
		if tr := target(t, res, "core"); tr.Result.Counts[store.CellReleased] != 1 {
			t.Fatalf("core: %+v", res)
		}
		rs := en.releasesIn(t, "mirror", "json")
		var prov map[string]string
		if len(rs) == 1 {
			mustJSON(t, rs[0].Provenance, &prov)
		}
		if len(rs) != 1 || rs[0].Visibility != store.Public || !strings.HasSuffix(prov["url"], "/v2.0.0/linux_amd64/json.duckdb_extension.gz") {
			t.Errorf("core into passthrough: %+v %v", rs, prov)
		}
	})
}

// Spec 0014: a running record whose lease expired shows failed (interrupted), and the next import
// of the tenant rewrites it; an input under the rate floor ends the import.
func TestImportInterruptedAndSlow(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		bu := en.bundler(t)
		en.bundleUpstream(t, "air", "x")
		tn, _ := en.st.GetTenant(ctx, "acme")
		rec := store.BundleImport{TenantID: tn.ID, ManifestID: "m", ManifestCreatedAt: time.Now(), ManifestSHA256: strings.Repeat("a", 64),
			Exporter: "{}", BundleKey: bu.fp, Actor: "os:1:t"}
		must(t, func() error {
			return en.st.InTenantTx(ctx, tn.ID, nil, func(tx *store.Tx) error { return tx.InsertBundleImport(ctx, &rec) })
		})
		if got, err := en.up.GetImport(ctx, admin, "acme", rec.ID); err != nil || got.State != store.ImportFailed || !got.Interrupted {
			t.Errorf("an interrupted record: %+v %v", got, err)
		}
		x1 := signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "x_duckdb_cpp_init")
		data, _ := bu.make([]bcell{{v: "v2.0.0", name: "x", file: x1}}, nil)
		en.importB(t, bu, data, upstream.ImportOptions{})
		if got, _ := en.st.GetBundleImport(ctx, tn.ID, rec.ID); got.State != store.ImportFailed || got.FinishedAt.IsZero() {
			t.Errorf("not rewritten: %+v", got)
		}
		// a trickle: under the rate floor once the floor's wait has passed
		pr, pw := io.Pipe()
		go func() {
			for _, b := range data {
				if _, err := pw.Write([]byte{b}); err != nil {
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		defer pw.Close()
		_, err := en.up.Import(ctx, admin, "acme", pr, upstream.ImportOptions{BundleKeys: []string{bu.fp}, Holder: "t",
			Idle: time.Minute, Floor: 300 * time.Millisecond})
		if err == nil || !strings.Contains(err.Error(), "bytes per second") {
			t.Errorf("a trickle: %v", err)
		}
	})
}

// Spec 0014 phase 1b, part 2: a bundle carrying feeds imports its files as before; its feed entries
// are read through (they are imported from part 4).
func TestImportReadsFeedsThrough(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		bu := en.bundler(t)
		en.bundleUpstream(t, "air", "x")
		keys := []byte(`{"keys":[]}`)
		batch, _ := bundle.MarshalBatch(nil)
		bu.feeds = map[string][]byte{"feeds/1/attestation-keys.json": keys, "feeds/1/batch-1.json": batch}
		x1 := signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "x_duckdb_cpp_init")
		data, _ := bu.make([]bcell{{v: "v2.0.0", name: "x", file: x1}}, func(m *bundle.Manifest) {
			m.Feeds = []bundle.Feed{{URL: "https://gw.example.org/api/v1/tenants/reviews", Entries: []bundle.FileRef{
				{Path: "feeds/1/attestation-keys.json", SHA256: hexSHA(keys), Size: int64(len(keys))},
				{Path: "feeds/1/batch-1.json", SHA256: hexSHA(batch), Size: int64(len(batch))}}, Bodies: []bundle.FeedBody{}}}
		})
		res := en.importB(t, bu, data, upstream.ImportOptions{})
		if tr := target(t, res, "air"); tr.Status != upstream.TargetImported || tr.Result.Counts[store.CellReleased] != 1 || res.Feeds != 1 {
			t.Fatalf("import: %+v %d feeds", tr, res.Feeds)
		}
	})
}
