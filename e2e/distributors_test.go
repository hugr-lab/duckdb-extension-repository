package e2e

import (
	"bytes"
	"context"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
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
