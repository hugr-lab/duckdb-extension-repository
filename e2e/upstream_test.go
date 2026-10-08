package e2e

import (
	"context"
	"log/slog"
	"net"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
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
