package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0012 phase 2: the pinned DuckDB runs kista_demo, a C++ extension built with the kista SDK
// and the licence vectors' test key (sdk/cpp/test/extension), against kista serve with licensing
// on. kista_demo_use(n) needs a licence (Check), reads the max_attached_sources limit and counts
// an attach event with a sources counter.
func TestSDKLicense(t *testing.T) {
	b := needBuild(t)
	ext, ok := b.extensions["kista_demo"]
	if !ok || ext == "" {
		t.Fatal("kista_demo is not built (make e2e-duckdb)")
	}
	signKey, err := vectors.LoadKey("../internal/license/testdata", vectors.TrustedKey)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&signKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := license.ParseKeys([]string{string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))})
	if err != nil {
		t.Fatal(err)
	}
	idpKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := &e2eIDP{key: idpKey}
	k := startKistaLicensed(t, b, &auth.Verifier{Fetch: idp}, trusted)
	ctx := context.Background()
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}, Fetch: idp, PublicURL: "https://" + k.addr}
	if _, err := adm.AddIssuer(ctx, serveAdmin, "acme", store.Issuer{Name: "corp", URL: e2eIssuer}); err != nil {
		t.Fatal(err)
	}
	if err := adm.AddAudience(ctx, serveAdmin, "acme", "api://kista-acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "subject:corp|alice", []string{"install"}, tenants.GrantOn{Channel: "prod"}); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "subject:corp|tom", []string{"admin"}, tenants.GrantOn{}); err != nil {
		t.Fatal(err)
	}
	k.release("httpfs", release.AddOptions{})
	k.release("kista_demo", release.AddOptions{Private: true})
	pemKey := k.pem(t, k.activeKey(t))
	token := idp.token(t, "alice", "api://kista-acme")

	now := time.Now().Unix()
	signWith := func(key *rsa.PrivateKey, id, plan string, iat, exp int64, extra string) string {
		payload := fmt.Sprintf(`{"v":1,"iss":"https://issuer.example","jti":%q,"sub":"acme","holder":"Acme Test","ext":"kista_demo",`+
			`"plan":%q,"limits":{"max_attached_sources":5},"iat":%d,"exp":%d%s}`, id, plan, iat, exp, extra)
		return vectors.Sign(key, vectors.Header(key), payload)
	}
	sign := func(id, plan string, iat, exp int64, extra string) string {
		return signWith(signKey, id, plan, iat, exp, extra)
	}
	stored, err := k.licenses.Import(ctx, serveAdmin, "acme", sign("lic-pro", "pro", now-60, now+30*86400, ""), licenses.Create)
	if err != nil {
		t.Fatal(err)
	}

	extDir := t.TempDir()
	// DuckDB 2.0 keeps an extension installed from a named repository under repositories/<name>
	file := filepath.Join(extDir, b.versionDir, b.platform, "repositories", "r", "kista_demo.kista-license")
	opts := map[string]string{"extension_directory": extDir}
	boot := []string{
		createRepo("boot", k.httpURL(), pemKey),
		"INSTALL httpfs FROM boot",
		"LOAD httpfs FROM boot",
		"SET ca_cert_file = " + sqlString(testCA(t).caFile),
		createRepo("r", k.httpsURL(), pemKey),
	}
	secret := "CREATE SECRET s (TYPE http, BEARER_TOKEN " + sqlString(token) + ", SCOPE " + sqlString(k.httpsURL()+"/") + ")"
	licenseRow := "SELECT status, plan, source, holder, license_id, limits FROM kista_demo_license()"
	row := func(t *testing.T, r result) []any {
		t.Helper()
		if !r.OK || len(r.Rows) != 1 {
			t.Fatalf("%s: %v %s", r.Stmt, r.Rows, r.Error)
		}
		return r.Rows[0]
	}

	// INSTALL and LOAD from the private channel: the licence is fetched with the http secret,
	// written beside the module, valid; a limit is read; usage is reported
	mark := k.mark()
	res := newSession(t, b, opts).exec(append(boot, secret, "INSTALL kista_demo FROM r", "LOAD kista_demo FROM r", licenseRow,
		"SELECT * FROM kista_demo_use(3)", "SELECT * FROM kista_demo_use(9)")...)
	mustOK(t, res[:len(res)-1])
	n := len(boot) + 3
	if got := row(t, res[n]); got[0] != "valid" || got[1] != "pro" || got[2] != "repository" || got[3] != "Acme Test" ||
		got[4] != "lic-pro" || got[5] != `{"max_attached_sources":5}` {
		t.Fatalf("the licence: %v", got)
	}
	if got := row(t, res[n+1]); got[0] != "pro" || got[1] != "5" {
		t.Fatalf("kista_demo_use(3): %v", got)
	}
	mustFail(t, res[n+2], "allows 5 sources")
	if b, err := os.ReadFile(file); err != nil || !strings.HasPrefix(string(b), "eyJ") {
		t.Fatalf("the licence beside the module: %q %v", b, err)
	}
	if got := k.statuses("/-/licenses/kista_demo", mark); len(got) != 1 || got[0] != 200 {
		t.Fatalf("the licence fetch: %v", got)
	}
	if got := k.statuses("/-/usage/kista_demo", mark); len(got) != 1 || got[0] != 204 {
		t.Fatalf("the usage report, sent when the connection closed: %v", got)
	}
	// the usage, read as an administrator reads it: GET …/stats/usage
	k.usage.Flush(ctx)
	pool := x509.NewCertPool()
	caPEM, _ := os.ReadFile(testCA(t).caFile)
	pool.AppendCertsFromPEM(caPEM)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	req, _ := http.NewRequest("GET", "https://"+k.addr+"/api/v1/tenants/acme/stats/usage?group=version,status", nil)
	req.Header.Set("Authorization", "Bearer "+idp.token(t, "tom", "api://kista-acme"))
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var stats struct {
		Rows []struct {
			Version, Status, Event, Counter string
			Value                           int64
		} `json:"rows"`
	}
	err = json.NewDecoder(resp.Body).Decode(&stats)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stats/usage: %d %v", resp.StatusCode, err)
	}
	counts := map[string]int64{}
	for _, r := range stats.Rows {
		counts[r.Event+"/"+r.Counter+"/"+r.Status+"/"+r.Version] += r.Value
	}
	if len(counts) != 3 || counts["load//valid/1.2.0"] != 1 || counts["attach//valid/1.2.0"] != 1 || counts["attach/sources/valid/1.2.0"] != 3 {
		t.Fatalf("usage in /stats/usage: %v", counts)
	}

	// at LOAD itself, with nothing calling the extension: the hook's own connection sees a global
	// setting and the http secret (a session-only setting, as above, defers it to the first use)
	mark = k.mark()
	global := append([]string{}, boot...)
	global[3] = "SET GLOBAL ca_cert_file = " + sqlString(testCA(t).caFile)
	mustOK(t, newSession(t, b, opts).exec(append(global, secret, "LOAD kista_demo FROM r")...))
	if got := k.statuses("/-/licenses/kista_demo", mark); len(got) != 1 || got[0] != 304 {
		t.Fatalf("the fetch at LOAD: %v", got)
	}

	// an unchanged licence answers 304 (here at the first use: the session's own ca_cert_file)
	mark = k.mark()
	res = newSession(t, b, opts).exec(append(boot, secret, "LOAD kista_demo FROM r", licenseRow)...)
	mustOK(t, res)
	if got := k.statuses("/-/licenses/kista_demo", mark); len(got) != 1 || got[0] != 304 {
		t.Fatalf("the unchanged licence: %v", got)
	}

	// an upgrade: a new licence with another plan, seen without reinstalling at a new process's
	// LOAD (from the repository: the file still holds the old one)
	ent, err := k.licenses.Import(ctx, serveAdmin, "acme", sign("lic-ent", "enterprise", now-30, now+60*86400, ""), stored.Version)
	if err != nil {
		t.Fatal(err)
	}
	mark = k.mark()
	res = newSession(t, b, opts).exec(append(boot, secret, "LOAD kista_demo FROM r", licenseRow)...)
	mustOK(t, res)
	if got := row(t, res[len(boot)+2]); got[0] != "valid" || got[1] != "enterprise" || got[2] != "repository" {
		t.Fatalf("a new process: %v", got)
	}
	if got := k.statuses("/-/licenses/kista_demo", mark); len(got) != 1 || got[0] != 200 {
		t.Fatalf("the new licence fetched: %v", got)
	}
	// and in a running process with myext_license_refresh(): without the secret first, the file
	if _, err := k.licenses.Import(ctx, serveAdmin, "acme", sign("lic-biz", "business", now-20, now+90*86400, ""),
		ent.Version); err != nil {
		t.Fatal(err)
	}
	res = newSession(t, b, opts).exec(append(boot, "LOAD kista_demo FROM r", licenseRow, secret,
		"SELECT status, plan, source FROM kista_demo_license_refresh()")...)
	mustOK(t, res)
	if got := row(t, res[len(boot)+1]); got[1] != "enterprise" || got[2] != "file" {
		t.Fatalf("without the secret, the file: %v", got)
	}
	if got := row(t, res[len(boot)+3]); got[0] != "valid" || got[1] != "business" || got[2] != "repository" {
		t.Fatalf("after refresh: %v", got)
	}

	// offline (no secret: the repository answers 401, as an unreachable one would get no answer): the
	// file is used
	offline := func(t *testing.T, token string) []result {
		t.Helper()
		if token == "" {
			os.Remove(file)
		} else if err := os.WriteFile(file, []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return newSession(t, b, opts).exec(append(boot, "LOAD kista_demo FROM r",
			"SELECT status, source FROM kista_demo_license()", "SELECT * FROM kista_demo_use(1)")...)
	}
	res = offline(t, sign("lic-file", "pro", now-60, now+86400, ""))
	mustOK(t, res)
	if got := row(t, res[len(boot)+1]); got[0] != "valid" || got[1] != "file" {
		t.Fatalf("offline: %v", got)
	}
	// in grace: allowed
	res = offline(t, sign("lic-grace", "pro", now-10*86400, now-86400, `,"grace":604800`))
	mustOK(t, res)
	if got := row(t, res[len(boot)+1]); got[0] != "grace" {
		t.Fatalf("grace: %v", got)
	}
	// expired, and missing: refused by Check with its message
	res = offline(t, sign("lic-old", "pro", now-10*86400, now-86400, `,"grace":0`))
	mustOK(t, res[:len(res)-1])
	mustFail(t, res[len(res)-1], "the licence for kista_demo expired")
	res = offline(t, "")
	mustOK(t, res[:len(res)-1])
	if got := row(t, res[len(boot)+1]); got[0] != "missing" {
		t.Fatalf("missing: %v", got)
	}
	mustFail(t, res[len(res)-1], "no licence for kista_demo: ask your administrator, or place kista_demo.kista-license next to the extension")
	// a licence signed by a key the SDK does not trust: invalid
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	res = offline(t, signWith(other, "lic-other", "pro", now-60, now+86400, ""))
	mustOK(t, res[:len(res)-1])
	mustFail(t, res[len(res)-1], "kid_untrusted")
}
