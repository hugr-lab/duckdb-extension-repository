package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func handler(t *testing.T) *Handler {
	t.Helper()
	files := fstest.MapFS{
		"index.html":         {Data: []byte(`<!doctype html><base href="/ui/"><script type="module" src="assets/app-1.js"></script>`)},
		"assets/app-1.js":    {Data: []byte("console.log(1)")},
		"mfe/kista.js":       {Data: []byte("export const contract = 1")},
		"mfe/assets/f.woff2": {Data: []byte("font")},
	}
	origins := func(_ context.Context, s Scope) []string {
		switch {
		case s.Server:
			return []string{"https://ops.example"}
		case s.Tenant == "acme":
			return []string{"https://acme-idp.example", "https://token.acme.example"}
		}
		return nil
	}
	h, err := New(Options{FS: files, Origins: origins, ConnectSrc: []string{"https://otel.example"}})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h http.Handler, method, path string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestScopeOf(t *testing.T) {
	for p, want := range map[string]Scope{
		"/ui/": {}, "/ui/index.html": {}, "/ui/server": {Server: true}, "/ui/server/tenants/acme": {Server: true},
		"/ui/t/acme": {Tenant: "acme"}, "/ui/t/acme/channels/prod": {Tenant: "acme"}, "/ui/serverx": {},
	} {
		if got := ScopeOf(p); got != want {
			t.Errorf("%s: %+v, want %+v", p, got, want)
		}
	}
}

// Spec 0015: the SPA fallback, 404 for missing assets, methods, cache headers, and a CSP per scope
// in which one tenant's IdP never appears in another scope's policy.
func TestServing(t *testing.T) {
	h := handler(t)
	if w := get(h, "GET", "/ui"); w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/ui/" {
		t.Fatalf("/ui: %d %v", w.Code, w.Header())
	}
	for _, p := range []string{"/ui/", "/ui/t/acme/channels/prod", "/ui/server/tenants"} {
		w := get(h, "GET", p)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `<base href="/ui/">`) || w.Header().Get("Cache-Control") != "no-cache" ||
			w.Header().Get("ETag") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: %d %v", p, w.Code, w.Header())
		}
	}
	if w := get(h, "HEAD", "/ui/t/acme/"); w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("HEAD: %d %d", w.Code, w.Body.Len())
	}
	tag := get(h, "GET", "/ui/").Header().Get("ETag")
	if w := get(h, "GET", "/ui/t/acme/", "If-None-Match", tag); w.Code != http.StatusNotModified || w.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("the page again: %d %v", w.Code, w.Header())
	}
	if w := get(h, "GET", "/ui/assets/missing.js"); w.Code != 404 {
		t.Errorf("a missing asset: %d", w.Code)
	}
	if w := get(h, "GET", "/ui/assets/app-1.js"); w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("an asset: %d %v", w.Code, w.Header())
	}
	if w := get(h, "GET", "/ui/mfe/kista.js"); w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("ETag") == "" {
		t.Errorf("the module: %d %v", w.Code, w.Header())
	} else if w2 := get(h, "GET", "/ui/mfe/kista.js", "If-None-Match", w.Header().Get("ETag")); w2.Code != http.StatusNotModified {
		t.Errorf("the module again: %d", w2.Code)
	}
	if w := get(h, "POST", "/ui/"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
	if w := get(h, "GET", "/ui/../etc/passwd"); w.Code != 404 {
		t.Errorf("a dot-dot path: %d", w.Code)
	}
	csp := func(p string) string { return get(h, "GET", p).Header().Get("Content-Security-Policy") }
	landing, server, acme, other := csp("/ui/"), csp("/ui/server/"), csp("/ui/t/acme/"), csp("/ui/t/other/")
	for name, c := range map[string]string{"landing": landing, "server": server, "other tenant": other} {
		if strings.Contains(c, "acme") {
			t.Errorf("%s's policy names acme's IdP: %s", name, c)
		}
	}
	if !strings.Contains(acme, "connect-src 'self' https://acme-idp.example https://token.acme.example https://otel.example") ||
		!strings.Contains(acme, "form-action 'self' https://acme-idp.example") || strings.Contains(acme, "ops.example") {
		t.Errorf("acme's policy: %s", acme)
	}
	if !strings.Contains(server, "https://ops.example") || !strings.Contains(landing, "frame-ancestors 'none'") ||
		!strings.Contains(landing, "script-src 'self';") {
		t.Errorf("policies: %s | %s", server, landing)
	}
}

// A binary built without the console serves a placeholder that says so.
func TestPlaceholder(t *testing.T) {
	h, err := New(Options{FS: fstest.MapFS{}})
	if err != nil {
		t.Fatal(err)
	}
	if w := get(h, "GET", "/ui/t/acme/"); w.Code != 200 || !strings.Contains(w.Body.String(), "make console") {
		t.Fatalf("the placeholder: %d %s", w.Code, w.Body)
	}
}
