package api

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// The feed (spec 0011 phase 1b): a tenant's attestation keys, the feed it serves to principals
// holding feed, and the feeds it subscribes to.

// --- attestation keys ---

type attestationKeyJSON struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
	PublicKey   string `json:"public_key"`
	ETag        string `json:"etag,omitempty"`
}

func attestationKeyOf(k store.Key, manage bool) attestationKeyJSON {
	o := attestationKeyJSON{ID: k.ID, Fingerprint: k.Fingerprint, State: k.State,
		PublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: k.PublicKey}))}
	if manage {
		o.ETag = versionTag(k.Version)
	}
	return o
}

// attestationKeys answers a tenant's published attestation keys: public by design, like
// .well-known, so subscribers can pin them.
func (h *Handler) attestationKeys(w http.ResponseWriter, r *http.Request, c caller, p params) {
	if h.o.Keys == nil {
		notFound(w)
		return
	}
	ks, err := h.o.Keys.PublicAttestation(r.Context(), p["t"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	out := []attestationKeyJSON{}
	for _, k := range ks {
		out = append(out, attestationKeyOf(k, false))
	}
	answer(w, r, map[string]any{"keys": out}, true)
}

func (h *Handler) addAttestationKey(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		SignerRef string `json:"signer_ref"`
		Active    bool   `json:"active"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	k, err := h.o.Keys.AddAttestation(r.Context(), a, p["t"], in.SignerRef, in.Active)
	if err != nil {
		h.keyAddErr(w, err)
		return
	}
	created(w, r, "/api/v1/tenants/"+esc(p["t"])+"/attestation-keys/"+esc(k.ID), attestationKeyOf(k, true), versionTag(k.Version))
}

func attestationKeyState(activate bool) handler {
	return func(h *Handler, w http.ResponseWriter, r *http.Request, c caller, p params) {
		fv := r.URL.Query().Get("force")
		if fv != "" && fv != "false" && !c.admin {
			notFound(w) // force is server-only
			return
		}
		if fv != "" && fv != "false" && fv != "true" {
			problem(w, http.StatusBadRequest, typeInvalid, "force is true or false")
			return
		}
		expected, ok := expectedVersion(w, r)
		if !ok {
			return
		}
		a, _ := c.actor()
		do := h.o.Keys.RetireAttestation
		if activate {
			do = h.o.Keys.ActivateAttestation
		}
		k, err := do(r.Context(), a, p["t"], p["id"], expected, fv == "true")
		if err != nil {
			h.channelErr(w, err, false)
			return
		}
		reply(w, r, http.StatusOK, attestationKeyOf(k, true), versionTag(k.Version), noStore)
	}
}

// --- the feed a tenant serves ---

// feed answers a subscriber's bodies with statements (not a management route: a feed credential
// may be long-lived).
func (h *Handler) feed(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Bodies []attachment.FeedBody `json:"bodies"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	ans, err := h.o.Attachments.Feed(r.Context(), a, p["t"], in.Bodies)
	if err != nil {
		h.attachmentErr(w, err)
		return
	}
	reply(w, r, http.StatusOK, ans, "-", noStore)
}

// feedDocument sends a document a statement names, from the fixed path the subscriber fetches.
func (h *Handler) feedDocument(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	err := h.o.Attachments.FeedDocument(r.Context(), a, p["t"], p["id"], func(x store.Attachment, b []byte) error {
		download(w, x, b)
		return nil
	})
	if err != nil {
		h.attachmentErr(w, err)
	}
}

// --- the feeds a tenant subscribes to ---

type feedJSON struct {
	Name          string          `json:"name"`
	URL           string          `json:"url"`
	Credential    string          `json:"credential"`
	Keys          []string        `json:"keys"`
	Kinds         []string        `json:"kinds"`
	Readers       bool            `json:"readers"`
	Offer         string          `json:"offer"`
	Channels      []string        `json:"channels,omitempty"`
	State         string          `json:"state"`
	Status        string          `json:"status"`
	LastError     string          `json:"last_error,omitempty"`
	LastSuccessAt string          `json:"last_success_at,omitempty"`
	LastRunAt     string          `json:"last_run_at,omitempty"`
	LastRun       json.RawMessage `json:"last_run,omitempty"`
	NextRunAt     string          `json:"next_run_at,omitempty"`
	RunRequested  bool            `json:"run_requested"`
	CreatedAt     string          `json:"created_at"`
	CreatedBy     string          `json:"created_by"`
	ETag          string          `json:"etag"`
}

func (h *Handler) feedOf(r *http.Request, f store.Feed, c caller) feedJSON {
	o := feedJSON{Name: f.Name, URL: f.URL, Credential: f.Credential, Keys: f.Keys, Kinds: f.Kinds, Readers: f.Readers, Offer: f.Offer,
		State: f.State, Status: f.Status, LastError: f.LastError, LastSuccessAt: timeOf(f.LastSuccessAt), LastRunAt: timeOf(f.LastRunAt),
		NextRunAt: timeOf(f.NextRunAt), RunRequested: !f.RequestedAt.IsZero(), CreatedAt: timeOf(f.CreatedAt),
		CreatedBy: createdBy(f.CreatedBy, c), ETag: versionTag(f.Version)}
	if o.Keys == nil {
		o.Keys = []string{}
	}
	if o.Kinds == nil {
		o.Kinds = []string{}
	}
	if f.LastRun != "" && json.Valid([]byte(f.LastRun)) {
		o.LastRun = json.RawMessage(f.LastRun)
	}
	if len(f.Channels) > 0 {
		chs, err := h.o.Store.ListChannels(r.Context(), c.tenant.Name)
		if err == nil {
			for _, ch := range chs {
				for _, id := range f.Channels {
					if ch.ID == id {
						o.Channels = append(o.Channels, ch.Name)
					}
				}
			}
		}
	}
	return o
}

// feedRoute answers 404 on a server without feeds (Options.Feeds nil).
func feedRoute(f handler) handler {
	return func(h *Handler, w http.ResponseWriter, r *http.Request, c caller, p params) {
		if h.o.Feeds == nil {
			notFound(w)
			return
		}
		f(h, w, r, c, p)
	}
}

func (h *Handler) listFeeds(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	fs, err := h.o.Feeds.List(r.Context(), a, p["t"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	out := []feedJSON{}
	for _, f := range fs {
		out = append(out, h.feedOf(r, f, c))
	}
	reply(w, r, http.StatusOK, map[string]any{"feeds": out}, "", noStore)
}

func (h *Handler) addFeed(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Name       string   `json:"name"`
		URL        string   `json:"url"`
		Credential string   `json:"credential"`
		Keys       []string `json:"keys"`
		Kinds      []string `json:"kinds"`
		Readers    bool     `json:"readers"`
		Offer      string   `json:"offer"`
		Channels   []string `json:"channels"`
	}
	if !body(w, r, &in) {
		return
	}
	a, _ := c.actor()
	f, err := h.o.Feeds.Add(r.Context(), a, p["t"], attachment.FeedSpec{Name: in.Name, URL: in.URL, Credential: in.Credential,
		Keys: in.Keys, Kinds: in.Kinds, Readers: in.Readers, Offer: in.Offer, Channels: in.Channels})
	if err != nil {
		h.serviceErr(w, err, true)
		return
	}
	created(w, r, "/api/v1/tenants/"+esc(p["t"])+"/feeds/"+esc(f.Name), h.feedOf(r, f, c), versionTag(f.Version))
}

func (h *Handler) getFeed(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	f, err := h.o.Feeds.Get(r.Context(), a, p["t"], p["name"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, h.feedOf(r, f, c), versionTag(f.Version), noStore)
}

func (h *Handler) changeFeed(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in attachment.FeedChange
	if !body(w, r, &in) {
		return
	}
	expected, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	f, err := h.o.Feeds.Change(r.Context(), a, p["t"], p["name"], in, expected)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, h.feedOf(r, f, c), versionTag(f.Version), noStore)
}

func (h *Handler) syncFeed(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	f, err := h.o.Feeds.Sync(r.Context(), a, p["t"], p["name"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusAccepted, h.feedOf(r, f, c), versionTag(f.Version), noStore)
}

func (h *Handler) removeFeed(w http.ResponseWriter, r *http.Request, c caller, p params) {
	docs := r.URL.Query().Get("documents")
	if docs != "" && docs != "true" && docs != "false" {
		problem(w, http.StatusBadRequest, typeInvalid, "documents is true or false")
		return
	}
	expected, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	err := h.o.Feeds.Remove(r.Context(), a, p["t"], p["name"], docs == "true", expected)
	if errors.Is(err, store.ErrConflict) {
		problem(w, http.StatusPreconditionFailed, typePrecondition, "it changed: read it again")
		return
	}
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	noContent(w)
}
