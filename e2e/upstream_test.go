package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0009: a kista mirrors another kista's channel (a repository upstream, its key pinned by
// fingerprint) into a signed channel, re-signed with its own key; DuckDB at the pin installs and
// loads the mirrored extension from it.
func TestUpstreamMirror(t *testing.T) {
	b := needBuild(t)
	ctx := context.Background()
	src := startKista(t, b) // the upstream
	src.release("loadable_extension_demo", release.AddOptions{})
	dst := startKista(t, b) // mirrors it
	// the upstream is reached as localhost: egress refuses the deployment's own host (127.0.0.1)
	_, port, _ := net.SplitHostPort(src.addr)
	eg, err := egress.New(egress.Config{AllowLoopbackHTTP: true, OwnHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	up := &upstream.Service{Store: dst.st, Releases: dst.rel, Blob: dst.blob, Fetch: eg, Authz: authz.ServerAdmin{},
		MaxBody: 256 << 20, MaxIngests: 2, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	spec := upstream.Spec{Name: "src", Kind: store.UpstreamRepo, Prefix: "http://localhost:" + port + "/acme/prod", Channel: "prod",
		Keys: []string{src.activeKey(t).Fingerprint}, Platforms: []string{b.platform}, Visibility: store.Public,
		Entries: []store.UpstreamEntry{{Name: "loadable_extension_demo"}}}
	// the deployment's own host is refused
	own := spec
	own.Name, own.Prefix = "own", "http://127.0.0.1:"+port+"/acme/prod"
	if _, err := up.Add(ctx, serveAdmin, "acme", own); err == nil {
		t.Fatal("an upstream at the deployment's own host was added")
	}
	if _, err := up.Add(ctx, serveAdmin, "acme", spec); err != nil {
		t.Fatal(err)
	}
	if _, err := up.Sync(ctx, serveAdmin, "acme", "src", false); err != nil {
		t.Fatal(err)
	}
	(&upstream.Runner{Service: up, Holder: "e2e", Log: slog.New(slog.DiscardHandler)}).Once(ctx)
	cells, err := up.Cells(ctx, serveAdmin, "acme", "src", "", [3]string{}, 10)
	if err != nil || len(cells) != 1 || cells[0].Outcome != store.CellReleased {
		t.Fatalf("the cell: %+v %v", cells, err)
	}
	rels, err := dst.rel.List(ctx, serveAdmin, "acme", "prod", "loadable_extension_demo")
	if err != nil || len(rels) != 1 || rels[0].Origin != store.OriginUpstream {
		t.Fatalf("the mirrored release: %+v %v", rels, err)
	}
	// re-signed: the downstream's key verifies it, the upstream's does not install from it
	mustOK(t, newSession(t, b, nil).exec(
		createRepo("m", dst.httpURL(), dst.pem(t, dst.activeKey(t))),
		"INSTALL loadable_extension_demo FROM m",
		"LOAD loadable_extension_demo FROM m",
	))
	res := newSession(t, b, nil).exec(
		createRepo("m", dst.httpURL(), src.pem(t, src.activeKey(t))),
		"INSTALL loadable_extension_demo FROM m",
	)
	mustFail(t, res[1], "signature")
}

// Spec 0009 phase 3: a kista mirrors another kista's private channel with a token_file credential
// (a token of an issuer the upstream tenant trusts, naming a principal it grants install), over
// https; the mirrored release is private in the downstream, where DuckDB at the pin installs and
// loads it with a granted token, and fails without one.
func TestUpstreamPrivate(t *testing.T) {
	b := needBuild(t)
	ctx := context.Background()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := &e2eIDP{key: key}
	src := startKistaWith(t, b, &auth.Verifier{Fetch: idp}) // the upstream
	adm := &tenants.AuthAdmin{Store: src.st, Authz: authz.ServerAdmin{}, Fetch: idp, PublicURL: "https://" + src.addr}
	if _, err := adm.AddIssuer(ctx, serveAdmin, "acme", store.Issuer{Name: "corp", URL: e2eIssuer}); err != nil {
		t.Fatal(err)
	}
	if err := adm.AddAudience(ctx, serveAdmin, "acme", "api://kista-acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "subject:corp|mirror", []string{"install"}, "prod", ""); err != nil {
		t.Fatal(err)
	}
	src.release("loadable_extension_demo", release.AddOptions{Private: true})
	dst := startKistaWith(t, b, &auth.Verifier{Fetch: idp}) // mirrors it
	dadm := &tenants.AuthAdmin{Store: dst.st, Authz: authz.ServerAdmin{}, Fetch: idp, PublicURL: "https://" + dst.addr}
	if _, err := dadm.AddIssuer(ctx, serveAdmin, "acme", store.Issuer{Name: "corp", URL: e2eIssuer}); err != nil {
		t.Fatal(err)
	}
	if err := dadm.AddAudience(ctx, serveAdmin, "acme", "api://kista-dst"); err != nil {
		t.Fatal(err)
	}
	if _, err := dadm.AddGrant(ctx, serveAdmin, "acme", "subject:corp|alice", []string{"install"}, "prod", ""); err != nil {
		t.Fatal(err)
	}
	dst.release("httpfs", release.AddOptions{})
	_, port, _ := net.SplitHostPort(src.addr)
	p, _ := strconv.Atoi(port)
	eg, err := egress.New(egress.Config{OwnHost: "127.0.0.1", CAFile: testCA(t).caFile,
		Allow: []egress.Allow{{Prefix: netip.MustParsePrefix("127.0.0.1/32"), Ports: []uint16{uint16(p)}},
			{Prefix: netip.MustParsePrefix("::1/128"), Ports: []uint16{uint16(p)}}}})
	if err != nil {
		t.Fatal(err)
	}
	prefix := "https://localhost:" + port + "/acme/prod"
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
	spec := upstream.Spec{Name: "src", Kind: store.UpstreamRepo, Prefix: prefix, Channel: "prod", Credential: "src",
		Keys: []string{src.activeKey(t).Fingerprint}, Platforms: []string{b.platform},
		Entries: []store.UpstreamEntry{{Name: "loadable_extension_demo"}}}
	if _, err := up.Add(ctx, serveAdmin, "acme", spec); err != nil {
		t.Fatal(err)
	}
	run := func() store.UpstreamCell {
		t.Helper()
		if _, err := up.Sync(ctx, serveAdmin, "acme", "src", false); err != nil {
			t.Fatal(err)
		}
		(&upstream.Runner{Service: up, Holder: "e2e", Log: slog.New(slog.DiscardHandler)}).Once(ctx)
		cells, err := up.Cells(ctx, serveAdmin, "acme", "src", "", [3]string{}, 10)
		if err != nil || len(cells) != 1 {
			t.Fatalf("the cells: %+v %v", cells, err)
		}
		return cells[0]
	}
	if c := run(); c.Outcome != store.CellReleased {
		t.Fatalf("the cell: %+v", c)
	}
	rels, err := dst.rel.List(ctx, serveAdmin, "acme", "prod", "loadable_extension_demo")
	if err != nil || len(rels) != 1 || rels[0].Visibility != store.Private {
		t.Fatalf("the mirrored release is private: %+v %v", rels, err)
	}
	// installed from the downstream with a granted token (httpfs, a secret scoped to its prefix)
	pemKey := dst.pem(t, dst.activeKey(t))
	mustOK(t, newSession(t, b, nil).exec(
		createRepo("boot", dst.httpURL(), pemKey),
		"INSTALL httpfs FROM boot",
		"LOAD httpfs FROM boot",
		"SET ca_cert_file = "+sqlString(testCA(t).caFile),
		createRepo("m", dst.httpsURL(), pemKey),
		"CREATE SECRET s (TYPE http, BEARER_TOKEN "+sqlString(idp.token(t, "alice", "api://kista-dst"))+", SCOPE "+sqlString(dst.httpsURL()+"/")+")",
		"INSTALL loadable_extension_demo FROM m",
		"LOAD loadable_extension_demo FROM m",
	))
	// and not without one
	res := newSession(t, b, nil).exec(createRepo("m", dst.httpURL(), pemKey), "INSTALL loadable_extension_demo FROM m")
	if res[len(res)-1].OK {
		t.Fatal("a private mirrored release was installed without a token")
	}
	// a principal the upstream tenant does not grant: kista answers it as missing (spec 0006), and
	// the cell is missing
	if err := os.WriteFile(tokenFile, []byte(idp.token(t, "nobody", "api://kista-acme")), 0o600); err != nil {
		t.Fatal(err)
	}
	src.release("loadable_extension_demo", release.AddOptions{Private: true})
	if c := run(); c.Outcome != store.CellMissing {
		t.Fatalf("a token without the grant: %+v", c)
	}
}
