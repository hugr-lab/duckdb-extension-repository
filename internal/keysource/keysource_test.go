package keysource

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

type fakeBackend struct {
	key     *rsa.PrivateKey
	opens   atomic.Int32
	checks  atomic.Int32
	failChk atomic.Bool
	delay   time.Duration
	active  atomic.Int32
	maxSeen atomic.Int32
}

func (f *fakeBackend) Kind() string { return "fake" }
func (f *fakeBackend) ParseKey(s string) (Key, error) {
	return Key{Name: s, Version: "1"}, nil
}
func (f *fakeBackend) Open(context.Context, Key) (Handle, error) {
	f.opens.Add(1)
	return &fakeHandle{f: f}, nil
}

type fakeHandle struct{ f *fakeBackend }

func (h *fakeHandle) Public() *rsa.PublicKey { return &h.f.key.PublicKey }
func (h *fakeHandle) Identity() string       { return "k:1" }
func (h *fakeHandle) Check(context.Context) error {
	h.f.checks.Add(1)
	if h.f.failChk.Load() {
		return errors.New("disabled")
	}
	return nil
}
func (h *fakeHandle) Sign(ctx context.Context, d []byte) ([]byte, string, error) {
	n := h.f.active.Add(1)
	defer h.f.active.Add(-1)
	for {
		m := h.f.maxSeen.Load()
		if n <= m || h.f.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	select {
	case <-time.After(h.f.delay):
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	sig, err := rsa.SignPKCS1v15(rand.Reader, h.f.key, crypto.SHA256, d)
	return sig, "k:1", err
}

func newFake(t *testing.T) *fakeBackend {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeBackend{key: k}
}

func TestRegistry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := signer.GenerateKeyFile(filepath.Join(dir, "a.pem")); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(signer.Resolver{FileDir: dir, AllowFile: true})
	f := newFake(t)
	if err := reg.Add("file", f, Options{Allow: []string{"*"}}); err == nil {
		t.Fatal("source named file")
	}
	if err := reg.Add("src", f, Options{Allow: []string{"*"}, MaxConcurrency: 2, Recheck: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add("src", f, Options{Allow: []string{"*"}}); err == nil {
		t.Fatal("duplicate source")
	}
	// file references keep working through the built-in source
	if sg, err := reg.Open(ctx, "file:a.pem"); err != nil || sg == nil {
		t.Fatalf("file: %v", err)
	}
	sg, err := reg.Open(ctx, "src:k")
	if err != nil {
		t.Fatal(err)
	}
	if sg.ID() != "src:k" {
		t.Fatalf("ID %q", sg.ID())
	}
	// the concurrency limit holds
	f.delay = 50 * time.Millisecond
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sg.Sign(ctx, extfile.BodyHash{1}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.maxSeen.Load() > 2 {
		t.Fatalf("%d concurrent signs, limit 2", f.maxSeen.Load())
	}
	// the re-check runs when due and fails closed
	f.delay = 0
	c := sg.(*contract)
	c.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	f.failChk.Store(true)
	if _, err := sg.Sign(ctx, extfile.BodyHash{1}); !errors.Is(err, ErrKey) {
		t.Fatalf("re-check: %v", err)
	}
	if f.checks.Load() == 0 {
		t.Fatal("no re-check ran")
	}
	// a call that outlives its timeout fails
	slow := newFake(t)
	slow.delay = time.Second
	reg2 := NewRegistry(signer.Resolver{})
	reg2.Add("slow", slow, Options{Allow: []string{"*"}, Timeout: 20 * time.Millisecond})
	sg2, err := reg2.Open(ctx, "slow:k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sg2.Sign(ctx, extfile.BodyHash{1}); err == nil {
		t.Fatal("no timeout")
	}
}
