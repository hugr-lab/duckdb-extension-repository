package api

import (
	"net/http"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0043: the channels' order, a strict chain per package type, for the console's display only.

// orderReply answers an order with its ETag, a PUT's If-Match.
func orderReply(w http.ResponseWriter, r *http.Request, edges []store.OrderEdge) {
	if edges == nil {
		edges = []store.OrderEdge{}
	}
	reply(w, r, http.StatusOK, map[string]any{"order": edges}, tenants.OrderTag(edges), noStore)
}

// channelOrder reads the order of the tenant's channels: GET …/channel-order[?type=].
func (h *Handler) channelOrder(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	edges, err := h.o.Tenants.ChannelOrder(r.Context(), a, p["t"], reqType(r))
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	orderReply(w, r, edges)
}

// setChannelOrder replaces the order of the tenant's channels of a type with the body's, the whole
// order ({"order": [{"channel", "next"}, …]}; an empty list clears it): PUT …/channel-order[?type=],
// If-Match optional (412 when it no longer matches).
func (h *Handler) setChannelOrder(w http.ResponseWriter, r *http.Request, c caller, p params) {
	expected := ""
	if r.Header.Get("If-Match") != "" {
		var ok bool
		if expected, ok = ifMatch(w, r); !ok {
			return
		}
	}
	var in struct {
		Order []store.OrderEdge `json:"order"`
	}
	if !body(w, r, &in, maxBody) {
		return
	}
	a, _ := c.actor()
	edges, err := h.o.Tenants.SetChannelOrder(r.Context(), a, p["t"], reqType(r), in.Order, expected)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	orderReply(w, r, edges)
}
