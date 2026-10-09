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
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream/duckdbkeys"
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
	Log        Logger
	// DuckDB replaces DuckDB's repositories and keys (tests); the zero value is the real ones.
	DuckDB DuckDB

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
}

// Spec is what an administrator gives to add an upstream.
type Spec struct {
	Name, Kind, Prefix, Channel, Mode, Visibility string
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
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.Upstream{}, err
	}
	if sp.Mode == "" {
		sp.Mode = store.ModeMirror
	}
	askedPrivate := sp.Visibility == store.Private
	if sp.Visibility == "" {
		sp.Visibility = store.Private
	}
	u := store.Upstream{TenantID: t.ID, Name: sp.Name, Kind: sp.Kind, Mode: sp.Mode, Visibility: sp.Visibility,
		State: store.UpstreamActive, CreatedBy: a.String()}
	if err := store.ValidName(sp.Name); err != nil {
		return u, err
	}
	switch sp.Kind {
	case store.UpstreamCore, store.UpstreamCommunity:
		if sp.Prefix != "" || len(sp.Keys) > 0 {
			return u, fmt.Errorf("%w: a %s upstream takes no prefix or keys", store.ErrInvalid, sp.Kind)
		}
	case store.UpstreamRepo:
		if u.Prefix, err = s.checkPrefix(sp.Prefix); err != nil {
			return u, err
		}
		if len(sp.Keys) == 0 {
			return u, fmt.Errorf("%w: a repository upstream pins at least one key", store.ErrInvalid)
		}
	default:
		return u, fmt.Errorf("%w: kind %q (duckdb-core, duckdb-community or repository)", store.ErrInvalid, sp.Kind)
	}
	if sp.Mode != store.ModeMirror && sp.Mode != store.ModePullThrough {
		return u, fmt.Errorf("%w: mode %q (mirror or pull-through)", store.ErrInvalid, sp.Mode)
	}
	if sp.Visibility != store.Public && sp.Visibility != store.Private {
		return u, fmt.Errorf("%w: visibility %q", store.ErrInvalid, sp.Visibility)
	}
	for _, k := range sp.Keys {
		if err := checkFingerprint(k); err != nil {
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
	for i := range sp.Entries {
		if err := checkEntry(sp.Kind, sp.Mode, &sp.Entries[i]); err != nil {
			return u, err
		}
	}
	u.Keys, u.Platforms, u.Entries = dedup(sp.Keys), dedup(sp.Platforms), sp.Entries
	sc, err := s.Store.GetServeChannel(ctx, tenant, sp.Channel)
	if err != nil {
		return u, err
	}
	passthrough := sc.Channel.Kind == store.ChannelPassthrough
	if passthrough {
		// DuckDB checks a core-typed repository against its core keys only: nothing else would load
		if u.Kind != store.UpstreamCore {
			return u, fmt.Errorf("%w: a passthrough channel takes duckdb-core upstreams only", store.ErrInvalid)
		}
		if u.Mode == store.ModePullThrough { // its callers carry no token: nothing would ever pull
			return u, fmt.Errorf("%w: a passthrough channel takes mirrors only", store.ErrInvalid)
		}
		if askedPrivate {
			return u, fmt.Errorf("%w: a passthrough channel's releases are public", store.ErrInvalid)
		}
		u.Visibility = store.Public // public by nature
	}
	u.ChannelID = sc.Channel.ID
	if u.Kind == store.UpstreamRepo {
		if _, err := s.repoKeys(ctx, u); err != nil {
			return u, fmt.Errorf("%w: %w", store.ErrInvalid, err)
		}
	}
	err = s.Store.InTx(ctx, store.TenantUpstreamLock(t.ID), func(tx *store.Tx) error {
		if _, err := tx.GetUpstream(ctx, t.ID, u.Name); err == nil {
			return fmt.Errorf("%w: upstream %s", store.ErrExists, u.Name)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		names := make([]string, len(u.Entries))
		for i, e := range u.Entries {
			names[i] = e.Name
		}
		if err := s.collisions(ctx, tx, u, names); err != nil {
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
					if reserved.Kind(b.Name) == reserved.Core && !s.Releases.DuckDBCore(b) {
						if _, err := tx.AddShadow(ctx, t.ID, b.Name, a.String()); err != nil {
							return err
						}
					}
				}
			}
		}
		if err := tx.InsertUpstream(ctx, &u); err != nil {
			return err
		}
		if err := s.checkCells(ctx, tx, u); err != nil {
			return err
		}
		return bumpTenant(ctx, tx, t.ID)
	})
	return u, err
}

// checkPrefix normalises a repository prefix: https (http to loopback in development), no user
// info, query, fragment or dot segments, and a port egress may reach.
func (s *Service) checkPrefix(raw string) (string, error) {
	bad := func(why string) (string, error) {
		return "", fmt.Errorf("%w: prefix %q: %s", store.ErrInvalid, raw, why)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return bad("not an absolute URL")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return bad("no user info, query or fragment")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return bad("not https")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return bad("dot segments")
		}
	}
	if u.RawPath != "" {
		return bad("escaped path characters")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || !s.Fetch.PortAllowed(uint16(n)) {
			return bad("egress does not reach that port")
		}
	}
	u.Path = strings.TrimRight(path.Clean("/"+u.Path), "/")
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if u.Port() == map[string]string{"https": "443", "http": "80"}[u.Scheme] {
		u.Host = u.Hostname() // the default port is dropped: one upstream, one form
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	return u.String(), nil
}

func checkFingerprint(fp string) error {
	h, ok := strings.CutPrefix(fp, "sha256:")
	if !ok || len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
		return fmt.Errorf("%w: key fingerprint %q (sha256:<64 lowercase hex>)", store.ErrInvalid, fp)
	}
	return nil
}

// checkEntry validates an allowlist entry for an upstream of kind (spec 0009): a canonical name,
// reserved names only from their own repository unless allow_reserved, valid versions.
func checkEntry(kind, mode string, e *store.UpstreamEntry) error {
	if e.Name == "*" { // any name: pull-through only, never a reserved name, no versions
		if mode != store.ModePullThrough || len(e.Versions) > 0 || e.AllowReserved {
			return fmt.Errorf("%w: \"*\" is a pull-through upstream's entry, without versions or allow_reserved", store.ErrInvalid)
		}
		if kind == store.UpstreamCore { // every core name is reserved: "*" would take none
			return fmt.Errorf("%w: \"*\" takes no core name; list them", store.ErrInvalid)
		}
		return nil
	}
	if c := reserved.Canonical(e.Name); c != "" {
		return fmt.Errorf("%w: %q is an alias; DuckDB installs it as %q", store.ErrInvalid, e.Name, c)
	}
	if err := release.ValidName(e.Name); err != nil {
		return err
	}
	switch k := reserved.Kind(e.Name); {
	case k == reserved.Core && kind != store.UpstreamCore && !e.AllowReserved,
		k == reserved.Community && kind != store.UpstreamCommunity && !e.AllowReserved:
		return fmt.Errorf("%w: %q is a DuckDB %s name; a %s upstream provides it only with allow_reserved", store.ErrInvalid, e.Name, k, kind)
	}
	for _, v := range e.Versions {
		if err := release.ValidExtVersion(v); err != nil {
			return err
		}
	}
	e.Versions = dedup(e.Versions)
	return nil
}

func dedup(xs []string) []string {
	out := slices.Clone(xs)
	slices.Sort(out)
	return slices.Compact(out)
}

// collisions refuses names another upstream of the channel lists or a live replacement holds in
// the channel (under the tenant's upstream lock).
func (s *Service) collisions(ctx context.Context, tx *store.Tx, u store.Upstream, names []string) error {
	var taken, replaced []string
	for _, n := range names {
		owner, err := tx.ChannelEntryOwner(ctx, u.ChannelID, n, u.ID)
		if err != nil {
			return err
		}
		if owner != "" {
			taken = append(taken, n+" ("+owner+")")
		}
		r, err := tx.LiveReplacement(ctx, u.ChannelID, n)
		if err != nil {
			return err
		}
		if r {
			replaced = append(replaced, n)
		}
	}
	if len(taken) > 0 {
		return fmt.Errorf("%w: another upstream of the channel lists %s", ErrCollision, strings.Join(taken, ", "))
	}
	if len(replaced) > 0 {
		return fmt.Errorf("%w: the channel holds releases of %s that are not the upstream's; drop the entries or yank them",
			ErrCollision, strings.Join(replaced, ", "))
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
func (s *Service) change(ctx context.Context, a authz.Actor, tenant, name string, fn func(tx *store.Tx, u *store.Upstream) error) (store.Upstream, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.Upstream{}, err
	}
	var u store.Upstream
	err = s.Store.InTx(ctx, store.TenantUpstreamLock(t.ID), func(tx *store.Tx) error {
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
	return s.Store.InTx(ctx, store.TenantUpstreamLock(t.ID), func(tx *store.Tx) error {
		u, err := tx.GetUpstream(ctx, t.ID, name)
		if err != nil {
			return err
		}
		if err := tx.DeleteUpstream(ctx, u, expected); err != nil {
			return err
		}
		return bumpTenant(ctx, tx, t.ID)
	})
}

// Set changes an upstream's visibility (for its new releases) or state; resuming makes a mirror
// due.
func (s *Service) Set(ctx context.Context, a authz.Actor, tenant, name, visibility, state string, expected int64) (store.Upstream, error) {
	return s.change(ctx, a, tenant, name, func(tx *store.Tx, u *store.Upstream) error {
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

// PutEntry adds an allowlist entry, or replaces an existing one's versions and allow_reserved; it
// reports whether the entry is new.
func (s *Service) PutEntry(ctx context.Context, a authz.Actor, tenant, name string, e store.UpstreamEntry) (bool, error) {
	created := false
	_, err := s.change(ctx, a, tenant, name, func(tx *store.Tx, u *store.Upstream) error {
		if err := checkEntry(u.Kind, u.Mode, &e); err != nil {
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
	_, err := s.change(ctx, a, tenant, name, func(tx *store.Tx, u *store.Upstream) error {
		return tx.RemoveUpstreamEntry(ctx, u.ID, ext)
	})
	return err
}

// AddPlatform adds a platform to an upstream.
func (s *Service) AddPlatform(ctx context.Context, a authz.Actor, tenant, name, platform string) error {
	if err := release.ValidPlatform(platform); err != nil {
		return err
	}
	_, err := s.change(ctx, a, tenant, name, func(tx *store.Tx, u *store.Upstream) error {
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
	_, err := s.change(ctx, a, tenant, name, func(tx *store.Tx, u *store.Upstream) error {
		if len(u.Platforms) == 1 && u.Platforms[0] == platform {
			return fmt.Errorf("%w: an upstream fetches at least one platform", store.ErrInvalid)
		}
		return tx.RemoveUpstreamPlatform(ctx, u.ID, platform)
	})
	return err
}

// AddKey pins a key fingerprint of a repository upstream.
func (s *Service) AddKey(ctx context.Context, a authz.Actor, tenant, name, fingerprint string) error {
	if err := checkFingerprint(fingerprint); err != nil {
		return err
	}
	_, err := s.change(ctx, a, tenant, name, func(tx *store.Tx, u *store.Upstream) error {
		if u.Kind != store.UpstreamRepo {
			return fmt.Errorf("%w: a %s upstream uses DuckDB's keys", store.ErrInvalid, u.Kind)
		}
		return tx.AddUpstreamKey(ctx, u.ID, fingerprint)
	})
	return err
}

// RemoveKey unpins a key (at least one stays).
func (s *Service) RemoveKey(ctx context.Context, a authz.Actor, tenant, name, fingerprint string) error {
	_, err := s.change(ctx, a, tenant, name, func(tx *store.Tx, u *store.Upstream) error {
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
	err = s.Store.InTx(ctx, store.TenantUpstreamLock(t.ID), func(tx *store.Tx) error {
		if u, err = tx.GetUpstream(ctx, t.ID, name); err != nil {
			return err
		}
		if u.State != store.UpstreamActive {
			return ErrPaused
		}
		if u.Mode == store.ModePullThrough {
			return ErrPullThrough
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
		out, err = tx.ListCells(ctx, u.ID, outcome, after, limit)
		return err
	})
	return out, err
}

// keys returns the keys a run verifies with.
func (s *Service) keys(ctx context.Context, u store.Upstream) ([]*rsa.PublicKey, error) {
	switch u.Kind {
	case store.UpstreamCore:
		if s.DuckDB.CoreKeys != nil {
			return s.DuckDB.CoreKeys, nil
		}
		return duckdbkeys.Core(), nil
	case store.UpstreamCommunity:
		if s.DuckDB.CommunityKeys != nil {
			return s.DuckDB.CommunityKeys, nil
		}
		return duckdbkeys.Community(), nil
	}
	return s.repoKeys(ctx, u)
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
	return &Service{Store: s.Store, Releases: s.Releases, Blob: s.Blob, Fetch: s.Fetch, Authz: az, Config: s.Config,
		MaxBody: s.MaxBody, MaxIngests: s.MaxIngests, TempDir: s.TempDir, Log: s.Log, DuckDB: s.DuckDB}
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
	return s.Store.InTx(ctx, store.TenantUpstreamLock(t.ID), func(tx *store.Tx) error { return tx.DeleteShadow(ctx, t.ID, name) })
}
