package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0009: upstreams (tenant administrators).

type entryJSON struct {
	Name          string   `json:"name"`
	Versions      []string `json:"versions"`
	AllowReserved bool     `json:"allow_reserved,omitempty"`
}

type upstreamJSON struct {
	Name       string          `json:"name"`
	Kind       string          `json:"kind"`
	Prefix     string          `json:"prefix,omitempty"`
	Channel    string          `json:"channel"`
	Mode       string          `json:"mode"`
	Visibility string          `json:"visibility"`
	State      string          `json:"state"`
	Run        string          `json:"run"` // idle | requested | dry_run_requested | running
	Keys       []string        `json:"keys"`
	Platforms  []string        `json:"platforms"`
	Extensions []entryJSON     `json:"extensions"`
	LastRunAt  string          `json:"last_run_at,omitempty"`
	LastRun    json.RawMessage `json:"last_run,omitempty"`
	CreatedAt  string          `json:"created_at"`
	CreatedBy  string          `json:"created_by"`
	Credential string          `json:"credential,omitempty"` // a configured credential's name (spec 0009 phase 3)
	ETag       string          `json:"etag"`
}

type cellJSON struct {
	DuckDBVersion string `json:"duckdb_version"`
	Platform      string `json:"platform"`
	Name          string `json:"name"`
	Outcome       string `json:"outcome"`
	Detail        string `json:"detail,omitempty"`
	BodyHash      string `json:"body_hash,omitempty"`
	FetchedAt     string `json:"fetched_at"`
}

func (h *Handler) upstreamOf(u store.Upstream, channels map[string]string, c caller) upstreamJSON {
	o := upstreamJSON{Name: u.Name, Kind: u.Kind, Prefix: u.Prefix, Channel: channels[u.ChannelID], Mode: u.Mode,
		Visibility: u.Visibility, State: u.State, Run: "idle", Keys: nonNil(u.Keys), Platforms: nonNil(u.Platforms),
		Extensions: []entryJSON{}, LastRunAt: timeOf(u.LastRunAt), CreatedAt: timeOf(u.CreatedAt),
		CreatedBy: createdBy(u.CreatedBy, c), Credential: u.Credential, ETag: versionTag(u.Version)}
	switch {
	case !u.RequestedAt.IsZero() && u.RequestDryRun:
		o.Run = "dry_run_requested"
	case !u.RequestedAt.IsZero():
		o.Run = "requested"
	}
	if u.LastRun != "" && json.Valid([]byte(u.LastRun)) {
		o.LastRun = json.RawMessage(u.LastRun)
	}
	for _, e := range u.Entries {
		o.Extensions = append(o.Extensions, entryJSON{Name: e.Name, Versions: nonNil(e.Versions), AllowReserved: e.AllowReserved})
	}
	return o
}

// channelNames maps a tenant's channel ids to names.
func (h *Handler) channelNames(w http.ResponseWriter, r *http.Request, tenant string) (map[string]string, bool) {
	chs, err := h.o.Store.ListChannels(r.Context(), tenant)
	if err != nil {
		h.fail(w, err)
		return nil, false
	}
	m := map[string]string{}
	for _, ch := range chs {
		m[ch.ID] = ch.Name
	}
	return m, true
}

// upstreamErr maps the upstream service's refusals before the common mapping.
func (h *Handler) upstreamErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, upstream.ErrCollision), errors.Is(err, upstream.ErrPaused), errors.Is(err, upstream.ErrPullThrough):
		problem(w, http.StatusConflict, typeConflict, publicRefusal(err))
	case errors.Is(err, upstream.ErrNoKeys):
		problem(w, http.StatusBadRequest, typeInvalid, publicRefusal(err))
	default:
		h.serviceErr(w, err, false)
	}
}

// running reports whether a replica holds an upstream's run lease.
func (h *Handler) running(r *http.Request, id string) bool {
	_, until, found, err := h.o.Store.LeaseState(r.Context(), "kista/upstream/"+id)
	return err == nil && found && until.After(time.Now())
}

func (h *Handler) replyUpstream(w http.ResponseWriter, r *http.Request, c caller, p params, u store.Upstream, status int) {
	chs, ok := h.channelNames(w, r, p["t"])
	if !ok {
		return
	}
	o := h.upstreamOf(u, chs, c)
	if h.running(r, u.ID) {
		o.Run = "running"
	}
	if status == http.StatusCreated {
		created(w, r, "/api/v1/tenants/"+esc(p["t"])+"/upstreams/"+esc(u.Name), o, o.ETag)
		return
	}
	reply(w, r, status, o, o.ETag, noStore)
}

func (h *Handler) listUpstreams(w http.ResponseWriter, r *http.Request, c caller, p params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	us, err := h.o.Upstreams.List(r.Context(), a, p["t"])
	if err != nil {
		h.upstreamErr(w, err)
		return
	}
	chs, ok := h.channelNames(w, r, p["t"])
	if !ok {
		return
	}
	us, next := paged(us, func(x store.Upstream) string { return x.Name }, limit, after)
	out := []upstreamJSON{}
	for _, u := range us {
		o := h.upstreamOf(u, chs, c)
		if h.running(r, u.ID) {
			o.Run = "running"
		}
		out = append(out, o)
	}
	reply(w, r, http.StatusOK, list("upstreams", out, next), "", noStore)
}

func (h *Handler) addUpstream(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Name       string      `json:"name"`
		Kind       string      `json:"kind"`
		Prefix     string      `json:"prefix"`
		Channel    string      `json:"channel"`
		Mode       string      `json:"mode"`
		Visibility string      `json:"visibility"`
		Keys       []string    `json:"keys"`
		Platforms  []string    `json:"platforms"`
		Extensions []entryJSON `json:"extensions"`
		Credential string      `json:"credential"`
	}
	if !body(w, r, &in) {
		return
	}
	sp := upstream.Spec{Name: in.Name, Kind: in.Kind, Prefix: in.Prefix, Channel: in.Channel, Mode: in.Mode,
		Visibility: in.Visibility, Keys: in.Keys, Platforms: in.Platforms, Credential: in.Credential}
	for _, e := range in.Extensions {
		sp.Entries = append(sp.Entries, store.UpstreamEntry{Name: e.Name, Versions: e.Versions, AllowReserved: e.AllowReserved})
	}
	a, _ := c.actor()
	u, err := h.o.Upstreams.Add(r.Context(), a, p["t"], sp)
	if errors.Is(err, store.ErrNotFound) {
		problem(w, http.StatusBadRequest, typeInvalid, "the channel does not exist")
		return
	}
	if err != nil {
		h.upstreamErr(w, err)
		return
	}
	h.replyUpstream(w, r, c, p, u, http.StatusCreated)
}

func (h *Handler) getUpstream(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	u, err := h.o.Upstreams.Get(r.Context(), a, p["t"], p["name"])
	if err != nil {
		h.upstreamErr(w, err)
		return
	}
	h.replyUpstream(w, r, c, p, u, http.StatusOK)
}

func (h *Handler) removeUpstream(w http.ResponseWriter, r *http.Request, c caller, p params) {
	expected, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	if err := h.o.Upstreams.Remove(r.Context(), a, p["t"], p["name"], expected); err != nil {
		h.upstreamErr(w, err)
		return
	}
	noContent(w)
}

// upstreamAction is public, private, pause or resume (If-Match).
func upstreamAction(visibility, state string) handler {
	return func(h *Handler, w http.ResponseWriter, r *http.Request, c caller, p params) {
		expected, ok := expectedVersion(w, r)
		if !ok {
			return
		}
		a, _ := c.actor()
		if expected == 0 { // If-Match: * is any version
			u, err := h.o.Upstreams.Get(r.Context(), a, p["t"], p["name"])
			if err != nil {
				h.upstreamErr(w, err)
				return
			}
			expected = u.Version
		}
		u, err := h.o.Upstreams.Set(r.Context(), a, p["t"], p["name"], visibility, state, expected)
		if err != nil {
			h.upstreamErr(w, err)
			return
		}
		h.replyUpstream(w, r, c, p, u, http.StatusOK)
	}
}

func (h *Handler) listEntries(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	u, err := h.o.Upstreams.Get(r.Context(), a, p["t"], p["name"])
	if err != nil {
		h.upstreamErr(w, err)
		return
	}
	chs, ok := h.channelNames(w, r, p["t"])
	if !ok {
		return
	}
	reply(w, r, http.StatusOK, map[string]any{"extensions": h.upstreamOf(u, chs, c).Extensions}, "", noStore)
}

// putEntry adds an allowlist entry (201), or replaces an existing one's versions and allow_reserved
// (200).
func (h *Handler) putEntry(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in entryJSON
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	isNew, err := h.o.Upstreams.PutEntry(r.Context(), a, p["t"], p["name"],
		store.UpstreamEntry{Name: in.Name, Versions: in.Versions, AllowReserved: in.AllowReserved})
	if err != nil {
		h.upstreamErr(w, err)
		return
	}
	status := http.StatusOK
	if isNew {
		status = http.StatusCreated
	}
	vs := slices.Clone(in.Versions)
	slices.Sort(vs)
	reply(w, r, status, entryJSON{Name: in.Name, Versions: nonNil(slices.Compact(vs)), AllowReserved: in.AllowReserved}, "", noStore)
}

func (h *Handler) removeEntry(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Upstreams.RemoveEntry(r.Context(), a, p["t"], p["name"], p["entry"]); err != nil {
		h.upstreamErr(w, err)
		return
	}
	noContent(w)
}

// upstreamItems lists, adds and removes platforms or keys.
func listUpstreamItems(key string) handler {
	return func(h *Handler, w http.ResponseWriter, r *http.Request, c caller, p params) {
		a, _ := c.actor()
		u, err := h.o.Upstreams.Get(r.Context(), a, p["t"], p["name"])
		if err != nil {
			h.upstreamErr(w, err)
			return
		}
		items := u.Platforms
		if key == "keys" {
			items = u.Keys
		}
		reply(w, r, http.StatusOK, map[string]any{key: nonNil(items)}, "", noStore)
	}
}

func addUpstreamItem(key string) handler {
	return func(h *Handler, w http.ResponseWriter, r *http.Request, c caller, p params) {
		var in struct {
			Platform    string `json:"platform"`
			Fingerprint string `json:"fingerprint"`
		}
		if !body(w, r, &in) {
			return
		}
		a, _ := c.actor()
		var err error
		if key == "keys" {
			err = h.o.Upstreams.AddKey(r.Context(), a, p["t"], p["name"], in.Fingerprint)
		} else {
			err = h.o.Upstreams.AddPlatform(r.Context(), a, p["t"], p["name"], in.Platform)
		}
		if err != nil {
			h.upstreamErr(w, err)
			return
		}
		reply(w, r, http.StatusCreated, in, "", noStore)
	}
}

func removeUpstreamItem(key string) handler {
	return func(h *Handler, w http.ResponseWriter, r *http.Request, c caller, p params) {
		a, _ := c.actor()
		var err error
		if key == "keys" {
			err = h.o.Upstreams.RemoveKey(r.Context(), a, p["t"], p["name"], p["item"])
		} else {
			err = h.o.Upstreams.RemovePlatform(r.Context(), a, p["t"], p["name"], p["item"])
		}
		if err != nil {
			h.upstreamErr(w, err)
			return
		}
		noContent(w)
	}
}

// syncUpstream asks for a run: 202 with the upstream and its run state.
func (h *Handler) syncUpstream(w http.ResponseWriter, r *http.Request, c caller, p params) {
	dry := false
	switch r.URL.Query().Get("dry_run") {
	case "", "false":
	case "true":
		dry = true
	default:
		problem(w, http.StatusBadRequest, typeInvalid, "dry_run is true or false")
		return
	}
	a, _ := c.actor()
	u, err := h.o.Upstreams.Sync(r.Context(), a, p["t"], p["name"], dry)
	if err != nil {
		h.upstreamErr(w, err)
		return
	}
	h.replyUpstream(w, r, c, p, u, http.StatusAccepted)
}

func (h *Handler) listCells(w http.ResponseWriter, r *http.Request, c caller, p params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	var key [3]string
	if after != "" {
		parts := strings.Split(after, "\x00")
		if len(parts) != 3 {
			problem(w, http.StatusBadRequest, typeInvalid, "the cursor is not one this server returned")
			return
		}
		key = [3]string{parts[0], parts[1], parts[2]}
	}
	a, _ := c.actor()
	cells, err := h.o.Upstreams.Cells(r.Context(), a, p["t"], p["name"], r.URL.Query().Get("outcome"), key, limit+1)
	if err != nil {
		h.upstreamErr(w, err)
		return
	}
	next := ""
	if len(cells) > limit {
		cells = cells[:limit]
		l := cells[len(cells)-1]
		next = base64.RawURLEncoding.EncodeToString([]byte(l.DuckDBVersion + "\x00" + l.Platform + "\x00" + l.Name))
	}
	out := []cellJSON{}
	for _, x := range cells {
		out = append(out, cellJSON{DuckDBVersion: x.DuckDBVersion, Platform: x.Platform, Name: x.Name, Outcome: x.Outcome,
			Detail: x.Detail, BodyHash: x.BodyHash, FetchedAt: timeOf(x.FetchedAt)})
	}
	reply(w, r, http.StatusOK, list("cells", out, next), "", noStore)
}

type shadowJSON struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by"`
}

// listShadows lists the core names the tenant replaced: its passthrough channels do not serve them.
func (h *Handler) listShadows(w http.ResponseWriter, r *http.Request, c caller, p params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	xs, err := h.o.Upstreams.Shadows(r.Context(), a, p["t"])
	if err != nil {
		h.upstreamErr(w, err)
		return
	}
	xs, next := paged(xs, func(x store.Shadow) string { return x.Name }, limit, after)
	out := []shadowJSON{}
	for _, x := range xs {
		out = append(out, shadowJSON{Name: x.Name, CreatedAt: timeOf(x.CreatedAt), CreatedBy: createdBy(x.CreatedBy, c)})
	}
	reply(w, r, http.StatusOK, list("shadows", out, next), "", noStore)
}

// removeShadow lifts a shadow: passthrough channels serve DuckDB's build again.
func (h *Handler) removeShadow(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Upstreams.RemoveShadow(r.Context(), a, p["t"], p["name"]); err != nil {
		h.upstreamErr(w, err)
		return
	}
	noContent(w)
}

// setCredential answers POST …/upstreams/{name}/credential {"credential": name or ""} (If-Match).
func (h *Handler) setCredential(w http.ResponseWriter, r *http.Request, c caller, p params) {
	expected, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	var in struct {
		Credential *string `json:"credential"`
	}
	if !body(w, r, &in) {
		return
	}
	if in.Credential == nil {
		problem(w, http.StatusBadRequest, typeInvalid, "credential is a name, or \"\" to clear it")
		return
	}
	a, _ := c.actor()
	// If-Match: * (0) is the version the transaction reads
	u, err := h.o.Upstreams.SetCredential(r.Context(), a, p["t"], p["name"], *in.Credential, expected)
	if err != nil {
		h.upstreamErr(w, err)
		return
	}
	h.replyUpstream(w, r, c, p, u, http.StatusOK)
}

// listCredentials answers GET /api/v1/credentials: the configured credentials' names, tenants and
// prefixes, never their settings (server administrators).
func (h *Handler) listCredentials(w http.ResponseWriter, r *http.Request, c caller, p params) {
	out := []map[string]any{}
	if h.o.Upstreams != nil {
		for _, i := range h.o.Upstreams.Credentials.Infos() {
			out = append(out, map[string]any{"name": i.Name, "kind": i.Kind, "tenants": nonNil(i.Tenants),
				"prefixes": nonNil(i.Prefixes), "allow_public": i.AllowPublic})
		}
	}
	reply(w, r, http.StatusOK, map[string]any{"credentials": out}, "", noStore)
}
