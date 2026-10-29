package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/api"
	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/stats"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0033 phase B part 1: a distributor's publisher, without grants and with a hold its
// distributor set, publishes into the tenant's catalogue only (its channel route refused); prod's
// auto rule places the build once the hold has passed, and DuckDB at the pin installs it; a
// withdrawal then makes the version served nowhere: a fresh INSTALL fails.
func TestDistributorWithdrawal(t *testing.T) {
	b := needBuild(t)
	k := startKista(t, b)
	ctx := context.Background()
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}}
	if _, err := adm.AddDistributorPublisher(ctx, serveAdmin, "acme", "enterest-ci", "enterest"); err != nil {
		t.Fatal(err)
	}
	key := publisherSigner(t)
	if _, err := adm.AddPublisherKey(ctx, serveAdmin, "acme", "enterest-ci", authorizedKey(key), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.SetPublisherHold(ctx, serveAdmin, "acme", "enterest-ci", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	const demo = "loadable_extension_demo"
	if st := k.publish(t, demo, key); st != 404 {
		t.Fatalf("a distributor's publisher on prod's route: %d", st)
	}
	if st := k.publishTo(t, "catalogue/extensions/"+demo+"/builds", demo, key, url.Values{"visibility": {"public"}}); st != 201 {
		t.Fatalf("into the catalogue: %d", st)
	}
	if _, err := k.rel.SetSource(ctx, serveAdmin, "acme", "prod", demo, "publisher:enterest-ci", store.RuleAuto); err != nil {
		t.Fatal(err)
	}
	placer := &release.Placer{Service: k.rel, Holder: "e2e", Log: slog.New(slog.DiscardHandler)}
	placed := func() bool {
		rs, err := k.rel.List(ctx, serveAdmin, "acme", "prod", demo)
		if err != nil {
			t.Fatal(err)
		}
		return len(rs) > 0
	}
	for deadline := time.Now().Add(10 * time.Second); !placed(); {
		if time.Now().After(deadline) {
			t.Fatal("the pass did not place the build when its hold ended")
		}
		placer.Once(ctx, false)
		if !placed() {
			time.Sleep(250 * time.Millisecond)
		}
	}
	pem := k.pem(t, k.activeKey(t))
	mustOK(t, newSession(t, b, nil).exec(createRepo("r", k.httpURL(), pem), "INSTALL "+demo+" FROM r", "LOAD "+demo+" FROM r"))

	// withdrawn by the distributor: served nowhere from then on
	data, err := os.ReadFile(k.b.extensions[demo])
	if err != nil {
		t.Fatal(err)
	}
	ef, err := extfile.Open(bytes.NewReader(data), int64(len(data)), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	w, err := k.rel.Withdraw(ctx, serveAdmin, "acme", "", "publisher:enterest-ci", demo, ef.Metadata.ExtensionVersion, "CVE-2026-0001")
	if err != nil || len(w.Builds) != 1 {
		t.Fatalf("withdrawn: %+v %v", w, err)
	}
	install := func() {
		t.Helper()
		res := newSession(t, b, nil).exec(createRepo("r", k.httpURL(), pem), "INSTALL "+demo+" FROM r")
		mustOK(t, res[:1])
		mustFail(t, res[1], "failed to download") // served nowhere: 404
	}
	install()
	placer.Once(ctx, true) // the sweep places nothing of it again
	install()
}

// Spec 0036 phase 2: a distributor's publisher in four tenants publishes kista_demo into each
// catalogue and sets its metadata (the icon route serves the bytes with kista's headers); prod
// places it in each, and DuckDB at the pin installs it from each with a token of the tenant's (an
// installer each), and from acme's own build of the name in another channel. With the store's clock
// past the week, the counts flushed and the daily pass run, the distributor's statistics answer one
// row, without the tenant's own build: t4 is public, and the three others reach the minimum (3), so
// four downloads and four installer-days. A pass with the minimum at 4 leaves the snapshot as it
// is; computed again at 4 (the snapshot deleted), the row is withheld: t4's downloads only, no
// installer-days.
func TestDistributorStatistics(t *testing.T) {
	b := needBuild(t)
	idpKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &e2eIDP{key: idpKey}
	ops := auth.ServerIssuerID("ops")
	k := startKistaOpts(t, b, kistaOpts{verifier: &auth.Verifier{Fetch: idp}, api: func(o *api.Options) {
		o.Server = &auth.Server{Issuers: []store.Issuer{{ID: ops, Name: "ops", URL: e2eIssuer, Algorithms: auth.Algorithms,
			RolesClaim: []string{"roles"}, MaxTokenLifetime: 24 * time.Hour}}, Audiences: []string{"api://kista"},
			DistributionAdmins: auth.Principals{{IssuerID: ops, Kind: store.PrincipalRole, Value: "kista.distribution"}: true},
			Verifier:           &auth.Verifier{Fetch: idp}}
	}})
	ctx := context.Background()
	const demo = "kista_demo"
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}, Fetch: idp, PublicURL: "https://" + k.addr}
	k.release("httpfs", release.AddOptions{}) // acme's, for DuckDB's https client
	data, err := os.ReadFile(k.b.extensions[demo])
	if err != nil {
		t.Fatal(err)
	}
	ef, err := extfile.Open(bytes.NewReader(data), int64(len(data)), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	// channel makes a tenant's channel (acme's prod is the harness's) and answers its key, for DuckDB
	channel := func(tenant, ch string) string {
		t.Helper()
		if tenant != "acme" || ch != "prod" {
			if _, err := k.ten.CreateChannel(ctx, serveAdmin, tenant, ch, store.ChannelSigned, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := k.ten.SetChannelVersions(ctx, serveAdmin, tenant, ch, []string{b.versionDir}, nil); err != nil {
				t.Fatal(err)
			}
			file := tenant + "-" + ch + ".pem"
			if err := signer.GenerateKeyFile(filepath.Join(k.keyDir, file)); err != nil {
				t.Fatal(err)
			}
			if _, err := k.keys.Add(ctx, serveAdmin, tenant, ch, "file:"+file, true); err != nil {
				t.Fatal(err)
			}
		}
		ks, err := k.keys.List(ctx, serveAdmin, tenant, ch)
		if err != nil || len(ks) != 1 {
			t.Fatalf("%s/%s's key: %v", tenant, ch, err)
		}
		return k.pem(t, ks[0])
	}
	pool := x509.NewCertPool()
	caPEM, _ := os.ReadFile(testCA(t).caFile)
	pool.AppendCertsFromPEM(caPEM)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	do := func(req *http.Request) (*http.Response, []byte) {
		t.Helper()
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}
	icon := testIcon(t)
	bootPEM := channel("acme", "prod")
	var installs [][]string
	for i, tn := range []string{"acme", "t2", "t3", "t4"} {
		if tn != "acme" {
			if _, err := k.ten.CreateTenant(ctx, serveAdmin, tn, "", "", ""); err != nil {
				t.Fatal(err)
			}
		}
		pem := channel(tn, "prod")
		// alice installs from the tenant's prod with a token of the tenant's
		aud := "api://kista-" + tn
		if _, err := adm.AddIssuer(ctx, serveAdmin, tn, store.Issuer{Name: "corp", URL: e2eIssuer}); err != nil {
			t.Fatal(err)
		}
		if err := adm.AddAudience(ctx, serveAdmin, tn, aud); err != nil {
			t.Fatal(err)
		}
		if _, err := adm.AddGrant(ctx, serveAdmin, tn, "subject:corp|alice", []string{"install"}, tenants.GrantOn{Channel: "prod"}); err != nil {
			t.Fatal(err)
		}
		if _, err := adm.AddDistributorPublisher(ctx, serveAdmin, tn, "enterest-ci", "enterest"); err != nil {
			t.Fatal(err)
		}
		key := publisherSigner(t)
		if _, err := adm.AddPublisherKey(ctx, serveAdmin, tn, "enterest-ci", authorizedKey(key), time.Now().Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if st := k.publishFileIn(t, tn, "catalogue/extensions/"+demo+"/builds", k.b.extensions[demo], key,
			url.Values{"visibility": {"public"}}); st != 201 {
			t.Fatalf("%s: into the catalogue: %d", tn, st)
		}
		// its metadata, by a signed request; the icon served with kista's headers
		entry := "https://" + k.addr + "/api/v1/tenants/" + tn + "/catalogue/publisher:enterest-ci/extensions/" + demo
		body, _ := json.Marshal(map[string]any{"summary": "A demo",
			"icon": map[string]string{"content_type": "image/png", "data": base64.StdEncoding.EncodeToString(icon)}})
		req, _ := http.NewRequest("PUT", entry+"/meta", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		signRequest(t, req, key, body)
		if resp, got := do(req); resp.StatusCode != 200 {
			t.Fatalf("%s: the metadata: %d %s", tn, resp.StatusCode, got)
		}
		req, _ = http.NewRequest("GET", entry+"/icon", nil)
		signRequest(t, req, key, nil)
		resp, got := do(req)
		if resp.StatusCode != 200 || !bytes.Equal(got, icon) {
			t.Fatalf("%s: the icon: %d", tn, resp.StatusCode)
		}
		for h, v := range map[string]string{"Content-Type": "image/png", "X-Content-Type-Options": "nosniff",
			"Content-Security-Policy": "default-src 'none'; sandbox", "Cross-Origin-Resource-Policy": "same-origin",
			"Content-Disposition": `inline; filename="icon.png"`, "Cache-Control": "private, no-store"} {
			if got := resp.Header.Get(h); got != v {
				t.Fatalf("%s: the icon's %s: %q", tn, h, got)
			}
		}
		o := release.PlaceOptions{Source: "publisher:enterest-ci", Version: ef.Metadata.ExtensionVersion}
		if _, _, err := k.rel.Place(ctx, serveAdmin, tn, "prod", demo, o); err != nil {
			t.Fatalf("%s: placed: %v", tn, err)
		}
		r := fmt.Sprintf("r%d", i)
		installs = append(installs, []string{
			createRepo("boot", k.httpURL(), bootPEM),
			"INSTALL httpfs FROM boot",
			"LOAD httpfs FROM boot",
			"SET ca_cert_file = " + sqlString(testCA(t).caFile),
			createRepo(r, "https://"+k.addr+"/"+tn+"/prod", pem),
			"CREATE SECRET s (TYPE http, BEARER_TOKEN " + sqlString(idp.token(t, "alice", aud)) + ", SCOPE " +
				sqlString("https://"+k.addr+"/"+tn+"/prod/") + ")",
			"INSTALL " + demo + " FROM " + r,
		})
	}
	// acme's own build of the name, in a channel of its own
	pem := channel("acme", "own")
	if _, _, err := k.rel.Add(ctx, serveAdmin, "acme", "own", bytes.NewReader(data), release.AddOptions{Name: demo}); err != nil {
		t.Fatal(err)
	}
	installs = append(installs, []string{createRepo("own", "http://"+k.addr+"/acme/own", pem), "INSTALL " + demo + " FROM own"})
	for _, s := range installs { // a database each: an installed extension is not downloaded again
		mustOK(t, newSession(t, b, nil).exec(s...))
	}

	// past the week: the counts flushed, the install events written, the daily pass run
	k.counts.Flush(ctx)
	k.events.Close(ctx)
	today := time.Now().UTC()
	monday := stats.LastCompleteWeek(today.AddDate(0, 0, 9)) // today's week, complete in 9 days
	k.clock.set(monday.AddDate(0, 0, 9).Add(time.Hour))
	daily := func(minTenants int) {
		(&stats.Daily{Store: k.st, Holder: "e2e", EventRetention: 30 * 24 * time.Hour, Days: 10, Log: slog.New(slog.DiscardHandler),
			PublicTenants: []string{"t4"}, MinTenants: minTenants}).Once(ctx)
	}
	daily(3)
	if done, err := k.st.StatisticsDayDone(ctx, store.DayOf(today)); err != nil || !done {
		t.Fatalf("the daily pass: %v %v", done, err)
	}

	// the route, over https, with a distribution admin's token
	tok := idp.sign(t, map[string]any{"iss": e2eIssuer, "sub": "storefront", "aud": "api://kista", "roles": []any{"kista.distribution"}})
	type row struct {
		Week          string `json:"week"`
		Extension     string `json:"extension"`
		Downloads     int64  `json:"downloads"`
		Public        int64  `json:"public"`
		InstallerDays *int64 `json:"installer_days"`
		Withheld      bool   `json:"withheld"`
		MinTenants    int    `json:"min_tenants"`
	}
	statistics := func() []row {
		t.Helper()
		req, _ := http.NewRequest("GET", "https://"+k.addr+"/api/v1/distributor-stats/downloads?distributor=enterest", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, body := do(req)
		var out struct {
			Rows []row `json:"rows"`
		}
		if resp.StatusCode != 200 || json.Unmarshal(body, &out) != nil {
			t.Fatalf("the distributor's statistics: %d %s", resp.StatusCode, body)
		}
		return out.Rows
	}
	four := int64(4)
	want := []row{{Week: store.DayOf(monday), Extension: demo, Downloads: 4, Public: 1, InstallerDays: &four, MinTenants: 3}}
	if rows := statistics(); !reflect.DeepEqual(rows, want) {
		t.Fatalf("the minimum 3: %+v", rows)
	}
	daily(4) // a week is computed once
	if rows := statistics(); !reflect.DeepEqual(rows, want) {
		t.Fatalf("a snapshot computed again: %+v", rows)
	}
	for _, q := range []string{"DELETE FROM distributor_weeks", "DELETE FROM distributor_weeks_done"} {
		if err := store.ExecRaw(ctx, k.st, q); err != nil {
			t.Fatal(err)
		}
	}
	daily(4)
	if rows := statistics(); !reflect.DeepEqual(rows, []row{{Week: store.DayOf(monday), Extension: demo, Downloads: 1, Public: 1, Withheld: true,
		MinTenants: 4}}) {
		t.Fatalf("the minimum 4: %+v", rows)
	}
}

// testIcon is a 64x64 PNG as Go's encoder writes it (no optional chunks).
func testIcon(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
