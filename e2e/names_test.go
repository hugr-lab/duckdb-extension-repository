package e2e

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0039 PR 2: kista B mirrors kista A's prod with "*". B's run finds no name while A serves
// none; once A serves the demo, B's next run discovers it from A's -/names, fetches it, and DuckDB
// installs it from B.
func TestStarMirror(t *testing.T) {
	b := needBuild(t)
	ctx := context.Background()
	const demo = "loadable_extension_demo"
	a := startKista(t, b)
	bk := startKista(t, b)

	_, port, _ := net.SplitHostPort(a.addr)
	p, _ := strconv.Atoi(port)
	eg, err := egress.New(egress.Config{OwnHost: "127.0.0.1", CAFile: testCA(t).caFile,
		Allow: []egress.Allow{{Prefix: netip.MustParsePrefix("127.0.0.1/32"), Ports: []uint16{uint16(p)}},
			{Prefix: netip.MustParsePrefix("::1/128"), Ports: []uint16{uint16(p)}}}})
	if err != nil {
		t.Fatal(err)
	}
	up := &upstream.Service{Store: bk.st, Releases: bk.rel, Blob: bk.blob, Fetch: eg, Authz: authz.ServerAdmin{}, MaxBody: 256 << 20,
		MaxIngests: 2, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	u, err := up.Add(ctx, serveAdmin, "acme", upstream.Spec{Name: "src", Kind: store.UpstreamRepo, Prefix: "https://localhost:" + port + "/acme/prod",
		Channel: "prod", Visibility: store.Public, Keys: []string{a.activeKey(t).Fingerprint}, Platforms: []string{b.platform},
		Entries: []store.UpstreamEntry{{Name: "*"}}})
	if err != nil || !u.RemoteKista {
		t.Fatalf("B's mirror of A with *: %+v %v", u, err)
	}
	run := func() store.Upstream {
		t.Helper()
		if _, err := up.Sync(ctx, serveAdmin, "acme", "src", false); err != nil {
			t.Fatal(err)
		}
		(&upstream.Runner{Service: up, Holder: "e2e", Log: slog.New(slog.DiscardHandler)}).Once(ctx)
		u, err := up.Get(ctx, serveAdmin, "acme", "src")
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	if u := run(); len(u.Discovered) != 0 {
		t.Fatalf("discovered from an empty channel: %v", u.Discovered)
	}

	// A serves the demo, public
	data, err := os.ReadFile(b.extensions[demo])
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.rel.Add(ctx, serveAdmin, "acme", "prod", bytes.NewReader(data), release.AddOptions{Name: demo}); err != nil {
		t.Fatal(err)
	}
	if u := run(); !slices.Equal(u.Discovered, []string{demo}) {
		t.Fatalf("discovered: %v (last run %s)", u.Discovered, u.LastRun)
	}
	repo := createRepo("r", "http://"+bk.addr+"/acme/prod", bk.pem(t, activeKeyOf(t, bk, "prod")))
	mustOK(t, newSession(t, b, nil).exec(repo, "INSTALL "+demo+" FROM r"))
}
