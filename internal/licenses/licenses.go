// Package licenses keeps a tenant's licences (spec 0012 phase 1a): an administrator's import, the
// relay from credentialed upstreams, removal and visibility, and what the DuckDB routes serve.
// kista issues nothing: every licence it stores verifies against a key that a live build of its
// extension declares, recorded from a trusted source (spec 0021).
package licenses

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Errors a caller maps to answers.
var (
	ErrOff      = errors.New("licenses: licensing is off (licenses.enabled: false)")
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
	// Enabled is licenses.enabled: off, the licence routes and the relay are off.
	Enabled bool
	// Refuse are the kids that count nowhere (licenses.refuse_keys, spec 0021).
	Refuse map[string]bool
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

// tenant authorizes verb on the tenant and reads it: license for the licences themselves (spec
// 0029: a licence manager's, and a tenant administrator's, whom admin implies it), admin for the
// rest (the upstreams' relay, the stored token).
func (s *Service) tenant(ctx context.Context, a authz.Actor, verb authz.Verb, name string) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, verb, authz.Resource{Tenant: name}); err != nil {
		return store.Tenant{}, err
	}
	return s.Store.GetTenant(ctx, name)
}

// View is a licence as the API shows it: its claims and its state, never the token.
type View struct {
	// PackageType is the licence's package type when it is not the default (spec 0034).
	PackageType string            `json:"package_type,omitempty"`
	Name        string            `json:"name"`
	ID          string            `json:"id"`
	Issuer      string            `json:"issuer"`
	HolderID    string            `json:"holder_id"`
	Holder      string            `json:"holder,omitempty"`
	Plan        string            `json:"plan"`
	Versions    *license.Versions `json:"versions,omitempty"`
	Limits      []license.Limit   `json:"limits,omitempty"`
	IssuedAt    string            `json:"issued_at"`
	ExpiresAt   string            `json:"expires_at"`
	GraceUntil  string            `json:"grace_until"`
	Key         string            `json:"key"`
	Source      string            `json:"source"`
	Upstream    string            `json:"upstream,omitempty"`
	Visibility  string            `json:"visibility"`
	Status      license.Status    `json:"status"`
	Version     int64             `json:"-"`
	Claims      *license.Claims   `json:"-"`
	Created     bool              `json:"-"` // an import stored it where none was
}

func seconds(n int64) string { return time.Unix(n, 0).UTC().Format(time.RFC3339) }

// view reads a stored licence's claims back from its token (verified when it was stored).
func (s *Service) view(ctx context.Context, l store.License) View {
	v := View{Name: l.Name, ID: l.LicenseID, Issuer: l.Issuer, HolderID: l.HolderID, Holder: l.Holder, Plan: l.Plan,
		IssuedAt: seconds(l.IssuedAt), ExpiresAt: seconds(l.ExpiresAt), GraceUntil: seconds(l.GraceUntil), Key: l.Key,
		Source: l.Source, Visibility: l.Visibility, Version: l.Version}
	if l.PackageType != pkgtype.Default {
		v.PackageType = l.PackageType
	}
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

// parseType reads the package type a caller names (spec 0034): "" is the default, an unknown type
// invalid.
func parseType(t string) (string, error) {
	t, err := pkgtype.Parse(t)
	if err != nil {
		return "", fmt.Errorf("%w: %v", store.ErrInvalid, err)
	}
	return t, nil
}

// List lists a tenant's licences of a package type ("": the default).
func (s *Service) List(ctx context.Context, a authz.Actor, tenant, packageType string) ([]View, error) {
	t, err := s.tenant(ctx, a, authz.VerbLicense, tenant)
	if err != nil {
		return nil, err
	}
	if packageType, err = parseType(packageType); err != nil {
		return nil, err
	}
	ls, err := s.Store.ListLicenses(ctx, t.ID, packageType)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(ls))
	for _, l := range ls {
		out = append(out, s.view(ctx, l))
	}
	return out, nil
}

// Get reads a tenant's licence for a package (type, "": the default; name).
func (s *Service) Get(ctx context.Context, a authz.Actor, tenant, packageType, name string) (View, error) {
	t, err := s.tenant(ctx, a, authz.VerbLicense, tenant)
	if err != nil {
		return View{}, err
	}
	if packageType, err = parseType(packageType); err != nil {
		return View{}, err
	}
	l, err := s.Store.GetLicense(ctx, t.ID, packageType, name)
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, l), nil
}

// Token is the stored licence's token, as received (the CLI's license token).
func (s *Service) Token(ctx context.Context, a authz.Actor, tenant, packageType, name string) (string, error) {
	t, err := s.tenant(ctx, a, authz.VerbAdmin, tenant)
	if err != nil {
		return "", err
	}
	if packageType, err = parseType(packageType); err != nil {
		return "", err
	}
	l, err := s.Store.GetLicense(ctx, t.ID, packageType, name)
	return l.Token, err
}

// parse reads a token for storing: a licence in force now (valid or in grace), its signature not
// verified yet (verify, with the name's keys).
func (s *Service) parse(token string) (*license.Token, error) {
	if !s.Enabled {
		return nil, ErrOff
	}
	t, err := license.Parse(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	switch st := t.StatusAt(s.Store.Now()); st {
	case license.Valid, license.Grace:
		return t, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrNotLive, st)
	}
}

// verify checks a token's signature against keys: those of the live builds of its name, all of
// them or one source's (spec 0021). A key the server refuses counts nowhere.
func (s *Service) verify(t *license.Token, keys []store.LicenseKey, whose string) error {
	if s.Refuse[t.Key] {
		return fmt.Errorf("%w: %w", ErrRefused, &license.Error{Rule: license.RuleKeyUntrusted,
			Msg: "key " + t.Key + " is refused by the server (licenses.refuse_keys)"})
	}
	pems := make([]string, 0, len(keys))
	for _, k := range keys {
		if !s.Refuse[k.Kid] {
			pems = append(pems, k.PEM)
		}
	}
	if len(pems) == 0 {
		return fmt.Errorf("%w: %w", ErrRefused, &license.Error{Rule: license.RuleKeyUntrusted,
			Msg: "no live build of " + t.Extension + whose + " declares a licence key"})
	}
	ks, err := license.ParseKeys(pems)
	if err != nil { // recorded keys were checked when read from the binary
		return err
	}
	if err := t.Verify(ks); err != nil {
		var le *license.Error
		if errors.As(err, &le) && le.Rule == license.RuleKeyUntrusted {
			err = &license.Error{Rule: license.RuleKeyUntrusted, Msg: "key " + t.Key + " is declared by no live build of " + t.Extension + whose}
		}
		return fmt.Errorf("%w: %w", ErrRefused, err)
	}
	return nil
}

// holderRefused turns the store's refusal of a licence of another holder than its tenant expects
// (spec 0028) into a licence refusal by the rule holder_mismatch; any other error is unchanged.
func holderRefused(err error) error {
	var hm *store.HolderMismatch
	if !errors.As(err, &hm) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrRefused, &license.Error{Rule: RuleHolder, Msg: hm.Error()})
}

func record(t *license.Token, tenantID, packageType, token, actor string) store.License {
	return store.License{TenantID: tenantID, PackageType: packageType, Name: t.Extension, LicenseID: t.ID, Issuer: t.Issuer, HolderID: t.HolderID,
		Holder: t.Holder, Plan: t.Plan, Token: token, Key: t.Key, IssuedAt: t.IssuedAt, ExpiresAt: t.ExpiresAt,
		GraceUntil: t.GraceUntil(), Visibility: store.Private, Actor: actor}
}

// Import stores a licence an administrator posts, under the name it names, of the package type the
// import names ("": the default; spec 0034). expected is the stored licence's version to replace
// (If-Match), 0 for whichever is stored (If-Match: *), or Create for none: a stored licence is
// never replaced without an If-Match.
func (s *Service) Import(ctx context.Context, a authz.Actor, tenant, packageType, token string, expected int64) (View, error) {
	t, err := s.tenant(ctx, a, authz.VerbLicense, tenant)
	if err != nil {
		return View{}, err
	}
	if packageType, err = parseType(packageType); err != nil {
		return View{}, err
	}
	tok, err := s.parse(token)
	if err != nil {
		return View{}, err
	}
	l := record(tok, t.ID, packageType, token, a.String())
	l.Source = store.LicenseImport
	err = s.Store.InTenantTx(ctx, t.ID, []string{store.LicenseLock(t.ID)}, func(tx *store.Tx) error {
		// the keys of the package's live builds, from any trusted source, anywhere in the tenant
		keys, err := tx.LiveLicenseKeys(ctx, t.ID, l.PackageType, l.Name, "", "")
		if err != nil {
			return err
		}
		if err := s.verify(tok, keys, ""); err != nil {
			return err
		}
		// spec 0028: the tenant's expected holder, as it is in this transaction. PutLicense checks it
		// again, for every path; checking here first refuses another holder's licence before the
		// stored one is compared (409 without If-Match), so the refusal names the rule
		if err := tx.CheckLicenseHolder(ctx, t.ID, l.HolderID); err != nil {
			return holderRefused(err)
		}
		old, err := tx.GetLicense(ctx, t.ID, l.PackageType, l.Name)
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
			return holderRefused(err)
		}
		return tx.Event(ctx, t.ID, a.String(), "license.import", "license:"+l.Name, typed(map[string]any{"name": l.Name,
			"license": l.LicenseID, "issuer": l.Issuer, "holder": l.HolderID, "plan": l.Plan, "expires_at": seconds(l.ExpiresAt),
			"replaced": replaced}, l.PackageType))
	})
	if err != nil {
		return View{}, err
	}
	v := s.view(ctx, l)
	v.Created = l.Version == 1
	return v, nil
}

// typed adds an event's package_type when it is not the default (spec 0034).
func typed(f map[string]any, packageType string) map[string]any {
	if packageType != pkgtype.Default {
		f["package_type"] = packageType
	}
	return f
}

// Remove removes a tenant's licence of a package (type, "": the default; name) (expected: its
// version; 0 any).
func (s *Service) Remove(ctx context.Context, a authz.Actor, tenant, packageType, name string, expected int64) error {
	t, err := s.tenant(ctx, a, authz.VerbLicense, tenant)
	if err != nil {
		return err
	}
	if packageType, err = parseType(packageType); err != nil {
		return err
	}
	return s.Store.InTenantTx(ctx, t.ID, []string{store.LicenseLock(t.ID)}, func(tx *store.Tx) error {
		l, err := tx.GetLicense(ctx, t.ID, packageType, name)
		if err != nil {
			return err
		}
		if err := tx.DeleteLicense(ctx, l, expected); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "license.remove", "license:"+name,
			typed(map[string]any{"name": name, "license": l.LicenseID, "reason": "admin"}, packageType))
	})
}

// SetVisibility makes a licence private or public (expected: its version; 0 any). Making one public
// is an administrator's (spec 0029): a public licence's token is served to anyone served a public
// release of the name, so a licence manager only makes one private.
func (s *Service) SetVisibility(ctx context.Context, a authz.Actor, tenant, packageType, name, visibility string, expected int64) (View, error) {
	verb := authz.VerbLicense
	if visibility != store.Private {
		verb = authz.VerbAdmin
	}
	t, err := s.tenant(ctx, a, verb, tenant)
	if err != nil {
		return View{}, err
	}
	if packageType, err = parseType(packageType); err != nil {
		return View{}, err
	}
	var l store.License
	err = s.Store.InTenantTx(ctx, t.ID, []string{store.LicenseLock(t.ID)}, func(tx *store.Tx) error {
		if l, err = tx.GetLicense(ctx, t.ID, packageType, name); err != nil {
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
			typed(map[string]any{"name": name, "license": l.LicenseID, "visibility": visibility}, packageType))
	})
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, l), nil
}

// Served adapts the service to the DuckDB routes (serve.Licenses).
type Served struct{ *Service }

// Enabled reports whether licensing is on (licenses.enabled).
func (s Served) Enabled() bool { return s.Service.Enabled }

// List is every licence of a tenant of a package type (a channel's: spec 0034, a licence is served
// only by a channel of its type) whose grace lasts and whose key the server does not refuse.
func (s Served) List(ctx context.Context, tenantID, packageType string) ([]store.License, error) {
	if !s.Service.Enabled {
		return nil, nil
	}
	ls, err := s.Store.ListLicenses(ctx, tenantID, packageType)
	if err != nil {
		return nil, err
	}
	now := s.Store.Now().Unix()
	out := ls[:0]
	for _, l := range ls {
		if now < l.GraceUntil && !s.Refuse[l.Key] {
			out = append(out, l)
		}
	}
	return out, nil
}

// Serve is what a DuckDB route serves for a package (the channel's type, a name) in a tenant: the
// stored licence while its grace lasts and its key is not refused (ok false: none, past its grace,
// or refused). The route serves it only in a channel where a live build declares its key.
func (s *Service) Serve(ctx context.Context, tenantID, packageType, name string) (store.License, bool, error) {
	if !s.Enabled {
		return store.License{}, false, nil
	}
	l, err := s.Store.GetLicense(ctx, tenantID, packageType, name)
	if errors.Is(err, store.ErrNotFound) {
		return l, false, nil
	}
	if err != nil {
		return l, false, err
	}
	return l, s.Store.Now().Unix() < l.GraceUntil && !s.Refuse[l.Key], nil
}

// NameKeys are the licence keys of a name's live builds (spec 0021).
type NameKeys struct {
	Name string    `json:"name"`
	Keys []KeyView `json:"keys"`
}

// KeyView is a licence key, the trusted sources that brought it and the number of live builds that
// declare it.
type KeyView struct {
	Kid     string            `json:"kid"`
	Sources []store.KeySource `json:"sources"`
	Builds  int               `json:"builds"`
}

// LicenseKeys lists the keys a tenant's licences of a package type ("": the default) are verified
// with: those of its live builds of the type, by name (one name, or all: name ""), without the keys
// the server refuses.
func (s *Service) LicenseKeys(ctx context.Context, a authz.Actor, tenant, packageType, name string) ([]NameKeys, error) {
	t, err := s.tenant(ctx, a, authz.VerbLicense, tenant)
	if err != nil {
		return nil, err
	}
	if packageType, err = parseType(packageType); err != nil {
		return nil, err
	}
	ks, err := s.Store.TenantLicenseKeys(ctx, t.ID, packageType, name)
	if err != nil {
		return nil, err
	}
	out := []NameKeys{}
	for _, k := range ks {
		if s.Refuse[k.Kid] {
			continue
		}
		if len(out) == 0 || out[len(out)-1].Name != k.Name {
			out = append(out, NameKeys{Name: k.Name})
		}
		last := &out[len(out)-1]
		last.Keys = append(last.Keys, KeyView{Kid: k.Kid, Sources: k.Sources, Builds: k.Builds})
	}
	return out, nil
}
