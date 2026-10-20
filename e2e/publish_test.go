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
	"os/exec"
	"path/filepath"
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

// Spec 0023: the publishing page's commands, run as written against kista with OpenSSH and curl:
// whoami, then a publication, both signed with ssh-keygen. Skipped without bash, ssh-keygen or curl.
func TestPublishRecipe(t *testing.T) {
	b := needBuild(t)
	for _, tool := range []string{"bash", "ssh-keygen", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	k := startKista(t, b)
	dir := t.TempDir()
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "e2e", "-f", filepath.Join(dir, "key")).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v %s", err, out)
	}
	pub, err := os.ReadFile(filepath.Join(dir, "key.pub"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}}
	if _, err := adm.AddPublisher(ctx, serveAdmin, "acme", "doc-ci"); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "publisher:doc-ci", []string{"publish"}, tenants.GrantOn{Channel: "prod"}); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddPublisherKey(ctx, serveAdmin, "acme", "doc-ci", string(pub), time.Time{}); err != nil {
		t.Fatal(err)
	}
	// the page's block, pointed at this kista, its prod channel and the demo
	page, err := os.ReadFile("../website/docs/publishing.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(page), "### Publishing from CI")
	if ok {
		_, rest, ok = strings.Cut(rest, "```bash\n")
	}
	script, _, _ := strings.Cut(rest, "```")
	if !ok || script == "" {
		t.Fatal("no bash block under Publishing from CI")
	}
	data, err := os.ReadFile(k.b.extensions["kista_demo"])
	if err != nil {
		t.Fatal(err)
	}
	ef, err := extfile.Open(bytes.NewReader(data), int64(len(data)), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range [][2]string{
		{"https://kista.example.com", "https://" + k.addr},
		{"build/release/extension/acl/acl.duckdb_extension", k.b.extensions["kista_demo"]},
		{"/channels/staging/extensions/acl/releases?version=1.4.0&platform=linux_amd64",
			"/channels/prod/extensions/kista_demo/releases?version=" + ef.Metadata.ExtensionVersion + "&platform=" + ef.Metadata.Platform},
	} {
		if !strings.Contains(script, r[0]) {
			t.Fatalf("the page's block no longer has %q", r[0])
		}
		script = strings.ReplaceAll(script, r[0], r[1])
	}
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "KISTA_PUBLISHER_KEY_FILE="+filepath.Join(dir, "key"), "CURL_CA_BUNDLE="+testCA(t).caFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the page's commands: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"publisher":"doc-ci"`) || !strings.Contains(string(out), `"origin":"publication"`) {
		t.Fatalf("whoami and the publication: %s", out)
	}
}
