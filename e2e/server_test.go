package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// request is one logged request. The e2e cases are written against base URL plus this log, so spec
// 0004 can run the same cases against kista serve.
type request struct {
	Method      string
	Path        string
	Auth        string // the Authorization header, as sent
	IfNoneMatch string
	Range       string
	Status      int
}

// fileServer is a loopback-only file server over a repository tree. It can require a Bearer token
// (for every path except .well-known), serves ETags (the SHA-256 of the file), and answers HEAD and
// Range through http.ServeContent.
type fileServer struct {
	srv   *httptest.Server
	root  string
	token string

	mu  sync.Mutex
	log []request
}

type serverOpts struct {
	tls   bool
	token string
}

func newFileServer(t *testing.T, root string, o serverOpts) *fileServer {
	t.Helper()
	fs := &fileServer{root: root, token: o.token}
	fs.srv = httptest.NewUnstartedServer(http.HandlerFunc(fs.serve))
	if o.tls {
		cert := testCA(t)
		fs.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert.leaf}}
		fs.srv.StartTLS()
	} else {
		fs.srv.Start()
	}
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *fileServer) URL() string { return fs.srv.URL }

func (fs *fileServer) requests() []request {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]request(nil), fs.log...)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (fs *fileServer) serve(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w, status: 200}
	defer func() {
		fs.mu.Lock()
		fs.log = append(fs.log, request{
			Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"),
			IfNoneMatch: r.Header.Get("If-None-Match"), Range: r.Header.Get("Range"), Status: rec.status,
		})
		fs.mu.Unlock()
	}()
	clean := path.Clean(r.URL.Path)
	public := strings.HasSuffix(clean, "/.well-known/duckdb-extension-repo.json") &&
		!strings.Contains(strings.TrimSuffix(clean, "/.well-known/duckdb-extension-repo.json"), ".well-known")
	if fs.token != "" && !public && r.Header.Get("Authorization") != "Bearer "+fs.token {
		rec.WriteHeader(http.StatusUnauthorized)
		return
	}
	p := filepath.Join(fs.root, filepath.FromSlash(clean))
	f, err := os.Open(p)
	if err != nil {
		rec.WriteHeader(http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		rec.WriteHeader(http.StatusNotFound)
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		rec.WriteHeader(http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(data)
	rec.Header().Set("ETag", `"`+hex.EncodeToString(sum[:16])+`"`)
	http.ServeContent(rec, r, fi.Name(), time.Time{}, f)
}

// tlsCert is a per-run test CA and a leaf for 127.0.0.1; the CA is written to a file for
// ca_cert_file and never added to a system trust store.
type tlsCert struct {
	leaf   tls.Certificate
	caFile string
}

var (
	caOnce sync.Once
	caCert *tlsCert
	caErr  error
)

func testCA(t *testing.T) *tlsCert {
	t.Helper()
	caOnce.Do(func() { caCert, caErr = makeCA() })
	if caErr != nil {
		t.Fatal(caErr)
	}
	return caCert
}

func makeCA() (*tlsCert, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kista e2e CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caParsed, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caParsed, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "kista-e2e-ca")
	if err != nil {
		return nil, err
	}
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644); err != nil {
		return nil, err
	}
	return &tlsCert{
		leaf:   tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey},
		caFile: caFile,
	}, nil
}
