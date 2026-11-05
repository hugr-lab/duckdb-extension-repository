package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0042: version retention in channels: the rules, the plan, pins and the line act.

// ruleIn is a PUT's rule: the whole rule, a field left out unset.
type ruleIn struct {
	Keep  json.RawMessage `json:"keep"`
	Grace *string         `json:"grace"`
}

// readRule reads a PUT's rule; at a channel "none" is no retention there (stored 0), at a tenant the
// same as leaving keep out.
func readRule(w http.ResponseWriter, r *http.Request, channel bool) (store.RetentionRule, string, bool) {
	expected := ""
	if r.Header.Get("If-Match") != "" {
		var ok bool
		if expected, ok = ifMatch(w, r); !ok {
			return store.RetentionRule{}, "", false
		}
	}
	var in ruleIn
	if !body(w, r, &in, maxBody) {
		return store.RetentionRule{}, "", false
	}
	var out store.RetentionRule
	switch k := strings.TrimSpace(string(in.Keep)); k {
	case "", "null":
	case `"none"`:
		if channel {
			none := 0
			out.Keep = &none
		}
	default:
		n, err := strconv.ParseInt(k, 10, 64)
		if err != nil || n < 1 || n > store.MaxRetentionKeep {
			problem(w, http.StatusBadRequest, typeInvalid, fmt.Sprintf(`keep is a whole number from 1 to %d, or "none"`, store.MaxRetentionKeep))
			return store.RetentionRule{}, "", false
		}
		v := int(n)
		out.Keep = &v
	}
	if in.Grace != nil {
		d, err := time.ParseDuration(strings.TrimSpace(*in.Grace))
		if err != nil {
			problem(w, http.StatusBadRequest, typeInvalid, `grace is a duration such as "720h", "0s" to purge at the next pass`)
			return store.RetentionRule{}, "", false
		}
		out.Grace = &d
	}
	if err := store.ValidRetention(out, channel); err != nil {
		problem(w, http.StatusBadRequest, typeInvalid, publicMessage(err))
		return store.RetentionRule{}, "", false
	}
	return out, expected, true
}

// ruleOut is a stored rule as answers show it: keep (a number, "none" at a channel, null not set)
// and grace (null not set).
func ruleOut(r store.RetentionRule) map[string]any {
	out := map[string]any{"keep": nil, "grace": nil}
	for k, v := range release.RuleFields(r) {
		out[k] = v
	}
	return out
}

func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func (h *Handler) tenantRetention(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	rule, err := h.o.Releases.TenantRetention(r.Context(), a, p["t"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	out := ruleOut(rule)
	out["etag"] = release.RetentionTag(rule) // a PUT's If-Match: the stored rule's
	reply(w, r, http.StatusOK, out, "", noStore)
}

func (h *Handler) setTenantRetention(w http.ResponseWriter, r *http.Request, c caller, p params) {
	rule, expected, ok := readRule(w, r, false)
	if !ok {
		return
	}
	a, _ := c.actor()
	if _, err := h.o.Releases.SetTenantRetention(r.Context(), a, p["t"], rule, expected); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	h.tenantRetention(w, r, c, p)
}

func channelRetentionOut(channel string, v release.ChannelRetention) map[string]any {
	out := ruleOut(v.Rule)
	out["channel"], out["in_force"] = channel, release.InForceFields(v.InForce)
	out["checked_at"], out["next_run"] = timeOrNil(v.CheckedAt), timeOrNil(v.NextRun)
	out["etag"] = release.RetentionTag(v.Rule) // a PUT's If-Match: the stored rule's
	return out
}

func (h *Handler) channelRetention(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	v, err := h.o.Releases.ChannelRetention(r.Context(), a, p["t"], p["c"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	// the answer's ETag is over its bytes, so a change of what is in force (the tenant's default, the
	// pass's look) is never a 304; a PUT's If-Match is the body's etag, the stored rule's
	reply(w, r, http.StatusOK, channelRetentionOut(p["c"], v), "", noStore)
}

func (h *Handler) setChannelRetention(w http.ResponseWriter, r *http.Request, c caller, p params) {
	rule, expected, ok := readRule(w, r, true)
	if !ok {
		return
	}
	a, _ := c.actor()
	v, err := h.o.Releases.SetChannelRetention(r.Context(), a, p["t"], p["c"], rule, expected)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	reply(w, r, http.StatusOK, channelRetentionOut(p["c"], v), "", noStore)
}

// --- the plan ---

func planReleaseOut(x release.PlanRelease) map[string]any {
	out := map[string]any{"id": x.ID, "platform": x.Platform, "slot": x.Slot, "body_hash": x.BodyHash, "bytes": x.Bytes,
		"shared": x.Shared, "shared_by": nonNil(x.SharedBy), "others": x.Others}
	if x.Lines != nil {
		out["lines"] = x.Lines
	}
	return out
}

func planRetiredOut(x release.PlanRetired) map[string]any {
	return map[string]any{"source": x.Source, "name": x.Name, "version": x.Version, "release": planReleaseOut(x.Release),
		"retired_at": timeOrNil(x.RetiredAt), "purge_at": timeOrNil(x.PurgeAt), "pinned": x.Pinned}
}

func (h *Handler) retentionPlan(w http.ResponseWriter, r *http.Request, c caller, p params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	plan, err := h.o.Releases.RetentionPlan(r.Context(), a, p["t"], p["c"], after, limit)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	retire := []map[string]any{}
	for _, x := range plan.Retire {
		rels, kept := []map[string]any{}, []map[string]any{}
		for _, y := range x.Releases {
			rels = append(rels, planReleaseOut(y))
		}
		for _, y := range x.KeptBy {
			kept = append(kept, planReleaseOut(y))
		}
		retire = append(retire, map[string]any{"source": x.Source, "name": x.Name, "version": x.Version, "lines": nonNil(x.Lines),
			"platforms": nonNil(x.Platforms), "releases": rels, "bytes": x.Bytes, "kept": x.Kept, "kept_by": kept})
	}
	purge, retired := []map[string]any{}, []map[string]any{}
	for _, x := range plan.Purge {
		purge = append(purge, planRetiredOut(x))
	}
	for _, x := range plan.Retired {
		retired = append(retired, planRetiredOut(x))
	}
	out := map[string]any{"channel": p["c"], "in_force": release.InForceFields(plan.InForce), "next_run": timeOrNil(plan.NextRun),
		"purge_off": plan.PurgeOff, "retire": retire, "purge": purge, "retired": retired}
	if plan.Next != "" {
		out["next"] = base64.RawURLEncoding.EncodeToString([]byte(plan.Next))
	}
	reply(w, r, http.StatusOK, out, "", noStore)
}

// --- pins ---

func pinOut(x store.RetentionPin, c caller) map[string]any {
	out := map[string]any{"id": x.ID, "name": x.Name, "version": x.PackageVersion, "reason": x.Reason,
		"pinned_by": createdBy(x.PinnedBy, c), "pinned_at": timeOf(x.PinnedAt)}
	if x.DuckDBVersion != "" {
		out["duckdb_version"] = x.DuckDBVersion
	}
	return out
}

func (h *Handler) listPins(w http.ResponseWriter, r *http.Request, c caller, p params) {
	limit, after, ok := page(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	pins, err := h.o.Releases.Pins(r.Context(), a, p["t"], p["c"], r.URL.Query().Get("name"), after, limit+1)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	next := ""
	if len(pins) > limit {
		pins, next = pins[:limit], base64.RawURLEncoding.EncodeToString([]byte(store.PinKey(pins[limit-1])))
	}
	out := []map[string]any{}
	for _, x := range pins {
		out = append(out, pinOut(x, c))
	}
	reply(w, r, http.StatusOK, list("pins", out, next), "", noStore)
}

func (h *Handler) addPin(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Name          string `json:"name"`
		Version       string `json:"version"`
		DuckDBVersion string `json:"duckdb_version"`
		Reason        string `json:"reason"`
	}
	if !body(w, r, &in, maxBody) {
		return
	}
	a, _ := c.actor()
	pin, existed, err := h.o.Releases.Pin(r.Context(), a, p["t"], p["c"],
		store.RetentionPin{Name: in.Name, PackageVersion: in.Version, DuckDBVersion: in.DuckDBVersion, Reason: in.Reason})
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	if existed {
		reply(w, r, http.StatusOK, pinOut(pin, c), "-", noStore)
		return
	}
	created(w, r, "/api/v1/tenants/"+p["t"]+"/channels/"+p["c"]+"/retention/pins/"+pin.ID, pinOut(pin, c), "-")
}

func (h *Handler) removePin(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Releases.Unpin(r.Context(), a, p["t"], p["c"], p["id"]); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	noContent(w)
}

// --- the line act ---

func (h *Handler) retireLine(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		DuckDBVersion string `json:"duckdb_version"`
		Name          string `json:"name"`
	}
	if !body(w, r, &in, maxBody) {
		return
	}
	if in.DuckDBVersion == "" {
		problem(w, http.StatusBadRequest, typeInvalid, "duckdb_version is required")
		return
	}
	a, _ := c.actor()
	res, err := h.o.Releases.RetireLine(r.Context(), a, p["t"], p["c"], in.DuckDBVersion, in.Name)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	rel := func(xs []release.LineRelease) []map[string]any {
		out := []map[string]any{}
		for _, x := range xs {
			m := map[string]any{"id": x.ID, "name": x.Name, "version": x.Version, "platform": x.Platform}
			if x.Lines != nil {
				m["lines"] = x.Lines
			}
			out = append(out, m)
		}
		return out
	}
	reply(w, r, http.StatusOK, map[string]any{"duckdb_version": in.DuckDBVersion, "retired": rel(res.Retired), "kept": rel(res.Kept),
		"withdrawn": res.Withdrawn, "pins_removed": res.PinsRemoved}, "-", noStore)
}
