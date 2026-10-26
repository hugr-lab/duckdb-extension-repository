package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0021: kista_demo, published by a marked publisher with a licence imported there, travels in
// a bundle to an air-gapped site whose plan asks for licences; the site takes the licence into its
// bundle-mode upstream, and the pinned DuckDB installs kista_demo there and is licensed.
func TestSDKLicenseBundle(t *testing.T) {
	b := needBuild(t)
	if ext, ok := b.extensions["kista_demo"]; !ok || ext == "" {
		t.Fatal("kista_demo is not built (make e2e-duckdb)")
	}
	ctx := context.Background()
	idpKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := &e2eIDP{key: idpKey}

	// the exporting kista: kista_demo from a marked publisher in its own channel, and its licence
	src := startKistaLicensed(t, b, &auth.Verifier{Fetch: idp}, true)
	if st := src.publish(t, "kista_demo", src.publisherKey(t, "issuer-ci", true)); st != 201 {
		t.Fatalf("publish: %d", st)
	}
	signKey, err := vectors.LoadKey("../internal/license/testdata", vectors.TrustedKey)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	tok := vectors.Sign(signKey, vectors.Header(signKey), fmt.Sprintf(`{"v":1,"iss":"https://issuer.example","jti":"lic-bundle",`+
		`"sub":"acme","holder":"Acme Test","ext":"kista_demo","plan":"pro","limits":{"max_attached_sources":5},"iat":%d,"exp":%d}`,
		now-60, now+30*86400))
	if _, err := src.licenses.Import(ctx, serveAdmin, "acme", "", "publisher:issuer-ci", tok, licenses.Create); err != nil {
		t.Fatal(err)
	}
	srcUp := &upstream.Service{Store: src.st, Releases: src.rel, Blob: src.blob, Authz: authz.ServerAdmin{}, Licenses: src.licenses,
		MaxBody: 256 << 20, MaxIngests: 2, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}

	// the site reaches nothing; its bundle-mode upstream pins the exporting channel's key
	site := startKistaLicensed(t, b, &auth.Verifier{Fetch: idp}, true)
	site.installer(t, idp, "api://kista-site", "alice")
	site.release("httpfs", release.AddOptions{})
	none, err := egress.New(egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	siteUp := &upstream.Service{Store: site.st, Releases: site.rel, Blob: site.blob, Fetch: none, Authz: authz.ServerAdmin{},
		Licenses: site.licenses, MaxBody: 256 << 20, MaxIngests: 2, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	prefix := src.httpsURL()
	if _, err := siteUp.Add(ctx, serveAdmin, "acme", upstream.Spec{Name: "air", Kind: store.UpstreamRepo, Prefix: prefix, Channel: "prod",
		Mode: store.ModeBundle, Keys: []string{src.activeKey(t).Fingerprint}, Platforms: []string{b.platform},
		Entries: []store.UpstreamEntry{{Name: "kista_demo"}}}); err != nil {
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

	// the plan asks for acme's licences; the export agrees and carries the private build
	plan, err := siteUp.Plan(ctx, serveAdmin, "acme", upstream.PlanTargets{Licenses: true, LicenseHolder: "acme"}, []string{bundleFP})
	if err != nil {
		t.Fatal(err)
	}
	pd, _ := json.Marshal(plan)
	var buf bytes.Buffer
	res, err := srcUp.Export(ctx, serveAdmin, "acme", &buf, upstream.ExportOptions{Plan: pd, Key: bundleKey, Private: []string{prefix},
		Licenses: true, Holder: "e2e", PublicURL: "https://" + src.addr, Version: "kista e2e"})
	if err != nil || res.Failed() || len(res.Sources) != 1 || len(res.Sources[0].Licenses) != 1 {
		t.Fatalf("export: %+v %v", res, err)
	}
	ir, err := siteUp.Import(ctx, serveAdmin, "acme", &buf, upstream.ImportOptions{BundleKeys: []string{bundleFP}, Holder: "e2e"})
	if err != nil || len(ir.Targets) != 1 || ir.Targets[0].Result.Counts[store.CellReleased] != 1 || ir.Licenses == nil ||
		ir.Licenses.Counts["stored"] != 1 {
		t.Fatalf("import: %+v %+v %v", ir, ir.Licenses, err)
	}

	// installed from the site: the bundle's licence is served and valid
	sitePEM := site.pem(t, site.activeKey(t))
	r := newSession(t, b, map[string]string{"extension_directory": t.TempDir()}).exec(
		createRepo("boot", site.httpURL(), sitePEM),
		"INSTALL httpfs FROM boot",
		"LOAD httpfs FROM boot",
		"SET ca_cert_file = "+sqlString(testCA(t).caFile),
		createRepo("m", site.httpsURL(), sitePEM),
		"CREATE SECRET s (TYPE http, BEARER_TOKEN "+sqlString(idp.token(t, "alice", "api://kista-site"))+", SCOPE "+sqlString(site.httpsURL()+"/")+")",
		"INSTALL kista_demo FROM m",
		"LOAD kista_demo FROM m",
		"SELECT status, plan, source FROM kista_demo_license()",
	)
	mustOK(t, r)
	if got := r[len(r)-1].Rows; len(got) != 1 || got[0][0] != "valid" || got[0][1] != "pro" || got[0][2] != "repository" {
		t.Fatalf("the licence at the site: %v", got)
	}
}
