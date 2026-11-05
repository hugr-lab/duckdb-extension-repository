package bootstrap_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/bootstrap"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// splitIDP is the fake IdP with its keys on another host (Google's case), counting fetches.
type splitIDP struct {
	*fakeIDP
	mu    sync.Mutex
	calls int
}

func (s *splitIDP) Get(ctx context.Context, url string) ([]byte, http.Header, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if url == idpURL+"/.well-known/openid-configuration" {
		return []byte(`{"issuer":"` + idpURL + `","jwks_uri":"https://keys.example/jwks"}`), http.Header{}, nil
	}
	return s.fakeIDP.Get(ctx, url)
}

func hostList(t *testing.T, entries ...string) *egress.Client {
	t.Helper()
	h, err := egress.NewHosts(entries)
	if err != nil {
		t.Fatal(err)
	}
	c, err := egress.New(egress.Config{Hosts: h})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func refusal(p bootstrap.Plan, kind, name string) (bootstrap.Item, bool) {
	for _, i := range p.Items {
		if i.Kind == kind && i.Name == name && i.Action == bootstrap.Refused {
			return i, true
		}
	}
	return bootstrap.Item{}, false
}

// Spec 0044: the bootstrap's issuer whose discovered jwks_uri is on a host egress.hosts leaves out
// (the configuration could not see it) is a refused item naming the host, at once: never tried
// again for the retry window, nothing written.
func TestBootstrapIssuerHostRefused(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			idp := &splitIDP{fakeIDP: en.idp}
			en.d.Auth.Fetch, en.d.Auth.Hosts = idp, hostList(t, "login.example")
			o := serveOpts
			o.Retry = 30 * time.Second // a transient failure would be tried again for this long
			start := time.Now()
			res, err := bootstrap.Run(ctx, en.d, section(), o)
			if !errors.Is(err, bootstrap.ErrRefused) || !strings.Contains(err.Error(), "keys.example") || time.Since(start) > 5*time.Second {
				t.Fatalf("refused: %v after %v", err, time.Since(start))
			}
			if it, ok := refusal(res.Plan, "issuer", "entra"); !ok || !strings.Contains(it.Detail, "keys.example is not on this server's egress host list") {
				t.Fatalf("the plan: %+v", res.Plan.Items)
			}
			if idp.calls != 1 { // the discovery document, once; the keys never
				t.Fatalf("fetched %d times", idp.calls)
			}
			if _, err := en.st.GetTenant(ctx, "acme"); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("written before the refusal")
			}
		})
	}
}

// Spec 0044: the bootstrap's upstreams on hosts egress.hosts leaves out (a repository's prefix, the
// built-in host of duckdb-core) are refused items, their sources never read, never tried again.
func TestBootstrapUpstreamHostRefused(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			src, fp := newSource(t)
			b := en.withUpstreams(t, src, fp)
			hosts := hostList(t, "login.example")
			en.d.Auth.Hosts, en.d.Upstreams.Hosts = hosts, hosts
			o := serveOpts
			o.Retry = 30 * time.Second
			start := time.Now()
			res, err := bootstrap.Run(ctx, en.d, b, o)
			if !errors.Is(err, bootstrap.ErrRefused) || time.Since(start) > 5*time.Second {
				t.Fatalf("refused: %v after %v", err, time.Since(start))
			}
			for name, host := range map[string]string{"vendor": "up.example", "core": "extensions.duckdb.org"} {
				if it, ok := refusal(res.Plan, "upstream", name); !ok || !strings.Contains(it.Detail, host) {
					t.Errorf("%s: %+v", name, res.Plan.Items)
				}
			}
			if src.reads != 0 {
				t.Fatalf("the source was read %d times", src.reads)
			}
		})
	}
}
