package serve

import (
	"net/http"
	"strings"
)

// CORS for the console's micro-frontend in a shell on another origin (spec 0015 phase 1b): the
// module and its fonts under /ui/mfe/, and the API under /api/v1/. Exact origins only, never
// credentials: the console sends a bearer token, so a cookie is never needed.
const (
	corsAllowHeaders  = "Authorization, Content-Type, If-Match, If-None-Match, traceparent"
	corsExposeHeaders = "ETag, Location"
	corsMaxAge        = "600"
)

// cors answers a preflight (done) or marks a request from an allowed origin; w is what the rest
// of the request writes to. It applies only on https, to routes that are served.
func (h *Handler) cors(w http.ResponseWriter, r *http.Request) (_ http.ResponseWriter, done bool) {
	if len(h.o.CORSOrigins) == 0 || scheme(r) != "https" {
		return w, false
	}
	p := r.URL.EscapedPath()
	methods := ""
	switch {
	case strings.HasPrefix(p, "/api/v1/") && h.o.API != nil:
		methods = "GET, HEAD, POST, DELETE"
	case strings.HasPrefix(p, "/ui/mfe/") && h.o.Console != nil:
		methods = "GET, HEAD"
	default:
		return w, false
	}
	cw := &varyWriter{ResponseWriter: w}
	cw.Header().Add("Vary", "Origin") // also for a handler that never writes (an implicit 200)
	origin := r.Header.Get("Origin")
	allowed := origin != "" && h.corsAllowed(origin)
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		// a preflight: answered here, before the API's rate limiter and method rules
		if !allowed {
			plain(cw, http.StatusForbidden, []byte("forbidden\n"))
			return cw, true
		}
		hd := cw.Header()
		hd.Set("Access-Control-Allow-Origin", origin)
		hd.Set("Access-Control-Allow-Methods", methods)
		hd.Set("Access-Control-Allow-Headers", corsAllowHeaders)
		hd.Set("Access-Control-Max-Age", corsMaxAge)
		hd.Set("Cache-Control", "no-store")
		cw.WriteHeader(http.StatusNoContent)
		return cw, true
	}
	if allowed {
		cw.Header().Set("Access-Control-Allow-Origin", origin)
		cw.Header().Set("Access-Control-Expose-Headers", corsExposeHeaders)
	}
	return cw, false
}

func (h *Handler) corsAllowed(origin string) bool {
	for _, o := range h.o.CORSOrigins {
		if o == origin {
			return true
		}
	}
	return false
}

// varyWriter adds Origin to Vary when the response is written, whatever its handler set: an answer
// for one origin must never be cached for another.
type varyWriter struct {
	http.ResponseWriter
	wrote bool
}

// WriteHeader adds Origin again when the handler replaced Vary (the API sets its own).
func (v *varyWriter) WriteHeader(code int) {
	if !v.wrote {
		v.wrote = true
		hd := v.Header()
		has := false
		for _, l := range hd.Values("Vary") {
			for _, f := range strings.Split(l, ",") {
				has = has || strings.EqualFold(strings.TrimSpace(f), "Origin")
			}
		}
		if !has {
			hd.Add("Vary", "Origin")
		}
	}
	v.ResponseWriter.WriteHeader(code)
}

func (v *varyWriter) Write(p []byte) (int, error) {
	if !v.wrote {
		v.WriteHeader(http.StatusOK)
	}
	return v.ResponseWriter.Write(p)
}

func (v *varyWriter) Unwrap() http.ResponseWriter { return v.ResponseWriter }
