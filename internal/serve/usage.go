package serve

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/usage"
)

// usage takes an SDK's report, POST <prefix>/-/usage/<name> (spec 0012 phase 1b). Who may report
// is decided from the path and the token before the body is read, as the licence route decides: a
// token holding install on the name where a live release of it exists, or anyone served a public
// release of it; everyone else gets the answer of a missing path. A report is the client's claim:
// it is counted, and a principal holding install makes one extension.use event a day per version.
func (h *Handler) usage(w http.ResponseWriter, r *http.Request, rt route) {
	cc, r, ok := h.caller(w, r, rt)
	if !ok {
		return
	}
	full := cc.installs(rt.name) && cc.snap.Live(rt.name, release.Everything)
	if !full && !cc.snap.Live(rt.name, release.PublicOnly) {
		cc.missing()
		// a valid token without install on the name: refused, as on the binary routes
		if cc.actor != "" && !cc.installs(rt.name) && h.o.Events != nil {
			h.o.Events.Refusal(r.Context(), cc.sc.Tenant.ID, cc.actor, "authz.refused", audit.Refused, "route:usage",
				map[string]any{"route": r.Method + " usage", "status": http.StatusNotFound})
		}
		return
	}
	if h.o.Usage == nil {
		noContent(w) // statistics.usage_accept is false: nothing is kept
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		badReport(w, http.StatusUnsupportedMediaType, usage.RuleType+": a report is application/json")
		return
	}
	if r.ContentLength > usage.MaxSize {
		badReport(w, http.StatusRequestEntityTooLarge, usage.RuleSize+": a report is at most 64 KiB")
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(10 * time.Second)) // a body trickled in is cut off
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, usage.MaxSize))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		badReport(w, http.StatusRequestEntityTooLarge, usage.RuleSize+": a report is at most 64 KiB")
		return
	}
	if err != nil {
		badReport(w, http.StatusBadRequest, usage.RuleJSON+": the body ended early")
		return
	}
	rep, err := usage.Parse(b)
	var ue *usage.Error
	if errors.As(err, &ue) {
		badReport(w, http.StatusBadRequest, ue.Msg)
		return
	}
	vis := release.PublicOnly
	if full {
		vis = release.Everything
	}
	// a version the caller is not served here, or a platform or DuckDB version no DuckDB sends: dropped
	if !cc.snap.LiveVersion(rt.name, rep.Version, vis) || release.ValidPlatform(rep.Platform) != nil ||
		store.ValidDuckDBVersion(rep.DuckDBVersion) != nil {
		noContent(w)
		return
	}
	day := store.DayOf(h.st.Now())
	counts := map[store.UsageKey]int64{}
	var keys [][3]string
	for k, n := range rep.Totals() {
		counts[store.UsageKey{TenantID: cc.sc.Tenant.ID, ChannelID: cc.sc.Channel.ID, Name: rt.name, ExtVersion: rep.Version,
			Event: k[0], Counter: k[1], Status: rep.Status, Day: day, Authenticated: cc.actor != ""}] = n
		keys = append(keys, [3]string{k[0], k[1], rep.Status})
	}
	switch {
	case len(counts) == 0:
	case !h.usageKeys.Admit(cc.sc.Channel.ID+"/"+rt.name+"@"+rep.Version, day, keys):
		h.o.Usage.Drop() // more distinct names a day than a version's budget: dropped, and counted
	default:
		h.o.Usage.AddAll(counts) // over usage's budget: dropped, and counted in kista.usage.dropped
	}
	if full && h.o.Events != nil && h.uses.First(cc.actor, cc.sc.Channel.ID+"/"+rt.name+"@"+rep.Version, "", day) {
		h.o.Events.Event(r.Context(), cc.sc.Tenant.ID, cc.actor, "extension.use", "channel:"+cc.sc.Channel.Name+"/ext:"+rt.name,
			map[string]any{"version": rep.Version, "platform": rep.Platform, "duckdb_version": rep.DuckDBVersion,
				"license": rep.License, "status": rep.Status, "events": rep.Names()})
	}
	noContent(w)
}

// noContent answers 204, never cached.
func noContent(w http.ResponseWriter) {
	licenseHeaders(w.Header())
	w.WriteHeader(http.StatusNoContent)
}

// badReport answers a report refused, with the rule it breaks.
func badReport(w http.ResponseWriter, status int, msg string) {
	plain(w, status, []byte(strings.TrimSuffix(msg, "\n")+"\n"))
}
