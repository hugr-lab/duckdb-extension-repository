package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0010: reading events (the audit verb on the tenant, or a server administrator).

type eventJSON struct {
	ID        string          `json:"id"`
	At        string          `json:"at"`
	Kind      string          `json:"kind"`
	V         int             `json:"v"`
	Outcome   string          `json:"outcome"`
	Actor     string          `json:"actor"`
	ActorName string          `json:"actor_name,omitempty"`
	Subject   string          `json:"subject"`
	Data      json.RawMessage `json:"data"`
	Client    string          `json:"client,omitempty"`
	Request   string          `json:"request"`
}

// eventOf is an event for its reader: a server administrator's or the server host's own identity
// shows as "server" to a tenant's readers, without name or address (as createdBy).
func eventOf(e store.Event, c caller) eventJSON {
	data := json.RawMessage(e.Data)
	if !json.Valid(data) {
		data = json.RawMessage("{}")
	}
	o := eventJSON{ID: e.ID, At: e.At.UTC().Format(time.RFC3339Nano), Kind: e.Kind, V: e.V, Outcome: e.Outcome, Actor: e.Actor,
		ActorName: e.ActorName, Subject: e.Subject, Data: data, Client: e.Client, Request: e.Request}
	if masked := createdBy(e.Actor, c); masked != e.Actor {
		o.Actor, o.ActorName, o.Client = masked, "", ""
	}
	return o
}

// eventFilter reads the listing's query: since, until (RFC 3339), kind (spec 0040: a list), actor,
// subject (a prefix), outcome, order (desc, asc), cursor, limit.
func eventFilter(w http.ResponseWriter, r *http.Request) (store.EventFilter, bool) {
	limit, after, ok := page(w, r)
	if !ok {
		return store.EventFilter{}, false
	}
	q := r.URL.Query()
	f := store.EventFilter{Actor: q.Get("actor"), Subject: q.Get("subject"), Outcome: q.Get("outcome"), Limit: limit}
	if v := q.Get("kind"); v != "" {
		kinds, not, msg := kindList(v)
		if msg != "" {
			problem(w, http.StatusBadRequest, typeInvalid, msg)
			return f, false
		}
		f.Kinds, f.NotKinds = kinds, not
	}
	for _, t := range []struct {
		name string
		dst  *time.Time
	}{{"since", &f.Since}, {"until", &f.Until}} {
		if v := q.Get(t.name); v != "" {
			at, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				problem(w, http.StatusBadRequest, typeInvalid, t.name+" is an RFC 3339 time")
				return f, false
			}
			*t.dst = at
		}
	}
	switch q.Get("order") {
	case "", "desc":
	case "asc":
		f.Ascending = true
	default:
		problem(w, http.StatusBadRequest, typeInvalid, "order is asc or desc")
		return f, false
	}
	if after != "" {
		at, id, found := strings.Cut(after, "|")
		t, err := time.Parse(time.RFC3339Nano, at)
		if !found || err != nil {
			problem(w, http.StatusBadRequest, typeInvalid, "the cursor is not one this server returned")
			return f, false
		}
		f.AfterAt, f.AfterID = t, id
	}
	return f, true
}

// kindList reads kind (spec 0040): comma-separated kinds of the catalogue or families of them
// (grant.*), none twice, or after a leading ! those left out. It answers the catalogue's kinds they
// name (a family's, expanded: an exact match never depends on the database's collation), or why
// the list is refused.
func kindList(v string) (kinds []string, not bool, msg string) {
	v, not = strings.CutPrefix(v, "!")
	entries, seen, named := strings.Split(v, ","), map[string]bool{}, map[string]bool{}
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e, "!"):
			return nil, false, "kind is a list of kinds, or a list after ! of kinds left out, not both"
		case seen[e]:
			return nil, false, "kind names " + e + " twice"
		}
		seen[e] = true
		if family, ok := strings.CutSuffix(e, "*"); ok && strings.HasSuffix(family, ".") {
			found := false
			for _, k := range audit.Kinds() {
				if k := string(k.Kind); strings.HasPrefix(k, family) {
					if found = true; !named[k] {
						named[k], kinds = true, append(kinds, k)
					}
				}
			}
			if !found {
				return nil, false, "kind: no event kind is of the family " + e + " (see /api/v1/event-kinds)"
			}
			continue
		}
		if !audit.Known(audit.Kind(e)) {
			return nil, false, "kind: " + e + " is not an event kind (see /api/v1/event-kinds)"
		}
		if !named[e] {
			named[e], kinds = true, append(kinds, e)
		}
	}
	return kinds, not, ""
}

// eventCursor is the opaque cursor of a listing: the last event's (at, id), or a time covered with
// no id (spec 0040: the next read starts at it).
func eventCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func (h *Handler) listEventsOf(w http.ResponseWriter, r *http.Request, c caller, tenantID string) {
	f, ok := eventFilter(w, r)
	if !ok {
		return
	}
	// spec 0040: an ascending read stops events.settle behind now (an event can commit after a later
	// one is readable), and covers the time up to there
	var covered time.Time
	if f.Ascending {
		covered = h.o.Now().Add(-h.o.EventSettle)
		if !f.Until.IsZero() && f.Until.Before(covered) {
			covered = f.Until
		}
		f.Until = covered
	}
	limit := f.Limit
	f.Limit = limit + 1
	evs, err := h.o.Store.ListEvents(r.Context(), tenantID, f)
	if err != nil {
		h.fail(w, err)
		return
	}
	next := ""
	switch {
	case len(evs) > limit: // a full page
		evs = evs[:limit]
		l := evs[len(evs)-1]
		next = eventCursor(l.At, l.ID)
	case f.Ascending && f.AfterAt.After(covered): // a cursor past the covered time keeps its place
		next = eventCursor(f.AfterAt, f.AfterID)
	case f.Ascending: // the tail: whatever the filter matched, the time covered
		next = eventCursor(covered, "")
	}
	oldest, held, err := h.o.Store.OldestEvent(r.Context(), tenantID)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := []eventJSON{}
	for _, e := range evs {
		out = append(out, eventOf(e, c))
	}
	ans := list("events", out, next)
	if held {
		ans["oldest_at"] = oldest.UTC().Format(time.RFC3339Nano)
	}
	reply(w, r, http.StatusOK, ans, "", noStore)
}

func (h *Handler) getEventOf(w http.ResponseWriter, r *http.Request, c caller, tenantID, id string) {
	e, err := h.o.Store.GetEvent(r.Context(), tenantID, id)
	if errors.Is(err, store.ErrNotFound) {
		notFound(w)
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	reply(w, r, http.StatusOK, eventOf(e, c), "", noStore)
}

func (h *Handler) tenantEvents(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.listEventsOf(w, r, c, c.tenant.ID)
}

func (h *Handler) tenantEvent(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.getEventOf(w, r, c, c.tenant.ID, p["id"])
}

func (h *Handler) serverEvents(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.listEventsOf(w, r, c, audit.ServerTenant)
}

func (h *Handler) serverEvent(w http.ResponseWriter, r *http.Request, c caller, p params) {
	h.getEventOf(w, r, c, audit.ServerTenant, p["id"])
}

// eventKinds is the catalogue: each kind with its data fields.
func (h *Handler) eventKinds(w http.ResponseWriter, r *http.Request, c caller, p params) {
	reply(w, r, http.StatusOK, map[string]any{"version": audit.Version, "kinds": audit.Kinds()}, "", "public, max-age=3600")
}
