package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0014: a source kista serves a signed channel over https; a gateway kista mirrors it and
// exports a bundle for an air-gapped site's plan; the site, whose egress refuses every address,
// imports it into a bundle-mode upstream, and DuckDB at the pin installs and loads the extension
// from the site. A new version at the source travels in a second bundle and becomes current.
func TestBundleAirGapped(t *testing.T) {
	b := needBuild(t)
	ctx := context.Background()
	const name = "loadable_extension_demo"
	src := startKista(t, b)
	src.release(name, release.AddOptions{})
	_, port, _ := net.SplitHostPort(src.addr)
	prefix := "https://localhost:" + port + "/acme/prod"
	srcFP := src.activeKey(t).Fingerprint

	// the gateway mirrors the source
	gw := startKista(t, b)
	p, _ := strconv.Atoi(port)
	eg, err := egress.New(egress.Config{OwnHost: "127.0.0.1", CAFile: testCA(t).caFile,
		Allow: []egress.Allow{{Prefix: netip.MustParsePrefix("127.0.0.1/32"), Ports: []uint16{uint16(p)}},
			{Prefix: netip.MustParsePrefix("::1/128"), Ports: []uint16{uint16(p)}}}})
	if err != nil {
		t.Fatal(err)
	}
	gwUp := &upstream.Service{Store: gw.st, Releases: gw.rel, Blob: gw.blob, Fetch: eg, Authz: authz.ServerAdmin{},
		MaxBody: 256 << 20, MaxIngests: 2, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	if _, err := gwUp.Add(ctx, serveAdmin, "acme", upstream.Spec{Name: "src", Kind: store.UpstreamRepo, Prefix: prefix, Channel: "prod",
		Keys: []string{srcFP}, Platforms: []string{b.platform}, Visibility: store.Public, Entries: []store.UpstreamEntry{{Name: name}}}); err != nil {
		t.Fatal(err)
	}

	// the site reaches nothing; its bundle-mode upstream pins the source's key
	site := startKista(t, b)
	none, err := egress.New(egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	siteUp := &upstream.Service{Store: site.st, Releases: site.rel, Blob: site.blob, Fetch: none, Authz: authz.ServerAdmin{},
		MaxBody: 256 << 20, MaxIngests: 2, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	if _, err := siteUp.Add(ctx, serveAdmin, "acme", upstream.Spec{Name: "air", Kind: store.UpstreamRepo, Prefix: prefix, Channel: "prod",
		Mode: store.ModeBundle, Keys: []string{srcFP}, Platforms: []string{b.platform}, Visibility: store.Public,
		Entries: []store.UpstreamEntry{{Name: name}}}); err != nil {
		t.Fatal(err)
	}
	pemKey, bundleFP, err := bundle.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	bundleKey, err := bundle.ParsePrivateKey(pemKey)
	if err != nil {
		t.Fatal(err)
	}

	// plan at the site, export with -sync at the gateway, import at the site
	cycle := func() upstream.TargetResult {
		t.Helper()
		plan, err := siteUp.Plan(ctx, serveAdmin, "acme", upstream.PlanTargets{}, []string{bundleFP})
		if err != nil {
			t.Fatal(err)
		}
		pd, _ := json.Marshal(plan)
		var buf bytes.Buffer
		res, err := gwUp.Export(ctx, serveAdmin, "acme", &buf, upstream.ExportOptions{Plan: pd, Key: bundleKey, Sync: true, Holder: "e2e",
			PublicURL: "https://" + gw.addr, Version: "kista e2e"})
		if err != nil || res.Failed() {
			t.Fatalf("export: %+v %v", res, err)
		}
		ir, err := siteUp.Import(ctx, serveAdmin, "acme", &buf, upstream.ImportOptions{BundleKeys: []string{bundleFP}, Holder: "e2e"})
		if err != nil || len(ir.Targets) != 1 {
			t.Fatalf("import: %+v %v", ir, err)
		}
		return ir.Targets[0]
	}
	if tr := cycle(); tr.Result.Counts[store.CellReleased] != 1 {
		t.Fatalf("the first bundle: %+v", tr)
	}
	sitePEM := site.pem(t, site.activeKey(t))
	version := "SELECT extension_version FROM duckdb_extensions() WHERE extension_name = '" + name + "'"
	res := newSession(t, b, nil).exec(
		createRepo("site", site.httpURL(), sitePEM),
		"INSTALL "+name+" FROM site",
		"LOAD "+name+" FROM site",
		version,
	)
	mustOK(t, res)
	first := res[len(res)-1].Rows[0][0]

	// a new version at the source: the second bundle carries it, and it is current at the site
	next := withVersion(t, b.extensions[name], "e2e-2")
	f, err := os.Open(next)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.rel.Add(ctx, serveAdmin, "acme", "prod", f, release.AddOptions{Name: name}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if tr := cycle(); tr.Result.Counts[store.CellReleased] != 1 {
		t.Fatalf("the second bundle: %+v", tr)
	}
	res = newSession(t, b, nil).exec(
		createRepo("site", site.httpURL(), sitePEM),
		"FORCE INSTALL "+name+" FROM site",
		"LOAD "+name+" FROM site",
		version,
	)
	mustOK(t, res)
	if got := res[len(res)-1].Rows[0][0]; got != "e2e-2" || got == first {
		t.Fatalf("the site serves version %v (first %v)", got, first)
	}
}

// withVersion writes a copy of an extension with another extension version in its footer: a new
// body for the same build.
func withVersion(t *testing.T, path, version string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ef, err := extfile.Open(bytes.NewReader(data), int64(len(data)), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	md := ef.Metadata
	md.ExtensionVersion = version
	block, err := extfile.EncodeMetadata(md)
	if err != nil {
		t.Fatal(err)
	}
	at := ef.BodySize - extfile.MetadataSize
	out := bytes.Clone(data)
	copy(out[at:at+extfile.MetadataSize], block[:])
	p := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
