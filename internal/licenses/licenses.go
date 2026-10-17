// Package licenses keeps a tenant's licences (spec 0012 phase 1a): an administrator's import, the
// relay from credentialed upstreams, removal and visibility, and what the DuckDB routes serve.
// kista issues nothing: every licence it stores verifies against the operator's trusted keys.
package licenses

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Errors a caller maps to answers.
var (
	ErrOff      = errors.New("licenses: no trusted keys configured")
	ErrRefused  = errors.New("licenses: the licence is refused")      // with its rule
	ErrNotLive  = errors.New("licenses: the licence is not in force") // expired, or not yet valid
	ErrExists   = errors.New("licenses: a licence is stored for this extension; replace it with If-Match")
	ErrUpstream = errors.New("licenses: not an upstream that relays licences (a repository upstream with a credential, not in bundle mode)")
	ErrPaused   = errors.New("licenses: the upstream is paused: resume it to relay its licences")
	ErrChanged  = errors.New("licenses: the upstream changed during the relay (paused, removed, or another prefix or credential); the next run reads it again")
)

// Create is Import's expected version for a licence that must not exist yet.
const Create = -1

// Service manages licences.
type Service struct {
	Store *store.Store
	Authz authz.Authorizer
	// Keys are the trusted licence keys (config licenses.keys); none: licensing is off.
	Keys license.Keys
	// Fetch and Credentials are the relay's (spec 0009's egress client and credentials).
	Fetch       Fetcher
	Credentials credential.Registry
	// Interval is the time between an upstream's relay runs (licenses.refresh).
	Interval time.Duration
	Log      *slog.Logger
}

// WithAuthz returns a service with another authorizer, sharing the rest.
func (s *Service) WithAuthz(az authz.Authorizer) *Service {
	c := *s
	c.Authz = az
	return &c
}

func (s *Service) tenant(ctx context.Context, a authz.Actor, name string) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: name}); err != nil {
		return store.Tenant{}, err
	}
	return s.Store.GetTenant(ctx, name)
}

// View is a licence as the API shows it: its claims and its state, never the token.
type View struct {
	Name       string            `json:"name"`
	ID         string            `json:"id"`
	Issuer     string            `json:"issuer"`
	HolderID   string            `json:"holder_id"`
	Holder     string            `json:"holder,omitempty"`
	Plan       string            `json:"plan"`
	Versions   *license.Versions `json:"versions,omitempty"`
	Limits     []license.Limit   `json:"limits,omitempty"`
	IssuedAt   string            `json:"issued_at"`
	ExpiresAt  string            `json:"expires_at"`
	GraceUntil string            `json:"grace_until"`
	Key        string            `json:"key"`
	Source     string            `json:"source"`
	Upstream   string            `json:"upstream,omitempty"`
	Visibility string            `json:"visibility"`
	Status     license.Status    `json:"status"`
	Version    int64             `json:"-"`
	Claims     *license.Claims   `json:"-"`
	Created    bool              `json:"-"` // an import stored it where none was
}

func seconds(n int64) string { return time.Unix(n, 0).UTC().Format(time.RFC3339) }

// view reads a stored licence's claims back from its token (verified when it was stored).
func (s *Service) view(ctx context.Context, l store.License) View {
	v := View{Name: l.Name, ID: l.LicenseID, Issuer: l.Issuer, HolderID: l.HolderID, Holder: l.Holder, Plan: l.Plan,
		IssuedAt: seconds(l.IssuedAt), ExpiresAt: seconds(l.ExpiresAt), GraceUntil: seconds(l.GraceUntil), Key: l.Key,
		Source: l.Source, Visibility: l.Visibility, Version: l.Version}
	if t, err := license.Parse(l.Token); err == nil {
		v.Versions, v.Limits, v.Claims = t.Versions, t.Limits, &t.Claims
		v.Status = t.StatusAt(s.Store.Now())
	} else {
		v.Status = license.Invalid // a stored token that no longer parses: never served
	}
	if l.UpstreamID != "" {
		if u, err := s.upstreamName(ctx, l.UpstreamID); err == nil {
			v.Upstream = u
		}
	}
	return v
}

func (s *Service) upstreamName(ctx context.Context, id string) (string, error) {
	var name string
	err := s.Store.InTxLocks(ctx, nil, func(tx *store.Tx) error {
		u, err := tx.GetUpstreamByID(ctx, id)
		name = u.Name
		return err
	})
	return name, err
}

// KeyFingerprints are the trusted keys' fingerprints (for server administrators).
func (s *Service) KeyFingerprints() []string {
	out := slices.Collect(maps.Keys(s.Keys))
	slices.Sort(out)
	return out
}

// List lists a tenant's licences.
func (s *Service) List(ctx context.Context, a authz.Actor, tenant string) ([]View, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	ls, err := s.Store.ListLicenses(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(ls))
	for _, l := range ls {
		out = append(out, s.view(ctx, l))
	}
	return out, nil
}

// Get reads a tenant's licence for an extension.
func (s *Service) Get(ctx context.Context, a authz.Actor, tenant, name string) (View, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return View{}, err
	}
	l, err := s.Store.GetLicense(ctx, t.ID, name)
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, l), nil
}

// Token is the stored licence's token, as received (the CLI's license token).
func (s *Service) Token(ctx context.Context, a authz.Actor, tenant, name string) (string, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return "", err
	}
	l, err := s.Store.GetLicense(ctx, t.ID, name)
	return l.Token, err
}

// check verifies a token for storing: a licence by the trusted keys, in force now (valid or in
// grace). It returns the parsed token.
func (s *Service) check(token string) (*license.Token, error) {
	if len(s.Keys) == 0 {
		return nil, ErrOff
	}
	t, err := license.Parse(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	if err := t.Verify(s.Keys); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	switch st := t.StatusAt(s.Store.Now()); st {
	case license.Valid, license.Grace:
		return t, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrNotLive, st)
	}
}

func record(t *license.Token, tenantID, token, actor string) store.License {
	return store.License{TenantID: tenantID, Name: t.Extension, LicenseID: t.ID, Issuer: t.Issuer, HolderID: t.HolderID,
		Holder: t.Holder, Plan: t.Plan, Token: token, Key: t.Key, IssuedAt: t.IssuedAt, ExpiresAt: t.ExpiresAt,
		GraceUntil: t.GraceUntil(), Visibility: store.Private, Actor: actor}
}

// Import stores a licence an administrator posts, under the name it names. expected is the stored
// licence's version to replace (If-Match), 0 for whichever is stored (If-Match: *), or Create for
// none: a stored licence is never replaced without an If-Match.
func (s *Service) Import(ctx context.Context, a authz.Actor, tenant, token string, expected int64) (View, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return View{}, err
	}
	tok, err := s.check(token)
	if err != nil {
		return View{}, err
	}
	l := record(tok, t.ID, token, a.String())
	l.Source = store.LicenseImport
	err = s.Store.InTenantTx(ctx, t.ID, []string{store.LicenseLock(t.ID)}, func(tx *store.Tx) error {
		old, err := tx.GetLicense(ctx, t.ID, l.Name)
		replaced := ""
		switch {
		case errors.Is(err, store.ErrNotFound):
			if expected > 0 {
				return store.ErrConflict
			}
			old.Version = 0
		case err != nil:
			return err
		case expected == Create:
			return ErrExists
		case expected != 0 && expected != old.Version:
			return store.ErrConflict
		default:
			l.Visibility, replaced = old.Visibility, old.LicenseID
		}
		if err := tx.PutLicense(ctx, &l, old.Version); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "license.import", "license:"+l.Name, map[string]any{"name": l.Name,
			"license": l.LicenseID, "issuer": l.Issuer, "holder": l.HolderID, "plan": l.Plan, "expires_at": seconds(l.ExpiresAt),
			"replaced": replaced})
	})
	if err != nil {
		return View{}, err
	}
	v := s.view(ctx, l)
	v.Created = l.Version == 1
	return v, nil
}

// Remove removes a tenant's licence (expected: its version; 0 any).
func (s *Service) Remove(ctx context.Context, a authz.Actor, tenant, name string, expected int64) error {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return err
	}
	return s.Store.InTenantTx(ctx, t.ID, []string{store.LicenseLock(t.ID)}, func(tx *store.Tx) error {
		l, err := tx.GetLicense(ctx, t.ID, name)
		if err != nil {
			return err
		}
		if err := tx.DeleteLicense(ctx, l, expected); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "license.remove", "license:"+name,
			map[string]any{"name": name, "license": l.LicenseID, "reason": "admin"})
	})
}

// SetVisibility makes a licence private or public (expected: its version; 0 any).
func (s *Service) SetVisibility(ctx context.Context, a authz.Actor, tenant, name, visibility string, expected int64) (View, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return View{}, err
	}
	var l store.License
	err = s.Store.InTenantTx(ctx, t.ID, []string{store.LicenseLock(t.ID)}, func(tx *store.Tx) error {
		if l, err = tx.GetLicense(ctx, t.ID, name); err != nil {
			return err
		}
		if expected == 0 {
			expected = l.Version
		}
		if l.Visibility == visibility && expected == l.Version {
			return nil
		}
		if err := tx.SetLicenseVisibility(ctx, &l, visibility, a.String(), expected); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "license.change", "license:"+name,
			map[string]any{"name": name, "license": l.LicenseID, "visibility": visibility})
	})
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, l), nil
}

// Served adapts the service to the DuckDB routes (serve.Licenses).
type Served struct{ *Service }

// Enabled reports whether licensing is on (trusted keys are configured).
func (s Served) Enabled() bool { return len(s.Keys) > 0 }

// List is every licence of a tenant whose grace lasts.
func (s Served) List(ctx context.Context, tenantID string) ([]store.License, error) {
	if len(s.Keys) == 0 {
		return nil, nil
	}
	ls, err := s.Store.ListLicenses(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	now := s.Store.Now().Unix()
	out := ls[:0]
	for _, l := range ls {
		if now < l.GraceUntil {
			out = append(out, l)
		}
	}
	return out, nil
}

// Serve is what a DuckDB route serves for a name in a tenant: the stored licence while its grace
// lasts (ok false: none, or past its grace).
func (s *Service) Serve(ctx context.Context, tenantID, name string) (store.License, bool, error) {
	if len(s.Keys) == 0 {
		return store.License{}, false, nil
	}
	l, err := s.Store.GetLicense(ctx, tenantID, name)
	if errors.Is(err, store.ErrNotFound) {
		return l, false, nil
	}
	if err != nil {
		return l, false, err
	}
	return l, s.Store.Now().Unix() < l.GraceUntil, nil
}
