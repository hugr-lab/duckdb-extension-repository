package upstream_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
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
	en.run.Once(ctx)
	for _, name := range []string{"core", "community"} {
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
