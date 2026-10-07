package app

import (
	"errors"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
)

// KeySources builds every configured source without contacting it.
func TestKeySources(t *testing.T) {
	cfg := config.Default()
	cfg.Store = config.Store{Kind: "sqlite", SQLite: config.SQLite{Path: "/tmp/x.db"}, Migrate: "auto", Login: config.Login{Kind: "password"}}
	cfg.Signers.Sources = []config.Source{
		{Name: "prod", Kind: "azurekv", Allow: []string{"ext-"}, AzureKV: &config.AzureKVSource{Vault: "kista-prod", Cloud: "usgov"},
			Identity: &config.Identity{Kind: "managed", ClientID: "c"}},
		{Name: "bao", Kind: "vault", Allow: []string{"ext-"}, Vault: &config.VaultSource{Address: "https://bao.example:8200",
			Mount: "transit", SoftwareKeys: true, Auth: config.VaultAuth{Kind: "token_file", TokenFile: "/run/t"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	reg, err := KeySources(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, k, err := reg.Parse("prod:EXT-a/0123456789abcdef0123456789abcdef"); err != nil || k.Name != "ext-a" {
		t.Fatalf("azurekv reference: %+v %v", k, err)
	}
	if _, _, err := reg.Parse("bao:ext-a:v3"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Parse("prod:other/0123456789abcdef0123456789abcdef"); !errors.Is(err, keysource.ErrReference) {
		t.Fatalf("allow list: %v", err)
	}
}
