package serve

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// licenses answers spec 0012's licence routes: GET <prefix>/-/licenses/<name>, the licence for one
// extension (the SDK), and GET <prefix>/-/licenses, those the caller may use here (a downstream
// kista's relay). The decision comes from the path and the token, as on the binary routes: a
// private licence goes to a token holding install on the name, a public one also to whoever is
// served a public release of it; everything else answers as a missing path does for the caller.
func (h *Handler) licenses(w http.ResponseWriter, r *http.Request, rt route) {
	cc, r, ok := h.caller(w, r, rt)
	if !ok {
		return
	}
	ctx := r.Context()
	sc, installs, actor, snap, missing := cc.sc, cc.installs, cc.actor, cc.snap, cc.missing
	// may reports whether the caller may have a licence for a name in this channel: it is of the
	// entry the channel serves the name from (spec 0035: never through another entry, whatever keys
	// its builds declare), and a live build of that entry the caller sees declares its key (spec
	// 0021); the snapshot holds the selected sources' releases only, and their own sources' keys
	may := func(l store.License) bool {
		if src, ok := snap.Selected[l.Name]; !ok || src != l.Entry {
			return false
		}
		if installs(l.Name) && snap.LiveKey(l.Name, l.Key, release.Everything) {
			return true
		}
		return l.Visibility == store.Public && snap.LiveKey(l.Name, l.Key, release.PublicOnly)
	}
	// licensing off: as a missing path, so a downstream relay changes nothing (an empty list would
	// revoke everything it holds)
	if h.o.Licenses == nil || !h.o.Licenses.Enabled() {
		missing()
		return
	}
	if rt.kind == routeLicenses {
		if actor == "" {
			h.missing(w) // the list is for a downstream kista's credential
			return
		}
		ls, err := h.o.Licenses.List(ctx, sc.Tenant.ID, sc.Channel.PackageType)
		if err != nil {
			h.fail(w, err)
			return
		}
		out := struct {
			Licenses []string `json:"licenses"`
		}{Licenses: []string{}}
		for _, l := range ls {
			if may(l) {
				out.Licenses = append(out.Licenses, l.Token)
			}
		}
		b, _ := json.Marshal(out)
		hd := w.Header()
		hd.Set("Content-Type", "application/json")
		licenseHeaders(hd)
		hd.Set("Content-Length", strconv.Itoa(len(b)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(b)
		}
		return
	}
	// decided from the snapshot first: a caller who could have no licence costs no database read
	could := installs(rt.name) && snap.Live(rt.name, release.Everything) || snap.Live(rt.name, release.PublicOnly)
	var l store.License
	found := false
	if could {
		var err error
		if l, found, err = h.o.Licenses.Serve(ctx, sc.Tenant.ID, sc.Channel.PackageType, rt.name, snap.Selected[rt.name]); err != nil {
			h.fail(w, err)
			return
		}
	}
	if !found || !may(l) {
		missing()
		// a valid token without install on the name: refused, as on the binary routes
		if actor != "" && !installs(rt.name) && h.o.Events != nil {
			h.o.Events.Refusal(ctx, sc.Tenant.ID, actor, "authz.refused", audit.Refused, "route:license",
				map[string]any{"route": r.Method + " license", "status": http.StatusNotFound})
		}
		return
	}
	etag := `"` + l.LicenseID + "." + strconv.FormatInt(l.IssuedAt, 10) + `"`
	hd := w.Header()
	licenseHeaders(hd)
	hd.Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	hd.Set("Content-Type", "application/jwt")
	hd.Set("Content-Length", strconv.Itoa(len(l.Token)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(l.Token))
	}
}

// licenseHeaders are every licence answer's: never cached, varying with the token.
func licenseHeaders(hd http.Header) {
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Cache-Control", "private, no-store")
	hd.Set("Vary", "Authorization")
}

// channelCaller is who calls a spec 0012 route of a signed channel, and the channel's snapshot.
type channelCaller struct {
	sc       store.ServeChannel
	installs func(name string) bool
	actor    string // "" for no valid token
	snap     *release.Snapshot
	missing  func() // the answer of a missing path for this caller
}

// caller reads the channel and the caller of a spec 0012 route; false when it answered (a missing
// tenant or channel, a passthrough channel, a failure).
func (h *Handler) caller(w http.ResponseWriter, r *http.Request, rt route) (channelCaller, *http.Request, bool) {
	ctx := r.Context()
	sc, err := h.st.GetServeChannel(ctx, rt.tenant, rt.channel)
	if err != nil || sc.Tenant.State != store.TenantActive {
		h.storeError(w, err)
		return channelCaller{}, r, false
	}
	if sc.Channel.Kind != store.ChannelSigned {
		notFound(w) // a passthrough channel has no licences and takes no reports
		return channelCaller{}, r, false
	}
	installs, actor, r := h.identify(r, sc, rt.kind.String())
	snap, err := h.rv.snaps.Get(ctx, sc.Channel)
	if err != nil {
		h.fail(w, err)
		return channelCaller{}, r, false
	}
	return channelCaller{sc: sc, installs: installs, actor: actor, snap: snap, missing: func() {
		if actor != "" {
			notFound(w) // a valid token without the grant, or nothing there: the same
		} else {
			h.missing(w)
		}
	}}, r, true
}
