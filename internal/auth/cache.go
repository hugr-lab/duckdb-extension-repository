package auth

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// TenantAuths caches each tenant's issuers, audiences, grants and extension groups, one entry per
// tenant at the auth_version it was read at, which every change bumps (spec 0006, 0017): the DuckDB
// routes and the API read them through one cache.
type TenantAuths struct {
	Store *store.Store

	mu    sync.Mutex
	cache map[string]store.TenantAuth // tenant id
}

// Get returns a tenant's auth at the auth_version the caller read or later: an entry is served when
// it is at least that version, and replaced only by a later one, so callers holding older tenant
// rows do not make it thrash.
func (c *TenantAuths) Get(ctx context.Context, t store.Tenant) (store.TenantAuth, error) {
	c.mu.Lock()
	ta, ok := c.cache[t.ID]
	c.mu.Unlock()
	if ok && ta.Version >= t.AuthVersion {
		return ta, nil
	}
	ta, err := c.Store.GetTenantAuth(ctx, t.ID)
	if err != nil {
		return ta, err
	}
	c.mu.Lock()
	if c.cache == nil || len(c.cache) >= 4096 {
		c.cache = map[string]store.TenantAuth{}
	}
	if old, ok := c.cache[t.ID]; !ok || old.Version < ta.Version {
		c.cache[t.ID] = ta
	}
	c.mu.Unlock()
	return ta, nil
}

// ForTenant narrows a tenant's issuers, audiences and grants to what a token for it may carry: the
// canonical audience <public_url>/<tenant>, and the assigned audiences outside the public URL (one
// under it would be another tenant's canonical audience, or the server's: never honoured, whatever
// the store holds). The DuckDB routes and the API verify through it.
func ForTenant(ta store.TenantAuth, publicURL, tenant string) (store.TenantAuth, string) {
	// an issuer record at GitHub Actions' URL is never used (spec 0008, 0023): any workflow can
	// request a token from it for any audience
	var kept []store.Issuer
	for _, is := range ta.Issuers {
		if !RefusedIssuer(is.URL) {
			kept = append(kept, is)
		}
	}
	ta.Issuers = kept
	p := strings.TrimSuffix(publicURL, "/")
	if p == "" {
		return ta, ""
	}
	var assigned []string
	for _, a := range ta.Audiences {
		if a != p && !strings.HasPrefix(a, p+"/") {
			assigned = append(assigned, a)
		}
	}
	ta.Audiences = assigned
	return ta, p + "/" + tenant
}

// FailureLog logs failed tokens at most once a minute per tenant: the reason class and the issuer
// record, never the token or its claims.
type FailureLog struct {
	Log *slog.Logger

	mu   sync.Mutex
	last map[string]time.Time // tenant id
}

// Record logs err if it is a token failure and the tenant's last line is a minute old.
func (l *FailureLog) Record(t store.Tenant, from string, err error) {
	var f *Failure
	if !errors.As(err, &f) {
		return
	}
	now := time.Now()
	l.mu.Lock()
	if now.Sub(l.last[t.ID]) < time.Minute {
		l.mu.Unlock()
		return
	}
	if l.last == nil || len(l.last) >= 4096 {
		l.last = map[string]time.Time{}
	}
	l.last[t.ID] = now
	l.mu.Unlock()
	log := l.Log
	if log == nil {
		log = slog.Default()
	}
	log.Warn(from+": a token was not valid", "tenant", t.Name, "issuer", f.Issuer, "reason", f.Reason)
}
