package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/stats"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// distributorStats answers GET distributor-stats/downloads?distributor=&from=&to=[&type=] (spec 0036
// phase 2): a distributor's downloads per extension per complete week, summed over every tenant (a
// deleting one left out), for distribution admins and server administrators. No answer names a
// tenant or a channel. Public tenants (statistics.public_tenants, matched by name now) always count;
// every other tenant's downloads, and on their own its installer-days, count only in a row with at
// least the minimum of such tenants.
func (h *Handler) distributorStats(w http.ResponseWriter, r *http.Request, c caller, _ params) {
	q := r.URL.Query()
	distributor := q.Get("distributor")
	if distributor == "" || len(distributor) > store.MaxHolder || len(q["distributor"]) != 1 {
		problem(w, http.StatusBadRequest, typeInvalid, "distributor names one distributor, as its publishers are marked")
		return
	}
	weeks, err := stats.DistributorWeeks(h.o.Store.Now(), q.Get("from"), q.Get("to"))
	if err != nil { // stats.ErrWeeks: its message says why
		problem(w, http.StatusBadRequest, typeInvalid, strings.TrimPrefix(err.Error(), stats.ErrWeeks.Error()+": "))
		return
	}
	ids, err := h.o.Store.TenantIDsByName(r.Context(), h.o.PublicTenants)
	if err != nil {
		h.fail(w, err)
		return
	}
	public := map[string]bool{}
	for _, id := range ids {
		public[id] = true
	}
	parts, err := h.o.Store.DistributorParts(r.Context(), distributor, reqType(r), weeks)
	if err != nil {
		h.fail(w, err)
		return
	}
	last, _ := time.Parse(time.DateOnly, weeks[len(weeks)-1])
	reply(w, r, http.StatusOK, map[string]any{"distributor": distributor, "from": weeks[0], "to": store.DayOf(last.AddDate(0, 0, 6)),
		"min_tenants": h.o.DistributorMinTenants, "rows": stats.Distributor(parts, public, h.o.DistributorMinTenants)}, "", noStore)
}
