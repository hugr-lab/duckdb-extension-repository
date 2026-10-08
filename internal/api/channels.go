package api

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Phase 3: channel-level management.

// channelErr maps the key and release services' refusals (409) before the common mapping.
func (h *Handler) channelErr(w http.ResponseWriter, err error, bodyRef bool) {
	for _, e := range []error{keys.ErrState, keys.ErrTooSoon, keys.ErrNoKeys, release.ErrState, release.ErrSlot} {
		if errors.Is(err, e) {
			problem(w, http.StatusConflict, typeConflict, publicRefusal(err))
			return
		}
	}
	if errors.Is(err, keys.ErrMismatch) {
		problem(w, http.StatusBadRequest, typeInvalid, "the signer's key is not the registered key")
		return
	}
	h.serviceErr(w, err, bodyRef)
}

// publicRefusal is a refusal's message: the services word them for the caller (state names,
// durations), never with internal error strings.
func publicRefusal(err error) string {
	msg := err.Error()
	for _, p := range []string{"keys: ", "release: "} {
		msg = strings.TrimPrefix(msg, p)
	}
	if len(msg) > 300 {
		msg = strings.ToValidUTF8(msg[:300], "")
	}
	return msg
}

// expectedVersion reads a required If-Match of a versioned resource: "v<n>", or * (0: any).
func expectedVersion(w http.ResponseWriter, r *http.Request) (int64, bool) {
	tag, ok := ifMatch(w, r)
	if !ok {
		return 0, false
	}
	if tag == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(tag, `"v`), `"`), 10, 64)
	if err != nil || n <= 0 || versionTag(n) != tag {
		problem(w, http.StatusPreconditionFailed, typePrecondition, "it changed: read it again")
		return 0, false
	}
	return n, true
}

func timeOf(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// --- channels ---

func (h *Handler) createChannel(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	ch, err := h.o.Tenants.CreateChannel(r.Context(), a, p["t"], in.Name, in.Kind)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	// a channel's GET is the index's channel detail: no version ETag, no If-Match on channels
	created(w, r, "/api/v1/tenants/"+esc(p["t"])+"/channels/"+esc(ch.Name), map[string]any{"name": ch.Name, "kind": ch.Kind}, "-")
}

func (h *Handler) channelVersions(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	vs, err := h.o.Tenants.ChannelVersions(r.Context(), a, p["t"], p["c"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	slices.SortFunc(vs, release.CompareVersions)
	reply(w, r, http.StatusOK, map[string]any{"duckdb_versions": nonNil(vs)}, "", noStore)
}

func (h *Handler) setChannelVersion(w http.ResponseWriter, r *http.Request, c caller, p params, v string, add bool) {
	a, _ := c.actor()
	var err error
	if add {
		_, err = h.o.Tenants.SetChannelVersions(r.Context(), a, p["t"], p["c"], []string{v}, nil)
	} else {
		_, err = h.o.Tenants.SetChannelVersions(r.Context(), a, p["t"], p["c"], nil, []string{v})
	}
	if err != nil {
		h.serviceErr(w, err, false) // an unknown version to add is the service's ErrInvalid
		return
	}
	h.channelVersions(w, r, c, p)
}

func (h *Handler) addChannelVersion(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		DuckDBVersion string `json:"duckdb_version"`
	}
	if !body(w, r, &in) {
		return
	}
	h.setChannelVersion(w, r, c, p, in.DuckDBVersion, true)
}

func (h *Handler) removeChannelVersion(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.setChannelVersion(w, r, c, p, p["v"], false)
}

// --- keys ---

type keyJSON struct {
	ID             string `json:"id"`
	Fingerprint    string `json:"fingerprint"`
	State          string `json:"state"`
	SignerRef      string `json:"signer_ref,omitempty"` // server administrators only
	TrustedSince   string `json:"trusted_since,omitempty"`
	StateChangedAt string `json:"state_changed_at"`
	StateChangedBy string `json:"state_changed_by"`
	CreatedAt      string `json:"created_at"`
	CreatedBy      string `json:"created_by"`
	ETag           string `json:"etag"`
}

func keyOf(k store.Key, c caller) keyJSON {
	o := keyJSON{ID: k.ID, Fingerprint: k.Fingerprint, State: k.State, TrustedSince: timeOf(k.TrustedSince),
		StateChangedAt: timeOf(k.StateChangedAt), StateChangedBy: createdBy(k.StateChangedBy, c), CreatedAt: timeOf(k.CreatedAt),
		CreatedBy: createdBy(k.CreatedBy, c), ETag: versionTag(k.Version)}
	if c.admin {
		o.SignerRef = k.SignerRef
	}
	return o
}

func (h *Handler) channelKeys(w http.ResponseWriter, r *http.Request, c caller, p params) ([]store.Key, bool) {
	a, _ := c.actor()
	ks, err := h.o.Keys.List(r.Context(), a, p["t"], p["c"])
	if err != nil {
		h.serviceErr(w, err, false)
		return nil, false
	}
	return ks, true
}

// keyView is the channel's keys and its re-sign status: the serving and active keys, how many
// releases still lack the active key's signature, and when a re-signer last held the channel.
func (h *Handler) keyView(w http.ResponseWriter, r *http.Request, c caller, p params) {
	ks, ok := h.channelKeys(w, r, c, p)
	if !ok {
		return
	}
	ctx := r.Context()
	ch, err := h.o.Store.GetChannel(ctx, p["t"], p["c"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	out := map[string]any{}
	list := []keyJSON{}
	for _, k := range ks {
		list = append(list, keyOf(k, c))
		if k.ID == ch.ServingKeyID {
			out["serving_key"] = k.ID
		}
		if k.State == store.KeyActive {
			out["active_key"] = k.ID
			n, err := h.o.Store.CountUnsigned(ctx, ch.ID, k.ID)
			if err != nil {
				h.fail(w, err)
				return
			}
			out["unsigned_by_active"] = n
		}
	}
	out["keys"] = list
	holder, until, found, err := h.o.Store.LeaseState(ctx, "resign/"+ch.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if found {
		now := h.o.Now()
		last := until
		if last.After(now) {
			last = now
		}
		rs := map[string]any{"working": until.After(now), "last_held_at": timeOf(last)}
		if c.admin {
			rs["holder"] = holder
		}
		out["resigner"] = rs
	}
	reply(w, r, http.StatusOK, out, "", noStore)
}

func (h *Handler) getKey(w http.ResponseWriter, r *http.Request, c caller, p params) {
	ks, ok := h.channelKeys(w, r, c, p)
	if !ok {
		return
	}
	for _, k := range ks {
		if k.ID == p["id"] || k.Fingerprint == p["id"] { // as activate and retire take either
			reply(w, r, http.StatusOK, keyOf(k, c), versionTag(k.Version), noStore)
			return
		}
	}
	notFound(w)
}

type keyEventJSON struct {
	ID     string `json:"id"`
	KeyID  string `json:"key_id"`
	From   string `json:"from,omitempty"`
	To     string `json:"to"`
	Actor  string `json:"actor"`
	Forced bool   `json:"forced,omitempty"`
	At     string `json:"at"`
}

func (h *Handler) keyEvents(w http.ResponseWriter, r *http.Request, c caller, p params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	evs, err := h.o.Keys.Events(r.Context(), a, p["t"], p["c"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	key := func(e store.KeyEvent) string { return e.At.UTC().Format("20060102T150405.000000000") + "|" + e.ID }
	evs, next := paged(evs, key, limit, after)
	out := []keyEventJSON{}
	for _, e := range evs {
		out = append(out, keyEventJSON{ID: e.ID, KeyID: e.KeyID, From: e.From, To: e.To, Actor: createdBy(e.Actor, c),
			Forced: e.Forced, At: timeOf(e.At)})
	}
	reply(w, r, http.StatusOK, list("events", out, next), "", noStore)
}

func (h *Handler) addKey(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		SignerRef string `json:"signer_ref"`
		Active    bool   `json:"active"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	k, err := h.o.Keys.Add(r.Context(), a, p["t"], p["c"], in.SignerRef, in.Active)
	if err != nil {
		h.keyAddErr(w, err)
		return
	}
	created(w, r, "/api/v1/tenants/"+esc(p["t"])+"/channels/"+esc(p["c"])+"/keys/"+esc(k.ID), keyOf(k, c), versionTag(k.Version))
}

// keyAddErr: a signer that cannot be opened or refuses the key is the caller's (a server
// administrator's) reference, 400 (a source unreachable now too); the reason stays in the log.
func (h *Handler) keyAddErr(w http.ResponseWriter, err error) {
	if errors.Is(err, keys.ErrSigner) {
		h.o.Log.Warn("api: a signer reference was refused", "error", err)
		problem(w, http.StatusBadRequest, typeInvalid, "the signer reference could not be opened, or its key was refused; see the server log")
		return
	}
	h.channelErr(w, err, false)
}

func (h *Handler) keyState(w http.ResponseWriter, r *http.Request, c caller, p params, activate bool) {
	// force is server-only: decided before anything else, like the other server-only operations
	fv := r.URL.Query().Get("force")
	if fv != "" && fv != "false" && !c.admin {
		h.o.Log.Info("api: refused", "actor", c.logName(), "route", "keys/{id}/force", "t", p["t"], "c", p["c"], "id", p["id"],
			"status", http.StatusNotFound, "client", client(r))
		notFound(w)
		return
	}
	force := false
	switch fv {
	case "", "false":
	case "true":
		force = true
	default:
		problem(w, http.StatusBadRequest, typeInvalid, "force is true or false")
		return
	}
	expected, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	do := h.o.Keys.Retire
	if activate {
		do = h.o.Keys.Activate
	}
	k, err := do(r.Context(), a, p["t"], p["c"], p["id"], expected, force)
	if err != nil {
		h.channelErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, keyOf(k, c), versionTag(k.Version), noStore)
}

func (h *Handler) activateKey(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.keyState(w, r, c, p, true)
}

func (h *Handler) retireKey(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.keyState(w, r, c, p, false)
}

// --- releases ---

type releaseJSON struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Version        string `json:"version"`
	Platform       string `json:"platform"`
	Slot           string `json:"slot"`
	ABI            string `json:"abi"`
	DuckDBVersion  string `json:"build_duckdb_version,omitempty"`
	CAPI           string `json:"build_c_api,omitempty"`
	State          string `json:"state"`
	Visibility     string `json:"visibility"`
	Seq            int64  `json:"seq"` // 0: never current (served on the versioned path only)
	BodyHash       string `json:"body_hash"`
	CreatedAt      string `json:"created_at"`
	CreatedBy      string `json:"created_by"`
	StateChangedAt string `json:"state_changed_at,omitempty"`
	StateChangedBy string `json:"state_changed_by,omitempty"`
	ETag           string `json:"etag"`
}

func releaseOf(x store.Candidate, c caller) releaseJSON {
	o := releaseJSON{ID: x.ID, Name: x.Name, Version: x.ExtVersion, Platform: x.Platform, Slot: x.Slot, ABI: x.ABI,
		DuckDBVersion: x.DuckDBVersion, State: x.State, Visibility: x.Visibility, Seq: x.Seq, BodyHash: x.BodyHash,
		CreatedAt: timeOf(x.CreatedAt), CreatedBy: createdBy(x.CreatedBy, c), StateChangedAt: timeOf(x.StateChangedAt),
		StateChangedBy: createdBy(x.StateChangedBy, c), ETag: versionTag(x.Version)}
	if x.CAPI != nil {
		o.CAPI = x.CAPI.String()
	}
	return o
}

// releaseKey orders releases as (created_at, id).
func releaseKey(x store.Candidate) string {
	return x.CreatedAt.UTC().Format("20060102T150405.000000000") + "|" + x.ID
}

func (h *Handler) listReleases(w http.ResponseWriter, r *http.Request, c caller, p params, name string) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	state := r.URL.Query().Get("state")
	if state != "" && state != store.ReleaseActive && state != store.ReleaseDeprecated && state != store.ReleaseYanked {
		problem(w, http.StatusBadRequest, typeInvalid, "state is active, deprecated or yanked")
		return
	}
	a, _ := c.actor()
	rs, err := h.o.Releases.List(r.Context(), a, p["t"], p["c"], name)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	if state != "" {
		rs = slices.DeleteFunc(rs, func(x store.Candidate) bool { return x.State != state })
	}
	rs, next := paged(rs, releaseKey, limit, after) // oldest first: a cursor survives new releases
	out := []releaseJSON{}
	for _, x := range rs {
		out = append(out, releaseOf(x, c))
	}
	reply(w, r, http.StatusOK, list("releases", out, next), "", noStore)
}

func (h *Handler) channelReleases(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.listReleases(w, r, c, p, "")
}

func (h *Handler) extReleases(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.listReleases(w, r, c, p, p["ext"])
}

func (h *Handler) getRelease(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	rs, err := h.o.Releases.List(r.Context(), a, p["t"], p["c"], p["ext"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	for _, x := range rs {
		if x.ID == p["id"] {
			reply(w, r, http.StatusOK, releaseOf(x, c), versionTag(x.Version), noStore)
			return
		}
	}
	notFound(w)
}

var changes = map[string]release.Change{"yank": release.Yank, "deprecate": release.Deprecate, "activate": release.Activate,
	"current": release.MakeCurrent, "public": release.SetPublic, "private": release.SetPrivate}

func (h *Handler) changeRelease(w http.ResponseWriter, r *http.Request, c caller, p params) {
	ch, ok := changes[p["change"]]
	if !ok {
		notFound(w)
		return
	}
	expected, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	if _, err := h.o.Releases.Apply(r.Context(), a, p["t"], p["c"], p["ext"], p["id"], ch, expected); err != nil {
		h.channelErr(w, err, false)
		return
	}
	h.getRelease(w, r, c, p)
}
