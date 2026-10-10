package app

import (
	"context"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
)

type slowFetcher struct {
	calls atomic.Int32
	delay time.Duration
	doc   string
}

func (f *slowFetcher) Get(ctx context.Context, _ string) ([]byte, http.Header, error) {
	f.calls.Add(1)
	select {
	case <-time.After(f.delay):
		return []byte(f.doc), nil, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// Spec 0015: an issuer never read answers its own origin within the page's budget, one read at a
// time, then its discovered origins; the server's and the tenants' egress keep separate entries.
func TestOriginCache(t *testing.T) {
	f := &slowFetcher{delay: 200 * time.Millisecond, doc: `{"issuer":"https://idp.example","token_endpoint":"https://tok.example/t"}`}
	c := &originCache{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	got := c.get(ctx, f, false, "https://idp.example")
	cancel()
	if !slices.Equal(got, []string{"https://idp.example"}) {
		t.Fatalf("before the read: %v", got)
	}
	for range 5 {
		c.get(context.Background(), f, false, "https://idp.example") // waits for the one read in flight
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("%d reads, want 1", n)
	}
	if got := c.get(context.Background(), f, false, "https://idp.example"); !slices.Equal(got, []string{"https://idp.example", "https://tok.example"}) {
		t.Fatalf("after the read: %v", got)
	}
	if got := c.get(context.Background(), f, true, "https://idp.example"); f.calls.Load() != 2 || len(got) != 2 {
		t.Fatalf("the server's entry is its own: %v after %d reads", got, f.calls.Load())
	}
}

// ui.enabled false: no console routes (the API answers 404 when Console is nil).
func TestConsoleInfoOff(t *testing.T) {
	off := false
	var cfg config.Config
	cfg.UI.Enabled = &off
	if ConsoleInfo(cfg) != nil {
		t.Fatal("console info with the console off")
	}
	cfg.UI.Enabled = nil
	cfg.Serve.PublicURL = "https://kista.example"
	if c := ConsoleInfo(cfg); c == nil || c.Audience != "https://kista.example" || c.AdminTokenMaxAge != 3600 {
		t.Fatalf("console info: %+v", c)
	}
}
