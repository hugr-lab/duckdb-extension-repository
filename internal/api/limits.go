package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0038: a tenant's limits.

// limitsOut is a tenant's limits as answers show them: for each, what is set ("default",
// "unlimited" or a number), what applies (a number, or null for unlimited bytes) and what is used.
func limitsOut(l tenants.Limits) map[string]any {
	out := map[string]any{}
	for _, n := range store.LimitNames {
		out[n] = map[string]any{"set": tenants.LimitValue(l.Set.Get(n)), "limit": tenants.LimitInForce(l.Limit.Get(n)), "used": l.Used.Get(n)}
	}
	return map[string]any{"limits": out}
}

func (h *Handler) tenantLimits(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	t, l, err := h.o.Tenants.TenantLimits(r.Context(), a, p["t"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	limitsReply(w, r, t, l)
}

// limitsReply answers a tenant's limits with the ETag of its stored values, for a PUT's If-Match;
// never 304 to If-None-Match, since what is used and in force change without them.
func limitsReply(w http.ResponseWriter, r *http.Request, t store.Tenant, l tenants.Limits) {
	w.Header().Set("ETag", tenants.LimitsTag(t.Limits))
	reply(w, r, http.StatusOK, limitsOut(l), "-", noStore)
}

// setTenantLimits sets the limits the body names, the others unchanged; If-Match, when sent, is the
// GET's ETag.
func (h *Handler) setTenantLimits(w http.ResponseWriter, r *http.Request, c caller, p params) {
	expected := ""
	if r.Header.Get("If-Match") != "" {
		var ok bool
		if expected, ok = ifMatch(w, r); !ok {
			return
		}
	}
	var in map[string]json.RawMessage
	if !body(w, r, &in, maxBody) {
		return
	}
	set, err := parseLimits(in)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	a, _ := c.actor()
	t, l, err := h.o.Tenants.SetTenantLimits(r.Context(), a, p["t"], set, expected)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	limitsReply(w, r, t, l)
}

// parseLimits reads a limits object: each value a number, "unlimited", "default", or for bytes a
// size ("50GiB").
func parseLimits(in map[string]json.RawMessage) (map[string]int64, error) {
	set := map[string]int64{}
	for n, raw := range in {
		if !slices.Contains(store.LimitNames, n) {
			return nil, fmt.Errorf("%w: unknown limit %q (%s)", store.ErrInvalid, n, strings.Join(store.LimitNames, ", "))
		}
		s := string(raw) // a number as written; null, true or a fraction is no limit's value
		if len(raw) > 0 && raw[0] == '"' {
			if err := json.Unmarshal(raw, &s); err != nil {
				return nil, fmt.Errorf("%w: %s is a number or a string", store.ErrInvalid, n)
			}
		}
		v, err := store.ParseLimit(n, s)
		if err != nil {
			return nil, err
		}
		set[n] = v
	}
	return set, nil
}

// limitProblem answers a refusal by a tenant limit (spec 0038): 429 tenant-limit naming the limit,
// without Retry-After (it clears as the tenant frees room or its limit is raised) and without
// figures. It reports whether err was one.
func limitProblem(w http.ResponseWriter, err error) bool {
	var le *store.LimitError
	if !errors.As(err, &le) {
		return false
	}
	writeProblemWith(w, http.StatusTooManyRequests, typeTenantLimit, le.Error(), true, map[string]any{"limit": le.Limit})
	return true
}
