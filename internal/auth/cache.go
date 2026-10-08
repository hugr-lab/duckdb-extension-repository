package auth

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// TenantAuths caches each tenant's issuers, audiences and grants by its auth_version, which every
// change bumps (spec 0006): the DuckDB routes and the API read them through one cache.
type TenantAuths struct {
	Store *store.Store

	mu    sync.Mutex
	cache map[string]store.TenantAuth
}

// Get returns a tenant's issuers, audiences and grants for the auth_version the caller read.
func (c *TenantAuths) Get(ctx context.Context, t store.Tenant) (store.TenantAuth, error) {
	k := t.ID + "|" + strconv.FormatInt(t.AuthVersion, 10)
	c.mu.Lock()
	ta, ok := c.cache[k]
	c.mu.Unlock()
	if ok {
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
	c.cache[k] = ta
	c.mu.Unlock()
	return ta, nil
}

// InstallScope returns what principals may install in a channel: every extension (a grant on the
// tenant or the channel), or these names (grants on extensions in every channel or in this one).
func InstallScope(p Principals, grants []store.Grant, channelID string) (all bool, names []string) {
	for _, g := range grants {
		if !p[Key{g.IssuerID, g.Kind, g.Value}] {
			continue
		}
		if !slices.Contains(g.Verbs, store.VerbInstall) && (!slices.Contains(g.Verbs, store.VerbAdmin) || g.Kind == store.PrincipalIssuer) {
			continue
		}
		if g.ChannelID != "" && g.ChannelID != channelID {
			continue
		}
		if g.Extension == "" {
			return true, nil
		}
		if !slices.Contains(names, g.Extension) {
			names = append(names, g.Extension)
		}
	}
	return false, names
}

// ForTenant narrows a tenant's issuers, audiences and grants to what a token for it may carry: the
// canonical audience <public_url>/<tenant>, and the assigned audiences outside the public URL (one
// under it would be another tenant's canonical audience, or the server's: never honoured, whatever
// the store holds). The DuckDB routes and the API verify through it.
func ForTenant(ta store.TenantAuth, publicURL, tenant string, providers Providers) (store.TenantAuth, string) {
	// an issuer record at a trusted-publishing provider's URL is never used (spec 0008): the
	// provider's tokens are publishers' credentials, never a tenant's principals
	if len(providers) > 0 {
		var kept []store.Issuer
		for _, is := range ta.Issuers {
			if !providers.Has(is.URL) {
				kept = append(kept, is)
			}
		}
		ta.Issuers = kept
	}
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
