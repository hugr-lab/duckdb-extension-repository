package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

func TestBlobService(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "kista.yaml")
	body := "profile: dev\nstore: { kind: sqlite, sqlite: { path: " + filepath.Join(dir, "kista.db") + " } }\n" +
		"rotation: { min_trusted: 1h, min_demoted: 1h }\n"
	if err := os.WriteFile(cfgFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.InTx(ctx, "", func(tx *store.Tx) error {
		return tx.CreateTenant(ctx, &store.Tenant{Name: "acme-cn", StorageDomain: "cn"})
	}); err != nil {
		t.Fatal(err)
	}
	svc, err := BlobService(ctx, cfg, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if d := svc.Domains(); len(d) != 1 || d[0] != "default" {
		t.Fatalf("domains %v", d)
	}
	// the dev default: beside the database
	for _, p := range []string{"blobs/kista-domain.json", "spool"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Fatal(err)
		}
	}
	if m, _ := svc.MissingDomains(ctx); len(m) != 1 || m[0] != "acme-cn" {
		t.Fatalf("missing %v", m)
	}
}
