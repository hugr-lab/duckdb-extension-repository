package keys_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

func TestMain(m *testing.M) {
	code := m.Run()
	storetest.Cleanup()
	os.Exit(code)
}

var (
	ctx   = context.Background()
	admin = authz.Actor{Kind: authz.ActorOS, ID: "1000:test"}
	week  = 7 * 24 * time.Hour
)

// clock is a settable clock for the store.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	svc   *keys.Service
	ten   *tenants.Service
	dir   string
	clock *clock
}

// newEnv returns a keys service on the engine with a tenant "acme" and channels "prod" (signed)
// and "mirror" (passthrough), and a key directory.
func newEnv(t *testing.T, e storetest.Engine) *env {
	t.Helper()
	s := e.Open(t)
	c := &clock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	s.Now = c.now
	dir := t.TempDir()
	en := &env{
		svc: &keys.Service{Store: s, Signers: signer.Resolver{FileDir: dir, AllowFile: true},
			Authz: authz.ServerAdmin{}, MinTrusted: week, MinDemoted: week},
		ten:   &tenants.Service{Store: s, Authz: authz.ServerAdmin{}},
		dir:   dir,
		clock: c,
	}
	if _, err := en.ten.CreateTenant(ctx, admin, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	for name, kind := range map[string]string{"prod": store.ChannelSigned, "mirror": store.ChannelPassthrough, "staging": store.ChannelSigned} {
		if _, err := en.ten.CreateChannel(ctx, admin, "acme", name, kind); err != nil {
			t.Fatal(err)
		}
	}
	return en
}

// keyFile creates a key file and returns its file: reference.
func (en *env) keyFile(t *testing.T, name string) string {
	t.Helper()
	if err := signer.GenerateKeyFile(filepath.Join(en.dir, name)); err != nil {
		t.Fatal(err)
	}
	return "file:" + name
}

func each(t *testing.T, f func(t *testing.T, en *env)) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) { f(t, newEnv(t, e)) })
	}
}

func states(t *testing.T, en *env, channel string) map[string]string {
	t.Helper()
	ks, err := en.svc.List(ctx, admin, "acme", channel)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, k := range ks {
		out[k.Fingerprint] = k.State
	}
	return out
}

func wellKnownFPs(t *testing.T, en *env, channel string) []string {
	t.Helper()
	doc, _, err := en.svc.WellKnown(ctx, admin, "acme", channel)
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		SignatureKeys []string `json:"signature_keys"`
	}
	if err := json.Unmarshal(doc, &w); err != nil {
		t.Fatal(err)
	}
	var fps []string
	for _, k := range w.SignatureKeys {
		pub, err := extfile.ParsePublicKey(k)
		if err != nil {
			t.Fatal(err)
		}
		fps = append(fps, extfile.Fingerprint(pub))
	}
	return fps
}

func TestRotation(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		a, err := en.svc.Add(ctx, admin, "acme", "prod", en.keyFile(t, "a.pem"), true)
		if err != nil {
			t.Fatal(err)
		}
		if a.State != store.KeyActive {
			t.Fatalf("first key %s", a.State)
		}
		en.clock.add(time.Hour)
		b, err := en.svc.Add(ctx, admin, "acme", "prod", en.keyFile(t, "b.pem"), false)
		if err != nil {
			t.Fatal(err)
		}
		if got := wellKnownFPs(t, en, "prod"); len(got) != 2 || got[0] != a.Fingerprint || got[1] != b.Fingerprint {
			t.Fatalf(".well-known %v", got)
		}

		// too soon, then forced is possible but we wait instead
		if _, err := en.svc.Activate(ctx, admin, "acme", "prod", b.ID, false); !errors.Is(err, keys.ErrTooSoon) {
			t.Fatalf("early activation: %v", err)
		}
		en.clock.add(week)
		if _, err := en.svc.Activate(ctx, admin, "acme", "prod", b.Fingerprint, false); err != nil {
			t.Fatal(err)
		}
		if st := states(t, en, "prod"); st[a.Fingerprint] != store.KeyTrusted || st[b.Fingerprint] != store.KeyActive {
			t.Fatalf("after activation %v", st)
		}
		if got := wellKnownFPs(t, en, "prod"); got[0] != b.Fingerprint {
			t.Fatalf("active key not first: %v", got)
		}

		// rollback: the demoted key was trusted long ago, so it can be activated at once
		if _, err := en.svc.Activate(ctx, admin, "acme", "prod", a.ID, false); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		if _, err := en.svc.Activate(ctx, admin, "acme", "prod", b.ID, false); err != nil {
			t.Fatal(err)
		}

		// retire: never the active key; a just-demoted key only after MinDemoted
		if _, err := en.svc.Retire(ctx, admin, "acme", "prod", b.ID, false); !errors.Is(err, keys.ErrState) {
			t.Fatalf("retire active: %v", err)
		}
		if _, err := en.svc.Retire(ctx, admin, "acme", "prod", a.ID, false); !errors.Is(err, keys.ErrTooSoon) {
			t.Fatalf("early retire: %v", err)
		}
		en.clock.add(week)
		if _, err := en.svc.Retire(ctx, admin, "acme", "prod", a.ID, false); err != nil {
			t.Fatal(err)
		}
		if got := wellKnownFPs(t, en, "prod"); len(got) != 1 || got[0] != b.Fingerprint {
			t.Fatalf(".well-known after retire %v", got)
		}
		// a retired key never comes back
		if _, err := en.svc.Activate(ctx, admin, "acme", "prod", a.ID, true); !errors.Is(err, keys.ErrState) {
			t.Fatalf("reactivate retired: %v", err)
		}

		evs, err := en.svc.Events(ctx, admin, "acme", "prod")
		if err != nil {
			t.Fatal(err)
		}
		// add a, add b, (a→trusted, b→active), (b→trusted, a→active), (a→trusted, b→active), a→retired
		if len(evs) != 9 {
			t.Fatalf("%d events: %+v", len(evs), evs)
		}
		for _, e := range evs {
			if e.Actor != admin.String() || e.Forced {
				t.Fatalf("event %+v", e)
			}
		}
	})
}

func TestForce(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		if _, err := en.svc.Add(ctx, admin, "acme", "prod", en.keyFile(t, "a.pem"), true); err != nil {
			t.Fatal(err)
		}
		b, err := en.svc.Add(ctx, admin, "acme", "prod", en.keyFile(t, "b.pem"), false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := en.svc.Activate(ctx, admin, "acme", "prod", b.ID, true); err != nil {
			t.Fatal(err)
		}
		evs, _ := en.svc.Events(ctx, admin, "acme", "prod")
		last := evs[len(evs)-1]
		if last.KeyID != b.ID || last.To != store.KeyActive || !last.Forced {
			t.Fatalf("forced activation not recorded: %+v", last)
		}
	})
}

func TestAddRules(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		// passthrough channels hold no keys
		if _, err := en.svc.Add(ctx, admin, "acme", "mirror", en.keyFile(t, "m.pem"), false); !errors.Is(err, keys.ErrState) {
			t.Fatalf("passthrough: %v", err)
		}
		// a signed channel with no key has no .well-known
		if _, _, err := en.svc.WellKnown(ctx, admin, "acme", "prod"); !errors.Is(err, keys.ErrNoKeys) {
			t.Fatalf("empty .well-known: %v", err)
		}
		a, err := en.svc.Add(ctx, admin, "acme", "prod", en.keyFile(t, "a.pem"), false)
		if err != nil {
			t.Fatal(err)
		}
		// --active only on a channel that never had a key
		if _, err := en.svc.Add(ctx, admin, "acme", "prod", en.keyFile(t, "b.pem"), true); !errors.Is(err, keys.ErrState) {
			t.Fatalf("second key as active: %v", err)
		}
		// the same key cannot serve two channels
		if _, err := en.svc.Add(ctx, admin, "acme", "staging", "file:a.pem", false); !errors.Is(err, store.ErrExists) {
			t.Fatalf("shared key: %v", err)
		}
		// a key is addressed through its own channel only
		if _, err := en.svc.Activate(ctx, admin, "acme", "staging", a.ID, true); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("key through another channel: %v", err)
		}
		// at most MaxKeys non-retired keys
		for i := 2; i < keys.MaxKeys+1; i++ { // a.pem is one; b.pem failed
			if _, err := en.svc.Add(ctx, admin, "acme", "prod", en.keyFile(t, "k"+strconv.Itoa(i)+".pem"), false); err != nil {
				t.Fatalf("key %d: %v", i, err)
			}
		}
		if _, err := en.svc.Add(ctx, admin, "acme", "prod", en.keyFile(t, "over.pem"), false); !errors.Is(err, keys.ErrState) {
			t.Fatalf("over the cap: %v", err)
		}
		// references outside the key directory are refused
		for _, ref := range []string{"file:../x.pem", "file:/etc/passwd", "file:", "azurekv:https://v.vault.azure.net/keys/k/1", "s3://x"} {
			if _, err := en.svc.Add(ctx, admin, "acme", "staging", ref, false); err == nil {
				t.Errorf("%s accepted", ref)
			}
		}
		// only the server administrator acts
		p := authz.Actor{Kind: authz.ActorPrincipal, ID: "iss|sub"}
		if _, err := en.svc.List(ctx, p, "acme", "prod"); !errors.Is(err, authz.ErrDenied) {
			t.Fatalf("principal: %v", err)
		}
	})
}

// fakeOpener returns signers that misbehave.
type fakeOpener struct{ s signer.Signer }

func (f fakeOpener) Open(context.Context, string) (signer.Signer, error) { return f.s, nil }

type badSigner struct{ signer.Signer }

func (b badSigner) Sign(context.Context, extfile.BodyHash) ([]byte, error) {
	return make([]byte, extfile.SignatureSize), nil
}

func TestProbeAndMismatch(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		ref := en.keyFile(t, "a.pem")
		good, err := signer.Resolver{FileDir: en.dir, AllowFile: true}.Open(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		bad := *en.svc
		bad.Signers = fakeOpener{badSigner{good}}
		if _, err := bad.Add(ctx, admin, "acme", "prod", ref, false); err == nil {
			t.Fatal("a key that cannot sign was added")
		}
		k, err := en.svc.Add(ctx, admin, "acme", "prod", ref, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := en.svc.OpenSigner(ctx, k); err != nil {
			t.Fatal(err)
		}
		// the file changes under the reference: the signer fails closed
		if err := os.Remove(filepath.Join(en.dir, "a.pem")); err != nil {
			t.Fatal(err)
		}
		en.keyFile(t, "a.pem")
		if _, err := en.svc.OpenSigner(ctx, k); !errors.Is(err, keys.ErrMismatch) {
			t.Fatalf("changed key file: %v", err)
		}
	})
}

func TestWellKnownDeterministic(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		var fps []string
		for i := 0; i < 4; i++ {
			k, err := en.svc.Add(ctx, admin, "acme", "prod", en.keyFile(t, "k"+strconv.Itoa(i)+".pem"), i == 0)
			if err != nil {
				t.Fatal(err)
			}
			fps = append(fps, k.Fingerprint)
		}
		d1, v1, err := en.svc.WellKnown(ctx, admin, "acme", "prod")
		if err != nil {
			t.Fatal(err)
		}
		d2, v2, _ := en.svc.WellKnown(ctx, admin, "acme", "prod")
		if string(d1) != string(d2) || v1 != v2 {
			t.Fatal("not deterministic")
		}
		// keys added in the same instant are ordered by fingerprint after the active key
		got := wellKnownFPs(t, en, "prod")
		if got[0] != fps[0] || len(got) != 4 {
			t.Fatalf("order %v", got)
		}
		for i := 2; i < len(got); i++ {
			if got[i-1] > got[i] {
				t.Fatalf("trusted keys not sorted: %v", got)
			}
		}
		// every key change bumps the channel version
		en.clock.add(time.Minute)
		if _, err := en.svc.Add(ctx, admin, "acme", "prod", en.keyFile(t, "k9.pem"), false); err != nil {
			t.Fatal(err)
		}
		if _, v3, _ := en.svc.WellKnown(ctx, admin, "acme", "prod"); v3 <= v1 {
			t.Fatalf("channel version %d -> %d", v1, v3)
		}
	})
}

func TestRetiredKeysCount(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		a, err := en.svc.Add(ctx, admin, "acme", "staging", en.keyFile(t, "a.pem"), false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := en.svc.Retire(ctx, admin, "acme", "staging", a.ID, true); err != nil {
			t.Fatal(err)
		}
		// a channel that only ever had a retired key still had a key: no --active
		if _, err := en.svc.Add(ctx, admin, "acme", "staging", en.keyFile(t, "b.pem"), true); !errors.Is(err, keys.ErrState) {
			t.Fatalf("--active after a retired key: %v", err)
		}
		// retired keys do not count against the cap
		for i := 0; i < keys.MaxKeys; i++ {
			if _, err := en.svc.Add(ctx, admin, "acme", "staging", en.keyFile(t, "c"+strconv.Itoa(i)+".pem"), false); err != nil {
				t.Fatalf("key %d: %v", i, err)
			}
		}
		// Retire and Activate address a key only through its own channel and tenant
		if _, err := en.ten.CreateTenant(ctx, admin, "other", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := en.ten.CreateChannel(ctx, admin, "other", "staging", store.ChannelSigned); err != nil {
			t.Fatal(err)
		}
		list, _ := en.svc.List(ctx, admin, "acme", "staging")
		for _, ch := range [][2]string{{"acme", "prod"}, {"other", "staging"}} {
			if _, err := en.svc.Retire(ctx, admin, ch[0], ch[1], list[1].ID, true); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("retire through %v: %v", ch, err)
			}
			if _, err := en.svc.Activate(ctx, admin, ch[0], ch[1], list[1].Fingerprint, true); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("activate through %v: %v", ch, err)
			}
		}
	})
}

func TestOpenSignerChecksStoredKey(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		k, err := en.svc.Add(ctx, admin, "acme", "prod", en.keyFile(t, "a.pem"), true)
		if err != nil {
			t.Fatal(err)
		}
		for name, mutate := range map[string]func(*store.Key){
			"empty fingerprint": func(k *store.Key) { k.Fingerprint = "" },
			"other public key":  func(k *store.Key) { k.PublicKey = append([]byte{}, k.PublicKey[:len(k.PublicKey)-1]...) },
			"no public key":     func(k *store.Key) { k.PublicKey = nil },
		} {
			bad := k
			mutate(&bad)
			if _, err := en.svc.OpenSigner(ctx, bad); !errors.Is(err, keys.ErrMismatch) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}
