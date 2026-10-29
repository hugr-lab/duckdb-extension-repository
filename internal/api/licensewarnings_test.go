package api_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0035 phase 2: a write's licence warnings are answered only to a caller who administers the
// path; a licence manager's import, a promote-only principal's and a publisher's signed promotion
// answer none, while each writes its event.
func TestLicenseWarningAnswers(t *testing.T) {
	m := newMgmt(t)
	const T = "/api/v1/tenants/acme"
	tom, lm := m.toks["tenant admin"], m.toks["server licence mgr"]
	tn, err := m.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	// tresor's builds are a distributor's publisher's (nobody), checked, its key declared; prod has
	// its public releases
	if err := m.st.InTx(ctx, "", func(tx *store.Tx) error { // a distributor's publisher, marked at its creation
		return tx.InsertPublisher(ctx, &store.Publisher{TenantID: tn.ID, Name: "nobody", CreatedBy: "os:1:t", Distributor: "enterest"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.LicenseKey(ctx, m.st, tn.ID, "tresor", store.KeySourcePublisher, "nobody", &m.licKey.PublicKey); err != nil {
		t.Fatal(err)
	}
	if err := store.ExecRaw(ctx, m.st, "UPDATE builds SET checked = ? WHERE name = ?", true, "tresor"); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []string{"edge", "edge2"} {
		if _, err := m.env.ten.CreateChannel(ctx, admin, "acme", ch, store.ChannelSigned, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := m.env.ten.SetChannelVersions(ctx, admin, "acme", ch, []string{"v2.0.0"}, nil); err != nil {
			t.Fatal(err)
		}
		if err := signer.GenerateKeyFile(filepath.Join(m.dir, ch+".pem")); err != nil {
			t.Fatal(err)
		}
		if _, err := m.env.keys.Add(ctx, admin, "acme", ch, "file:"+ch+".pem", true); err != nil {
			t.Fatal(err)
		}
	}
	events := 0
	check := func(what string, r resp, status int, channels ...string) {
		t.Helper()
		if r.status != status {
			t.Fatalf("%s: %d %s", what, r.status, r.body)
		}
		evs, err := m.st.ListEvents(ctx, tn.ID, store.EventFilter{Kind: string(store.LicenseWarningKind), Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if want := events + max(1, len(channels)); len(evs) != want { // one per channel; one unanswered
			t.Fatalf("%s: %d events, want %d: %+v", what, len(evs), want, evs)
		}
		events = len(evs)
		ws, answered := r.json(t)["warnings"]
		if len(channels) == 0 {
			if answered || strings.Contains(string(r.body), "warnings") {
				t.Fatalf("%s: warnings answered: %s", what, r.body)
			}
			return
		}
		if got := fmt.Sprint(ws); !strings.Contains(got, "source:publisher:nobody") || !strings.Contains(got, "channel:"+channels[0]) ||
			len(ws.([]any)) != len(channels) {
			t.Fatalf("%s: warnings %v", what, ws)
		}
	}

	// a licence manager's import into the distributor's publisher's entry, served publicly in prod:
	// the event, no warning answered
	check("a licence manager's import", m.call(t, "POST", T+"/licenses?source=publisher:nobody", lm, m.licence("tresor", "pro")), 201)
	// a tenant administrator's placement into staging (placing takes promote, which admin does not
	// imply): answered
	if _, err := m.adm.AddGrant(ctx, admin, "acme", "subject:corp|tom", []string{"promote"}, tenants.GrantOn{}); err != nil {
		t.Fatal(err)
	}
	check("an administrator's placement", m.call(t, "POST", T+"/channels/staging/extensions/tresor/releases/place", tom,
		`{"source":"publisher:nobody","version":"1.0"}`), 201, "staging")
	// a promote-only principal (publish and promote on tresor, no admin): the event only
	for _, g := range []tenants.GrantOn{{Channel: "staging", Package: "tresor"}, {Channel: "edge", Package: "tresor"}} {
		if _, err := m.adm.AddGrant(ctx, admin, "acme", "subject:corp|pam", []string{"publish", "promote"}, g); err != nil {
			t.Fatal(err)
		}
	}
	pam := m.keys.sign(t, idpURL, map[string]any{"sub": "pam", "aud": publicURL + "/acme"})
	check("a promote-only principal's promotion", m.call(t, "POST", T+"/channels/edge/extensions/tresor/releases/promote", pam,
		`{"from_channel":"staging","version":"1.0"}`), 201)
	// a publisher's signed promotion: the event only
	if r := m.call(t, "POST", T+"/publishers", tom, `{"name":"edge-ci"}`); r.status != 201 {
		t.Fatalf("add publisher: %d %s", r.status, r.body)
	}
	for _, body := range []string{`{"principal":"publisher:edge-ci","verbs":["publish"],"channel":"staging","extension":"tresor"}`,
		`{"principal":"publisher:edge-ci","verbs":["promote"],"channel":"edge2","extension":"tresor"}`} {
		if r := m.call(t, "POST", T+"/grants", tom, body); r.status != 201 {
			t.Fatalf("grant: %d %s", r.status, r.body)
		}
	}
	ci := m.addKey(t, tom, "acme", "edge-ci")
	check("a publisher's signed promotion", m.call(t, "POST", T+"/channels/edge2/extensions/tresor/releases/promote", ci,
		`{"from_channel":"staging","version":"1.0"}`), 201)
	// a server administrator makes the licence public (spec 0037: a distributor's entry is not the
	// tenant administrators' to write): answered for every channel serving the entry
	g := m.call(t, "GET", T+"/licenses/tresor?source=publisher:nobody", tom, "")
	check("an administrator makes it public", m.call(t, "POST", T+"/licenses/tresor/public?source=publisher:nobody", m.toks["server admin"], "",
		"If-Match", g.header.Get("ETag")), 200, "edge", "edge2", "prod", "staging")
}
