package api_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"golang.org/x/crypto/ssh"
)

// pubSigner is a publisher key a test holds: the private half signs requests (spec 0023).
type pubSigner struct {
	s       ssh.Signer
	id, fp  string
	at      time.Time // signed-at; zero: now
	authKey string    // the OpenSSH line registered
}

// newSigner makes an Ed25519 key.
func newSigner(t *testing.T) *pubSigner {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return &pubSigner{s: s, fp: ssh.FingerprintSHA256(s.PublicKey()),
		authKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))) + " ci"}
}

// addKey registers a new key on a publisher with token tok and returns its handle, which call and
// upload take as a token.
func (m *mgmt) addKey(t *testing.T, tok, tenant, publisher string) string {
	t.Helper()
	k := newSigner(t)
	r := m.call(t, "POST", "/api/v1/tenants/"+tenant+"/publishers/"+publisher+"/keys", tok, `{"public_key":"`+k.authKey+`"}`)
	if r.status != 201 {
		t.Fatalf("register a key on %s: %d %s", publisher, r.status, r.body)
	}
	k.id = r.json(t)["id"].(string)
	return m.hold(k)
}

// hold keeps a signer and returns its handle.
func (m *mgmt) hold(k *pubSigner) string {
	if m.signers == nil {
		m.signers = map[string]*pubSigner{}
	}
	h := "sig:" + strconv.Itoa(len(m.signers)+1)
	m.signers[h] = k
	return h
}

// authorize sets a request's credential: a Bearer token, or for a signer's handle the signed
// request's headers over its method, path and body.
func (m *mgmt) authorize(t *testing.T, req *http.Request, token string, body []byte) {
	t.Helper()
	k, ok := m.signers[token]
	if !ok {
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return
	}
	at := k.at
	if at.IsZero() {
		at = time.Now()
	}
	st := auth.NewStatement(req.Method, req.RequestURI, body, at)
	sig, err := auth.SignStatement(k.s, st)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(auth.StatementHeader, base64.StdEncoding.EncodeToString(st))
	req.Header.Set(auth.SignatureHeader, base64.StdEncoding.EncodeToString(sig))
}
