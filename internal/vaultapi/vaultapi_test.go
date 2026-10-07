package vaultapi

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPRefused(t *testing.T) {
	for _, c := range []Config{
		{Address: "http://vault.example:8200"},
		{Address: "http://vault.example:8200", AllowHTTP: true},
		{Address: "ftp://127.0.0.1"},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	if _, err := New(Config{Address: "http://127.0.0.1:8200", AllowHTTP: true}); err != nil {
		t.Fatal(err)
	}
}

func TestTokenFileMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t")
	os.WriteFile(p, []byte("x"), 0o644)
	os.Chmod(p, 0o644)
	if _, err := readTokenFile(p); err == nil {
		t.Fatal("a world-readable token file was accepted")
	}
	os.Chmod(p, 0o600)
	if tok, err := readTokenFile(p); err != nil || tok != "x" {
		t.Fatal(tok, err)
	}
}

func TestRedirectsAndReLogin(t *testing.T) {
	var logins, forbidden atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/jwt/login":
			n := logins.Add(1)
			w.Write([]byte(`{"auth":{"client_token":"t` + string(rune('0'+n)) + `","lease_duration":3600}}`))
		case "/v1/redirect":
			http.Redirect(w, r, "http://evil.example/", http.StatusFound)
		case "/v1/data":
			if r.Header.Get("X-Vault-Token") == "t1" && forbidden.Add(1) == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if r.Header.Get("X-Vault-Namespace") != "ns1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"data":{"ok":true}}`))
		}
	}))
	defer srv.Close()
	jwt := filepath.Join(t.TempDir(), "jwt")
	os.WriteFile(jwt, []byte("a.b.c"), 0o600)
	c, err := New(Config{Address: srv.URL, AllowHTTP: true, AuthKind: AuthJWT, Role: "r", TokenFile: jwt, Namespace: "ns1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var out struct{ Data map[string]bool }
	if err := c.Do(ctx, http.MethodGet, "data", nil, &out); err != nil || !out.Data["ok"] {
		t.Fatalf("re-login after 403: %v %v", err, out)
	}
	if logins.Load() != 2 {
		t.Fatalf("%d logins", logins.Load())
	}
	var ve *Error
	if err := c.Do(ctx, http.MethodGet, "redirect", nil, nil); !errors.As(err, &ve) || ve.Status != http.StatusFound {
		t.Fatalf("redirect followed: %v", err)
	}
	// lazy renewal at 2/3 of the lease
	c.now = func() time.Time { return time.Now().Add(41 * time.Minute) }
	if err := c.Do(ctx, http.MethodGet, "data", nil, &out); err != nil {
		t.Fatal(err)
	}
	if logins.Load() != 3 {
		t.Fatalf("no renewal at 2/3 of the lease: %d logins", logins.Load())
	}
}

func TestErrorIsShort(t *testing.T) {
	err := vaultError(400, []byte(`{"errors":["`+strings.Repeat("x", 500)+`"]}`))
	if len(err.Error()) > 260 {
		t.Fatalf("%d bytes", len(err.Error()))
	}
}

func TestTokenFileRules(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct {
		body string
		mode os.FileMode
	}{"0640": {"x", 0o640}, "empty": {"  \n", 0o600}} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(c.body), c.mode)
		os.Chmod(p, c.mode)
		if _, err := readTokenFile(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestKubernetesLoginAndTLS(t *testing.T) {
	var path atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			path.Store(r.URL.Path)
			w.Write([]byte(`{"auth":{"client_token":"t","lease_duration":60}}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	jwt := filepath.Join(t.TempDir(), "sa")
	os.WriteFile(jwt, []byte("a.b.c"), 0o600)
	// without the CA the server's certificate is not trusted
	c, err := New(Config{Address: srv.URL, AuthKind: AuthKubernetes, Role: "r", TokenFile: jwt})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Do(context.Background(), http.MethodGet, "x", nil, nil); err == nil {
		t.Fatal("an untrusted certificate was accepted")
	}
	// with ca_file it is
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pemCert(srv), 0o644)
	c, err = New(Config{Address: srv.URL, AuthKind: AuthKubernetes, Role: "r", TokenFile: jwt, CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Do(context.Background(), http.MethodGet, "x", nil, nil); err != nil {
		t.Fatal(err)
	}
	if path.Load() != "/v1/auth/kubernetes/login" {
		t.Fatalf("login at %v", path.Load())
	}
}

func pemCert(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

func TestRetries429AndBudget(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "t")
	os.WriteFile(tok, []byte("t"), 0o600)
	c, _ := New(Config{Address: srv.URL, AllowHTTP: true, AuthKind: AuthTokenFile, TokenFile: tok})
	if err := c.Do(context.Background(), http.MethodGet, "x", nil, nil); err != nil || n.Load() != 3 {
		t.Fatalf("429 twice then ok: %v after %d", err, n.Load())
	}
	n.Store(-10)
	if err := c.Do(context.Background(), http.MethodGet, "x", nil, nil); err == nil {
		t.Fatal("more than 3 attempts")
	}
	if got := n.Load(); got != -7 {
		t.Fatalf("%d attempts, want 3", got+10)
	}
}

// A burst of 403s causes one forced re-login, not one per call.
func Test403Burst(t *testing.T) {
	var logins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			logins.Add(1)
			w.Write([]byte(`{"auth":{"client_token":"t","lease_duration":3600}}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	jwt := filepath.Join(t.TempDir(), "jwt")
	os.WriteFile(jwt, []byte("a.b.c"), 0o600)
	c, _ := New(Config{Address: srv.URL, AllowHTTP: true, AuthKind: AuthJWT, Role: "r", TokenFile: jwt})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Do(context.Background(), http.MethodGet, "x", nil, nil) }()
	}
	wg.Wait()
	if got := logins.Load(); got > 2 {
		t.Fatalf("%d logins for 8 denied calls", got)
	}
}

// A failed renewal keeps the old token while it is valid, backs off, and is not retried per call.
func TestRenewalFailureKeepsToken(t *testing.T) {
	var logins atomic.Int32
	var failLogin atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			logins.Add(1)
			if failLogin.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Write([]byte(`{"auth":{"client_token":"t1","lease_duration":3600}}`))
			return
		}
		if r.Header.Get("X-Vault-Token") != "t1" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	jwt := filepath.Join(t.TempDir(), "jwt")
	os.WriteFile(jwt, []byte("a.b.c"), 0o600)
	c, _ := New(Config{Address: srv.URL, AllowHTTP: true, AuthKind: AuthJWT, Role: "r", TokenFile: jwt})
	ctx := context.Background()
	if err := c.Do(ctx, http.MethodGet, "x", nil, nil); err != nil {
		t.Fatal(err)
	}
	failLogin.Store(true)
	base := time.Now()
	c.now = func() time.Time { return base.Add(45 * time.Minute) } // renewal due, token still valid
	for i := 0; i < 5; i++ {
		if err := c.Do(ctx, http.MethodGet, "x", nil, nil); err != nil {
			t.Fatalf("call %d with a valid old token: %v", i, err)
		}
	}
	if got := logins.Load(); got != 2 {
		t.Fatalf("%d logins: the failed renewal was retried per call", got)
	}
	c.now = func() time.Time { return base.Add(61 * time.Minute) } // expired
	if err := c.Do(ctx, http.MethodGet, "x", nil, nil); err == nil {
		t.Fatal("an expired token was kept")
	}
}
