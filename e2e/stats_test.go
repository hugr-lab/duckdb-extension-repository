package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0010 phase 2: one INSTALL is one download, whatever requests DuckDB makes for it (the
// built-in client without httpfs; httpfs with a token), and an authenticated one is one install
// event.
func TestInstallCounts(t *testing.T) {
	b := needBuild(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := &e2eIDP{key: key}
	k := startKistaWith(t, b, &auth.Verifier{Fetch: idp})
	ctx := context.Background()
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}, Fetch: idp, PublicURL: "https://" + k.addr}
	if _, err := adm.AddIssuer(ctx, serveAdmin, "acme", store.Issuer{Name: "corp", URL: e2eIssuer}); err != nil {
		t.Fatal(err)
	}
	if err := adm.AddAudience(ctx, serveAdmin, "acme", "api://kista-acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "subject:corp|alice", []string{"install"}, "prod", ""); err != nil {
		t.Fatal(err)
	}
	k.release("httpfs", release.AddOptions{})
	k.release("loadable_extension_demo", release.AddOptions{Private: true})
	pemKey := k.pem(t, k.activeKey(t))
	counts := func() (anon, authd int64) {
		t.Helper()
		k.counts.Flush(ctx)
		acme, _ := k.st.GetTenant(ctx, "acme")
		rows, err := k.st.DownloadRows(ctx, acme.ID, store.StatsFilter{From: "2000-01-01", To: "2999-12-31"}, 100, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			anon += r.Count - r.Authenticated
			authd += r.Authenticated
		}
		return anon, authd
	}
	// the built-in client: httpfs itself, anonymous, over plain http
	mustOK(t, newSession(t, b, nil).exec(createRepo("boot", k.httpURL(), pemKey), "INSTALL httpfs FROM boot"))
	if anon, authd := counts(); anon != 1 || authd != 0 {
		t.Fatalf("one INSTALL without httpfs: %d anonymous, %d authenticated", anon, authd)
	}
	// httpfs with a token: the private extension
	tok := idp.token(t, "alice", "api://kista-acme")
	mustOK(t, newSession(t, b, nil).exec(
		createRepo("boot", k.httpURL(), pemKey),
		"INSTALL httpfs FROM boot",
		"LOAD httpfs FROM boot",
		"SET ca_cert_file = "+sqlString(testCA(t).caFile),
		createRepo("r", k.httpsURL(), pemKey),
		"CREATE SECRET s (TYPE http, BEARER_TOKEN "+sqlString(tok)+", SCOPE "+sqlString(k.httpsURL()+"/")+")",
		"INSTALL loadable_extension_demo FROM r",
	))
	if anon, authd := counts(); anon != 2 || authd != 1 {
		t.Fatalf("one INSTALL with httpfs: %d anonymous, %d authenticated", anon, authd)
	}
	k.events.Close(ctx)
	acme, _ := k.st.GetTenant(ctx, "acme")
	evs, err := k.st.ListEvents(ctx, acme.ID, store.EventFilter{Kind: "install"})
	if err != nil || len(evs) != 1 || !strings.HasPrefix(evs[0].Actor, "principal:acme/") || !strings.HasSuffix(evs[0].Actor, "|alice") {
		t.Fatalf("install events: %+v %v", evs, err)
	}
}
