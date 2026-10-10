package app

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/api"
	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/web/console"
)

// ConsoleInfo is the administration console's server sign-in configuration (spec 0015); nil when
// the console is off.
func ConsoleInfo(cfg config.Config) *api.Console {
	if !cfg.UIEnabled() {
		return nil
	}
	urls := map[string]string{}
	for _, s := range cfg.Auth.ServerIssuers {
		urls[s.Name] = s.URL
	}
	c := &api.Console{Environment: cfg.UI.Environment, AdminTokenMaxAge: int64(cfg.AdminTokenMaxAge().Seconds())}
	if auds := cfg.ServerAudiences(); len(auds) > 0 {
		c.Audience = auds[0]
	}
	for _, s := range cfg.UIServerClients() {
		c.Issuers = append(c.Issuers, tenants.ConsoleIssuer{Name: s.Issuer, Issuer: urls[s.Issuer], ClientID: s.ClientID,
			Scopes: s.Scopes, AudienceParameter: s.AudienceParameter, Audience: c.Audience})
	}
	return c
}

// originCache keeps each issuer's sign-in origins (from its discovery document) for the console's
// CSP, per egress (server or tenants: one fetcher's refusal never shapes the other's policy). An
// entry is used at once even when stale and refreshed in the background, once at a time; an issuer
// never read waits for its first read at most a page load's budget, its own origin meanwhile.
type originCache struct {
	mu sync.Mutex
	m  map[originKey]*originEntry
}

type originKey struct {
	server bool
	issuer string
}

type originEntry struct {
	origins []string
	until   time.Time
	reading chan struct{} // closed when the read in flight ends; nil: none
}

func (c *originCache) get(ctx context.Context, f auth.Fetcher, server bool, issuer string) []string {
	k := originKey{server, issuer}
	c.mu.Lock()
	if c.m == nil || len(c.m) >= 4096 {
		c.m = map[originKey]*originEntry{}
	}
	e := c.m[k]
	if e == nil {
		own, _ := auth.OriginOf(issuer)
		e = &originEntry{}
		if own != "" {
			e.origins = []string{own}
		}
		c.m[k] = e
	}
	if e.reading == nil && !time.Now().Before(e.until) {
		e.reading = make(chan struct{})
		go c.read(f, e, issuer)
	}
	first, wait := e.until.IsZero(), e.reading
	c.mu.Unlock()
	if first && wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return e.origins
}

func (c *originCache) read(f auth.Fetcher, e *originEntry, issuer string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	origins, err := auth.DiscoverOrigins(ctx, f, issuer)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil && len(origins) > 0 {
		e.origins, e.until = origins, time.Now().Add(time.Hour)
	} else {
		e.until = time.Now().Add(time.Minute) // its own origin, read again a minute later
	}
	close(e.reading)
	e.reading = nil
}

// ConsoleOrigins returns the IdP origins of a console scope (spec 0015): the server clients'
// issuers (read through the server issuers' egress) for the server scope, a tenant's issuers with
// a console client (the tenants' egress) for its scope; none for an unknown or suspended tenant.
func ConsoleOrigins(cfg config.Config, st *store.Store, serverFetch, tenantFetch auth.Fetcher) func(context.Context, console.Scope) []string {
	cache := &originCache{}
	urls := map[string]string{}
	for _, s := range cfg.Auth.ServerIssuers {
		urls[s.Name] = s.URL
	}
	return func(ctx context.Context, s console.Scope) []string {
		var out []string
		add := func(f auth.Fetcher, issuer string) {
			for _, o := range cache.get(ctx, f, s.Server, issuer) {
				if !slices.Contains(out, o) {
					out = append(out, o)
				}
			}
		}
		if s.Server {
			for _, c := range cfg.UIServerClients() {
				add(serverFetch, urls[c.Issuer])
			}
			return out
		}
		t, err := st.GetTenant(ctx, s.Tenant)
		if err != nil || t.State != store.TenantActive {
			return nil
		}
		iss, _, err := st.ConsoleClients(ctx, t.ID)
		if err != nil {
			return nil
		}
		for _, is := range iss {
			add(tenantFetch, is.URL)
		}
		return out
	}
}
