package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0033 phase A2: holds on an eager mirror, a publisher and a channel (PUT {"hold": "24h"}).

// holdOf shows a hold as answers do: a Go duration ("24h0m0s"), "" for none.
func holdOf(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}

// readHold reads a hold's body, {"hold": "<duration>"} ("0s" or "0" for none), or answers 400.
func readHold(w http.ResponseWriter, r *http.Request) (time.Duration, bool) {
	var in struct {
		Hold *string `json:"hold"`
	}
	if !body(w, r, &in, maxBody) {
		return 0, false
	}
	if in.Hold == nil {
		problem(w, http.StatusBadRequest, typeInvalid, `hold is required: a duration such as "24h", "0s" for none`)
		return 0, false
	}
	d, err := time.ParseDuration(strings.TrimSpace(*in.Hold))
	if err != nil {
		problem(w, http.StatusBadRequest, typeInvalid, `hold is a duration such as "24h", "0s" for none`)
		return 0, false
	}
	if err := store.ValidHold(d); err != nil {
		problem(w, http.StatusBadRequest, typeInvalid, publicMessage(err))
		return 0, false
	}
	return d, true
}

// optionalVersion reads an optional If-Match of a versioned resource (0: none, any version).
func optionalVersion(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if len(r.Header.Values("If-Match")) == 0 {
		return 0, true
	}
	return expectedVersion(w, r)
}

// setUpstreamHold sets an eager mirror's hold: PUT …/upstreams/{name}/hold {"hold": "24h"}, If-Match
// optional; the answer is the upstream.
func (h *Handler) setUpstreamHold(w http.ResponseWriter, r *http.Request, c caller, p params) {
	expected, ok := optionalVersion(w, r)
	if !ok {
		return
	}
	d, ok := readHold(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	u, err := h.o.Upstreams.SetHold(r.Context(), a, p["t"], p["name"], d, expected)
	if err != nil {
		h.upstreamErr(w, err)
		return
	}
	h.replyUpstream(w, r, c, p, u, http.StatusOK)
}

// setPublisherHold sets a publisher's hold: PUT …/publishers/{name}/hold {"hold": "24h"}; the answer
// is the publisher.
func (h *Handler) setPublisherHold(w http.ResponseWriter, r *http.Request, c caller, p params) {
	d, ok := readHold(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	pb, err := h.o.Auth.SetPublisherHold(r.Context(), a, p["t"], p["name"], d)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	keys, err := h.o.Auth.ListPublisherKeys(r.Context(), a, p["t"], pb.Name)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, publisherOf(pb, keys, c), "", noStore)
}

// distributorHold reads the tenant's minimum hold for distributors' publishers (spec 0037): GET
// …/distributor-hold.
func (h *Handler) distributorHold(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	d, err := h.o.Auth.DistributorHold(r.Context(), a, p["t"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, map[string]any{"hold": d.String()}, "", noStore)
}

// setDistributorHold sets the tenant's minimum hold for distributors' publishers (spec 0037): PUT
// …/distributor-hold {"hold": "24h"}; the answer is the hold.
func (h *Handler) setDistributorHold(w http.ResponseWriter, r *http.Request, c caller, p params) {
	d, ok := readHold(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	if _, err := h.o.Auth.SetDistributorHold(r.Context(), a, p["t"], d); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, map[string]any{"hold": d.String()}, "", noStore)
}

// channelHold reads a channel's hold: GET …/channels/{c}/hold.
func (h *Handler) channelHold(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	d, err := h.o.Releases.ChannelHold(r.Context(), a, p["t"], p["c"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, map[string]any{"channel": p["c"], "hold": d.String()}, "", noStore)
}

// setChannelHold sets a channel's hold: PUT …/channels/{c}/hold {"hold": "24h"}.
func (h *Handler) setChannelHold(w http.ResponseWriter, r *http.Request, c caller, p params) {
	d, ok := readHold(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	if _, err := h.o.Releases.SetChannelHold(r.Context(), a, p["t"], p["c"], d); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, map[string]any{"channel": p["c"], "hold": d.String()}, "", noStore)
}

// purgeHeldParam reads a key removal's purge_held (true or false, once; absent: false).
func purgeHeldParam(w http.ResponseWriter, r *http.Request) (bool, bool) {
	q := r.URL.Query()
	for k, vs := range q {
		if k != "purge_held" || len(vs) != 1 {
			problem(w, http.StatusBadRequest, typeInvalid, "the parameter is purge_held, once")
			return false, false
		}
	}
	switch q.Get("purge_held") {
	case "", "false":
		return false, true
	case "true":
		return true, true
	}
	problem(w, http.StatusBadRequest, typeInvalid, "purge_held is true or false")
	return false, false
}
