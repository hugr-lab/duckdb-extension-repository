package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/stats"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// distributorRowJSON is one week of one extension of a distributor's statistics.
type distributorRowJSON struct {
	Week          string `json:"week"`
	Extension     string `json:"extension"`
	Downloads     int64  `json:"downloads"`
	Public        int64  `json:"public"`
	InstallerDays *int64 `json:"installer_days,omitempty"`
	Withheld      bool   `json:"withheld"`
	MinTenants    int    `json:"min_tenants"` // the minimum the week was computed with
}

// distributorStats answers GET distributor-stats/downloads?distributor=&from=&to=[&type=] (spec 0036
// phase 2): a distributor's downloads per extension per complete week, summed over the tenants,
// for distribution admins and server administrators. It reads the weeks' snapshots, each computed
// once by the daily pass when the week was complete (public tenants and the minimum applied then),
// so an answered week never changes. It answers only weeks computed: to is clamped to the newest,
// from to the oldest kept, and before any week is computed the answer is 400. No answer names a
// tenant or a channel.
func (h *Handler) distributorStats(w http.ResponseWriter, r *http.Request, c caller, _ params) {
	q := r.URL.Query()
	distributor := q.Get("distributor")
	if distributor == "" || len(distributor) > store.MaxDistributor || len(q["distributor"]) != 1 {
		problem(w, http.StatusBadRequest, typeInvalid, "distributor names one distributor, as its publishers are marked (at most 128 bytes)")
		return
	}
	oldest, newest, ok, err := h.o.Store.DistributorWeeksSpan(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	if !ok {
		problem(w, http.StatusBadRequest, typeInvalid, "no week is computed yet (a week is computed two days after it ends)")
		return
	}
	lo, err1 := time.Parse(time.DateOnly, oldest)
	hi, err2 := time.Parse(time.DateOnly, newest)
	if err := errors.Join(err1, err2); err != nil {
		h.fail(w, err)
		return
	}
	if last := stats.LastCompleteWeek(h.o.Store.Now()); hi.After(last) { // never a week not complete (a clock set back)
		hi = last
	}
	weeks, err := stats.DistributorWeeks(lo, hi, q.Get("from"), q.Get("to"))
	var we *stats.WeeksError
	if errors.As(err, &we) {
		problem(w, http.StatusBadRequest, typeInvalid, we.Reason)
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	from, to := weeks[0], weeks[len(weeks)-1]
	snaps, err := h.o.Store.DistributorWeeks(r.Context(), distributor, reqType(r), from, to)
	if err != nil {
		h.fail(w, err)
		return
	}
	rows := make([]distributorRowJSON, 0, len(snaps))
	for _, s := range snaps {
		rows = append(rows, distributorRowJSON{Week: s.Week, Extension: s.Extension, Downloads: s.Downloads, Public: s.Public,
			InstallerDays: s.InstallerDays, Withheld: s.Withheld, MinTenants: s.MinTenants})
	}
	reply(w, r, http.StatusOK, map[string]any{"distributor": distributor, "from": from, "to": to, "rows": rows}, "", noStore)
}
