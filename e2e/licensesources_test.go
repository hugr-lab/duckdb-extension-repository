package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0033 phase B part 2, spec 0035: kista_demo from a distributor's publisher, marked for
// licence keys, published into the catalogue only; its licence is imported into that entry
// (?source=) before any placement and served once prod's auto rule places the build, and the
// pinned DuckDB is licensed. The tenant's upstream (another kista) then brings kista_demo into
// nightly with the same key: two entries, one licensed, so nightly serves no licence (missing) and
// prod keeps its own. Switching nightly's rule to the distributor's entry serves its licence there
// (valid); switching back answers 404 again. Spec 0035 phase 2: prod's release made public answers
// the warning and writes its event.
func TestLicenseFromDistributor(t *testing.T) {
	b := needBuild(t)
	if ext, ok := b.extensions["kista_demo"]; !ok || ext == "" {
		t.Fatal("kista_demo is not built (make e2e-duckdb)")
	}
	ctx := context.Background()
	idpKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := &e2eIDP{key: idpKey}
	signKey, err := vectors.LoadKey("../internal/license/testdata", vectors.TrustedKey)
	if err != nil {
		t.Fatal(err)
	}

	k := startKistaLicensed(t, b, &auth.Verifier{Fetch: idp}, true)
	k.installer(t, idp, "api://kista-acme", "alice")
	k.release("httpfs", release.AddOptions{})
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}, Fetch: idp, PublicURL: "https://" + k.addr}
	if _, err := adm.AddDistributorPublisher(ctx, serveAdmin, "acme", "enterest-ci", "enterest"); err != nil {
		t.Fatal(err)
	}
	k.markPublisher(t, "enterest-ci", true)
	key := publisherSigner(t)
	if _, err := adm.AddPublisherKey(ctx, serveAdmin, "acme", "enterest-ci", authorizedKey(key), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := k.publishTo(t, "catalogue/extensions/kista_demo/builds", "kista_demo", key, nil); st != 201 {
		t.Fatalf("into the catalogue: %d", st)
	}
	now := time.Now().Unix()
	tok := vectors.Sign(signKey, vectors.Header(signKey), fmt.Sprintf(`{"v":1,"iss":"https://issuer.example","jti":"lic-dist",`+
		`"sub":"acme","holder":"Acme Test","ext":"kista_demo","plan":"pro","limits":{"max_attached_sources":5},"iat":%d,"exp":%d}`,
		now-60, now+30*86400))
	v, err := k.licenses.Import(ctx, serveAdmin, "acme", "", "publisher:enterest-ci", tok, licenses.Create)
	if err != nil || v.Entry != "publisher:enterest-ci" {
		t.Fatalf("the licence, before any placement: %+v %v", v, err)
	}
	token := idp.token(t, "alice", "api://kista-acme")
	pool := x509.NewCertPool()
	caPEM, _ := os.ReadFile(testCA(t).caFile)
	pool.AppendCertsFromPEM(caPEM)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	licenseGet := func(channel string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", "https://"+k.addr+"/acme/"+channel+"/-/licenses/kista_demo", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// placed by prod's auto rule: served there, and DuckDB at the pin is licensed
	if _, err := k.rel.SetSource(ctx, serveAdmin, "acme", "prod", "kista_demo", "publisher:enterest-ci", store.RuleAuto); err != nil {
		t.Fatal(err)
	}
	if st := licenseGet("prod"); st != http.StatusNotFound {
		t.Fatalf("prod's licence before the placement: %d", st)
	}
	(&release.Placer{Service: k.rel, Holder: "e2e", Log: slog.New(slog.DiscardHandler)}).Once(ctx, false)
	if st := licenseGet("prod"); st != http.StatusOK {
		t.Fatalf("prod's licence once placed: %d", st)
	}
	pemKey := k.pem(t, k.activeKey(t))
	boot := []string{
		createRepo("boot", k.httpURL(), pemKey),
		"INSTALL httpfs FROM boot",
		"LOAD httpfs FROM boot",
		"SET ca_cert_file = " + sqlString(testCA(t).caFile),
	}
	licensed := func(channel string) []any {
		t.Helper()
		url := "https://" + k.addr + "/acme/" + channel
		r := newSession(t, b, map[string]string{"extension_directory": t.TempDir()}).exec(append(boot,
			createRepo("r", url, k.pem(t, activeKeyOf(t, k, channel))),
			"CREATE SECRET s (TYPE http, BEARER_TOKEN "+sqlString(token)+", SCOPE "+sqlString(url+"/")+")",
			"INSTALL kista_demo FROM r",
			"LOAD kista_demo FROM r",
			"SELECT status, plan FROM kista_demo_license()")...)
		mustOK(t, r)
		if rows := r[len(r)-1].Rows; len(rows) == 1 {
			return rows[0]
		}
		t.Fatalf("the licence row: %v", r[len(r)-1].Rows)
		return nil
	}
	if got := licensed("prod"); got[0] != "valid" || got[1] != "pro" {
		t.Fatalf("installed from prod: %v", got)
	}

	// the tenant's upstream brings kista_demo into nightly with the same key, copied
	src := startKistaLicensed(t, b, &auth.Verifier{Fetch: idp}, true)
	src.installer(t, idp, "api://kista-src", "mirror")
	if st := src.publish(t, "kista_demo", src.publisherKey(t, "copy-ci", true)); st != 201 {
		t.Fatalf("publish at the upstream: %d", st)
	}
	if _, err := k.ten.CreateChannel(ctx, serveAdmin, "acme", "nightly", store.ChannelSigned, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := k.ten.SetChannelVersions(ctx, serveAdmin, "acme", "nightly", []string{b.versionDir}, nil); err != nil {
		t.Fatal(err)
	}
	if err := signer.GenerateKeyFile(filepath.Join(k.keyDir, "n.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := k.keys.Add(ctx, serveAdmin, "acme", "nightly", "file:n.pem", true); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "subject:corp|alice", []string{"install"}, tenants.GrantOn{Channel: "nightly"}); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(src.addr)
	p, _ := strconv.Atoi(port)
	eg, err := egress.New(egress.Config{OwnHost: "127.0.0.1", CAFile: testCA(t).caFile,
		Allow: []egress.Allow{{Prefix: netip.MustParsePrefix("127.0.0.1/32"), Ports: []uint16{uint16(p)}},
			{Prefix: netip.MustParsePrefix("::1/128"), Ports: []uint16{uint16(p)}}}})
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(idp.token(t, "mirror", "api://kista-src")), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := credential.New([]config.Credential{{Name: "src", Tenants: []string{"acme"}, Prefixes: []string{"https://localhost:" + port + "/acme/"},
		Kind: "token_file", TokenFile: tokenFile}}, credential.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	up := &upstream.Service{Store: k.st, Releases: k.rel, Blob: k.blob, Fetch: eg, Authz: authz.ServerAdmin{}, Credentials: creds,
		MaxBody: 256 << 20, MaxIngests: 2, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	if _, err := up.Add(ctx, serveAdmin, "acme", upstream.Spec{Name: "org", Kind: store.UpstreamRepo, Prefix: "https://localhost:" + port + "/acme/prod",
		Channel: "nightly", Credential: "src", Keys: []string{src.activeKey(t).Fingerprint}, Platforms: []string{b.platform},
		Entries: []store.UpstreamEntry{{Name: "kista_demo"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := up.Sync(ctx, serveAdmin, "acme", "org", false); err != nil {
		t.Fatal(err)
	}
	(&upstream.Runner{Service: up, Holder: "e2e", Log: slog.New(slog.DiscardHandler)}).Once(ctx)
	if cells, err := up.Cells(ctx, serveAdmin, "acme", "org", "", [3]string{}, 10); err != nil || len(cells) != 1 ||
		cells[0].Outcome != store.CellReleased {
		t.Fatalf("the mirror run: %+v %v", cells, err)
	}
	ks, err := k.licenses.LicenseKeys(ctx, serveAdmin, "acme", "", "kista_demo", "")
	if err != nil || len(ks) != 1 || len(ks[0].Keys) != 1 || len(ks[0].Keys[0].Sources) != 2 {
		t.Fatalf("the key, live for both entries: %+v %v", ks, err)
	}
	// nightly serves no licence from the upstream's entry; prod keeps its own
	if st := licenseGet("nightly"); st != http.StatusNotFound {
		t.Fatalf("nightly's licence: %d", st)
	}
	if got := licensed("nightly"); got[0] != "missing" {
		t.Fatalf("installed from nightly: %v", got)
	}
	if st := licenseGet("prod"); st != http.StatusOK {
		t.Fatalf("prod's licence after the copy: %d", st)
	}
	// nightly's rule switched to the distributor's entry: its auto rule places the build, and the
	// licence is served there; switched back, kista's answer is 404 again
	if _, err := k.rel.SetSource(ctx, serveAdmin, "acme", "nightly", "kista_demo", "publisher:enterest-ci", store.RuleAuto); err != nil {
		t.Fatal(err)
	}
	(&release.Placer{Service: k.rel, Holder: "e2e", Log: slog.New(slog.DiscardHandler)}).Once(ctx, false)
	if st := licenseGet("nightly"); st != http.StatusOK {
		t.Fatalf("nightly's licence from the distributor's entry: %d", st)
	}
	if got := licensed("nightly"); got[0] != "valid" || got[1] != "pro" {
		t.Fatalf("installed from nightly, the distributor's entry: %v", got)
	}
	if _, err := k.rel.SetSource(ctx, serveAdmin, "acme", "nightly", "kista_demo", "upstream:org", ""); err != nil {
		t.Fatal(err)
	}
	if st := licenseGet("nightly"); st != http.StatusNotFound {
		t.Fatalf("nightly's licence switched back: %d", st)
	}

	// spec 0035 phase 2: prod's release (private, as the catalogue publication was) made public over
	// the API by a tenant administrator: done, with a warning in the answer and one event, advisory
	tn, err := k.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	warnings := func() []store.Event {
		t.Helper()
		evs, err := k.st.ListEvents(ctx, tn.ID, store.EventFilter{Kind: string(store.LicenseWarningKind), Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
	if evs := warnings(); len(evs) != 0 {
		t.Fatalf("warned before anything was public: %+v", evs)
	}
	if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "subject:corp|tom", []string{"admin"}, tenants.GrantOn{}); err != nil {
		t.Fatal(err)
	}
	rs, err := k.rel.List(ctx, serveAdmin, "acme", "prod", "kista_demo")
	if err != nil || len(rs) != 1 || rs[0].Visibility != store.Private {
		t.Fatalf("prod's release: %+v %v", rs, err)
	}
	req, _ := http.NewRequest("POST", "https://"+k.addr+"/api/v1/tenants/acme/channels/prod/extensions/kista_demo/releases/"+rs[0].ID+"/public", nil)
	req.Header.Set("Authorization", "Bearer "+idp.token(t, "tom", "api://kista-acme"))
	req.Header.Set("If-Match", fmt.Sprintf(`"v%d"`, rs[0].Version))
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var ans struct {
		Visibility string `json:"visibility"`
		Warnings   []struct{ Name, Source, Channel, Message string }
	}
	err = json.NewDecoder(resp.Body).Decode(&ans)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || ans.Visibility != store.Public || len(ans.Warnings) != 1 ||
		ans.Warnings[0].Name != "kista_demo" || ans.Warnings[0].Source != "publisher:enterest-ci" || ans.Warnings[0].Channel != "prod" ||
		!strings.Contains(ans.Warnings[0].Message, "advisory") {
		t.Fatalf("made public: %d %+v %v", resp.StatusCode, ans, err)
	}
	if evs := warnings(); len(evs) != 1 || !strings.Contains(evs[0].Data, `"act":"release.public"`) || evs[0].Subject != "channel:prod/ext:kista_demo" {
		t.Fatalf("the warning's event: %+v", evs)
	}
	// still served, as before: the warning changes nothing
	if st := licenseGet("prod"); st != http.StatusOK {
		t.Fatalf("prod's licence after the warning: %d", st)
	}
}

// activeKeyOf is a channel's active key.
func activeKeyOf(t *testing.T, k *kista, channel string) store.Key {
	t.Helper()
	ks, err := k.keys.List(context.Background(), serveAdmin, "acme", channel)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range ks {
		if key.State == store.KeyActive {
			return key
		}
	}
	t.Fatalf("no active key in %s", channel)
	return store.Key{}
}
