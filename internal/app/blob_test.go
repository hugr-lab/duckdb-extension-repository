package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/google/uuid"

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

// An azureblob domain from config (Azurite in dev) goes through the same startup checks.
func TestBlobServiceAzurite(t *testing.T) {
	ep := os.Getenv("KISTA_TEST_AZUREBLOB")
	if ep == "" {
		if os.Getenv("KISTA_TEST_REQUIRE_BLOB") == "1" {
			t.Fatal("KISTA_TEST_AZUREBLOB is required")
		}
		t.Skip("KISTA_TEST_AZUREBLOB not set")
	}
	ctx := context.Background()
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="), 0o600); err != nil {
		t.Fatal(err)
	}
	cred, _ := container.NewSharedKeyCredential("devstoreaccount1", "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==")
	c, err := container.NewClientWithSharedKeyCredential(ep+"/kista-app", cred, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create(ctx, nil); err != nil && !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(dir, "kista.yaml")
	body := "profile: dev\nstore: { kind: sqlite, sqlite: { path: " + filepath.Join(dir, "kista.db") + " } }\n" +
		"rotation: { min_trusted: 1h, min_demoted: 1h }\n" +
		"blob:\n  domains:\n    - { name: default, kind: azureblob, azureblob: { account: devstoreaccount1, container: kista-app, prefix: " +
		"a" + strings.ReplaceAll(uuid.NewString(), "-", "") + "/, endpoint: \"" + ep + "\", account_key_file: " + key + " } }\n"
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
	svc, err := BlobService(ctx, cfg, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc.Close()
	// a restart finds its own marker and pin
	svc, err = BlobService(ctx, cfg, st, nil)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	svc.Close()
}
