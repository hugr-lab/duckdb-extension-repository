//go:build live

package gcpkms_test

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"os"
	"testing"

	"golang.org/x/oauth2"

	gcpcloud "github.com/hugr-lab/duckdb-extension-repository/internal/cloud/gcp"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/gcpkms"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

// liveTokens are the stack's service account: an external_account file through kista's own
// credentials (KISTA_LIVE_GCP_CREDENTIALS and KISTA_LIVE_GCP_PRINCIPAL, as in GitHub Actions), or
// an access token got by impersonation (KISTA_LIVE_GCP_TOKEN, deploy/gcp/live.sh).
func liveTokens(t *testing.T) oauth2.TokenSource {
	t.Helper()
	if f := os.Getenv("KISTA_LIVE_GCP_CREDENTIALS"); f != "" {
		ts, err := gcpcloud.TokenSource(f, os.Getenv("KISTA_LIVE_GCP_PRINCIPAL"), gcpcloud.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	if tok := os.Getenv("KISTA_LIVE_GCP_TOKEN"); tok != "" {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok, TokenType: "Bearer"})
	}
	t.Skip("neither KISTA_LIVE_GCP_CREDENTIALS nor KISTA_LIVE_GCP_TOKEN is set")
	return nil
}

// TestLive signs with real Cloud KMS keys, the stack deploy/gcp/up.sh makes: KISTA_LIVE_GCP_KEYRING
// (projects/<p>/locations/<l>/keyRings/kista), with liveTokens. KISTA_LIVE_GCP_SOFTWARE=1 for a
// stack with software keys. Run with deploy/gcp/live.sh, or go test -tags live.
func TestLive(t *testing.T) {
	ring := os.Getenv("KISTA_LIVE_GCP_KEYRING")
	if ring == "" {
		t.Skip("KISTA_LIVE_GCP_KEYRING is not set")
	}
	ts := liveTokens(t)
	reg := keysource.NewRegistry(signer.Resolver{})
	if err := reg.Add("gcp", gcpkms.New(ring, ts, os.Getenv("KISTA_LIVE_GCP_SOFTWARE") != "1", nil, gcpkms.Options{}),
		keysource.Options{Allow: []string{"ext-"}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for ref, purpose := range map[string]string{"gcp:ext-channel/1": keysource.PurposeChannel,
		"gcp:ext-attestation/1": keysource.PurposeAttestation} {
		sg, err := reg.OpenPurpose(ctx, ref, purpose)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		d := extfile.BodyHash(sha256.Sum256([]byte("kista live test")))
		sig, err := sg.Sign(ctx, d)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if _, ok := extfile.Verify(d, sig, []*rsa.PublicKey{sg.Public()}); !ok {
			t.Fatalf("%s: the signature does not verify as DuckDB verifies", ref)
		}
	}
	// the label is read live: an attestation key is not a channel key
	if _, err := reg.OpenPurpose(ctx, "gcp:ext-attestation/1", keysource.PurposeChannel); !errors.Is(err, keysource.ErrKey) {
		t.Fatalf("the attestation key opened as a channel key: %v", err)
	}
	// a version that does not exist
	if _, err := reg.OpenPurpose(ctx, "gcp:ext-channel/9", keysource.PurposeChannel); err == nil {
		t.Fatal("a missing version opened")
	}
}
