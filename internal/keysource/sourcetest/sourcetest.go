// Package sourcetest is the contract suite every key-source backend runs against its fake (spec
// 0004, "Testing"). A backend provides a Harness: a fake that holds a real RSA key and can be
// mutated; the suite checks the common contract and the refusals the backend supports.
package sourcetest

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
)

// Mutation changes the fake's key or answers.
type Mutation string

// Mutations a harness may support. "Open" mutations make a key unusable: refused at open, and, after
// open, at the next re-check.
const (
	// key properties (refused at open and at re-check)
	Disabled        Mutation = "disabled"
	Expired         Mutation = "expired"
	NotYetValid     Mutation = "not yet valid"
	Exportable      Mutation = "exportable"
	ReleasePolicy   Mutation = "release policy"
	Imported        Mutation = "imported"
	WrongType       Mutation = "wrong type"
	SoftwareKey     Mutation = "software key where HSM is required"
	Size3072        Mutation = "3072-bit key"
	Exponent3       Mutation = "e = 3"
	CanEncrypt      Mutation = "may encrypt or wrap"
	NoPurpose       Mutation = "no purpose marker"
	CertBacked      Mutation = "certificate-backed"
	OtherVersion    Mutation = "metadata for another version"
	PublicKeyChange Mutation = "public key changed"

	// answers (checked on sign)
	SignOtherVersion Mutation = "sign answer names another version"
	CorruptSignature Mutation = "signature does not verify"

	// transport
	Hang             Mutation = "server hangs"
	SecretInBody     Mutation = "error body carries a secret"
	SecretInSignBody Mutation = "sign error body carries a secret"
	Transient        Mutation = "one transient failure"
	AlwaysFail       Mutation = "every attempt fails"
)

// KeyMutations are refused at open and stop signing after open.
var KeyMutations = []Mutation{Disabled, Expired, NotYetValid, Exportable, ReleasePolicy, Imported, WrongType,
	SoftwareKey, Size3072, Exponent3, CanEncrypt, NoPurpose, CertBacked, OtherVersion}

// Secret is planted by SecretInBody; it must never appear in an error.
const Secret = "hunter2-planted-secret"

// Harness is a backend's fake.
type Harness interface {
	// New starts a fresh fake (a valid key) and returns a registry with the source "src" and the
	// reference to the key.
	New(t *testing.T, opts keysource.Options) (*keysource.Registry, string)
	// Key is the fake's private key (for the known answer).
	Key() *rsa.PrivateKey
	// Mutate applies a mutation; it returns false if the backend has no such property.
	Mutate(t *testing.T, m Mutation) bool
}

var ctx = context.Background()

func digest() extfile.BodyHash { return extfile.BodyHash(sha256.Sum256([]byte("an extension body"))) }

// Run runs the contract suite.
func Run(t *testing.T, newHarness func(t *testing.T) Harness) {
	t.Run("known answer", func(t *testing.T) {
		h := newHarness(t)
		reg, ref := h.New(t, keysource.Options{})
		sg, err := reg.Open(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		d := digest()
		sig, err := sg.Sign(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := rsa.SignPKCS1v15(nil, h.Key(), crypto.SHA256, d[:])
		if string(sig) != string(want) {
			t.Fatal("not the bytes rsa.SignPKCS1v15 produces over the digest")
		}
		if sg.ID() != ref {
			t.Fatalf("ID %q, want the reference %q", sg.ID(), ref)
		}
	})
	for _, m := range KeyMutations {
		t.Run("refused at open: "+string(m), func(t *testing.T) {
			h := newHarness(t)
			reg, ref := h.New(t, keysource.Options{})
			if !h.Mutate(t, m) {
				t.Skip("not a property of this backend")
			}
			if _, err := reg.Open(ctx, ref); !errors.Is(err, keysource.ErrKey) {
				t.Fatalf("got %v, want ErrKey", err)
			}
		})
		t.Run("stops signing after open: "+string(m), func(t *testing.T) {
			h := newHarness(t)
			reg, ref := h.New(t, keysource.Options{Recheck: time.Nanosecond})
			sg, err := reg.Open(ctx, ref)
			if err != nil {
				t.Fatal(err)
			}
			if !h.Mutate(t, m) {
				t.Skip("not a property of this backend")
			}
			if _, err := sg.Sign(ctx, digest()); !errors.Is(err, keysource.ErrKey) {
				t.Fatalf("signed after %s: %v", m, err)
			}
		})
	}
	t.Run("public key changed after open", func(t *testing.T) {
		h := newHarness(t)
		reg, ref := h.New(t, keysource.Options{Recheck: time.Nanosecond})
		sg, err := reg.Open(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if !h.Mutate(t, PublicKeyChange) {
			t.Skip("not supported")
		}
		if _, err := sg.Sign(ctx, digest()); !errors.Is(err, keysource.ErrKey) {
			t.Fatalf("signed after the public key changed: %v", err)
		}
	})
	for _, m := range []Mutation{SignOtherVersion, CorruptSignature} {
		t.Run("sign: "+string(m), func(t *testing.T) {
			h := newHarness(t)
			reg, ref := h.New(t, keysource.Options{})
			sg, err := reg.Open(ctx, ref)
			if err != nil {
				t.Fatal(err)
			}
			if !h.Mutate(t, m) {
				t.Skip("not supported")
			}
			if _, err := sg.Sign(ctx, digest()); !errors.Is(err, keysource.ErrSign) {
				t.Fatalf("got %v, want ErrSign", err)
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		h := newHarness(t)
		reg, ref := h.New(t, keysource.Options{Timeout: 300 * time.Millisecond})
		if !h.Mutate(t, Hang) {
			t.Skip("not supported")
		}
		start := time.Now()
		if _, err := reg.Open(ctx, ref); err == nil {
			t.Fatal("opened a hanging server")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("open took %s", d)
		}
	})
	t.Run("one transient failure is retried", func(t *testing.T) {
		h := newHarness(t)
		reg, ref := h.New(t, keysource.Options{})
		if !h.Mutate(t, Transient) {
			t.Skip("not supported")
		}
		sg, err := reg.Open(ctx, ref)
		if err != nil {
			t.Fatalf("open after one transient failure: %v", err)
		}
		if _, err := sg.Sign(ctx, digest()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("persistent failures are not retried forever", func(t *testing.T) {
		h := newHarness(t)
		reg, ref := h.New(t, keysource.Options{})
		if !h.Mutate(t, AlwaysFail) {
			t.Skip("not supported")
		}
		if _, err := reg.Open(ctx, ref); err == nil {
			t.Fatal("opened while every attempt fails")
		}
	})
	t.Run("sign errors carry no body", func(t *testing.T) {
		h := newHarness(t)
		reg, ref := h.New(t, keysource.Options{})
		sg, err := reg.Open(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if !h.Mutate(t, SecretInSignBody) {
			t.Skip("not supported")
		}
		_, err = sg.Sign(ctx, digest())
		if err == nil || strings.Contains(err.Error(), Secret) {
			t.Fatalf("sign error leaked the body: %v", err)
		}
	})
	t.Run("errors carry no body", func(t *testing.T) {
		h := newHarness(t)
		reg, ref := h.New(t, keysource.Options{})
		if !h.Mutate(t, SecretInBody) {
			t.Skip("not supported")
		}
		_, err := reg.Open(ctx, ref)
		if err == nil || strings.Contains(err.Error(), Secret) {
			t.Fatalf("error leaked the body: %v", err)
		}
	})
}
