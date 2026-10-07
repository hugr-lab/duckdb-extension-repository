package keys

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

func TestWellKnownBytes(t *testing.T) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	now := time.Now()
	doc, err := wellKnown([]store.Key{{State: store.KeyActive, PublicKey: der, TrustedSince: now, Fingerprint: "f"}})
	if err != nil {
		t.Fatal(err)
	}
	// exactly one field, a JSON array of SPKI PEM strings with \n line breaks
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	want, _ := json.Marshal(map[string][]string{"signature_keys": {pemKey}})
	if string(doc) != string(want) || !strings.HasPrefix(string(doc), `{"signature_keys":["-----BEGIN PUBLIC KEY-----\n`) ||
		!strings.HasSuffix(string(doc), `-----END PUBLIC KEY-----\n"]}`) {
		t.Fatalf("document:\n%s", doc)
	}
	// over 64 KiB is refused
	big := make([]byte, MaxWellKnown)
	if _, err := wellKnown([]store.Key{{State: store.KeyActive, PublicKey: big, TrustedSince: now}}); !errors.Is(err, ErrState) {
		t.Fatalf("oversize: %v", err)
	}
	// retired keys are left out; only retired means no keys
	if _, err := wellKnown([]store.Key{{State: store.KeyRetired, PublicKey: der}}); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("retired only: %v", err)
	}
}
