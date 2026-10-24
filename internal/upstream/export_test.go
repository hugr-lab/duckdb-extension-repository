package upstream_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// gateway is an export's world: acme mirrors an https repository (a plan's prefixes are https) into
// prod through "acme-repo"; site is a second tenant whose bundle-mode upstream "air" plans the same
// source and imports what acme exports.
type gateway struct {
	*env
	tokenFile string
	priv      ed25519.PrivateKey
	fp        string // the bundle key's
	files     map[string][]byte
}

func newGateway(t *testing.T, e storetest.Engine) *gateway {
	t.Helper()
	en, tokenFile := privateEnv(t, e)
	pemKey, fp, err := bundle.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := bundle.ParsePrivateKey(pemKey)
	if err != nil {
		t.Fatal(err)
	}
	must(t, func() error { _, err := en.ten.CreateTenant(ctx, admin, "site", "", "", ""); return err })
	must(t, func() error {
		_, err := en.ten.CreateChannel(ctx, admin, "site", "prod", store.ChannelSigned)
		return err
	})
	must(t, func() error {
		_, err := en.ten.SetChannelVersions(ctx, admin, "site", "prod", []string{"v2.0.0"}, nil)
		return err
	})
	if err := signer.GenerateKeyFile(filepath.Join(en.dir, "site.pem")); err != nil {
		t.Fatal(err)
	}
	must(t, func() error { _, err := en.ks.Add(ctx, admin, "site", "prod", "file:site.pem", true); return err })
	return &gateway{env: en, tokenFile: tokenFile, priv: priv, fp: fp, files: map[string][]byte{}}
}

// put serves a version of a name at the repository, remembering the file.
func (g *gateway) put(t *testing.T, name, ver string, pad int) []byte {
	f := signed(t, g.key, cpp(ver, "v2.0.0"), pad, name+"_duckdb_cpp_init")
	g.repo.put("v2.0.0", "linux_amd64", name, f)
	g.files[name+"@"+ver] = f
	return f
}

func (g *gateway) air(t *testing.T, entries ...string) {
	t.Helper()
	sp := g.bundleSpec("air", entries...)
	sp.Prefix = g.repo.srv.URL
	if _, err := g.up.Add(ctx, admin, "site", sp); err != nil {
		t.Fatal(err)
	}
}

func (g *gateway) plan(t *testing.T) bundle.Plan {
	t.Helper()
	p, err := g.up.Plan(ctx, admin, "site", upstream.PlanTargets{}, []string{g.fp})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func planBytes(t *testing.T, p bundle.Plan) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (g *gateway) export(t *testing.T, p bundle.Plan, o upstream.ExportOptions) (upstream.ExportResult, []byte) {
	t.Helper()
	o.Plan = planBytes(t, p)
	if o.Key == nil {
		o.Key = g.priv
	}
	if o.Holder == "" {
		o.Holder = "test"
	}
	if o.Version == "" {
		o.Version = "kista test"
	}
	var buf bytes.Buffer
	res, err := g.up.Export(ctx, admin, "acme", &buf, o)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if res.SHA256 != hexSHA(buf.Bytes()) {
		t.Fatalf("the reported sha256 %s is not the bundle's", res.SHA256)
	}
	return res, buf.Bytes()
}

func (g *gateway) importSite(t *testing.T, data []byte) upstream.ImportResult {
	t.Helper()
	res, err := g.up.Import(ctx, admin, "site", bytes.NewReader(data), upstream.ImportOptions{BundleKeys: []string{g.fp}, Holder: "test"})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return res
}

// cells reads a bundle: its manifest's cells by name, and each file inflated by path.
func (g *gateway) open(t *testing.T, data []byte) (*bundle.Manifest, map[string]bundle.Cell, map[string][]byte) {
	t.Helper()
	b, err := bundle.Open(bytes.NewReader(data), time.Now(), func(fp string) bool { return fp == g.fp })
	if err != nil {
		t.Fatal(err)
	}
	cells := map[string]bundle.Cell{}
	for _, s := range b.Manifest.Sources {
		for _, c := range s.Cells {
			cells[c.Name] = c
		}
	}
	files := map[string][]byte{}
	for {
		it, err := b.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if it.File == "" {
			continue
		}
		var out bytes.Buffer
		if _, _, err := bundle.Inflate(it.R, 1<<30, &out); err != nil {
			t.Fatal(err)
		}
		files[it.File] = out.Bytes()
	}
	if !b.Ended() || len(b.Missing()) != 0 {
		t.Fatalf("the bundle is incomplete: %v", b.Missing())
	}
	return b.Manifest, cells, files
}

func outcomes(r upstream.ExportResult) map[string]int {
	out := map[string]int{}
	for _, s := range r.Sources {
		for k, n := range s.Counts {
			out[k] += n
		}
	}
	return out
}

// Spec 0014: a connected kista exports what its mirror holds for a site's plan, each file as its
// source served it, and the site imports it.
func TestExport(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			// a credentialed mirror: its releases are private and authenticated
			g.repo.token = "T-sub"
			if err := os.WriteFile(g.tokenFile, []byte("T-sub\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			g.put(t, "tresor", "1.0", 1)
			g.put(t, "acl", "1.0", 2)
			sp := g.spec(store.UpstreamEntry{Name: "tresor"}, store.UpstreamEntry{Name: "acl"}, store.UpstreamEntry{Name: "gone"})
			sp.Credential = "sub"
			if _, err := g.up.Add(ctx, admin, "acme", sp); err != nil {
				t.Fatal(err)
			}
			g.sync(t, false)
			g.air(t, "tresor", "acl", "gone", "zzz")
			plan := g.plan(t)
			prefix := plan.Sources[0].Prefix

			// without -private the mirror's releases stay
			res, _ := g.export(t, plan, upstream.ExportOptions{})
			if c := outcomes(res); c[bundle.Rejected] != 2 || c[bundle.Missing] != 2 || res.Files != 0 || res.Failed() {
				t.Fatalf("without -private: %+v", res)
			}
			details := map[string]string{}
			for _, p := range res.Sources[0].Problems {
				details[p.Name] = p.Detail
			}
			if details["tresor"] != "private on the exporting kista" || details["zzz"] != "outside the exporting matrix" || details["gone"] != "" || len(details) != 4 {
				t.Fatalf("reasons: %v", details)
			}
			if !slices.Contains(res.Sources[0].Keys, g.env.fp) || res.Sources[0].From != "upstreams acme-repo" {
				t.Fatalf("the source's summary: %+v", res.Sources[0])
			}

			// a second, unchanged run keeps the cell record fresh: fetched_at is the record's
			g.sync(t, false)
			res, data := g.export(t, plan, upstream.ExportOptions{Private: []string{prefix}})
			if c := outcomes(res); c[bundle.Fetched] != 2 || c[bundle.Missing] != 2 || res.Files != 2 || res.Sources[0].Bytes != res.Bytes {
				t.Fatalf("with -private: %+v", res)
			}
			m, cells, files := g.open(t, data)
			if m.Exporter.Version != "kista test" || m.Plan.Tenant != "site" || m.Plan.SHA256 != res.PlanSHA256 || m.ID != res.ID {
				t.Fatalf("the manifest: %+v", m)
			}
			for _, name := range []string{"tresor", "acl"} {
				c := cells[name]
				rec := g.cell(t, name)
				if !bytes.Equal(files[c.File], g.files[name+"@1.0"]) {
					t.Errorf("%s: the file is not the source's", name)
				}
				if !c.Authenticated {
					t.Errorf("%s: a credentialed mirror's cell is not authenticated", name)
				}
				if !c.FetchedAt.Equal(rec.FetchedAt) || c.ETag != rec.ETag || c.BodyHash != rec.BodyHash {
					t.Errorf("%s: fetched_at %s etag %s, the record's %s %s", name, c.FetchedAt, c.ETag, rec.FetchedAt, rec.ETag)
				}
			}
			ir := g.importSite(t, data)
			if tr := target(t, ir, "air"); tr.Result.Counts[store.CellReleased] != 2 || tr.Result.Counts[store.CellMissing] != 2 {
				t.Fatalf("the import: %+v", tr)
			}

			// the site holds both now: unchanged, by body_hash; an ETag alone never decides
			plan = g.plan(t)
			res, data = g.export(t, plan, upstream.ExportOptions{Private: []string{prefix}})
			if c := outcomes(res); c[bundle.Unchanged] != 2 || res.Files != 0 {
				t.Fatalf("unchanged: %+v", res)
			}
			if tr := target(t, g.importSite(t, data), "air"); tr.Result.Counts[store.CellUnchanged] != 2 {
				t.Fatalf("an unchanged import: %+v", tr)
			}
			for i, c := range plan.Sources[0].Cells {
				if c.Name == "tresor" {
					plan.Sources[0].Cells[i].BodyHash = ""
				}
			}
			if res, _ := g.export(t, plan, upstream.ExportOptions{Private: []string{prefix}}); outcomes(res)[bundle.Fetched] != 1 {
				t.Fatalf("an ETag alone: %+v", res)
			}

			// -sync runs the mirror first: a new version at the source is in the bundle
			plan = g.plan(t)
			g.put(t, "tresor", "1.1", 3)
			res, data = g.export(t, plan, upstream.ExportOptions{Private: []string{prefix}, Sync: true})
			_, cells, files = g.open(t, data)
			if !bytes.Equal(files[cells["tresor"].File], g.files["tresor@1.1"]) || cells["acl"].Outcome != bundle.Unchanged {
				t.Fatalf("-sync: %+v", cells)
			}
			if tr := target(t, g.importSite(t, data), "air"); tr.Result.Counts[store.CellReleased] != 1 {
				t.Fatalf("the new version's import: %+v", tr)
			}

			// refusals before anything is read
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			var buf bytes.Buffer
			if _, err := g.up.Export(ctx, admin, "acme", &buf, upstream.ExportOptions{Plan: planBytes(t, plan), Key: other}); !errors.Is(err, upstream.ErrExportKey) || buf.Len() != 0 {
				t.Errorf("an unpinned key: %v, %d bytes", err, buf.Len())
			}
			if _, err := g.up.Export(ctx, admin, "acme", &buf, upstream.ExportOptions{Plan: planBytes(t, plan), Private: []string{"https://other.example/x"}}); !errors.Is(err, upstream.ErrExportPrivate) {
				t.Errorf("-private of no source: %v", err)
			}
			bad := plan
			bad.Tenant = "Not A Name"
			if _, err := g.up.Export(ctx, admin, "acme", &buf, upstream.ExportOptions{Plan: planBytes(t, bad)}); !errors.Is(err, bundle.ErrInvalid) {
				t.Errorf("a plan with a bad grammar: %v", err)
			}
			if _, err := g.up.Export(ctx, admin, "acme", &buf, upstream.ExportOptions{Plan: []byte(`{"format":"kista-bundle-plan/v1","extra":1}`)}); !errors.Is(err, bundle.ErrInvalid) {
				t.Errorf("a plan with an unknown field: %v", err)
			}
			if buf.Len() != 0 {
				t.Errorf("a refused export wrote %d bytes", buf.Len())
			}

			// the event, with no credential in it or the bundle
			tn, _ := g.st.GetTenant(ctx, "acme")
			evs, _ := g.st.ListEvents(ctx, tn.ID, store.EventFilter{Kind: "bundle.export", Limit: 100})
			if len(evs) != 5 || !strings.Contains(evs[0].Data, `"manifest":"`+res.ID+`"`) || !strings.Contains(evs[0].Data, `"bundle_key":"`+g.fp+`"`) ||
				!strings.Contains(evs[0].Data, `"plan":"`+res.PlanSHA256+`"`) || !strings.Contains(evs[0].Data, `"counts":{`) ||
				evs[0].Subject != "tenant:acme" {
				t.Fatalf("events: %+v", evs)
			}
			if bytes.Contains(data, []byte("T-sub")) {
				t.Error("a token in the bundle")
			}
		})
	}
}

// Spec 0014: the release decides, not the cell record: yanks, deprecations, blocks and a release
// made current after the run; another origin's release; the mirror's refusals; filters.
func TestExportOutcomes(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			names := []string{"yanked", "deprecated", "blocked", "older", "wrongkey", "replaced", "filtered", "only"}
			for i, n := range names {
				g.put(t, n, "1.0", i+1)
			}
			otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
			g.repo.put("v2.0.0", "linux_amd64", "wrongkey", signed(t, otherKey, cpp("1.0", "v2.0.0"), 50, "wrongkey_duckdb_cpp_init"))
			var entries []store.UpstreamEntry
			for _, n := range names[:len(names)-1] {
				entries = append(entries, store.UpstreamEntry{Name: n})
			}
			sp := g.spec(entries...)
			sp.Visibility = store.Public
			if _, err := g.up.Add(ctx, admin, "acme", sp); err != nil {
				t.Fatal(err)
			}
			// a second mirror of the source, into another channel: it alone lists "only", and holds
			// "older" at 1.1 where acme-repo, first by name, serves 1.0
			b := g.spec(store.UpstreamEntry{Name: "older"}, store.UpstreamEntry{Name: "only"})
			b.Name, b.Channel, b.Visibility = "b-repo", "other", store.Public
			if _, err := g.up.Add(ctx, admin, "acme", b); err != nil {
				t.Fatal(err)
			}
			g.sync(t, false)
			g.put(t, "older", "1.1", 20)
			g.sync(t, false)
			if _, err := g.up.Sync(ctx, admin, "acme", "b-repo", false); err != nil {
				t.Fatal(err)
			}
			g.run.Once(ctx)
			// a pull-through upstream of the same source never asked for "lazy"
			pt := g.spec(store.UpstreamEntry{Name: "*"})
			pt.Name, pt.Channel, pt.Mode, pt.Visibility = "pt", "other", store.ModePullThrough, store.Public
			if _, err := g.up.Add(ctx, admin, "acme", pt); err != nil {
				t.Fatal(err)
			}
			id := func(name string) string { return g.releases(t, name)[0].ID }
			for name, c := range map[string]release.Change{"yanked": release.Yank, "deprecated": release.Deprecate} {
				if _, err := g.rel.Apply(ctx, admin, "acme", "prod", name, id(name), c, 0); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := g.rel.Block(ctx, admin, "acme", g.releases(t, "blocked")[0].BodyHash, "bad"); err != nil {
				t.Fatal(err)
			}
			rs := g.releases(t, "older")
			v10 := rs[len(rs)-1]
			if _, err := g.rel.Apply(ctx, admin, "acme", "prod", "older", v10.ID, release.MakeCurrent, 0); err != nil {
				t.Fatal(err)
			}
			// the tenant's own build of "replaced", its source switched to the tenant (spec 0031)
			if _, err := g.rel.SetSource(ctx, admin, "acme", "prod", "replaced", "tenant", ""); err != nil {
				t.Fatal(err)
			}
			repl := signed(t, nil, cpp("9.0", "v2.0.0"), 60, "replaced_duckdb_cpp_init")
			if _, _, err := g.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(repl), release.AddOptions{Name: "replaced"}); err != nil {
				t.Fatal(err)
			}
			g.air(t, append(names, "lazy")...)
			plan := g.plan(t)
			for i, c := range plan.Sources[0].Cells {
				if c.Name == "filtered" {
					plan.Sources[0].Cells[i].Versions = []string{"7.0"}
				}
			}
			res, data := g.export(t, plan, upstream.ExportOptions{})
			_, cells, files := g.open(t, data)
			want := map[string][2]string{
				"yanked":     {bundle.Rejected, "withdrawn on the exporting kista"},
				"deprecated": {bundle.Rejected, "withdrawn on the exporting kista"},
				"blocked":    {bundle.Rejected, "withdrawn on the exporting kista"},
				"older":      {bundle.Fetched, ""},
				"wrongkey":   {bundle.Rejected, g.cell(t, "wrongkey").Detail},
				"replaced":   {bundle.Rejected, "not this source's build"},
				"filtered":   {bundle.Missing, "no release of the plan's versions on the exporting kista"},
				"lazy":       {bundle.Missing, "never fetched by pull-through"},
				"only":       {bundle.Fetched, ""},
			}
			for name, w := range want {
				if c := cells[name]; c.Outcome != w[0] || c.Detail != w[1] {
					t.Errorf("%s: %s %q, want %s %q", name, c.Outcome, c.Detail, w[0], w[1])
				}
			}
			if g.cell(t, "wrongkey").Outcome != store.CellRejected {
				t.Fatalf("the mirror did not refuse wrongkey: %+v", g.cell(t, "wrongkey"))
			}
			// the release made current is the one DuckDB installs: 1.0, not the newer cell record's
			if !bytes.Equal(files[cells["older"].File], g.files["older@1.0"]) {
				t.Error("older: not the current release's file")
			}
			// the cell record holds 1.1 now: 1.0 is dated by its own fetch, with no ETag
			var prov struct {
				FetchedAt time.Time `json:"fetched_at"`
			}
			_ = json.Unmarshal([]byte(v10.Provenance), &prov)
			if c := cells["older"]; c.ETag != "" || !c.FetchedAt.Equal(prov.FetchedAt) || c.FetchedAt.Equal(g.cell(t, "older").FetchedAt) {
				t.Errorf("older: fetched_at %s etag %q, the release's %s", c.FetchedAt, c.ETag, prov.FetchedAt)
			}
			if !bytes.Equal(files[cells["only"].File], g.files["only@1.0"]) || res.Sources[0].From != "upstreams acme-repo,b-repo,pt" {
				t.Errorf("only, from the second mirror: %+v", res.Sources[0])
			}
			if res.Failed() {
				t.Fatalf("failed: %+v", res)
			}
			if cells["older"].Authenticated {
				t.Error("a public release of a mirror without a credential is authenticated")
			}
			// a release private on the gateway that -private lets out stays private at the site
			if _, err := g.rel.Apply(ctx, admin, "acme", "prod", "older", v10.ID, release.SetPrivate, 0); err != nil {
				t.Fatal(err)
			}
			_, data = g.export(t, plan, upstream.ExportOptions{Private: []string{plan.Sources[0].Prefix}})
			if _, cells, _ := g.open(t, data); cells["older"].Outcome != bundle.Fetched || !cells["older"].Authenticated {
				t.Errorf("a private release: %+v", cells["older"])
			}
		})
	}
}

// Spec 0014: sources a gateway cannot serve fail as a whole and are left out; an own signed channel
// is a source, signed by its own key; an own passthrough channel never is.
func TestExportSources(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			g.put(t, "tresor", "1.0", 1)
			if _, err := g.up.Add(ctx, admin, "acme", g.spec(store.UpstreamEntry{Name: "tresor"})); err != nil {
				t.Fatal(err)
			}
			g.sync(t, false)
			keys, err := g.ks.List(ctx, admin, "acme", "prod")
			if err != nil || len(keys) != 1 {
				t.Fatal(keys, err)
			}
			prodFP := keys[0].Fingerprint
			sp := g.bundleSpec("own", "tresor")
			sp.Prefix, sp.Keys = "https://kista.example/acme/prod", []string{prodFP}
			if _, err := g.up.Add(ctx, admin, "site", sp); err != nil {
				t.Fatal(err)
			}
			cell := []bundle.PlanCell{{DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "tresor", Versions: []string{}}}
			own := "https://kista.example/acme"
			plan := bundle.Plan{Format: bundle.PlanFormat, CreatedAt: time.Now().UTC(), Tenant: "site", BundleKeys: []string{g.fp},
				Sources: []bundle.PlanSource{
					{Kind: store.UpstreamRepo, Prefix: own + "/mirror", Keys: []string{prodFP}, Cells: cell},
					{Kind: store.UpstreamRepo, Prefix: own + "/other", Keys: []string{g.env.fp}, Cells: cell},
					{Kind: store.UpstreamRepo, Prefix: own + "/prod", Keys: []string{prodFP}, Cells: cell},
					{Kind: store.UpstreamRepo, Prefix: "https://nowhere.example/x", Keys: []string{prodFP}, Cells: cell},
				}}
			res, data := g.export(t, plan, upstream.ExportOptions{PublicURL: "https://Kista.Example/", Private: []string{own + "/prod"}})
			errs := map[string]string{}
			for _, s := range res.Sources {
				errs[s.Prefix] = s.Error
			}
			if errs[own+"/mirror"] != "a passthrough channel has no key of its own and is never a source" ||
				errs[own+"/other"] != "the .well-known file lists none of the plan's keys" || errs[own+"/prod"] != "" ||
				errs["https://nowhere.example/x"] != "not mirrored here" || !res.Failed() {
				t.Fatalf("sources: %v", errs)
			}
			m, cells, files := g.open(t, data)
			if len(m.Sources) != 1 || m.Sources[0].Prefix != own+"/prod" {
				t.Fatalf("the bundle's sources: %+v", m.Sources)
			}
			c := cells["tresor"]
			rel := g.releases(t, "tresor")[0]
			if c.Outcome != bundle.Fetched || !c.Authenticated || !c.FetchedAt.Equal(rel.CreatedAt.UTC()) {
				t.Fatalf("the own channel's cell: %+v", c)
			}
			// the file is the body signed by the channel's key, not the source's
			f := files[c.File]
			ef, err := extfile.Open(bytes.NewReader(f), int64(len(f)), 1<<30)
			if err != nil {
				t.Fatal(err)
			}
			pub, err := x509.ParsePKIXPublicKey(keys[0].PublicKey)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := extfile.Verify(ef.Hash, ef.Signature, []*rsa.PublicKey{pub.(*rsa.PublicKey)}); !ok {
				t.Fatal("the own channel's file does not verify with its key")
			}
			// the site takes it from a bundle-mode upstream of that prefix
			ir := g.importSite(t, data)
			if tr := target(t, ir, "own"); tr.Result.Counts[store.CellReleased] != 1 {
				t.Fatalf("the import: %+v", tr)
			}
			srs, err := g.rel.List(ctx, admin, "site", "prod", "tresor")
			if err != nil || len(srs) != 1 || srs[0].Visibility != store.Private {
				t.Fatalf("an authenticated cell is private at the site: %+v %v", srs, err)
			}

			// after a key rotation, a site that pins either key receives a file that key verifies
			if err := signer.GenerateKeyFile(filepath.Join(g.dir, "prod2.pem")); err != nil {
				t.Fatal(err)
			}
			k2, err := g.ks.Add(ctx, admin, "acme", "prod", "file:prod2.pem", false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := g.ks.Activate(ctx, admin, "acme", "prod", k2.ID, 0, false); err != nil {
				t.Fatal(err)
			}
			if _, _, err := g.rel.Resign(ctx, k2.ChannelID, nil); err != nil {
				t.Fatal(err)
			}
			pub2, _ := x509.ParsePKIXPublicKey(k2.PublicKey)
			// both pinned: the serving key's signature
			both := []string{prodFP, k2.Fingerprint}
			slices.Sort(both)
			for fp, key := range map[string]*rsa.PublicKey{prodFP: pub.(*rsa.PublicKey), k2.Fingerprint: pub2.(*rsa.PublicKey),
				strings.Join(both, ","): pub2.(*rsa.PublicKey)} {
				one := bundle.Plan{Format: bundle.PlanFormat, CreatedAt: time.Now().UTC(), Tenant: "site", BundleKeys: []string{g.fp},
					Sources: []bundle.PlanSource{{Kind: store.UpstreamRepo, Prefix: own + "/prod", Keys: strings.Split(fp, ","), Cells: cell}}}
				_, data := g.export(t, one, upstream.ExportOptions{PublicURL: "https://kista.example", Private: []string{own + "/prod"}})
				_, cells, files := g.open(t, data)
				f := files[cells["tresor"].File]
				ef, err := extfile.Open(bytes.NewReader(f), int64(len(f)), 1<<30)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := extfile.Verify(ef.Hash, ef.Signature, []*rsa.PublicKey{key}); !ok {
					t.Errorf("a plan pinning %s: the file does not verify with it", fp)
				}
			}
		})
	}
}

// Spec 0014: a DuckDB kind's files checked with DuckDB's keys; each file checked with the plan's
// pins as the import will check it; a body the gateway cannot read is left out.
func TestExportChecks(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			g.up.DuckDB = upstream.DuckDB{CoreURL: g.repo.srv.URL, CoreKeys: []*rsa.PublicKey{&g.key.PublicKey}}
			g.put(t, "tresor", "1.0", 1)
			g.put(t, "acl", "1.0", 2)
			g.put(t, "httpfs", "1.0", 3)
			sp := g.spec(store.UpstreamEntry{Name: "tresor"}, store.UpstreamEntry{Name: "acl"})
			sp.Visibility = store.Public
			if _, err := g.up.Add(ctx, admin, "acme", sp); err != nil {
				t.Fatal(err)
			}
			if _, err := g.up.Add(ctx, admin, "acme", upstream.Spec{Name: "core", Kind: store.UpstreamCore, Channel: "other", Visibility: store.Public,
				Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "httpfs"}}}); err != nil {
				t.Fatal(err)
			}
			g.sync(t, false)
			if _, err := g.up.Sync(ctx, admin, "acme", "core", false); err != nil {
				t.Fatal(err)
			}
			g.run.Once(ctx)
			// the repository lists a second key, the one the plan pins: the mirror's files do not verify with it
			k2, _ := rsa.GenerateKey(rand.Reader, 2048)
			der, _ := x509.MarshalPKIXPublicKey(&k2.PublicKey)
			g.repo.keys = append(g.repo.keys, base64.StdEncoding.EncodeToString(der))
			// acl's stored body is marked corrupt
			rec, err := g.st.GetBlob(ctx, "default", g.releases(t, "acl")[0].BodyHash)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := g.st.MarkBlobCorrupt(ctx, rec); err != nil {
				t.Fatal(err)
			}
			cell := func(n string) []bundle.PlanCell {
				return []bundle.PlanCell{{DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: n, Versions: []string{}}}
			}
			prefix := g.releasesPrefix(t)
			plan := bundle.Plan{Format: bundle.PlanFormat, CreatedAt: time.Now().UTC(), Tenant: "site", BundleKeys: []string{g.fp},
				Sources: []bundle.PlanSource{
					{Kind: store.UpstreamCore, Cells: cell("httpfs")},
					{Kind: store.UpstreamRepo, Prefix: prefix, Keys: []string{extfile.Fingerprint(&k2.PublicKey)},
						Cells: append(cell("acl"), cell("tresor")...)},
				}}
			res, data := g.export(t, plan, upstream.ExportOptions{})
			_, cells, files := g.open(t, data)
			if c := cells["httpfs"]; c.Outcome != bundle.Fetched || c.Authenticated || !bytes.Equal(files[c.File], g.files["httpfs@1.0"]) ||
				res.Sources[0].From != "upstreams core" {
				t.Errorf("a core cell: %+v %+v", c, res.Sources[0])
			}
			if c := cells["tresor"]; c.Outcome != bundle.Rejected || !strings.Contains(c.Detail, "signature does not verify") {
				t.Errorf("a file the plan's keys do not verify: %+v", c)
			}
			if c := cells["acl"]; c.Outcome != bundle.Rejected || c.Detail != "the body is not available on the exporting kista" {
				t.Errorf("a corrupt body: %+v", c)
			}
		})
	}
}

// releasesPrefix is the repository's prefix as upstreams store it.
func (g *gateway) releasesPrefix(t *testing.T) string {
	t.Helper()
	u, err := g.up.Get(ctx, admin, "acme", "acme-repo")
	if err != nil {
		t.Fatal(err)
	}
	return u.Prefix
}
