// Package console serves kista's administration console (spec 0015) at /ui/: the build embedded
// from dist, with a content security policy per scope.
package console

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// dist holds the builds (make console): the standalone console in dist/app, the micro-frontend in
// dist/mfe (spec 0015 phase 1b). A binary built without dist/app serves placeholder at /ui/ and no
// micro-frontend: npm run build makes both.
//
//go:embed all:dist
var dist embed.FS

const placeholder = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>kista</title></head>
<body><p>This kista binary was built without its console: build it with <code>make console</code>,
then <code>make build</code>.</p></body></html>
`

// placeholderFS holds only the placeholder app/index.html (read with fs.ReadFile).
type placeholderFS struct{}

func (placeholderFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (placeholderFS) ReadFile(name string) ([]byte, error) {
	if name != "app/index.html" {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}
	return []byte(placeholder), nil
}

// Scope is the part of the console a page belongs to: the landing, the server's, or a tenant's.
type Scope struct {
	Server bool
	Tenant string // a tenant's scope
}

// Options configure the console.
type Options struct {
	// Origins returns the IdP origins a scope's sign-in talks to (its CSP's connect-src and
	// form-action); nil or empty for none.
	Origins        func(ctx context.Context, s Scope) []string
	FrameAncestors []string
	ConnectSrc     []string
	FS             fs.FS // the builds, app/ and mfe/; nil: the embedded ones
}

// Handler serves /ui/.
type Handler struct {
	o     Options
	files fs.FS
	etags map[string]string // index.html and mfe/kista.js: strong ETags
}

// New makes the console's handler.
func New(o Options) (*Handler, error) {
	files := o.FS
	if files == nil {
		sub, err := fs.Sub(dist, "dist")
		if err != nil {
			return nil, err
		}
		files = sub
	}
	if _, err := fs.Stat(files, "app/index.html"); err != nil {
		files = placeholderFS{}
	}
	h := &Handler{o: o, files: files, etags: map[string]string{}}
	for _, name := range []string{"app/index.html", "mfe/kista.js"} {
		if b, err := fs.ReadFile(files, name); err == nil {
			sum := sha256.Sum256(b)
			h.etags[name] = `"` + hex.EncodeToString(sum[:16]) + `"`
		}
	}
	return h, nil
}

// ScopeOf is the scope of a path under /ui/.
func ScopeOf(p string) Scope {
	rest := strings.TrimPrefix(p, "/ui/")
	switch {
	case rest == "server" || strings.HasPrefix(rest, "server/"):
		return Scope{Server: true}
	case strings.HasPrefix(rest, "t/"):
		t, _, _ := strings.Cut(strings.TrimPrefix(rest, "t/"), "/")
		return Scope{Tenant: t}
	}
	return Scope{}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if p == "/ui" {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
		return
	}
	if !strings.HasPrefix(p, "/ui/") || strings.Contains(p, "..") {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(p, "/ui/")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	mfe := strings.HasPrefix(name, "mfe/") // the micro-frontend's files: never the SPA fallback
	if !mfe && (path.Ext(name) == "" || name == "index.html") {
		h.index(w, r, ScopeOf(p))
		return
	}
	if !mfe {
		name = "app/" + name
	}
	b, err := fs.ReadFile(h.files, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if tag, ok := h.etags[name]; ok {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", tag)
	} else if strings.HasPrefix(name, "app/assets/") || strings.HasPrefix(name, "mfe/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}

// index serves the page of a scope with its policy.
func (h *Handler) index(w http.ResponseWriter, r *http.Request, s Scope) {
	b, err := fs.ReadFile(h.files, "app/index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var idp []string
	if (s.Server || s.Tenant != "") && h.o.Origins != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second) // an issuer never read: its own origin after
		idp = h.o.Origins(ctx, s)
		cancel()
	}
	w.Header().Set("Content-Security-Policy", h.policy(idp))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", h.etags["app/index.html"])
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}

// policy is the CSP of a scope's page, given its IdP origins.
func (h *Handler) policy(idp []string) string {
	join := func(base string, lists ...[]string) string {
		parts := []string{base}
		for _, l := range lists {
			parts = append(parts, l...)
		}
		return strings.Join(parts, " ")
	}
	frame := "'none'"
	if len(h.o.FrameAncestors) > 0 {
		frame = strings.Join(h.o.FrameAncestors, " ")
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self'",
		"img-src 'self' data:",
		"font-src 'self'",
		join("connect-src 'self'", idp, h.o.ConnectSrc),
		"object-src 'none'",
		"base-uri 'self'",
		join("form-action 'self'", idp),
		"frame-ancestors " + frame,
	}, "; ")
}
