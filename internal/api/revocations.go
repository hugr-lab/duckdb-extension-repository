package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0039: an upstream's revocations (tenant administrators).

type revocationJSON struct {
	Build        string `json:"build"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Platform     string `json:"platform"`
	BodyHash     string `json:"body_hash"`
	State        string `json:"state"`  // the remote's, as last seen
	Reason       string `json:"reason"` // the remote's
	SeenAt       string `json:"seen_at"`
	AppliedState string `json:"applied_state,omitempty"`
	AppliedAt    string `json:"applied_at,omitempty"`
	LiftedState  string `json:"lifted_state,omitempty"`
	LiftedAt     string `json:"lifted_at,omitempty"`
	// UnlistedAt: the remote no longer lists it (or its build went); kept for what was applied or
	// lifted of it, and no longer applied
	UnlistedAt string `json:"unlisted_at,omitempty"`
	// Status is applied (something applied is left to lift), lifted, or waiting: stored and nothing
	// applied (the switch off, or due at the next run)
	Status string `json:"status"`
}

func revocationOf(x store.UpstreamRevocation) revocationJSON {
	o := revocationJSON{Build: x.BuildID, Name: x.Name, Version: x.PackageVersion, Platform: x.Platform, BodyHash: x.BodyHash, State: x.State,
		Reason: x.Reason, SeenAt: timeOf(x.SeenAt), AppliedState: x.AppliedState, AppliedAt: timeOf(x.AppliedAt), LiftedState: x.LiftedState,
		LiftedAt: timeOf(x.LiftedAt), UnlistedAt: timeOf(x.UnlistedAt), Status: "waiting"}
	switch {
	case x.Liftable():
		o.Status = "applied"
	case x.LiftedState != "":
		o.Status = "lifted"
	}
	return o
}

// upstreamRevocations answers an upstream's switch and stored revocations: GET
// …/upstreams/{name}/revocations.
func (h *Handler) upstreamRevocations(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	u, err := h.o.Upstreams.Get(r.Context(), a, p["t"], p["name"])
	if err != nil {
		h.upstreamErr(w, err)
		return
	}
	rs, err := h.o.Upstreams.RevocationsOf(r.Context(), a, p["t"], p["name"])
	if err != nil {
		h.upstreamErr(w, err)
		return
	}
	out := []revocationJSON{}
	for _, x := range rs {
		out = append(out, revocationOf(x))
	}
	// the ETag is the answer's content: a run storing, applying or forgetting a row moves it, though
	// the upstream's version stays (If-Match on the switch and a lift takes the upstream's, from its
	// record)
	body := map[string]any{"remote_kista": u.RemoteKista, "revocations": u.Revocations, "stored": out}
	b, _ := json.Marshal(body)
	sum := sha256.Sum256(b)
	reply(w, r, http.StatusOK, body, `"`+hex.EncodeToString(sum[:16])+`"`, noStore)
}

// setRevocations switches applying the remote's revocations: POST …/upstreams/{name}/revocations
// {"revocations": true|false}, If-Match.
func (h *Handler) setRevocations(w http.ResponseWriter, r *http.Request, c caller, p params) {
	expected, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	var in struct {
		Revocations *bool `json:"revocations"`
	}
	if !body(w, r, &in, maxBody) {
		return
	}
	if in.Revocations == nil {
		problem(w, http.StatusBadRequest, typeInvalid, "revocations is true or false")
		return
	}
	a, _ := c.actor()
	u, err := h.o.Upstreams.SetRevocations(r.Context(), a, p["t"], p["name"], *in.Revocations, expected)
	if err != nil {
		h.upstreamErr(w, err)
		return
	}
	h.replyUpstream(w, r, c, p, u, http.StatusOK)
}

// liftRevocation lifts an upstream's revocation of a build: DELETE
// …/upstreams/{name}/revocations/{build}, If-Match; it answers how many releases it restored.
func (h *Handler) liftRevocation(w http.ResponseWriter, r *http.Request, c caller, p params) {
	expected, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	n, err := h.o.Upstreams.LiftRevocation(r.Context(), a, p["t"], p["name"], p["build"], expected)
	if err != nil {
		h.upstreamErr(w, err)
		return
	}
	reply(w, r, http.StatusOK, map[string]any{"build": p["build"], "restored": n}, "", noStore)
}
