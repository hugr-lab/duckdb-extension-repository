// Package licenses keeps a tenant's licences (spec 0012 phase 1a): an administrator's import, the
// relay from credentialed upstreams, removal and visibility, and what the DuckDB routes serve.
// kista issues nothing: every licence it stores verifies against a key that a live build of its
// extension declares, recorded from a trusted source (spec 0021). A licence belongs to a catalogue
// entry (spec 0035): a source's builds of the name in the tenant, named by every write; it verifies
// against that entry's own keys, and a channel serves the licence of the entry it serves the name
// from.
package licenses

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
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
	// ErrSources is a licence read without its entry where the name has licences in several
	// entries (spec 0033 phase B; spec 0035).
	ErrSources = errors.New("licenses: the extension has licences in several entries")
	// ErrDistributorsEntry is a write on a distributor's publisher's entry by another writer than
	// its licence manager or a server administrator (spec 0037); the error is a *DistributorsEntry.
	ErrDistributorsEntry = errors.New("licenses: a distributor's entry: its licence manager or a server administrator writes it")
)

// DistributorsEntry is a write refused on a distributor's publisher's entry (spec 0037): what it
// would have written, for the refusal's event.
type DistributorsEntry struct {
	Name string // the licence's name ("" when the import's token names none)
	// NameUnverified: Name was read from an import's token before it was verified
	NameUnverified bool
	Source         string // the entry, as the caller named it
	Entry          string // the entry, resolved: upstream-id:<id> or publisher-id:<id>
	Op             string // import, remove, public or private
}

func (e *DistributorsEntry) Error() string { return ErrDistributorsEntry.Error() }

// Is makes a DistributorsEntry an ErrDistributorsEntry.
func (e *DistributorsEntry) Is(target error) bool { return target == ErrDistributorsEntry }

// RuleOlder refuses a licence older (by iat) than one the tenant holds of the same package, from
// the same issuer for the same holder (spec 0037).
const RuleOlder = "older"

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
	PackageType string `json:"package_type,omitempty"`
	Name        string `json:"name"`
	// Entry is the licence's catalogue entry (spec 0035), by its source: upstream:<name>,
	// publisher:<name>, or a removed one's upstream-id:<id> or publisher-id:<id>.
	Entry      string            `json:"entry"`
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
	Source     string            `json:"source"` // how it arrived: import or upstream
	Upstream   string            `json:"upstream,omitempty"`
	Visibility string            `json:"visibility"`
	Status     license.Status    `json:"status"`
	// Distributor is its entry's distributor when the entry is a distributor's publisher's (spec
	// 0037: its licence manager or a server administrator writes it), omitted otherwise
	Distributor string          `json:"distributor,omitempty"`
	Version     int64           `json:"-"`
	Claims      *license.Claims `json:"-"`
	Created     bool            `json:"-"` // an import stored it where none was
}

func seconds(n int64) string { return time.Unix(n, 0).UTC().Format(time.RFC3339) }

// labels names entries by their sources as the licence routes do (spec 0033 phase B; spec 0035): a
// live upstream's or publisher's as spec 0031 does (upstream:<name>, publisher:<name>), a removed
// one's by its id (upstream-id:<id>, publisher-id:<id>); and knows the distributors' publishers'
// entries, live or removed, with their distributors (spec 0037).
type labels struct {
	names map[string]string
	dists map[string]string // source -> its distributor
}

func (s *Service) labels(ctx context.Context, tenantID string) (labels, error) {
	m, err := s.Store.SourceLabels(ctx, tenantID)
	if err != nil {
		return labels{}, err
	}
	ds, err := s.Store.DistributorSources(ctx, tenantID)
	if err != nil {
		return labels{}, err
	}
	lb := labels{names: m, dists: map[string]string{}}
	for src, d := range ds {
		lb.dists[src] = d.Distributor
	}
	return lb, nil
}

func (lb labels) of(src string) string {
	if l, ok := lb.names[src]; ok {
		return l
	}
	return idLabel(src)
}

// idLabel is a source by its id: upstream-id:<id>, publisher-id:<id>.
func idLabel(src string) string {
	if kind, id, ok := strings.Cut(src, ":"); ok && (kind == store.KeySourceUpstream || kind == store.KeySourcePublisher) {
		return kind + "-id:" + id
	}
	return src
}

// named reports whether a caller's name for a source (its label, or its id form) is src.
func (lb labels) named(src, given string) bool { return given == lb.of(src) || given == idLabel(src) }

// view reads a stored licence's claims back from its token (verified when it was stored).
func (s *Service) view(ctx context.Context, lb labels, l store.License) View {
	v := View{Name: l.Name, Entry: lb.of(l.Entry), ID: l.LicenseID, Issuer: l.Issuer, HolderID: l.HolderID, Holder: l.Holder, Plan: l.Plan,
		IssuedAt: seconds(l.IssuedAt), ExpiresAt: seconds(l.ExpiresAt), GraceUntil: seconds(l.GraceUntil), Key: l.Key,
		Source: l.Source, Visibility: l.Visibility, Version: l.Version, Distributor: lb.dists[l.Entry]}
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

// List lists a tenant's licences of a package type ("": the default), of one entry (its source's
// label or id form; "": every entry).
func (s *Service) List(ctx context.Context, a authz.Actor, tenant, packageType, source string) ([]View, error) {
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
	lb, err := s.labels(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(ls))
	for _, l := range ls {
		if source == "" || lb.named(l.Entry, source) {
			out = append(out, s.view(ctx, lb, l))
		}
	}
	return out, nil
}

// pick is the licence of a name a caller reads: of the entry it names, or without one the only
// licence of the name (ErrSources when there are several).
func pick(ls []store.License, lb labels, name, source string) (store.License, error) {
	var found []store.License
	for _, l := range ls {
		if source == "" || lb.named(l.Entry, source) {
			found = append(found, l)
		}
	}
	switch {
	case len(found) == 0 && source != "":
		return store.License{}, fmt.Errorf("%w: no licence of %s from %s", store.ErrNotFound, name, source)
	case len(found) == 0:
		return store.License{}, fmt.Errorf("%w: licence %s", store.ErrNotFound, name)
	case len(found) > 1:
		srcs := make([]string, 0, len(found))
		for _, l := range found {
			srcs = append(srcs, lb.of(l.Entry))
		}
		slices.Sort(srcs)
		return store.License{}, fmt.Errorf("%w: %s", ErrSources, strings.Join(srcs, ", "))
	}
	return found[0], nil
}

// licenseOf reads a licence a caller names, in a transaction.
func (s *Service) licenseOf(ctx context.Context, tx *store.Tx, lb labels, tenantID, packageType, name, source string) (store.License, error) {
	ls, err := tx.NameLicenses(ctx, tenantID, packageType, name)
	if err != nil {
		return store.License{}, err
	}
	return pick(ls, lb, name, source)
}

// Get reads a tenant's licence for a package (type, "": the default; name) in an entry (its source's
// label or id form; "": the name's only licence).
func (s *Service) Get(ctx context.Context, a authz.Actor, tenant, packageType, name, source string) (View, error) {
	t, err := s.tenant(ctx, a, authz.VerbLicense, tenant)
	if err != nil {
		return View{}, err
	}
	if packageType, err = parseType(packageType); err != nil {
		return View{}, err
	}
	lb, err := s.labels(ctx, t.ID)
	if err != nil {
		return View{}, err
	}
	var l store.License
	if err := s.Store.InTxLocks(ctx, nil, func(tx *store.Tx) error {
		l, err = s.licenseOf(ctx, tx, lb, t.ID, packageType, name, source)
		return err
	}); err != nil {
		return View{}, err
	}
	return s.view(ctx, lb, l), nil
}

// Token is the stored licence's token, as received (the CLI's license token).
func (s *Service) Token(ctx context.Context, a authz.Actor, tenant, packageType, name, source string) (string, error) {
	t, err := s.tenant(ctx, a, authz.VerbAdmin, tenant)
	if err != nil {
		return "", err
	}
	if packageType, err = parseType(packageType); err != nil {
		return "", err
	}
	lb, err := s.labels(ctx, t.ID)
	if err != nil {
		return "", err
	}
	var l store.License
	err = s.Store.InTxLocks(ctx, nil, func(tx *store.Tx) error {
		l, err = s.licenseOf(ctx, tx, lb, t.ID, packageType, name, source)
		return err
	})
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

// refused is the refusal of a key the server refuses (licenses.refuse_keys); nil for another key.
func (s *Service) refused(t *license.Token) error {
	if !s.Refuse[t.Key] {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrRefused, &license.Error{Rule: license.RuleKeyUntrusted,
		Msg: "key " + t.Key + " is refused by the server (licenses.refuse_keys)"})
}

// verify checks a token's signature against keys: the live keys of its name in its entry (spec
// 0021, 0035). A key the server refuses counts nowhere.
func (s *Service) verify(t *license.Token, keys []store.LicenseKey, whose string) error {
	if err := s.refused(t); err != nil {
		return err
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

// verifyIn verifies a token against the live keys of its name in an entry (spec 0035: only that
// entry's own builds count). label names the entry in a refusal.
func (s *Service) verifyIn(ctx context.Context, tx *store.Tx, tenantID, packageType, entry string, tok *license.Token, label string) error {
	if err := s.refused(tok); err != nil {
		return err
	}
	keys, err := tx.LiveLicenseKeys(ctx, tenantID, packageType, tok.Extension, entry)
	if err != nil {
		return err
	}
	return s.verify(tok, keys, " from "+label)
}

// entry reads the catalogue entry a caller names by its source (spec 0035): for an administrator, a
// live upstream or publisher of the tenant whose builds can declare licence keys (upstream:<name>,
// publisher:<name>, upstream-id:<id>, publisher-id:<id>). With removed set (a removal), the id
// form of a removed source names its entry too, live false. Anything else is invalid, naming why.
// A licence manager names only distributors' publishers' entries (distributorEntry).
func (s *Service) entry(ctx context.Context, a authz.Actor, tenantID, label string, removed bool) (src string, live bool, err error) {
	invalid := func(why string) (string, bool, error) {
		return "", false, fmt.Errorf("%w: ?source= %s", store.ErrInvalid, why)
	}
	idForm := false
	switch label {
	case "":
		return invalid("names the licence's entry: upstream:<name>, publisher:<name>, upstream-id:<id> or publisher-id:<id>")
	case store.SourceTenant:
		if a.Kind == authz.ActorLicenseManager {
			return "", false, authz.ErrDenied
		}
		return invalid("tenant: the tenant's own builds declare no licence keys, so its entries carry no licence")
	}
	if a.Kind == authz.ActorLicenseManager {
		return s.distributorEntry(ctx, tenantID, label, removed)
	}
	for _, kind := range []string{store.KeySourceUpstream, store.KeySourcePublisher} {
		if id, ok := strings.CutPrefix(label, kind+"-id:"); ok {
			if src, idForm = kind+":"+id, true; !store.ValidLicenseEntry(src) {
				return invalid(fmt.Sprintf("%q is malformed", label))
			}
		}
	}
	if !idForm {
		if !strings.HasPrefix(label, store.SourceUpstreamPrefix) && !strings.HasPrefix(label, store.SourcePublisherPrefix) {
			return invalid(fmt.Sprintf("%q is malformed: upstream:<name>, publisher:<name>, upstream-id:<id> or publisher-id:<id>", label))
		}
		if src, err = s.Store.ResolveSource(ctx, tenantID, label); errors.Is(err, store.ErrNoSource) {
			return invalid(fmt.Sprintf("%q is no upstream or publisher of the tenant", label))
		} else if err != nil {
			return "", false, err
		}
	}
	live, kind, err := s.Store.LicenseEntrySource(ctx, tenantID, src)
	switch {
	case err != nil:
		return "", false, err
	case !live && removed && idForm:
		return src, false, nil
	case !live && idForm:
		return invalid(fmt.Sprintf("%q is removed, or no upstream or publisher of the tenant", label))
	case !live:
		return invalid(fmt.Sprintf("%q is no upstream or publisher of the tenant", label))
	case kind != "" && kind != store.UpstreamRepo:
		return invalid(fmt.Sprintf("%q is a DuckDB %s upstream: it declares no licence keys, so its entries carry no licence", label,
			strings.TrimPrefix(kind, "duckdb-")))
	}
	return src, true, nil
}

// distributorEntry reads the entry a licence manager names (spec 0035): one of the tenant's
// distributors' publishers, as distribution admins name them: a live one by publisher:<name> or
// publisher-id:<id>, a removed one by publisher-id:<id> on a removal only. Any other source,
// whatever it is, answers as a refused caller (authz.ErrDenied: a missing path, audited), before
// anything about it is checked: its server-wide token reads every licence, but writes only into
// distributors' publishers' entries.
func (s *Service) distributorEntry(ctx context.Context, tenantID, label string, removed bool) (string, bool, error) {
	srcs, err := s.Store.DistributorSources(ctx, tenantID)
	if err != nil {
		return "", false, err
	}
	if id, ok := strings.CutPrefix(label, store.SourcePublisherIDPrefix); ok {
		if d, ok := srcs[store.PublisherSource(id)]; ok && id != "" && (!d.Removed || removed) {
			return d.Source, !d.Removed, nil
		}
		return "", false, authz.ErrDenied
	}
	if name, ok := strings.CutPrefix(label, store.SourcePublisherPrefix); ok {
		for _, d := range srcs {
			if !d.Removed && d.Name == name {
				return d.Source, true, nil
			}
		}
	}
	return "", false, authz.ErrDenied
}

// write is a licence write as its refusal names it (spec 0037): the licence's name (unverified:
// read from an import's token before it was verified), the entry as the caller named it, and the
// operation (import, remove, public, private).
type write struct {
	name, label, op string
	unverified      bool
}

// recheck reads a write's entry again in its transaction, under the licence lock (spec 0035): a
// licence manager's entry must still be a distributor's publisher's in the tenant (a refused caller
// otherwise), anyone else's must not be one unless a server administrator writes (spec 0037:
// refuseDistributorsEntry), and a source removed since the entry was read takes no import and no
// visibility change (live).
func (s *Service) recheck(ctx context.Context, tx *store.Tx, a authz.Actor, tenantID, entry string, live bool, w write) error {
	if a.Kind == authz.ActorLicenseManager {
		d, err := tx.SourceDistributor(ctx, tenantID, entry)
		if err != nil {
			return err
		}
		if d == "" {
			return authz.ErrDenied
		}
	} else if err := s.refuseDistributorsEntry(ctx, tx, a, tenantID, entry, w); err != nil {
		return err
	}
	if !live {
		return nil
	}
	ok, _, err := tx.LicenseEntrySource(ctx, tenantID, entry)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: ?source= %q was removed", store.ErrInvalid, w.label)
	}
	return nil
}

// refuseDistributorsEntry refuses a write on a distributor's publisher's entry, live or removed
// (spec 0037), unless a server administrator makes it (a licence manager's entries are those only:
// distributorEntry, recheck): the licence is the distributor's contract with the holder, which a
// tenant administrator only reads. The refusal names what the write was. It reads in tx when one is
// given, outside a transaction otherwise.
func (s *Service) refuseDistributorsEntry(ctx context.Context, tx *store.Tx, a authz.Actor, tenantID, src string, w write) error {
	if a.Kind == authz.ActorLicenseManager {
		return nil
	}
	var d string
	var err error
	if tx != nil {
		d, err = tx.SourceDistributor(ctx, tenantID, src)
	} else {
		d, err = s.Store.SourceDistributor(ctx, tenantID, src)
	}
	if err != nil || d == "" {
		return err
	}
	if ok, err := s.serverAdmin(ctx, a); ok || err != nil {
		return err
	}
	return &DistributorsEntry{Name: w.name, NameUnverified: w.unverified, Source: w.label, Entry: idLabel(src), Op: w.op}
}

// serverAdmin reports whether an actor is a server administrator (a server token, the CLI): admin
// on the server.
func (s *Service) serverAdmin(ctx context.Context, a authz.Actor) (bool, error) {
	err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server)
	if errors.Is(err, authz.ErrDenied) {
		return false, nil
	}
	return err == nil, err
}

// older refuses (rule older) a licence l when the tenant holds a later one, by iat, of the same
// package from the same issuer for the same holder, in any entry, that still counts (spec 0037): in
// a distributor's publisher's entry (live or removed) whatever its key, since only licence managers
// and server administrators write there (so an issuer's rotated key still supersedes its older
// licences); in any other entry only when verified by the same key. A stored licence's key is the
// fingerprint of the key material that verified it (license.ParseKeys computes it), so a licence a
// tenant administrator signs with another key, whatever its claims say (their own key on an
// upstream's build, a forged kid), never blocks the issuer's. It counts until its grace ends, or while the server refuses its key
// (licenses.refuse_keys: a licence signed with a key later compromised blocks nothing); a key no
// longer live in its entry still counts, since a tenant administrator can make it so (removing the
// distributor's publisher, blocking its bodies). An equal iat is not older. The caller holds the
// tenant's licence lock, which every licence write takes: nothing is stored between this check and
// its write.
func (s *Service) older(ctx context.Context, tx *store.Tx, lb labels, l store.License) error {
	ls, err := tx.LaterLicenses(ctx, l.TenantID, l.PackageType, l.Name, l.Issuer, l.HolderID, l.IssuedAt)
	if err != nil {
		return err
	}
	now := s.Store.Now().Unix()
	for _, o := range ls {
		if o.GraceUntil <= now || s.Refuse[o.Key] {
			continue
		}
		if o.Key != l.Key {
			d, err := tx.SourceDistributor(ctx, l.TenantID, o.Entry)
			if err != nil {
				return err
			}
			if d == "" {
				continue // another key outside a distributor's entry: anyone's
			}
		}
		return fmt.Errorf("%w: %w", ErrRefused, &license.Error{Rule: RuleOlder, Msg: fmt.Sprintf(
			"a later licence of %s from this issuer for this holder is stored in %s (issued %s, this one %s): to go back, remove it first",
			l.Name, lb.of(o.Entry), seconds(o.IssuedAt), seconds(l.IssuedAt))})
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

// Import stores a licence an administrator or a licence manager posts, under the name it names, of
// the package type the import names ("": the default; spec 0034), in the catalogue entry the
// import names by its source (spec 0035; a licence manager: a distributor's publisher's only). It
// verifies against that entry's live keys. expected is the stored licence's version to replace
// (If-Match), 0 for whichever is stored (If-Match: *), or Create for none: a stored licence is never
// replaced without an If-Match.
func (s *Service) Import(ctx context.Context, a authz.Actor, tenant, packageType, source, token string, expected int64) (View, error) {
	t, err := s.tenant(ctx, a, authz.VerbLicense, tenant)
	if err != nil {
		return View{}, err
	}
	if packageType, err = parseType(packageType); err != nil {
		return View{}, err
	}
	if !s.Enabled {
		return View{}, ErrOff
	}
	src, _, err := s.entry(ctx, a, t.ID, source, false)
	if err != nil {
		return View{}, err
	}
	name := "" // the refusal's, read from the token unverified
	if p, err := license.Parse(token); err == nil {
		name = p.Extension
	}
	if err := s.refuseDistributorsEntry(ctx, nil, a, t.ID, src, write{name: name, label: source, op: "import", unverified: true}); err != nil {
		return View{}, err
	}
	tok, err := s.parse(token)
	if err != nil {
		return View{}, err
	}
	lb, err := s.labels(ctx, t.ID)
	if err != nil {
		return View{}, err
	}
	l := record(tok, t.ID, packageType, token, a.String())
	l.Entry, l.Source = src, store.LicenseImport
	err = s.Store.InTenantTx(ctx, t.ID, []string{store.LicenseLock(t.ID)}, func(tx *store.Tx) error {
		// the token's name, not verified yet (verifyIn comes next)
		if err := s.recheck(ctx, tx, a, t.ID, src, true, write{name: l.Name, label: source, op: "import", unverified: true}); err != nil {
			return err
		}
		// the entry's live keys: those its source recorded on its own live builds anywhere in the
		// tenant, or on its newest available version in the catalogue
		if err := s.verifyIn(ctx, tx, t.ID, l.PackageType, src, tok, lb.of(src)); err != nil {
			return err
		}
		// spec 0028: the tenant's expected holder, as it is in this transaction. PutLicense checks it
		// again, for every path; checking here first refuses another holder's licence before the
		// stored one is compared (409 without If-Match), so the refusal names the rule
		if err := tx.CheckLicenseHolder(ctx, t.ID, l.HolderID); err != nil {
			return holderRefused(err)
		}
		// spec 0037: never older than a licence the tenant holds, a replacement's too; before the
		// stored one is compared, so the refusal names the rule (not 409 without If-Match)
		if err := s.older(ctx, tx, lb, l); err != nil {
			return err
		}
		old, err := tx.GetLicense(ctx, t.ID, l.PackageType, l.Name, src)
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
		tx.SetWarningAct("license.import") // spec 0035 phase 2
		if err := tx.PutLicense(ctx, &l, old.Version); err != nil {
			return holderRefused(err)
		}
		return tx.Event(ctx, t.ID, a.String(), "license.import", "license:"+l.Name, typed(map[string]any{"name": l.Name,
			"license": l.LicenseID, "issuer": l.Issuer, "holder": l.HolderID, "plan": l.Plan, "expires_at": seconds(l.ExpiresAt),
			"replaced": replaced, "source": lb.of(src)}, l.PackageType))
	})
	if err != nil {
		return View{}, err
	}
	v := s.view(ctx, lb, l)
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

// Remove removes a tenant's licence of a package (type, "": the default; name) in the entry the
// caller names by its source (spec 0035: required; a removed source's by its id form)
// (expected: its version; 0 any).
func (s *Service) Remove(ctx context.Context, a authz.Actor, tenant, packageType, name, source string, expected int64) error {
	t, err := s.tenant(ctx, a, authz.VerbLicense, tenant)
	if err != nil {
		return err
	}
	if packageType, err = parseType(packageType); err != nil {
		return err
	}
	src, live, err := s.entry(ctx, a, t.ID, source, true)
	if err != nil {
		return err
	}
	w := write{name: name, label: source, op: "remove"}
	if err := s.refuseDistributorsEntry(ctx, nil, a, t.ID, src, w); err != nil {
		return err
	}
	lb, err := s.labels(ctx, t.ID)
	if err != nil {
		return err
	}
	return s.Store.InTenantTx(ctx, t.ID, []string{store.LicenseLock(t.ID)}, func(tx *store.Tx) error {
		if err := s.recheck(ctx, tx, a, t.ID, src, false, w); err != nil {
			return err
		}
		l, err := tx.GetLicense(ctx, t.ID, packageType, name, src)
		if errors.Is(err, store.ErrNotFound) && !live {
			return fmt.Errorf("%w: ?source= %q is removed, and its entry holds no licence of %s", store.ErrInvalid, source, name)
		}
		if err != nil {
			return err
		}
		if err := tx.DeleteLicense(ctx, l, expected); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "license.remove", "license:"+name,
			typed(map[string]any{"name": name, "license": l.LicenseID, "reason": "admin", "source": lb.of(src)}, packageType))
	})
}

// SetVisibility makes a licence private or public, in the entry the caller names by its source
// (spec 0035: required) (expected: its version; 0 any). Making one public is an administrator's
// (spec 0029): a public licence's token is served to anyone served a public release of the name, so
// a licence manager only makes one private, of a distributor's publisher's entry.
func (s *Service) SetVisibility(ctx context.Context, a authz.Actor, tenant, packageType, name, source, visibility string, expected int64) (View, error) {
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
	src, _, err := s.entry(ctx, a, t.ID, source, false)
	if err != nil {
		return View{}, err
	}
	w := write{name: name, label: source, op: visibility}
	if err := s.refuseDistributorsEntry(ctx, nil, a, t.ID, src, w); err != nil {
		return View{}, err
	}
	lb, err := s.labels(ctx, t.ID)
	if err != nil {
		return View{}, err
	}
	var l store.License
	err = s.Store.InTenantTx(ctx, t.ID, []string{store.LicenseLock(t.ID)}, func(tx *store.Tx) error {
		if err := s.recheck(ctx, tx, a, t.ID, src, true, w); err != nil {
			return err
		}
		if l, err = tx.GetLicense(ctx, t.ID, packageType, name, src); err != nil {
			return err
		}
		if expected == 0 {
			expected = l.Version
		}
		if l.Visibility == visibility && expected == l.Version {
			return nil
		}
		tx.SetWarningAct("license.change") // spec 0035 phase 2
		if err := tx.SetLicenseVisibility(ctx, &l, visibility, a.String(), expected); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "license.change", "license:"+name,
			typed(map[string]any{"name": name, "license": l.LicenseID, "visibility": visibility, "source": lb.of(src)}, packageType))
	})
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, lb, l), nil
}

// Served adapts the service to the DuckDB routes (serve.Licenses).
type Served struct{ *Service }

// Enabled reports whether licensing is on (licenses.enabled).
func (s Served) Enabled() bool { return s.Service.Enabled }

// List is every licence of a tenant of a package type (a channel's: spec 0034, a licence is served
// only by a channel of its type), of every entry, whose grace lasts and whose key the server does
// not refuse; the route keeps those of the entries its channel serves the names from.
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

// Serve is what a DuckDB route serves for a package (the channel's type, a name) in a tenant from an
// entry (the source the channel serves the name from, spec 0035): the stored licence of that entry
// while its grace lasts and its key is not refused (ok false: none, past its grace, or refused). The
// route serves it only in a channel where a live build of the entry declares its key.
func (s *Service) Serve(ctx context.Context, tenantID, packageType, name, source string) (store.License, bool, error) {
	if !s.Enabled || source == "" {
		return store.License{}, false, nil
	}
	l, err := s.Store.GetLicense(ctx, tenantID, packageType, name, source)
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

// KeyView is a licence key and the entries for which it is live (spec 0035): what an import into
// each would accept for the key, each with the number of its builds declaring the key.
type KeyView struct {
	Kid     string            `json:"kid"`
	Sources []store.KeySource `json:"sources"`
}

// LicenseKeys lists the keys a tenant's licences of a package type ("": the default) are verified
// with, by name (one name, or all: name ""), without the keys the server refuses: exactly what an
// import's key check would accept now, per entry (spec 0035; of one entry, named by its source, or
// of all: source "").
func (s *Service) LicenseKeys(ctx context.Context, a authz.Actor, tenant, packageType, name, source string) ([]NameKeys, error) {
	t, err := s.tenant(ctx, a, authz.VerbLicense, tenant)
	if err != nil {
		return nil, err
	}
	if packageType, err = parseType(packageType); err != nil {
		return nil, err
	}
	src := ""
	if source != "" {
		if src, _, err = s.entry(ctx, a, t.ID, source, false); err != nil {
			return nil, err
		}
	}
	ks, err := s.Store.TenantLicenseKeys(ctx, t.ID, packageType, name, src)
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
		last.Keys = append(last.Keys, KeyView{Kid: k.Kid, Sources: k.Sources})
	}
	return out, nil
}
