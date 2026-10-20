package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"golang.org/x/crypto/ssh"
)

// publisherKey adds the publisher name to acme with publish on prod and returns a key of it (spec
// 0023); marked makes it a source of licence keys (spec 0021), as a server administrator does.
func (k *kista) publisherKey(t *testing.T, name string, marked bool) ssh.Signer {
	t.Helper()
	ctx := context.Background()
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}}
	if _, err := adm.AddPublisher(ctx, serveAdmin, "acme", name); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "publisher:"+name, []string{"publish"}, tenants.GrantOn{Channel: "prod"}); err != nil {
		t.Fatal(err)
	}
	k.markPublisher(t, name, marked) // before its credentials: a publisher holding any is not marked
	key := publisherSigner(t)
	if _, err := adm.AddPublisherKey(ctx, serveAdmin, "acme", name, authorizedKey(key), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return key
}

// publisherSigner is a new Ed25519 publisher key.
func publisherSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, pk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(pk)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// authorizedKey is a key's OpenSSH line, as ssh-keygen writes it in the .pub file.
func authorizedKey(s ssh.Signer) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))) + " e2e"
}

// signRequest signs a request as a publisher does (spec 0023): its method, path and query as sent,
// and its body.
func signRequest(t *testing.T, req *http.Request, s ssh.Signer, body []byte) {
	t.Helper()
	st := auth.NewStatement(req.Method, req.URL.RequestURI(), body, time.Now())
	sig, err := auth.SignStatement(s, st)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(auth.StatementHeader, base64.StdEncoding.EncodeToString(st))
	req.Header.Set(auth.SignatureHeader, base64.StdEncoding.EncodeToString(sig))
}

// markPublisher marks a publisher a source of licence keys, or unmarks it.
func (k *kista) markPublisher(t *testing.T, name string, on bool) {
	t.Helper()
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}}
	if _, err := adm.SetPublisherLicenseKeys(context.Background(), serveAdmin, "acme", name, on); err != nil {
		t.Fatal(err)
	}
}

// publish posts a built extension to acme/prod's publication route (spec 0008), signed with a
// publisher's key (spec 0023), private, and returns the answer's status.
func (k *kista) publish(t *testing.T, name string, key ssh.Signer) int {
	t.Helper()
	data, err := os.ReadFile(k.b.extensions[name])
	if err != nil {
		t.Fatal(err)
	}
	ef, err := extfile.Open(bytes.NewReader(data), int64(len(data)), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	q := url.Values{"version": {ef.Metadata.ExtensionVersion}, "platform": {ef.Metadata.Platform}}
	req, err := http.NewRequest("POST", "https://"+k.addr+"/api/v1/tenants/acme/channels/prod/extensions/"+name+"/releases?"+q.Encode(),
		bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	signRequest(t, req, key, data)
	pool := x509.NewCertPool()
	caPEM, _ := os.ReadFile(testCA(t).caFile)
	pool.AppendCertsFromPEM(caPEM)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Logf("publish %s: %d %s", name, resp.StatusCode, b)
	}
	return resp.StatusCode
}
