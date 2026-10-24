package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0021: kista_demo, published to one kista by a marked publisher with a licence imported
// there, is mirrored by a second kista, which records its key from the repository upstream and
// relays the licence; the pinned DuckDB installs it from the second kista and is licensed there.
func TestSDKLicenseRelay(t *testing.T) {
	b := needBuild(t)
	if ext, ok := b.extensions["kista_demo"]; !ok || ext == "" {
		t.Fatal("kista_demo is not built (make e2e-duckdb)")
	}
	ctx := context.Background()
	idpKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := &e2eIDP{key: idpKey}
	setup := func(k *kista, aud, sub string) { t.Helper(); k.installer(t, idp, aud, sub) }

	// the upstream: kista_demo from a marked publisher, and its licence
	src := startKistaLicensed(t, b, &auth.Verifier{Fetch: idp}, true)
	setup(src, "api://kista-acme", "mirror")
	if st := src.publish(t, "kista_demo", src.publisherKey(t, "issuer-ci", true)); st != 201 {
		t.Fatalf("publish: %d", st)
	}
	signKey, err := vectors.LoadKey("../internal/license/testdata", vectors.TrustedKey)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	tok := vectors.Sign(signKey, vectors.Header(signKey), fmt.Sprintf(`{"v":1,"iss":"https://issuer.example","jti":"lic-relay",`+
		`"sub":"acme","holder":"Acme Test","ext":"kista_demo","plan":"pro","limits":{"max_attached_sources":5},"iat":%d,"exp":%d}`,
		now-60, now+30*86400))
	if _, err := src.licenses.Import(ctx, serveAdmin, "acme", "", tok, licenses.Create); err != nil {
		t.Fatal(err)
	}

	// the downstream mirrors it with a credential, then relays the licence
	dst := startKistaLicensed(t, b, &auth.Verifier{Fetch: idp}, true)
	setup(dst, "api://kista-dst", "alice")
	dst.release("httpfs", release.AddOptions{})
	_, port, _ := net.SplitHostPort(src.addr)
	p, _ := strconv.Atoi(port)
	eg, err := egress.New(egress.Config{OwnHost: "127.0.0.1", CAFile: testCA(t).caFile,
		Allow: []egress.Allow{{Prefix: netip.MustParsePrefix("127.0.0.1/32"), Ports: []uint16{uint16(p)}},
			{Prefix: netip.MustParsePrefix("::1/128"), Ports: []uint16{uint16(p)}}}})
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(idp.token(t, "mirror", "api://kista-acme")), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := credential.New([]config.Credential{{Name: "src", Tenants: []string{"acme"}, Prefixes: []string{"https://localhost:" + port + "/acme/"},
		Kind: "token_file", TokenFile: tokenFile}}, credential.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	up := &upstream.Service{Store: dst.st, Releases: dst.rel, Blob: dst.blob, Fetch: eg, Authz: authz.ServerAdmin{}, Credentials: creds,
		MaxBody: 256 << 20, MaxIngests: 2, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	u, err := up.Add(ctx, serveAdmin, "acme", upstream.Spec{Name: "src", Kind: store.UpstreamRepo, Prefix: "https://localhost:" + port + "/acme/prod",
		Channel: "prod", Credential: "src", Keys: []string{src.activeKey(t).Fingerprint}, Platforms: []string{b.platform},
		Entries: []store.UpstreamEntry{{Name: "kista_demo"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.Sync(ctx, serveAdmin, "acme", "src", false); err != nil {
		t.Fatal(err)
	}
	(&upstream.Runner{Service: up, Holder: "e2e", Log: slog.New(slog.DiscardHandler)}).Once(ctx)
	if cells, err := up.Cells(ctx, serveAdmin, "acme", "src", "", [3]string{}, 10); err != nil || len(cells) != 1 ||
		cells[0].Outcome != store.CellReleased {
		t.Fatalf("the mirror run: %+v %v", cells, err)
	}
	dst.licenses.Fetch, dst.licenses.Credentials = eg, creds
	res, err := dst.licenses.RunUpstream(ctx, u.ID)
	if err != nil || res.Counts["stored"] != 1 {
		t.Fatalf("the relay: %+v %v", res, err)
	}
	if ks, err := dst.licenses.LicenseKeys(ctx, serveAdmin, "acme", "", "kista_demo"); err != nil || len(ks) != 1 ||
		ks[0].Keys[0].Kid != vectors.Kid(&signKey.PublicKey) || ks[0].Keys[0].Sources[0] != (store.KeySource{Kind: "upstream", Name: "src"}) {
		t.Fatalf("the downstream's keys: %+v %v", ks, err)
	}

	// installed from the downstream: the relayed licence is served and valid
	pemKey := dst.pem(t, dst.activeKey(t))
	r := newSession(t, b, map[string]string{"extension_directory": t.TempDir()}).exec(
		createRepo("boot", dst.httpURL(), pemKey),
		"INSTALL httpfs FROM boot",
		"LOAD httpfs FROM boot",
		"SET ca_cert_file = "+sqlString(testCA(t).caFile),
		createRepo("m", dst.httpsURL(), pemKey),
		"CREATE SECRET s (TYPE http, BEARER_TOKEN "+sqlString(idp.token(t, "alice", "api://kista-dst"))+", SCOPE "+sqlString(dst.httpsURL()+"/")+")",
		"INSTALL kista_demo FROM m",
		"LOAD kista_demo FROM m",
		"SELECT status, plan, source FROM kista_demo_license()",
	)
	mustOK(t, r)
	if got := r[len(r)-1].Rows; len(got) != 1 || got[0][0] != "valid" || got[0][1] != "pro" || got[0][2] != "repository" {
		t.Fatalf("the licence from the downstream: %v", got)
	}
}

// installer trusts the test issuer in acme with an audience, and grants sub install on prod.
func (k *kista) installer(t *testing.T, idp *e2eIDP, aud, sub string) {
	t.Helper()
	ctx := context.Background()
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}, Fetch: idp, PublicURL: "https://" + k.addr}
	if _, err := adm.AddIssuer(ctx, serveAdmin, "acme", store.Issuer{Name: "corp", URL: e2eIssuer}); err != nil {
		t.Fatal(err)
	}
	if err := adm.AddAudience(ctx, serveAdmin, "acme", aud); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "subject:corp|"+sub, []string{"install"}, tenants.GrantOn{Channel: "prod"}); err != nil {
		t.Fatal(err)
	}
}
