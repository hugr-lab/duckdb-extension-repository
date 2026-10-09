package api

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// view is the caller's view of a channel: public releases, plus those of the extensions it may
// install there (spec 0007).
func (c caller) view(channelID string) release.Visible {
	if c.admin {
		return release.Everything // a server administrator sees everything
	}
	if c.principals == nil {
		return release.PublicOnly
	}
	all, names := auth.InstallScope(c.principals, c.ta.Grants, channelID)
	if all {
		return release.Everything
	}
	return func(r *store.Candidate) bool { return r.Visibility == store.Public || slices.Contains(names, r.Name) }
}

// --- whoami ---

type grantOut struct {
	Principal string   `json:"principal"`
	Verbs     []string `json:"verbs"`
	Channel   string   `json:"channel,omitempty"`
	Extension string   `json:"extension,omitempty"`
}

func (h *Handler) whoami(w http.ResponseWriter, r *http.Request, c caller, _ params) {
	switch {
	case c.anonymous():
		unauthorized(w)
	case c.principals == nil:
		notFound(w) // a server token: /api/v1/whoami
	case c.pub != nil:
		answer(w, r, publisherWhoami(c), false)
	default:
		answer(w, r, whoamiOf(c), false)
	}
}

func whoamiOf(c caller) map[string]any {
	names := map[string]string{}
	for _, is := range c.ta.Issuers {
		names[is.ID] = is.Name
	}
	var ps []string
	for k := range c.principals {
		g := store.Grant{Kind: k.Kind, Value: k.Value, IssuerName: names[k.IssuerID]}
		ps = append(ps, g.Principal())
	}
	sort.Strings(ps)
	var grants []grantOut
	admin := false
	for _, g := range c.ta.Grants {
		if !c.principals[auth.Key{IssuerID: g.IssuerID, Kind: g.Kind, Value: g.Value}] {
			continue
		}
		grants = append(grants, grantOut{Principal: g.Principal(), Verbs: g.Verbs, Channel: g.ChannelName, Extension: g.Extension})
		// an issuer-wide grant's admin is ignored (spec 0007)
		admin = admin || slices.Contains(g.Verbs, store.VerbAdmin) && g.Kind != store.PrincipalIssuer
	}
	auditor := authz.Covers(c.principals, c.ta.Grants, store.VerbAudit, authz.Resource{Tenant: c.tenant.Name})
	return map[string]any{"tenant": c.tenant.Name, "principals": nonNil(ps), "grants": nonNil(grants), "holds_admin": admin,
		"holds_audit": auditor}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// --- channels ---

func (h *Handler) channels(w http.ResponseWriter, r *http.Request, c caller, _ params) {
	chs, err := h.o.Store.ListChannels(r.Context(), c.tenant.Name)
	if err != nil {
		h.fail(w, err)
		return
	}
	type out struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	list := []out{}
	for _, ch := range chs {
		list = append(list, out{ch.Name, ch.Kind})
	}
	answer(w, r, map[string]any{"channels": list}, c.anonymous())
}

// snapshot reads the path's channel and its snapshot (404 for an unknown channel).
func (h *Handler) snapshot(w http.ResponseWriter, r *http.Request, c caller, p params) (*release.Snapshot, bool) {
	ctx := r.Context()
	sc, err := h.o.Store.GetServeChannel(ctx, c.tenant.Name, p["c"])
	if errors.Is(err, store.ErrNotFound) {
		notFound(w)
		return nil, false
	}
	if err != nil {
		h.fail(w, err)
		return nil, false
	}
	snap, err := h.o.Snapshots.Get(ctx, sc.Channel)
	if err != nil {
		h.fail(w, err)
		return nil, false
	}
	return snap, true
}

type capiOut struct {
	DuckDBVersion string   `json:"duckdb_version"`
	CAPIs         []string `json:"c_api_maxima"`
}

type keyOut struct {
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
}

func (h *Handler) channelInfo(w http.ResponseWriter, r *http.Request, c caller, p params) {
	snap, ok := h.snapshot(w, r, c, p)
	if !ok {
		return
	}
	versions := []capiOut{}
	for _, v := range snap.Versions() {
		cs := slices.Clone(snap.CAPIs[v])
		slices.SortFunc(cs, func(a, b store.CAPI) int {
			return cmp.Or(cmp.Compare(a.Major, b.Major), cmp.Compare(a.Minor, b.Minor), cmp.Compare(a.Patch, b.Patch))
		})
		o := capiOut{DuckDBVersion: v, CAPIs: []string{}}
		for _, x := range cs {
			o.CAPIs = append(o.CAPIs, x.String())
		}
		versions = append(versions, o)
	}
	ks := []keyOut{}
	for _, k := range snap.Keys {
		if k.State == store.KeyActive || k.State == store.KeyTrusted {
			ks = append(ks, keyOut{k.Fingerprint, k.State})
		}
	}
	answer(w, r, map[string]any{"name": snap.Channel.Name, "kind": snap.Channel.Kind, "duckdb_versions": versions,
		"keys": ks}, c.anonymous())
}

// --- extensions ---

func (h *Handler) extensions(w http.ResponseWriter, r *http.Request, c caller, p params) {
	q := r.URL.Query()
	version, platform, cursor, limitS := q.Get("duckdb_version"), q.Get("platform"), q.Get("cursor"), q.Get("limit")
	limit := 100
	if limitS != "" {
		n, err := strconv.Atoi(limitS)
		if err != nil || n < 1 || n > 500 || limitS[0] < '0' || limitS[0] > '9' {
			problem(w, http.StatusBadRequest, typeInvalid, "limit is 1..500")
			return
		}
		limit = n
	}
	after := ""
	if cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			problem(w, http.StatusBadRequest, typeInvalid, "the cursor is not one this server returned")
			return
		}
		after = string(b)
	}
	if (version == "") != (platform == "") {
		problem(w, http.StatusBadRequest, typeInvalid, "duckdb_version and platform go together")
		return
	}
	snap, ok := h.snapshot(w, r, c, p)
	if !ok {
		return
	}
	vis := c.view(snap.Channel.ID)
	type agg struct{ public, private bool }
	names := map[string]*agg{}
	for i := range snap.Releases {
		rel := &snap.Releases[i]
		if !vis(rel) {
			continue
		}
		a := names[rel.Name]
		if a == nil {
			a = &agg{}
			names[rel.Name] = a
		}
		if rel.Visibility == store.Public {
			a.public = true
		} else {
			a.private = true
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		if n > after {
			sorted = append(sorted, n)
		}
	}
	sort.Strings(sorted)
	type row struct {
		Name       string `json:"name"`
		Visibility string `json:"visibility"`
		Current    string `json:"current,omitempty"` // with duckdb_version and platform
	}
	rows := []row{}
	next := ""
	for i, n := range sorted {
		if i == limit {
			next = base64.RawURLEncoding.EncodeToString([]byte(sorted[i-1]))
			break
		}
		a := names[n]
		vis2 := store.Public
		switch {
		case a.public && a.private:
			vis2 = "mixed"
		case a.private:
			vis2 = store.Private
		}
		rw := row{Name: n, Visibility: vis2}
		if version != "" {
			if cur := snap.Flat(version, platform, n, vis); cur != nil {
				rw.Current = cur.ExtVersion
			}
		}
		rows = append(rows, rw)
	}
	out := map[string]any{"extensions": rows}
	if next != "" {
		out["next"] = next
	}
	answer(w, r, out, c.anonymous())
}

type servesOut struct {
	DuckDBVersion string `json:"duckdb_version"`
	Path          string `json:"path"`                // the versioned path, relative to the channel's prefix
	FlatPath      string `json:"flat_path,omitempty"` // when this release is current there
}

type releaseOut struct {
	id            string
	created       time.Time
	Name          string      `json:"name"`
	ExtVersion    string      `json:"version"`
	Platform      string      `json:"platform"`
	ABI           string      `json:"abi"`
	DuckDBVersion string      `json:"build_duckdb_version,omitempty"`
	CAPI          string      `json:"build_c_api,omitempty"`
	State         string      `json:"state"`
	Visibility    string      `json:"visibility"`
	BodyHash      string      `json:"body_hash"`
	ServingKey    string      `json:"serving_key,omitempty"`
	CreatedAt     string      `json:"created_at"`
	Serves        []servesOut `json:"serves"`
	CurrentFor    []string    `json:"current_for"`
	WouldServe    []string    `json:"would_serve,omitempty"` // yanked rows: the DuckDB versions it would serve
	Origin        string      `json:"origin"`
	Upstream      string      `json:"upstream,omitempty"`   // an upstream release's upstream (spec 0009)
	Shadows       string      `json:"shadows,omitempty"`    // a reserved name's owner (spec 0008)
	Blocked       bool        `json:"blocked,omitempty"`    // a yanked row whose body is blocked
	Provenance    any         `json:"provenance,omitempty"` // for the extension's administrators and publishers
}

func gzName(name string) string { return name + ".duckdb_extension.gz" }

// resolver memoises the snapshot's resolution for one request's view.
type resolver struct {
	snap     *release.Snapshot
	vis      release.Visible
	versions []string
	flat     map[[3]string]*store.Candidate
	vers     map[[5]string]*store.Candidate
	// spec 0008: the tenant's blocked bodies, and whether the caller sees provenance (and who)
	blocked    map[string]bool
	provenance bool
	admin      bool
	c          caller
}

// indexResolver is a resolver for one extension's rows: with the tenant's blocks, and provenance
// for the extension's administrators and publishers (spec 0008).
func (h *Handler) indexResolver(w http.ResponseWriter, r *http.Request, c caller, snap *release.Snapshot, name string) (*resolver, bool) {
	rs := newResolver(snap, c.view(snap.Channel.ID))
	rs.blocked = snap.Blocked
	res := authz.Resource{Tenant: c.tenant.Name, Channel: snap.Channel.Name, Extension: name, Reserved: reserved.Kind(name) != ""}
	rs.c, rs.admin = c, c.admin || c.principals != nil && authz.Covers(c.principals, c.ta.Grants, store.VerbAdmin, res)
	rs.provenance = rs.admin
	for _, v := range []string{store.VerbPublish, store.VerbPromote} {
		rs.provenance = rs.provenance || c.principals != nil && authz.Covers(c.principals, c.ta.Grants, v, res)
	}
	return rs, true
}

func newResolver(snap *release.Snapshot, vis release.Visible) *resolver {
	return &resolver{snap: snap, vis: vis, versions: snap.Versions(), flat: map[[3]string]*store.Candidate{},
		vers: map[[5]string]*store.Candidate{}}
}

func (rs *resolver) flatOf(v, platform, name string) *store.Candidate {
	k := [3]string{v, platform, name}
	c, ok := rs.flat[k]
	if !ok {
		c = rs.snap.Flat(v, platform, name, rs.vis)
		rs.flat[k] = c
	}
	return c
}

// versionedOf resolves among active and deprecated releases, or (withYanked) as if nothing were
// yanked.
func (rs *resolver) versionedOf(v, platform, name, extVersion string, withYanked bool) *store.Candidate {
	k := [5]string{v, platform, name, extVersion, strconv.FormatBool(withYanked)}
	c, ok := rs.vers[k]
	if !ok {
		if withYanked {
			c = rs.snap.Versioned(v, platform, name, extVersion, rs.vis, store.ReleaseActive, store.ReleaseDeprecated, store.ReleaseYanked)
		} else {
			c = rs.snap.Versioned(v, platform, name, extVersion, rs.vis)
		}
		rs.vers[k] = c
	}
	return c
}

// row describes one release: the DuckDB versions whose versioned path resolves to it, and those
// whose flat path does; a yanked release is served nowhere and lists instead the versions whose
// versioned path it would serve, so a node that installed it learns it was yanked.
func (rs *resolver) row(rel *store.Candidate) releaseOut {
	o := releaseOut{id: rel.ID, created: rel.CreatedAt, Name: rel.Name, ExtVersion: rel.ExtVersion, Platform: rel.Platform,
		ABI: rel.ABI, DuckDBVersion: rel.DuckDBVersion, State: rel.State, Visibility: rel.Visibility, BodyHash: rel.BodyHash,
		ServingKey: rs.snap.ServingKey, CreatedAt: rel.CreatedAt.UTC().Format(time.RFC3339),
		Serves: []servesOut{}, CurrentFor: []string{}}
	o.Origin, o.Shadows = rel.Origin, release.Shadows(*rel, rs.snap.Provided)
	if rel.Origin == store.OriginUpstream {
		var p struct {
			Upstream string `json:"upstream"`
		}
		if json.Unmarshal([]byte(rel.Provenance), &p) == nil {
			o.Upstream = p.Upstream
		}
	}
	o.Blocked = rel.State == store.ReleaseYanked && rs.blocked[rel.BodyHash]
	if rs.provenance {
		o.Provenance = provenanceView(rel.Provenance, rs.c, rs.admin)
	}
	if rel.CAPI != nil {
		o.CAPI = rel.CAPI.String()
	}
	for _, v := range rs.versions {
		if rel.State == store.ReleaseYanked {
			if best := rs.versionedOf(v, rel.Platform, rel.Name, rel.ExtVersion, true); best != nil && best.ID == rel.ID {
				o.WouldServe = append(o.WouldServe, v)
			}
			continue
		}
		if best := rs.versionedOf(v, rel.Platform, rel.Name, rel.ExtVersion, false); best == nil || best.ID != rel.ID {
			continue
		}
		s := servesOut{DuckDBVersion: v, Path: rel.Name + "/" + rel.ExtVersion + "/" + v + "/" + rel.Platform + "/" + gzName(rel.Name)}
		if cur := rs.flatOf(v, rel.Platform, rel.Name); cur != nil && cur.ID == rel.ID {
			s.FlatPath = v + "/" + rel.Platform + "/" + gzName(rel.Name)
			o.CurrentFor = append(o.CurrentFor, v)
		}
		o.Serves = append(o.Serves, s)
	}
	return o
}

// reaches reports whether a row is reached on a DuckDB version: served there, or yanked from there.
func (o releaseOut) reaches(v string) bool {
	return slices.ContainsFunc(o.Serves, func(s servesOut) bool { return s.DuckDBVersion == v }) || slices.Contains(o.WouldServe, v)
}

// rowsOf computes the rows of one extension in the caller's view, newest first (optionally only one
// platform's).
func (rs *resolver) rowsOf(name, platform string) []releaseOut {
	var out []releaseOut
	for _, p := range rs.snap.Platforms(name) {
		if platform != "" && p != platform {
			continue
		}
		for _, rel := range rs.snap.Group(name, p) {
			if rs.vis(rel) {
				out = append(out, rs.row(rel))
			}
		}
	}
	slices.SortStableFunc(out, func(a, b releaseOut) int {
		if c := b.created.Compare(a.created); c != 0 {
			return c
		}
		return strings.Compare(b.id, a.id)
	})
	return out
}

func (h *Handler) extension(w http.ResponseWriter, r *http.Request, c caller, p params) {
	name, version, platform := p["name"], r.URL.Query().Get("duckdb_version"), r.URL.Query().Get("platform")
	if (version == "") != (platform == "") {
		problem(w, http.StatusBadRequest, typeInvalid, "duckdb_version and platform go together")
		return
	}
	snap, ok := h.snapshot(w, r, c, p)
	if !ok {
		return
	}
	rs, ok := h.indexResolver(w, r, c, snap, name)
	if !ok {
		return
	}
	rows := rs.rowsOf(name, platform)
	if version != "" {
		rows = slices.DeleteFunc(rows, func(o releaseOut) bool { return !o.reaches(version) })
	}
	// an unknown name, or one outside the view, is no rows: the same answer
	answer(w, r, map[string]any{"name": name, "releases": nonNil(rows)}, c.anonymous())
}

// item answers the node agent's question for name@version on a DuckDB version and platform:
// available, deprecated, yanked, or missing (which also means "not in your view"). With available or
// deprecated come the yanked rows that would have served the path (a higher C API major, say), so a
// node compares the body hash it installed.
func (h *Handler) item(w http.ResponseWriter, r *http.Request, c caller, p params) {
	name, extVersion := p["name"], p["v"]
	version, platform := r.URL.Query().Get("duckdb_version"), r.URL.Query().Get("platform")
	if version == "" || platform == "" {
		problem(w, http.StatusBadRequest, typeInvalid, "duckdb_version and platform are required")
		return
	}
	snap, ok := h.snapshot(w, r, c, p)
	if !ok {
		return
	}
	rs, ok := h.indexResolver(w, r, c, snap, name)
	if !ok {
		return
	}
	out := map[string]any{"name": name, "version": extVersion, "duckdb_version": version, "platform": platform}
	var yanked []releaseOut
	for _, rel := range snap.Group(name, platform) {
		if rel.ExtVersion == extVersion && rel.State == store.ReleaseYanked && rs.vis(rel) {
			if o := rs.row(rel); o.reaches(version) {
				yanked = append(yanked, o)
			}
		}
	}
	switch best := rs.versionedOf(version, platform, name, extVersion, false); {
	case best != nil:
		out["status"] = map[string]string{store.ReleaseActive: "available", store.ReleaseDeprecated: "deprecated"}[best.State]
		cur := rs.flatOf(version, platform, name)
		out["current"] = cur != nil && cur.ID == best.ID
		out["release"] = rs.row(best)
		out["yanked"] = nonNil(yanked)
	case len(yanked) > 0:
		out["status"] = "yanked"
		out["current"] = false
		out["yanked"] = yanked
	default:
		out["status"] = "missing"
	}
	answer(w, r, out, c.anonymous())
}
