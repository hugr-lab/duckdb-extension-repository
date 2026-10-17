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
	ctx := r.Context()
	sc, err := h.st.GetServeChannel(ctx, rt.tenant, rt.channel)
	if err != nil || sc.Tenant.State != store.TenantActive {
		h.storeError(w, err)
		return
	}
	if sc.Channel.Kind != store.ChannelSigned {
		notFound(w) // a passthrough channel has no licences
		return
	}
	installs, actor, r := h.identify(r, sc, rt.kind.String())
	missing := func() {
		if actor != "" {
			notFound(w) // a valid token without the grant, or no licence: the same
		} else {
			h.missing(w)
		}
	}
	snap, err := h.rv.snaps.Get(ctx, sc.Channel)
	if err != nil {
		h.fail(w, err)
		return
	}
	// may reports whether the caller may have a licence for a name in this channel
	may := func(l store.License) bool {
		if installs(l.Name) && snap.Live(l.Name, release.Everything) {
			return true
		}
		return l.Visibility == store.Public && snap.Live(l.Name, release.PublicOnly)
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
		ls, err := h.o.Licenses.List(ctx, sc.Tenant.ID)
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
	ok := false
	if could {
		if l, ok, err = h.o.Licenses.Serve(ctx, sc.Tenant.ID, rt.name); err != nil {
			h.fail(w, err)
			return
		}
	}
	if !ok || !may(l) {
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
