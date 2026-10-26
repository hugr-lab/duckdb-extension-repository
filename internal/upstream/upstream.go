// Package upstream fills a tenant's channels from upstreams (spec 0009): DuckDB's core and
// community repositories and other DuckDB repositories. It manages the upstream records and runs
// their matrices through the intake into signed channels.
package upstream

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream/duckdbkeys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream/upspec"
)

// DuckDB's repositories, over https (spec 0009).
const (
	CoreURL      = "https://extensions.duckdb.org"
	CommunityURL = "https://community-extensions.duckdb.org"
)

// Errors.
var (
	// ErrNoKeys is a repository whose .well-known file lists none of the pinned fingerprints.
	ErrNoKeys = errors.New("upstream: the repository lists none of the pinned keys")
	// ErrCollision is an allowlist name another upstream of the channel lists, or a replacement
	// holds in the channel (409).
	ErrCollision = errors.New("upstream")
	// ErrPullThrough is a run asked of a pull-through upstream, which fetches on a miss (409).
	ErrPullThrough = errors.New("upstream: a pull-through upstream fetches on a miss, not in runs")
	// ErrBundle is a run asked of a bundle-mode upstream, which only bundle imports fill (409).
	ErrBundle = errors.New("upstream: a bundle-mode upstream is filled by bundle imports, not runs")
	// ErrPaused is a run asked of a paused upstream (409).
	ErrPaused = errors.New("upstream: the upstream is paused; resume it first")
)

// Fetcher is egress as upstreams use it.
type Fetcher interface {
	Get(ctx context.Context, raw string) ([]byte, http.Header, error)
	Download(ctx context.Context, raw string, o egress.DownloadOptions, w io.Writer) (egress.Download, error)
	PortAllowed(port uint16) bool
}

// Config bounds runs (config upstreams:).
type Config struct {
	Concurrency  int           // cells fetched at once per replica (default 4)
	PerHost      int           // cells fetched at once per upstream host per replica (default 2)
	FetchTimeout time.Duration // default 10m
	MinRate      int64         // bytes per second (default 64 KiB/s)
	Interval     time.Duration // between scheduled runs (phase 1b); 0: runs on request only
}

// Service manages upstreams and runs them.
type Service struct {
	Store    *store.Store
	Releases *release.Service
	Blob     *blob.Service
	Fetch    Fetcher
	Authz    authz.Authorizer
	Config   Config
	// MaxBody is blob.max_body; MaxIngests the blob service's ingest slots (runs use half).
	MaxBody    int64
	MaxIngests int
	TempDir    string // downloads; "" is the system's
	// Feeds takes a bundle's statements into bundle-mode feeds (spec 0014 phase 1b); nil: a bundle's
	// feeds are read through
	Feeds *attachment.Feeds
	// Uploads are the limits on bundles uploaded over the API (spec 0014 phase 2)
	Uploads     UploadSettings
	wakeOnce    sync.Once
	wakeUploads chan struct{}
	Log         Logger
	// DuckDB replaces DuckDB's repositories and keys (tests); the zero value is the real ones.
	DuckDB DuckDB
	// Credentials are the configured credentials private upstreams use (phase 3).
	Credentials credential.Registry
	// Licenses are the tenant's licences a bundle carries out and in (spec 0021); nil: none.
	Licenses *licenses.Service

	gate gates
}

// DuckDB is where DuckDB's core and community repositories are and which keys sign them.
type DuckDB struct {
	CoreURL, CommunityURL   string
	CoreKeys, CommunityKeys []*rsa.PublicKey
}

// Logger is the logging a run does.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Spec is what an administrator gives to add an upstream.
type Spec struct {
	Name, Kind, Prefix, Channel, Mode, Visibility string
	Credential                                    string // a configured credential's name (phase 3)
	FetchMode                                     string // a mirror's: eager (the default) or lazy (spec 0032)
	Keys, Platforms                               []string
	Entries                                       []store.UpstreamEntry
}

func (s *Service) tenant(ctx context.Context, a authz.Actor, name string) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: name}); err != nil {
		return store.Tenant{}, err
	}
	return s.Store.GetTenant(ctx, name)
}

// Add creates an upstream (spec 0009). A repository's .well-known file must list a pinned key; a
// name with a live replacement in the channel, or on another upstream of the channel, is refused.
func (s *Service) Add(ctx context.Context, a authz.Actor, tenant string, sp Spec) (store.Upstream, error) {
	if err := reserved.Sync(ctx, s.Store); err != nil { // spec 0030: the entries are checked against the server's list
		return store.Upstream{}, err
	}
	return s.add(ctx, a, tenant, sp)
}

// CheckSource makes Add's checks of the upstream itself, for a channel of channelKind in tenant:
// its fields, the passthrough rules, its credential (from configuration), and a repository's
// .well-known read through egress with a pinned key among its keys. It reads nothing of the store,
// so the bootstrap checks an upstream of a tenant or channel it has not made yet (spec 0024). The
// answer is the upstream as Add would record it (defaulted, normalised), without ids.
func (s *Service) CheckSource(ctx context.Context, tenant string, sp Spec, channelKind string) (store.Upstream, error) {
	if sp.Mode == "" {
		sp.Mode = store.ModeMirror
	}
	askedPrivate := sp.Visibility == store.Private
	if sp.Visibility == "" {
		sp.Visibility = store.Private
	}
	u := store.Upstream{Name: sp.Name, Kind: sp.Kind, Mode: sp.Mode, Visibility: sp.Visibility, State: store.UpstreamActive,
		FetchMode: sp.FetchMode}
	if err := store.ValidName(sp.Name); err != nil {
		return u, err
	}
	var err error
	switch sp.Kind {
	case store.UpstreamCore, store.UpstreamCommunity:
		if sp.Prefix != "" || len(sp.Keys) > 0 {
			return u, fmt.Errorf("%w: a %s upstream takes no prefix or keys", store.ErrInvalid, sp.Kind)
		}
	case store.UpstreamRepo:
		portAllowed := func(p uint16) bool { return s.Fetch != nil && s.Fetch.PortAllowed(p) }
		if u.Prefix, err = upspec.Prefix(sp.Prefix, sp.Mode, portAllowed); err != nil {
			return u, err
		}
		if len(sp.Keys) == 0 {
			return u, fmt.Errorf("%w: a repository upstream pins at least one key", store.ErrInvalid)
		}
	default:
		return u, fmt.Errorf("%w: kind %q (duckdb-core, duckdb-community or repository)", store.ErrInvalid, sp.Kind)
	}
	if sp.Mode != store.ModeMirror && sp.Mode != store.ModePullThrough && sp.Mode != store.ModeBundle {
		return u, fmt.Errorf("%w: mode %q (mirror, pull-through or bundle)", store.ErrInvalid, sp.Mode)
	}
	if sp.Visibility != store.Public && sp.Visibility != store.Private {
		return u, fmt.Errorf("%w: visibility %q", store.ErrInvalid, sp.Visibility)
	}
	if err := checkFetchMode(sp.Mode, &u.FetchMode); err != nil {
		return u, err
	}
	for _, k := range sp.Keys {
		if err := upspec.Fingerprint(k); err != nil {
			return u, err
		}
	}
	if len(sp.Platforms) == 0 {
		return u, fmt.Errorf("%w: an upstream fetches at least one platform", store.ErrInvalid)
	}
	for _, p := range sp.Platforms {
		if err := release.ValidPlatform(p); err != nil {
			return u, err
		}
	}
	entries := slices.Clone(sp.Entries)
	for i := range entries {
		if err := upspec.Entry(sp.Kind, sp.Mode, &entries[i]); err != nil {
			return u, err
		}
	}
	u.Keys, u.Platforms, u.Entries = upspec.Dedup(sp.Keys), upspec.Dedup(sp.Platforms), entries
	if channelKind == store.ChannelPassthrough {
		// DuckDB checks a core-typed repository against its core keys only: nothing else would load
		if u.Kind != store.UpstreamCore {
			return u, fmt.Errorf("%w: a passthrough channel takes duckdb-core upstreams only", store.ErrInvalid)
		}
		if u.Mode == store.ModePullThrough { // its callers carry no token: nothing would ever pull
			return u, fmt.Errorf("%w: a passthrough channel takes no pull-through upstream", store.ErrInvalid)
		}
		if u.FetchMode == store.FetchLazy { // nor would anyone ask (spec 0032)
			return u, errPassthroughLazy
		}
		if askedPrivate {
			return u, fmt.Errorf("%w: a passthrough channel's releases are public", store.ErrInvalid)
		}
		u.Visibility = store.Public // public by nature
	}
	if sp.Credential != "" {
		if u.Mode == store.ModeBundle { // it contacts no source; the exporting side authenticates (spec 0014)
			return u, errBundleCredential
		}
		if err := s.checkCredential(sp.Credential, tenant, u, sp.Visibility == store.Public); err != nil {
			return u, err
		}
		u.Credential = sp.Credential
	}
	if u.Kind == store.UpstreamRepo && u.Mode != store.ModeBundle { // bundle mode: checked at import, from the bundle
		if _, err := s.repoKeys(ctx, u); err != nil {
			var se *egress.StatusError
			if errors.Is(err, egress.ErrConnect) || errors.As(err, &se) && se.Temporary() {
				// still ErrInvalid for the API (400, as before); the bootstrap tries ErrSourceFetch again
				return u, fmt.Errorf("%w: %w: %w", ErrSourceFetch, store.ErrInvalid, err)
			}
			return u, fmt.Errorf("%w: %w", store.ErrInvalid, err)
		}
	}
	return u, nil
}

// checkFetchMode checks a fetch mode for an upstream of mode, and defaults it (spec 0032): a mirror's
// is eager or lazy, eager by default; other modes take none.
func checkFetchMode(mode string, fetch *string) error {
	switch {
	case mode != store.ModeMirror && *fetch != "":
		return fmt.Errorf("%w: only a mirror takes a fetch mode", store.ErrInvalid)
	case mode != store.ModeMirror:
		return nil
	case *fetch == "":
		*fetch = store.FetchEager
	case *fetch != store.FetchEager && *fetch != store.FetchLazy:
		return fmt.Errorf("%w: fetch %q (eager or lazy)", store.ErrInvalid, *fetch)
	}
	return nil
}

var errPassthroughLazy = fmt.Errorf("%w: a passthrough channel takes no lazy mirror: its requests carry no token, so no one could ask", store.ErrInvalid)

// ErrSourceFetch is a source's .well-known that cannot be read for now: no connection, a timeout, a
// 5xx or 429 (a bootstrap tries again; spec 0024).
var ErrSourceFetch = errors.New("upstream: the source cannot be read for now")

func (s *Service) add(ctx context.Context, a authz.Actor, tenant string, sp Spec) (store.Upstream, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.Upstream{}, err
	}
	u, passthrough, err := s.prepareAdd(ctx, a, t, sp)
	if err != nil {
		return u, err
	}
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{store.TenantUpstreamLock(t.ID)}, func(tx *store.Tx) error {
		if err := s.insert(ctx, tx, a, t, &u, sp.Channel, passthrough); err != nil {
			return err
		}
		return bumpTenant(ctx, tx, t.ID)
	})
	return u, err
}

// prepareAdd makes the checks of the upstream itself (CheckSource) for its channel in tenant t.
func (s *Service) prepareAdd(ctx context.Context, a authz.Actor, t store.Tenant, sp Spec) (store.Upstream, bool, error) {
	sc, err := s.Store.GetServeChannel(ctx, t.Name, sp.Channel)
	if err != nil {
		return store.Upstream{}, false, err
	}
	// spec 0034: an upstream's names are of its home channel's type, and every source kista fetches
	// from (DuckDB's repositories, a remote channel) serves DuckDB extensions today
	if sc.Channel.PackageType != pkgtype.DuckDBExtension {
		return store.Upstream{}, false, fmt.Errorf("%w: the channel serves %s; an upstream fetches DuckDB extensions", store.ErrInvalid,
			sc.Channel.PackageType)
	}
	u, err := s.CheckSource(ctx, t.Name, sp, sc.Channel.Kind)
	u.TenantID, u.CreatedBy, u.ChannelID = t.ID, a.String(), sc.Channel.ID
	return u, sc.Channel.Kind == store.ChannelPassthrough, err
}

// CheckAddAll makes every check Add would make for each upstream, in order, as if each before it had
// been added (the bootstrap's check, spec 0024): the sources' .well-known reads, then one
// transaction of Add's own writes, rolled back. It answers the index of the first upstream refused,
// with its error; -1 and nil when Add would take them all.
func (s *Service) CheckAddAll(ctx context.Context, a authz.Actor, tenant string, sps []Spec) (int, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return 0, err
	}
	us := make([]store.Upstream, len(sps))
	pass := make([]bool, len(sps))
	for i, sp := range sps {
		if us[i], pass[i], err = s.prepareAdd(ctx, a, t, sp); err != nil {
			return i, err
		}
	}
	failed := -1
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{store.TenantUpstreamLock(t.ID)}, func(tx *store.Tx) error {
		for i := range us {
			if err := s.insert(ctx, tx, a, t, &us[i], sps[i].Channel, pass[i]); err != nil {
				failed = i
				return err
			}
		}
		return errPlanned
	})
	if errors.Is(err, errPlanned) {
		return -1, nil
	}
	return max(failed, 0), err
}

// errPlanned rolls CheckAddAll's transaction back.
var errPlanned = errors.New("upstream: planned, not added")

// insert records an upstream under the tenant's upstream lock: a name not taken, no collision in its
// channel, the shadows of a first passthrough upstream, its event, and the limits.
func (s *Service) insert(ctx context.Context, tx *store.Tx, a authz.Actor, t store.Tenant, u *store.Upstream, channel string, passthrough bool) error {
	if _, err := tx.GetUpstream(ctx, t.ID, u.Name); err == nil {
		return fmt.Errorf("%w: upstream %s", store.ErrExists, u.Name)
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	names := make([]string, len(u.Entries))
	for i, e := range u.Entries {
		names[i] = e.Name
	}
	if err := s.collisions(ctx, tx, *u, names); err != nil {
		return err
	}
	if err := tenantReserved(ctx, tx, t.ID, u.Entries); err != nil {
		return err
	}
	if passthrough { // once per tenant: replacements made before 1b become shadows
		first, err := tx.TakeShadowBackfill(ctx, t.ID)
		if err != nil {
			return err
		}
		if first {
			builds, err := tx.LiveSignedBuilds(ctx, t.ID)
			if err != nil {
				return err
			}
			for _, b := range builds {
				if reserved.Kind(pkgtype.DuckDBExtension, b.Name) == reserved.Core && !s.Releases.DuckDBCore(b) {
					added, err := tx.AddShadow(ctx, t.ID, b.Name, a.String())
					if err != nil {
						return err
					}
					if added {
						if err := tx.Event(ctx, t.ID, a.String(), "shadow.add", "shadow:"+b.Name, map[string]any{"name": b.Name}); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	if err := tx.InsertUpstream(ctx, u); err != nil {
		return err
	}
	f := map[string]any{"name": u.Name, "kind": u.Kind, "prefix": u.Prefix, "channel": channel, "mode": u.Mode, "visibility": u.Visibility,
		"keys": u.Keys, "platforms": u.Platforms, "extensions": names, "credential": u.Credential}
	if u.Mode == store.ModeMirror {
		f["fetch"] = u.FetchMode
	}
	if err := tx.Event(ctx, t.ID, a.String(), "upstream.add", "upstream:"+u.Name, f); err != nil {
		return err
	}
	return s.checkCells(ctx, tx, *u)
}

var errBundleCredential = fmt.Errorf("%w: a bundle-mode upstream contacts no source and takes no credential", store.ErrInvalid)

// checkCredential checks that an upstream may use a credential: a repository upstream of a tenant
// the credential lists, at a prefix it covers; public only when the credential allows it.
func (s *Service) checkCredential(name, tenant string, u store.Upstream, public bool) error {
	if u.Kind != store.UpstreamRepo {
		return fmt.Errorf("%w: only a repository upstream takes a credential", store.ErrInvalid)
	}
	c, err := s.Credentials.For(name, tenant, u.Prefix)
	if err != nil { // unknown or another's: the same answer, so a tenant never learns another's names
		return fmt.Errorf("%w: credential %s is not one this upstream may use", store.ErrInvalid, name)
	}
	if public && !c.AllowPublic {
		return fmt.Errorf("%w: credential %s does not allow public releases", store.ErrInvalid, name)
	}
	return nil
}

// SetCredential sets an upstream's credential, or clears it ("") (expected: its version). A public
// upstream takes a credential only when the credential allows public releases.
func (s *Service) SetCredential(ctx context.Context, a authz.Actor, tenant, name, cred string, expected int64) (store.Upstream, error) {
	return s.change(ctx, a, tenant, name, "credential", map[string]any{"value": cred}, func(tx *store.Tx, u *store.Upstream) error {
		if u.Mode == store.ModeBundle {
			return errBundleCredential
		}
		if cred != "" {
			if err := s.checkCredential(cred, tenant, *u, u.Visibility == store.Public); err != nil {
				return err
			}
		}
		return tx.SetUpstreamCredential(ctx, u, cred, expected)
	})
}

// SetUsage sets whether the upstream gets this tenant's usage daily (spec 0012 phase 1c): forward
// or off. A bundle-mode upstream contacts nothing, so it forwards nothing.
func (s *Service) SetUsage(ctx context.Context, a authz.Actor, tenant, name, value string, expected int64) (store.Upstream, error) {
	return s.change(ctx, a, tenant, name, "usage", map[string]any{"value": value}, func(tx *store.Tx, u *store.Upstream) error {
		return tx.SetUpstreamUsage(ctx, u, value, expected)
	})
}

// SetFetchMode sets a mirror's fetch mode (spec 0032; expected: its version). Turning eager makes it
// due at once (its whole matrix); turning lazy keeps its releases current: each cell a release
// fills is recorded as asked for.
func (s *Service) SetFetchMode(ctx context.Context, a authz.Actor, tenant, name, mode string, expected int64) (store.Upstream, error) {
	return s.change(ctx, a, tenant, name, "fetch", map[string]any{"value": mode}, func(tx *store.Tx, u *store.Upstream) error {
		if u.Mode != store.ModeMirror {
			return fmt.Errorf("%w: only a mirror takes a fetch mode", store.ErrInvalid)
		}
		if mode != store.FetchEager && mode != store.FetchLazy {
			return fmt.Errorf("%w: fetch %q (eager or lazy)", store.ErrInvalid, mode)
		}
		if mode == store.FetchLazy {
			ch, err := tx.ChannelByID(ctx, u.ChannelID)
			if err != nil {
				return err
			}
			if ch.Kind == store.ChannelPassthrough {
				return errPassthroughLazy
			}
			if u.Hold > 0 { // spec 0033: a lazy mirror fetches because someone is installing now
				return fmt.Errorf("%w: the mirror has a hold; a lazy mirror takes none (set its hold to 0 first)", store.ErrInvalid)
			}
		}
		was := u.FetchMode
		if err := tx.SetUpstreamFetchMode(ctx, u, mode, expected); err != nil {
			return err
		}
		switch {
		case was == mode:
			return nil
		case mode == store.FetchEager:
			return tx.MakeDue(ctx, u.ID, "")
		}
		return tx.AskFilled(ctx, u.ID)
	})
}

// SetHold sets an eager mirror's hold (spec 0033 phase A2; expected: its version, 0 for any): an
// entry its runs bring is held until its arrival plus the hold, and reaches no channel before. Raising
// it applies to the entries held now; lowering it, to those arriving from then on. A lazy or
// pull-through upstream takes none (it fetches because someone is installing now), nor one feeding
// a passthrough channel. A tenant administrator's.
func (s *Service) SetHold(ctx context.Context, a authz.Actor, tenant, name string, d time.Duration, expected int64) (store.Upstream, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.Upstream{}, err
	}
	if err := store.ValidHold(d); err != nil {
		return store.Upstream{}, err
	}
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{store.TenantUpstreamLock(t.ID)}, func(tx *store.Tx) error {
		u, err := tx.GetUpstream(ctx, t.ID, name)
		if err != nil {
			return err
		}
		if u.Mode != store.ModeMirror || u.FetchMode != store.FetchEager {
			return fmt.Errorf("%w: a hold is an eager mirror's; a lazy or pull-through upstream, or a bundle's, takes none", store.ErrInvalid)
		}
		ch, err := tx.ChannelByID(ctx, u.ChannelID)
		if err != nil {
			return err
		}
		if ch.Kind == store.ChannelPassthrough {
			return fmt.Errorf("%w: a passthrough channel's upstream takes no hold", store.ErrInvalid)
		}
		before := u.Hold
		if err := tx.SetUpstreamHold(ctx, &u, d, expected); err != nil {
			return err
		}
		held := 0
		if d > before {
			if held, err = tx.RaiseHeld(ctx, t.ID, store.UpstreamSource(u.ID), d); err != nil {
				return err
			}
		}
		return tx.Event(ctx, t.ID, a.String(), "catalogue.hold", "upstream:"+u.Name,
			map[string]any{"source": "upstream:" + u.Name, "hold": d.String(), "before": before.String(), "held": held})
	})
	if err != nil {
		return store.Upstream{}, err
	}
	return s.Get(ctx, a, tenant, name)
}

// tenantReserved refuses an entry for a name reserved in the tenant (spec 0030) without
// allow_reserved: the holder's builds are not to be replaced by a mirror unasked. It runs under the
// tenant's upstream lock, which a reservation takes too.
func tenantReserved(ctx context.Context, tx *store.Tx, tenantID string, entries []store.UpstreamEntry) error {
	for _, e := range entries {
		if e.Name == "*" || e.AllowReserved {
			continue
		}
		held, err := tx.TenantReserves(ctx, tenantID, pkgtype.DuckDBExtension, e.Name) // an upstream's names are DuckDB's
		if err != nil {
			return err
		}
		if held {
			return fmt.Errorf("%w: %q is reserved in the tenant; an upstream provides it only with allow_reserved", store.ErrInvalid, e.Name)
		}
	}
	return nil
}

// collisions refuses names another upstream of the channel lists (under the tenant's upstream
// lock). A name another source holds in the channel is not refused: the entry's cells are
// other_source until the channel's administrator switches the source (spec 0031).
func (s *Service) collisions(ctx context.Context, tx *store.Tx, u store.Upstream, names []string) error {
	var taken []string
	for _, n := range names {
		owner, err := tx.ChannelEntryOwner(ctx, u.ChannelID, n, u.ID)
		if err != nil {
			return err
		}
		if owner != "" {
			taken = append(taken, n+" ("+owner+")")
		}
	}
	if len(taken) > 0 {
		return fmt.Errorf("%w: another upstream of the channel lists %s", ErrCollision, strings.Join(taken, ", "))
	}
	return nil
}

// checkCells enforces the matrix caps for an upstream as stored in tx.
func (s *Service) checkCells(ctx context.Context, tx *store.Tx, u store.Upstream) error {
	n, err := tx.TenantCells(ctx, u.TenantID, "")
	if err != nil {
		return err
	}
	own, err := tx.TenantCells(ctx, u.TenantID, u.ID)
	if err != nil {
		return err
	}
	if n-own > store.MaxUpstreamCells {
		return fmt.Errorf("%w: the upstream's matrix would be %d cells (at most %d)", store.ErrInvalid, n-own, store.MaxUpstreamCells)
	}
	if n > store.MaxTenantCells {
		return fmt.Errorf("%w: the tenant's upstreams would fetch %d cells (at most %d)", store.ErrInvalid, n, store.MaxTenantCells)
	}
	return nil
}

// bumpTenant renews every channel's snapshot of a tenant: the index's shadows follow allowlists.
func bumpTenant(ctx context.Context, tx *store.Tx, tenantID string) error {
	ids, err := tx.TenantChannelIDs(ctx, tenantID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := tx.BumpReleaseVersion(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// List lists a tenant's upstreams.
func (s *Service) List(ctx context.Context, a authz.Actor, tenant string) ([]store.Upstream, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	var out []store.Upstream
	err = s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		out, err = tx.ListUpstreams(ctx, t.ID)
		return err
	})
	return out, err
}

// Get reads an upstream.
func (s *Service) Get(ctx context.Context, a authz.Actor, tenant, name string) (store.Upstream, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.Upstream{}, err
	}
	var u store.Upstream
	err = s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		u, err = tx.GetUpstream(ctx, t.ID, name)
		return err
	})
	return u, err
}

// change runs fn on an upstream under the tenant's upstream lock, moves the record's version, and
// renews the tenant's snapshots.
// The change's event (spec 0010) names what changed and its value.
func (s *Service) change(ctx context.Context, a authz.Actor, tenant, name string, what string, fields map[string]any,
	fn func(tx *store.Tx, u *store.Upstream) error) (store.Upstream, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.Upstream{}, err
	}
	var u store.Upstream
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{store.TenantUpstreamLock(t.ID)}, func(tx *store.Tx) error {
		if u, err = tx.GetUpstream(ctx, t.ID, name); err != nil {
			return err
		}
		before := u.Version
		if err := fn(tx, &u); err != nil {
			return err
		}
		if u.Version == before { // the record changed: a new ETag (Set and Sync move it themselves or not at all)
			if err := tx.BumpUpstream(ctx, &u); err != nil {
				return err
			}
		}
		f := map[string]any{"name": u.Name, "change": what}
		for k, v := range fields {
			f[k] = v
		}
		if err := tx.Event(ctx, t.ID, a.String(), "upstream.change", "upstream:"+u.Name, f); err != nil {
			return err
		}
		// every change renews the tenant's snapshots: the index's shadows follow allowlists, and
		// serving's pull-through follows platforms and states (spec 0009 phase 2)
		return bumpTenant(ctx, tx, t.ID)
	})
	if err != nil {
		return store.Upstream{}, err
	}
	return s.Get(ctx, a, tenant, name)
}

// Remove deletes an upstream (expected: its version, 0 from the CLI). Its releases stay; the names
// it provided are no longer reserved.
func (s *Service) Remove(ctx context.Context, a authz.Actor, tenant, name string, expected int64) error {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return err
	}
	locks := []string{store.TenantUpstreamLock(t.ID), store.LicenseLock(t.ID)}
	return s.Store.InTenantTxForUpdate(ctx, t.ID, locks, func(tx *store.Tx) error {
		u, err := tx.GetUpstream(ctx, t.ID, name)
		if err != nil {
			return err
		}
		// DeleteUpstream checks the version too; checked here first, so no licence goes for a stale
		// request
		if expected != 0 && expected != u.Version {
			return store.ErrConflict
		}
		// the licences its relay governs go with it (spec 0012; spec 0035: the relayed licences of its
		// entry; an import there stays, under the entry's id form), each recorded
		ls, err := tx.UpstreamLicenses(ctx, t.ID, u.ID)
		if err != nil {
			return err
		}
		for _, l := range ls {
			if err := tx.DeleteLicense(ctx, l, 0); err != nil {
				return err
			}
			// the licence's entry, as the licence routes name it
			f := map[string]any{"name": l.Name, "license": l.LicenseID, "reason": "upstream removed", "source": "upstream:" + u.Name}
			if l.PackageType != pkgtype.Default { // spec 0034: when it is not the default
				f["package_type"] = l.PackageType
			}
			if err := tx.Event(ctx, t.ID, a.String(), "license.remove", "license:"+l.Name, f); err != nil {
				return err
			}
		}
		if err := tx.DeleteUpstream(ctx, u, expected); err != nil {
			return err
		}
		if err := tx.Event(ctx, t.ID, a.String(), "upstream.remove", "upstream:"+u.Name, map[string]any{"name": u.Name}); err != nil {
			return err
		}
		return bumpTenant(ctx, tx, t.ID)
	})
}

// Set changes an upstream's visibility (for its new releases) or state; resuming makes a mirror
// due.
func (s *Service) Set(ctx context.Context, a authz.Actor, tenant, name, visibility, state string, expected int64) (store.Upstream, error) {
	set := map[string]any{}
	if visibility != "" {
		set["visibility"] = visibility
	}
	if state != "" {
		set["state"] = state
	}
	return s.change(ctx, a, tenant, name, "set", set, func(tx *store.Tx, u *store.Upstream) error {
		if visibility == store.Public && u.Credential != "" {
			if err := s.checkCredential(u.Credential, tenant, *u, true); err != nil {
				return err
			}
		}
		if visibility == store.Private {
			ch, err := tx.ChannelByID(ctx, u.ChannelID)
			if err != nil {
				return err
			}
			if ch.Kind == store.ChannelPassthrough {
				return fmt.Errorf("%w: a passthrough channel's releases are public", store.ErrInvalid)
			}
		}
		wasPaused := u.State == store.UpstreamPaused
		if err := tx.SetUpstream(ctx, u, visibility, state, expected); err != nil {
			return err
		}
		if wasPaused && u.State == store.UpstreamActive {
			return tx.MakeDue(ctx, u.ID, "")
		}
		return nil
	})
}

// PutEntry adds an allowlist entry, or replaces an existing one's versions, allow_reserved and
// eager; it reports whether the entry is new.
func (s *Service) PutEntry(ctx context.Context, a authz.Actor, tenant, name string, e store.UpstreamEntry) (bool, error) {
	if err := reserved.Sync(ctx, s.Store); err != nil { // spec 0030
		return false, err
	}
	created := false
	_, err := s.change(ctx, a, tenant, name, "extension.put", map[string]any{"value": e.Name, "versions": e.Versions,
		"allow_reserved": e.AllowReserved, "eager": e.Eager}, func(tx *store.Tx, u *store.Upstream) error {
		if err := upspec.Entry(u.Kind, u.Mode, &e); err != nil {
			return err
		}
		if err := tenantReserved(ctx, tx, u.TenantID, []store.UpstreamEntry{e}); err != nil {
			return err
		}
		created = !slices.ContainsFunc(u.Entries, func(x store.UpstreamEntry) bool { return x.Name == e.Name })
		if created {
			if err := s.collisions(ctx, tx, *u, []string{e.Name}); err != nil {
				return err
			}
		}
		if err := tx.PutUpstreamEntry(ctx, u.ID, e); err != nil {
			return err
		}
		if err := tx.MakeDue(ctx, u.ID, ""); err != nil {
			return err
		}
		return s.checkCells(ctx, tx, *u)
	})
	return created, err
}

// RemoveEntry removes an allowlist entry; its cells go with the next run.
func (s *Service) RemoveEntry(ctx context.Context, a authz.Actor, tenant, name, ext string) error {
	_, err := s.change(ctx, a, tenant, name, "extension.remove", map[string]any{"value": ext}, func(tx *store.Tx, u *store.Upstream) error {
		return tx.RemoveUpstreamEntry(ctx, u.ID, ext)
	})
	return err
}

// AddPlatform adds a platform to an upstream.
func (s *Service) AddPlatform(ctx context.Context, a authz.Actor, tenant, name, platform string) error {
	if err := release.ValidPlatform(platform); err != nil {
		return err
	}
	_, err := s.change(ctx, a, tenant, name, "platform.add", map[string]any{"value": platform}, func(tx *store.Tx, u *store.Upstream) error {
		if err := tx.AddUpstreamPlatform(ctx, u.ID, platform); err != nil {
			return err
		}
		if err := tx.MakeDue(ctx, u.ID, ""); err != nil {
			return err
		}
		return s.checkCells(ctx, tx, *u)
	})
	return err
}

// RemovePlatform removes a platform (at least one stays).
func (s *Service) RemovePlatform(ctx context.Context, a authz.Actor, tenant, name, platform string) error {
	_, err := s.change(ctx, a, tenant, name, "platform.remove", map[string]any{"value": platform}, func(tx *store.Tx, u *store.Upstream) error {
		if len(u.Platforms) == 1 && u.Platforms[0] == platform {
			return fmt.Errorf("%w: an upstream fetches at least one platform", store.ErrInvalid)
		}
		return tx.RemoveUpstreamPlatform(ctx, u.ID, platform)
	})
	return err
}

// AddKey pins a key fingerprint of a repository upstream.
func (s *Service) AddKey(ctx context.Context, a authz.Actor, tenant, name, fingerprint string) error {
	if err := upspec.Fingerprint(fingerprint); err != nil {
		return err
	}
	_, err := s.change(ctx, a, tenant, name, "key.add", map[string]any{"value": fingerprint}, func(tx *store.Tx, u *store.Upstream) error {
		if u.Kind != store.UpstreamRepo {
			return fmt.Errorf("%w: a %s upstream uses DuckDB's keys", store.ErrInvalid, u.Kind)
		}
		return tx.AddUpstreamKey(ctx, u.ID, fingerprint)
	})
	return err
}

// RemoveKey unpins a key (at least one stays).
func (s *Service) RemoveKey(ctx context.Context, a authz.Actor, tenant, name, fingerprint string) error {
	_, err := s.change(ctx, a, tenant, name, "key.remove", map[string]any{"value": fingerprint}, func(tx *store.Tx, u *store.Upstream) error {
		if len(u.Keys) == 1 && u.Keys[0] == fingerprint {
			return fmt.Errorf("%w: a repository upstream pins at least one key", store.ErrInvalid)
		}
		return tx.RemoveUpstreamKey(ctx, u.ID, fingerprint)
	})
	return err
}

// Sync asks for a run (a dry one checks without releasing); a replica holding the upstream's
// lease runs it.
func (s *Service) Sync(ctx context.Context, a authz.Actor, tenant, name string, dryRun bool) (store.Upstream, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.Upstream{}, err
	}
	var u store.Upstream
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{store.TenantUpstreamLock(t.ID)}, func(tx *store.Tx) error {
		if u, err = tx.GetUpstream(ctx, t.ID, name); err != nil {
			return err
		}
		switch u.Mode { // before the state: resuming would not help
		case store.ModePullThrough:
			return ErrPullThrough
		case store.ModeBundle:
			return ErrBundle
		}
		if u.State != store.UpstreamActive {
			return ErrPaused
		}
		return tx.RequestUpstreamRun(ctx, &u, dryRun) // the record's version stays: a request is not a change
	})
	return u, err
}

// Cells pages an upstream's cells after a cursor key, optionally of one outcome.
func (s *Service) Cells(ctx context.Context, a authz.Actor, tenant, name, outcome string, after [3]string, limit int) ([]store.UpstreamCell, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	var out []store.UpstreamCell
	err = s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		u, err := tx.GetUpstream(ctx, t.ID, name)
		if err != nil {
			return err
		}
		if outcome == NotAsked {
			out, err = s.notAsked(ctx, tx, u, after, limit)
			return err
		}
		out, err = tx.ListCells(ctx, u.ID, outcome, after, limit)
		return err
	})
	return out, err
}

// NotAsked is the cells API's outcome for a lazy upstream's cells offered but not fetched yet
// (spec 0032).
const NotAsked = notAsked

// notAsked pages a lazy upstream's matrix cells that nobody asked for, of no eager entry, and never
// fetched, by (DuckDB version, platform, name) after a cursor key; an eager upstream has none.
func (s *Service) notAsked(ctx context.Context, tx *store.Tx, u store.Upstream, after [3]string, limit int) ([]store.UpstreamCell, error) {
	if u.FetchMode != store.FetchLazy {
		return nil, nil
	}
	versions, err := s.Store.ChannelVersions(ctx, u.ChannelID)
	if err != nil {
		return nil, err
	}
	cells, err := tx.Cells(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	asks, err := tx.Asks(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	var keys [][3]string
	for _, e := range u.Entries {
		if e.Eager {
			continue
		}
		for _, p := range u.Platforms {
			for _, v := range versions {
				k := [3]string{v, p, e.Name}
				_, asked := asks[k]
				_, fetched := cells[k]
				if !asked && !fetched && slices.Compare(k[:], after[:]) > 0 {
					keys = append(keys, k)
				}
			}
		}
	}
	slices.SortFunc(keys, func(a, b [3]string) int { return slices.Compare(a[:], b[:]) })
	var out []store.UpstreamCell
	for _, k := range keys[:min(limit, len(keys))] {
		out = append(out, store.UpstreamCell{UpstreamID: u.ID, DuckDBVersion: k[0], Platform: k[1], Name: k[2], Outcome: notAsked})
	}
	return out, nil
}

// keys returns the keys a run verifies with.
func (s *Service) keys(ctx context.Context, u store.Upstream) ([]*rsa.PublicKey, error) {
	if u.Kind != store.UpstreamRepo {
		return s.duckdbKeys(u.Kind), nil
	}
	return s.repoKeys(ctx, u)
}

// duckdbKeys are the keys DuckDB signs a kind's files with: compiled in, or the tests' override.
func (s *Service) duckdbKeys(kind string) []*rsa.PublicKey {
	switch {
	case kind == store.UpstreamCore && s.DuckDB.CoreKeys != nil:
		return s.DuckDB.CoreKeys
	case kind == store.UpstreamCore:
		return duckdbkeys.Core()
	case kind == store.UpstreamCommunity && s.DuckDB.CommunityKeys != nil:
		return s.DuckDB.CommunityKeys
	case kind == store.UpstreamCommunity:
		return duckdbkeys.Community()
	}
	return nil
}

// maxWellKnown is the most of a .well-known file read, as DuckDB reads it.
const maxWellKnown = 64 << 10

// repoKeys reads a repository's .well-known file and keeps the keys whose fingerprints are pinned.
func (s *Service) repoKeys(ctx context.Context, u store.Upstream) ([]*rsa.PublicKey, error) {
	b, _, err := s.Fetch.Get(ctx, u.Prefix+"/.well-known/duckdb-extension-repo.json")
	if err != nil {
		return nil, err
	}
	if len(b) > maxWellKnown {
		return nil, fmt.Errorf("upstream: the .well-known file is over %d bytes", maxWellKnown)
	}
	var wk struct {
		Keys []string `json:"signature_keys"`
	}
	if err := json.Unmarshal(b, &wk); err != nil {
		return nil, fmt.Errorf("upstream: the .well-known file is not valid: %w", err)
	}
	var out []*rsa.PublicKey
	for _, k := range wk.Keys {
		pub, err := extfile.ParsePublicKey(k)
		if err != nil {
			continue
		}
		if slices.Contains(u.Keys, extfile.Fingerprint(pub)) {
			out = append(out, pub)
		}
	}
	if len(out) == 0 {
		return nil, ErrNoKeys
	}
	return out, nil
}

// prefix is where an upstream's files are.
func (s *Service) prefix(u store.Upstream) string {
	switch {
	case u.Kind == store.UpstreamCore && s.DuckDB.CoreURL != "":
		return s.DuckDB.CoreURL
	case u.Kind == store.UpstreamCore:
		return CoreURL
	case u.Kind == store.UpstreamCommunity && s.DuckDB.CommunityURL != "":
		return s.DuckDB.CommunityURL
	case u.Kind == store.UpstreamCommunity:
		return CommunityURL
	}
	return u.Prefix
}

// WithAuthz returns a service with another authorizer, sharing the configuration (a run's gates
// are its own).
func (s *Service) WithAuthz(az authz.Authorizer) *Service {
	c := &Service{Store: s.Store, Releases: s.Releases, Blob: s.Blob, Fetch: s.Fetch, Authz: az, Config: s.Config,
		MaxBody: s.MaxBody, MaxIngests: s.MaxIngests, TempDir: s.TempDir, Feeds: s.Feeds, Uploads: s.Uploads, Log: s.Log,
		DuckDB: s.DuckDB, Credentials: s.Credentials, Licenses: s.Licenses}
	wake := s.WakeUploads() // one channel: an upload the API completes wakes this replica's runner
	c.wakeOnce.Do(func() { c.wakeUploads = wake })
	return c
}

// Shadows lists a tenant's shadows (spec 0009): core names it replaced, which its passthrough
// channels do not serve.
func (s *Service) Shadows(ctx context.Context, a authz.Actor, tenant string) ([]store.Shadow, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	return s.Store.ListShadows(ctx, t.ID)
}

// RemoveShadow lifts a shadow: the tenant's passthrough channels serve DuckDB's build of the name
// again (until the next replacement is released).
func (s *Service) RemoveShadow(ctx context.Context, a authz.Actor, tenant, name string) error {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return err
	}
	return s.Store.InTenantTxForUpdate(ctx, t.ID, []string{store.TenantUpstreamLock(t.ID)}, func(tx *store.Tx) error {
		if err := tx.DeleteShadow(ctx, t.ID, name); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "shadow.remove", "shadow:"+name, map[string]any{"name": name})
	})
}
