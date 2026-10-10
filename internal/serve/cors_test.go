package serve

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Spec 0015 phase 1b: CORS for a shell on another origin, on /api/v1/ and /ui/mfe/ only, exact
// origins, preflights answered before the API, and Vary: Origin whatever the handler set.
func TestCORS(t *testing.T) {
	var reached []string
	next := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = append(reached, name+" "+r.Method)
			w.Header().Set("Vary", "Authorization") // the API's own
			w.Header().Set("ETag", `"x"`)
			w.WriteHeader(http.StatusOK)
		})
	}
	h := NewHandler(nil, nil, nil, Options{MaxDownloads: 1, Log: slog.New(slog.DiscardHandler), API: next("api"),
		Console: next("ui"), CORSOrigins: []string{"https://shell.example"}})
	do := func(scheme, method, path string, hdr ...string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r.WithContext(withScheme(r.Context(), scheme)))
		return w
	}
	const shell, other = "https://shell.example", "https://evil.example"
	pre := func(origin string) []string {
		return []string{"Origin", origin, "Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "authorization"}
	}
	vary := func(w *httptest.ResponseRecorder) string { return strings.Join(w.Header().Values("Vary"), ",") }

	// a preflight from the shell: answered here, the API never reached
	reached = nil
	w := do("https", "OPTIONS", "/api/v1/tenants/acme/whoami", pre(shell)...)
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != shell || w.Header().Get("Access-Control-Allow-Credentials") != "" ||
		!strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "If-Match") || w.Header().Get("Access-Control-Max-Age") != "600" ||
		!strings.Contains(vary(w), "Origin") || len(reached) != 0 {
		t.Fatalf("preflight: %d %v %v", w.Code, w.Header(), reached)
	}
	if m := do("https", "OPTIONS", "/ui/mfe/kista.js", pre(shell)...).Header().Get("Access-Control-Allow-Methods"); m != "GET, HEAD" {
		t.Errorf("the module's methods: %q", m)
	}
	// another origin's preflight: 403
	if w := do("https", "OPTIONS", "/api/v1/info", pre(other)...); w.Code != 403 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("another origin's preflight: %d %v", w.Code, w.Header())
	}
	// a request from the shell: allowed, ETag exposed, Vary keeps the API's and adds Origin
	reached = nil
	w = do("https", "GET", "/api/v1/info", "Origin", shell)
	if w.Header().Get("Access-Control-Allow-Origin") != shell || !strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), "ETag") ||
		vary(w) != "Authorization,Origin" || len(reached) != 1 {
		t.Errorf("a request from the shell: %v %v", w.Header(), reached)
	}
	// from another origin, or none: no CORS headers, but Vary: Origin all the same
	for _, o := range []string{other, ""} {
		w := do("https", "GET", "/api/v1/info", "Origin", o)
		if w.Header().Get("Access-Control-Allow-Origin") != "" || vary(w) != "Authorization,Origin" {
			t.Errorf("origin %q: %v", o, w.Header())
		}
	}
	// the standalone console and the other routes are not CORS resources; nor is plain http
	for _, c := range []struct{ scheme, path string }{{"https", "/ui/"}, {"https", "/ui/t/acme/"}, {"https", "/healthz"},
		{"http", "/api/v1/info"}} {
		if w := do(c.scheme, "GET", c.path, "Origin", shell); w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s %s: %v", c.scheme, c.path, w.Header())
		}
	}
	// without allowed origins, a preflight goes on to the API (its 405), as before
	h.o.CORSOrigins = nil
	reached = nil
	if do("https", "OPTIONS", "/api/v1/info", pre(shell)...); len(reached) != 1 {
		t.Errorf("no CORS: %v", reached)
	}
}
