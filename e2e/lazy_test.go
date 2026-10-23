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
	"time"

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

// Spec 0032: a lazy mirror. kista "dst" mirrors kista "src" lazily; kista "down" mirrors dst
// lazily too, with a credential, its entry eager. down's first run takes the eager entry's build
// before anyone asks: dst answers 503 while it fetches the cell, and down's fetcher asks again.
// DuckDB at the pin installs the other extension from dst on its first INSTALL (DuckDB retries the
// 503 by itself, after its Retry-After), and dst's next run refreshes the cell DuckDB asked for.
// The upstreams are repository upstreams at other kistas, standing in for duckdb-core: the tests
// reach no DuckDB repository, and the lazy paths do not depend on the kind (only the keys' source
// does, which the unit tests cover). down is lazy itself; a mirror fetches through the same retry.
func TestUpstreamLazy(t *testing.T) {
	b := needBuild(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const demo, kdemo = "loadable_extension_demo", "kista_demo"
	src := startKista(t, b)
	src.release(demo, release.AddOptions{})
	src.release(kdemo, release.AddOptions{})

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := &e2eIDP{key: key}
	dst := startKistaWith(t, b, &auth.Verifier{Fetch: idp})
	adm := &tenants.AuthAdmin{Store: dst.st, Authz: authz.ServerAdmin{}, Fetch: idp, PublicURL: "https://" + dst.addr}
	if _, err := adm.AddIssuer(ctx, serveAdmin, "acme", store.Issuer{Name: "corp", URL: e2eIssuer}); err != nil {
		t.Fatal(err)
	}
	if err := adm.AddAudience(ctx, serveAdmin, "acme", "api://kista-dst"); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"alice", "mirror"} {
		if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "subject:corp|"+who, []string{"install"}, tenants.GrantOn{Channel: "prod"}); err != nil {
			t.Fatal(err)
		}
	}
	dst.release("httpfs", release.AddOptions{})

	// dst: a lazy mirror of src, nothing eager; its puller serves first requests
	_, srcPort, _ := net.SplitHostPort(src.addr)
	eg, err := egress.New(egress.Config{AllowLoopbackHTTP: true, OwnHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	up := &upstream.Service{Store: dst.st, Releases: dst.rel, Blob: dst.blob, Fetch: eg, Authz: authz.ServerAdmin{},
		MaxBody: 256 << 20, MaxIngests: 2, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler),
		Config: upstream.Config{Interval: 6 * time.Hour}}
	if _, err := up.Add(ctx, serveAdmin, "acme", upstream.Spec{Name: "src", Kind: store.UpstreamRepo, Prefix: "http://localhost:" + srcPort + "/acme/prod",
		Channel: "prod", Keys: []string{src.activeKey(t).Fingerprint}, Platforms: []string{b.platform}, Visibility: store.Private,
		FetchMode: store.FetchLazy, Entries: []store.UpstreamEntry{{Name: demo}, {Name: kdemo}}}); err != nil {
		t.Fatal(err)
	}
	puller := &upstream.Puller{Service: up, Holder: "dst", Log: slog.New(slog.DiscardHandler)}
	dst.puller.p.Store(puller)
	running := make(chan struct{})
	go func() { puller.Run(ctx); close(running) }()
	// the workers end before dst's store and blobs close (cleanups run last first)
	t.Cleanup(func() { cancel(); <-running })
	// asks wait for a fetch's count: it follows the release, which a retry may be served first
	asks := func(want func(map[string]int64) bool) map[string]int64 {
		t.Helper()
		var got map[string]int64
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if got, _ = puller.Asks(ctx); want(got) {
				break
			}
		}
		return got
	}
	dstRun := &upstream.Runner{Service: up, Holder: "dst", Log: slog.New(slog.DiscardHandler)}
	dstRun.Once(ctx) // nothing asked, nothing eager: nothing fetched
	if cells, _ := up.Cells(ctx, serveAdmin, "acme", "src", "", [3]string{}, 10); len(cells) != 0 {
		t.Fatalf("a lazy mirror fetched unasked: %+v", cells)
	}

	// down: a lazy mirror of dst over https with a credential, kista_demo eager
	down := startKista(t, b)
	_, dstPort, _ := net.SplitHostPort(dst.addr)
	p, _ := strconv.Atoi(dstPort)
	deg, err := egress.New(egress.Config{OwnHost: "127.0.0.1", CAFile: testCA(t).caFile,
		Allow: []egress.Allow{{Prefix: netip.MustParsePrefix("127.0.0.1/32"), Ports: []uint16{uint16(p)}},
			{Prefix: netip.MustParsePrefix("::1/128"), Ports: []uint16{uint16(p)}}}})
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(idp.token(t, "mirror", "api://kista-dst")), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := credential.New([]config.Credential{{Name: "dst", Tenants: []string{"acme"}, Prefixes: []string{"https://localhost:" + dstPort + "/acme/"},
		Kind: "token_file", TokenFile: tokenFile}}, credential.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	dup := &upstream.Service{Store: down.st, Releases: down.rel, Blob: down.blob, Fetch: deg, Authz: authz.ServerAdmin{}, Credentials: creds,
		MaxBody: 256 << 20, MaxIngests: 2, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler),
		Config: upstream.Config{Interval: 6 * time.Hour}}
	if _, err := dup.Add(ctx, serveAdmin, "acme", upstream.Spec{Name: "dst", Kind: store.UpstreamRepo, Prefix: "https://localhost:" + dstPort + "/acme/prod",
		Channel: "prod", Credential: "dst", Keys: []string{dst.activeKey(t).Fingerprint}, Platforms: []string{b.platform},
		FetchMode: store.FetchLazy, Entries: []store.UpstreamEntry{{Name: kdemo, Eager: true}, {Name: demo}}}); err != nil {
		t.Fatal(err)
	}
	(&upstream.Runner{Service: dup, Holder: "down", Log: slog.New(slog.DiscardHandler)}).Once(ctx)
	cells, err := dup.Cells(ctx, serveAdmin, "acme", "dst", "", [3]string{}, 10)
	if err != nil || len(cells) != 1 || cells[0].Name != kdemo || cells[0].Outcome != store.CellReleased {
		t.Fatalf("down's first run: %+v %v", cells, err)
	}
	if got := asks(func(a map[string]int64) bool { return a[upstream.AskFetched] == 1 }); got[upstream.Ask503] < 1 || got[upstream.AskFetched] != 1 {
		t.Fatalf("dst's first requests from down: %v", got)
	}

	// DuckDB with a granted token: the first INSTALL from dst succeeds through its own retry
	before, _ := puller.Asks(ctx)
	pemKey := dst.pem(t, dst.activeKey(t))
	session := func(stmts ...string) []result {
		return newSession(t, b, nil).exec(append([]string{
			createRepo("boot", dst.httpURL(), pemKey),
			"INSTALL httpfs FROM boot",
			"LOAD httpfs FROM boot",
			"SET ca_cert_file = " + sqlString(testCA(t).caFile),
			createRepo("m", dst.httpsURL(), pemKey),
			"CREATE SECRET s (TYPE http, BEARER_TOKEN " + sqlString(idp.token(t, "alice", "api://kista-dst")) + ", SCOPE " + sqlString(dst.httpsURL()+"/") + ")",
		}, stmts...)...)
	}
	// a session installing what dst holds already: the time a session takes without a 503
	start := time.Now()
	mustOK(t, session("INSTALL "+kdemo+" FROM m"))
	baseline := time.Since(start)
	start = time.Now()
	mustOK(t, session("INSTALL "+demo+" FROM m", "LOAD "+demo+" FROM m"))
	took := time.Since(start) - baseline
	after := asks(func(a map[string]int64) bool { return a[upstream.AskFetched] == 2 })
	if after[upstream.Ask503] <= before[upstream.Ask503] || after[upstream.AskFetched] != 2 {
		t.Fatalf("DuckDB's first INSTALL was not answered 503 then served: before %v, after %v", before, after)
	}
	if took < 4*time.Second { // DuckDB waited the Retry-After (5 s), not only its own short retries
		t.Fatalf("DuckDB's first INSTALL took %v more than a served one: it did not wait the Retry-After", took)
	}

	// dst's next run refreshes the cell DuckDB asked for: a new version at src arrives
	f, err := os.Open(withVersion(t, b.extensions[demo], "e2e-2"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, _, err := src.rel.Add(ctx, serveAdmin, "acme", "prod", f, release.AddOptions{Name: demo}); err != nil {
		t.Fatal(err)
	}
	u, err := up.Get(ctx, serveAdmin, "acme", "src")
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.st.InTenantTx(ctx, u.TenantID, nil, func(tx *store.Tx) error { return tx.MakeDue(ctx, u.ID, "") }); err != nil {
		t.Fatal(err)
	}
	dstRun.Once(ctx)
	res := session("FORCE INSTALL "+demo+" FROM m", "LOAD "+demo+" FROM m",
		"SELECT extension_version FROM duckdb_extensions() WHERE extension_name = '"+demo+"'")
	mustOK(t, res)
	if got := res[len(res)-1].Rows[0][0]; got != "e2e-2" {
		t.Fatalf("after dst's next run DuckDB installs version %v", got)
	}
}
