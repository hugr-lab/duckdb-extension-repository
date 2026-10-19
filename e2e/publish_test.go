package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// publisherKey adds the publisher name to acme with publish on prod and returns an API key of it;
// marked makes it a source of licence keys (spec 0021), as a server administrator does.
func (k *kista) publisherKey(t *testing.T, name string, marked bool) string {
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
	_, key, err := adm.AddAPIKey(ctx, serveAdmin, "acme", name, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// markPublisher marks a publisher a source of licence keys, or unmarks it.
func (k *kista) markPublisher(t *testing.T, name string, on bool) {
	t.Helper()
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}}
	if _, err := adm.SetPublisherLicenseKeys(context.Background(), serveAdmin, "acme", name, on); err != nil {
		t.Fatal(err)
	}
}

// publish posts a built extension to acme/prod's publication route (spec 0008) with a publisher's
// API key, private, and returns the answer's status.
func (k *kista) publish(t *testing.T, name, key string) int {
	t.Helper()
	path := k.b.extensions[name]
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	ef, err := extfile.Open(f, fi.Size(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	q := url.Values{"version": {ef.Metadata.ExtensionVersion}, "platform": {ef.Metadata.Platform}}
	req, err := http.NewRequest("POST", "https://"+k.addr+"/api/v1/tenants/acme/channels/prod/extensions/"+name+"/releases?"+q.Encode(), f)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = fi.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+key)
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
