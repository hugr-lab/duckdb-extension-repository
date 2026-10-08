package e2e

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/fs"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/serve"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0006: DuckDB at the pin against kista serve itself (internal/serve on a dual listener: TLS
// and plain http on one port, as DuckDB's http->https rewrite after LOAD httpfs needs).

var serveAdmin = authz.Actor{Kind: authz.ActorOS, ID: "1:e2e"}

// syncBuffer collects the access log.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// statuses returns the statuses logged for request paths ending with suffix, since mark.
func (k *kista) statuses(suffix string, mark int) []int {
	k.access.mu.Lock()
	defer k.access.mu.Unlock()
	var out []int
	for _, line := range strings.Split(k.access.buf.String()[mark:], "\n") {
		var e struct {
			Msg    string `json:"msg"`
			Path   string `json:"path"`
			Status int    `json:"status"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.Msg == "request" && strings.HasSuffix(e.Path, suffix) {
			out = append(out, e.Status)
		}
	}
	return out
}

func (k *kista) mark() int {
	k.access.mu.Lock()
	defer k.access.mu.Unlock()
	return k.access.buf.Len()
}

type kista struct {
	access  syncBuffer
	t       *testing.T
	st      *store.Store
	keys    *keys.Service
	rel     *release.Service
	keyDir  string
	addr    string
	b       *build
	handler *serve.Handler
}

func (k *kista) httpURL() string  { return "http://" + k.addr + "/acme/prod" }
func (k *kista) httpsURL() string { return "https://" + k.addr + "/acme/prod" }

func startKista(t *testing.T, b *build) *kista { return startKistaWith(t, b, nil) }

// startKistaWith starts kista serve with a token verifier (nil: anonymous callers only).
func startKistaWith(t *testing.T, b *build, verifier *auth.Verifier) *kista {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	keyDir := t.TempDir()
	ks := &keys.Service{Store: st, Signers: signer.Resolver{FileDir: keyDir, AllowFile: true}, Authz: authz.ServerAdmin{}}
	fsStore, err := fs.Open(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	bs, err := blob.NewService(ctx, st, []blob.Domain{{Name: "default", Kind: "fs", Store: fsStore}},
		blob.Options{SpoolDir: filepath.Join(t.TempDir(), "spool"), MaxBody: 256 << 20, MaxIngests: 2, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bs.Close() })
	ten := &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = ten.CreateTenant(ctx, serveAdmin, "acme", "", "")
	must(err)
	kind := "dev"
	if isRelease(b.versionDir) {
		kind = "release"
	}
	_, err = ten.AddVersion(ctx, serveAdmin, b.versionDir, kind, []string{"v1.5.6", "v2.0.0"})
	must(err)
	_, err = ten.CreateChannel(ctx, serveAdmin, "acme", "prod", store.ChannelSigned)
	must(err)
	_, err = ten.SetChannelVersions(ctx, serveAdmin, "acme", "prod", []string{b.versionDir}, nil)
	must(err)
	k := &kista{t: t, st: st, keys: ks, keyDir: keyDir, b: b,
		rel: &release.Service{Store: st, Blob: bs, Signers: ks, Authz: authz.ServerAdmin{}}}
	k.addKey("a.pem", true)

	certFile, keyFile := writeLeaf(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	k.addr = ln.Addr().String()
	k.handler = serve.NewHandler(st, ks, bs, serve.Options{MaxDownloads: 16, MaxDownloadsPerClient: 16, MinRate: 1024,
		WriteIdleTimeout: 30 * time.Second, Log: slog.New(slog.NewJSONHandler(&k.access, nil)), Verifier: verifier,
		PublicURL: "https://" + k.addr})
	k.handler.SetReady(true)
	srv := &serve.Server{Handler: k.handler, Log: slog.New(slog.DiscardHandler), DrainTimeout: time.Millisecond, ShutdownTimeout: time.Second,
		Listeners: []serve.Listener{{Listener: config.Listener{Addr: k.addr, Scheme: "dual",
			TLS: &config.TLSConfig{CertFile: certFile, KeyFile: keyFile}}, Net: ln}}}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Run(rctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return k
}

// addKey creates a key file and adds it to the channel.
func (k *kista) addKey(file string, active bool) store.Key {
	k.t.Helper()
	if err := signer.GenerateKeyFile(filepath.Join(k.keyDir, file)); err != nil {
		k.t.Fatal(err)
	}
	key, err := k.keys.Add(context.Background(), serveAdmin, "acme", "prod", "file:"+file, active)
	if err != nil {
		k.t.Fatal(err)
	}
	return key
}

func (k *kista) release(name string, o release.AddOptions) store.Release {
	k.t.Helper()
	f, err := os.Open(k.b.extensions[name])
	if err != nil {
		k.t.Fatal(err)
	}
	defer f.Close()
	o.Name = name
	r, _, err := k.rel.Add(context.Background(), serveAdmin, "acme", "prod", f, o)
	if err != nil {
		k.t.Fatal(err)
	}
	return r
}

func (k *kista) pem(t *testing.T, key store.Key) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: key.PublicKey}))
}

func (k *kista) activeKey(t *testing.T) store.Key {
	t.Helper()
	ks, err := k.keys.List(context.Background(), serveAdmin, "acme", "prod")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range ks {
		if key.State == store.KeyActive {
			return key
		}
	}
	t.Fatal("no active key")
	return store.Key{}
}

// writeLeaf writes the test CA's leaf (127.0.0.1) as files for a listener.
func writeLeaf(t *testing.T) (string, string) {
	t.Helper()
	leaf := testCA(t).leaf
	dir := t.TempDir()
	var chain []byte
	for _, c := range leaf.Certificate {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c})...)
	}
	der, err := x509.MarshalPKCS8PrivateKey(leaf.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	cf, kf := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(cf, chain, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kf, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return cf, kf
}

// The bootstrap over http:// on kista serve, then installs over https after DuckDB's rewrite to
// https on the same port; .well-known over http with httpfs loaded; cpp and c_struct builds; the
// versioned path.
func TestServeBootstrap(t *testing.T) {
	b := needBuild(t)
	k := startKista(t, b)
	k.release("httpfs", release.AddOptions{})
	k.release("loadable_extension_demo", release.AddOptions{})
	k.release("demo_capi", release.AddOptions{})
	key := k.pem(t, k.activeKey(t))

	res := newSession(t, b, nil).exec(
		createRepo("r", k.httpURL(), key),
		"INSTALL httpfs FROM r",
		"LOAD httpfs FROM r",
		"SET ca_cert_file = "+sqlString(testCA(t).caFile),
		// httpfs is loaded: DuckDB rewrites http:// to https:// on the same port (the dual listener)
		"INSTALL loadable_extension_demo FROM r",
		"LOAD loadable_extension_demo FROM r",
		"SELECT test_alias_hello()",
		// keys from .well-known (read through httpfs, over http: not over the test CA)
		createRepo("w", k.httpURL()),
		"INSTALL demo_capi FROM w",
		"LOAD demo_capi FROM w",
		// the versioned path
		"FORCE INSTALL loadable_extension_demo FROM r VERSION 'default-version'",
	)
	mustOK(t, res)
	// a name the channel does not have
	res = newSession(t, b, nil).exec(createRepo("r", k.httpURL(), key), "INSTALL nothing_here FROM r")
	mustOK(t, res[:1])
	if res[1].OK {
		t.Fatal("installed a missing extension")
	}
}

// A key rotation: a new key becomes active, the re-signer moves the serving key; an install made
// before still loads, and a fresh install verifies with the new key.
func TestServeRotation(t *testing.T) {
	b := needBuild(t)
	k := startKista(t, b)
	k.release("loadable_extension_demo", release.AddOptions{})
	keyA := k.activeKey(t)
	pemA := k.pem(t, keyA)
	extDir := t.TempDir()
	opts := map[string]string{"extension_directory": extDir}
	mustOK(t, newSession(t, b, opts).exec(
		createRepo("r", k.httpURL(), pemA),
		"INSTALL loadable_extension_demo FROM r",
	))

	keyB := k.addKey("b.pem", false)
	ctx := context.Background()
	if _, err := k.keys.Activate(ctx, serveAdmin, "acme", "prod", keyB.ID, true); err != nil {
		t.Fatal(err)
	}
	ch, err := k.st.GetChannel(ctx, "acme", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if _, moved, err := k.rel.Resign(ctx, ch.ID, nil); err != nil || !moved {
		t.Fatalf("resign: %v %v", moved, err)
	}
	pemB := k.pem(t, keyB)
	// the old install, signed by A, still loads with both keys pinned
	res := newSession(t, b, opts).exec(
		createRepo("r", k.httpURL(), pemA, pemB),
		"LOAD loadable_extension_demo FROM r",
		"SELECT test_alias_hello()",
	)
	mustOK(t, res)
	// a fresh install is signed by B: it verifies with B alone, and not with A alone
	mustOK(t, newSession(t, b, nil).exec(
		createRepo("r", k.httpURL(), pemB),
		"INSTALL loadable_extension_demo FROM r",
		"LOAD loadable_extension_demo FROM r",
	))
	res = newSession(t, b, nil).exec(
		createRepo("r", k.httpURL(), pemA),
		"INSTALL loadable_extension_demo FROM r",
	)
	mustOK(t, res[:1])
	if res[1].OK {
		t.Fatal("a fresh install verified with the old key only")
	}
	if !strings.Contains(strings.ToLower(res[1].Error), "signature") {
		t.Fatalf("install with the old key only failed for another reason: %s", res[1].Error)
	}
}
