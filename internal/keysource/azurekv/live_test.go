//go:build live

package azurekv_test

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"os"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/cloud/azure"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/azurekv"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

// TestLive signs with a real Key Vault key: KISTA_LIVE_AZURE_HOST (e.g. kista.vault.azure.net) and
// KISTA_LIVE_AZURE_KEY (<name>/<version>), with the developer credential chain. Run manually or in a
// protected environment: go test -tags live ./internal/keysource/azurekv/
func TestLive(t *testing.T) {
	host, key := os.Getenv("KISTA_LIVE_AZURE_HOST"), os.Getenv("KISTA_LIVE_AZURE_KEY")
	if host == "" || key == "" {
		t.Skip("KISTA_LIVE_AZURE_HOST / KISTA_LIVE_AZURE_KEY are not set")
	}
	cred, err := azure.Credential(azure.Identity{Kind: "default"}, "public", true)
	if err != nil {
		t.Fatal(err)
	}
	src, err := azurekv.New(host, cred, !strings.EqualFold(os.Getenv("KISTA_LIVE_AZURE_SOFTWARE"), "1"), azurekv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	reg := keysource.NewRegistry(signer.Resolver{})
	reg.Add("live", src, keysource.Options{Allow: []string{"*"}})
	sg, err := reg.Open(context.Background(), "live:"+key)
	if err != nil {
		t.Fatal(err)
	}
	d := extfile.BodyHash(sha256.Sum256([]byte("kista live test")))
	sig, err := sg.Sign(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := extfile.Verify(d, sig, []*rsa.PublicKey{sg.Public()}); !ok {
		t.Fatal("the signature does not verify as DuckDB verifies")
	}
}
