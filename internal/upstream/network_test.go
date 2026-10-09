package upstream_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/serve"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0009: real files of a released DuckDB version from DuckDB's core and community repositories
// pass the intake with the generated keys (KISTA_TEST_NETWORK=1; several MB each).
func TestNetworkIntake(t *testing.T) {
	if os.Getenv("KISTA_TEST_NETWORK") != "1" {
		t.Skip("KISTA_TEST_NETWORK=1 fetches from extensions.duckdb.org and community-extensions.duckdb.org")
	}
	var sqlite storetest.Engine
	for _, e := range storetest.Engines(t) {
		if e.Name == "sqlite" {
			sqlite = e
		}
	}
	en := newEnvWith(t, sqlite, 64<<20, egress.Config{})
	must(t, func() error {
		_, err := en.ten.AddVersion(ctx, admin, "v1.4.1", "release", []string{"v1.2.0"})
		return err
	})
	must(t, func() error {
		_, err := en.ten.CreateChannel(ctx, admin, "acme", "dw", store.ChannelSigned)
		return err
	})
	must(t, func() error {
		_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "dw", []string{"v1.4.1"}, nil)
		return err
	})
	if err := signer.GenerateKeyFile(filepath.Join(en.dir, "dw.pem")); err != nil {
		t.Fatal(err)
	}
	must(t, func() error { _, err := en.ks.Add(ctx, admin, "acme", "dw", "file:dw.pem", true); return err })
	for _, sp := range []upstream.Spec{
		{Name: "core", Kind: store.UpstreamCore, Channel: "dw", Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "inet"}}},
		{Name: "community", Kind: store.UpstreamCommunity, Channel: "dw", Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "h3"}}},
	} {
		if _, err := en.up.Add(ctx, admin, "acme", sp); err != nil {
			t.Fatal(err)
		}
		if _, err := en.up.Sync(ctx, admin, "acme", sp.Name, false); err != nil {
			t.Fatal(err)
		}
	}
	// DuckDB's own file through a passthrough channel, with its own signature (spec 0009 phase 1b)
	must(t, func() error {
		_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "mirror", []string{"v1.4.1"}, nil)
		return err
	})
	if _, err := en.up.Add(ctx, admin, "acme", upstream.Spec{Name: "passthrough", Kind: store.UpstreamCore, Channel: "mirror",
		Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "inet"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := en.up.Sync(ctx, admin, "acme", "passthrough", false); err != nil {
		t.Fatal(err)
	}
	en.run.Once(ctx)
	h := serve.NewHandler(en.st, en.ks, en.blob, serve.Options{MaxDownloads: 8, MaxDownloadsPerClient: 8, MinRate: 1024,
		WriteIdleTimeout: 10 * time.Second, Log: slog.New(slog.DiscardHandler), PublicURL: "https://kista.example",
		Auths: &auth.TenantAuths{Store: en.st}, Snapshots: &release.Snapshots{Store: en.st}})
	h.SetReady(true)
	code, gz, _ := fetch(t, h, "GET", "/acme/mirror/v1.4.1/linux_amd64/inet.duckdb_extension.gz")
	if code != 200 {
		t.Fatalf("the passthrough answer: %d", code)
	}
	resp, err := http.Get(upstream.CoreURL + "/v1.4.1/linux_amd64/inet.duckdb_extension.gz")
	if err != nil {
		t.Fatal(err)
	}
	orig, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gunzip(t, gz), gunzip(t, orig)) {
		t.Fatal("the passthrough file is not DuckDB's")
	}
	for _, name := range []string{"core", "community", "passthrough"} {
		u, err := en.up.Get(ctx, admin, "acme", name)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %s", name, u.LastRun)
		var r upstream.Result
		mustJSON(t, u.LastRun, &r)
		if r.Counts[store.CellReleased] != 1 {
			t.Errorf("%s: %+v", name, r)
		}
	}
}
