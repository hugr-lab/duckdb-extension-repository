package e2e

import (
	"bytes"
	"context"
	"errors"
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

// Spec 0033 phase A2: a publisher with a hold is refused on a channel's route and publishes into the
// catalogue; DuckDB at the pin installs its build from prod only once the hold has passed and the
// placement pass, woken by the hold's end, placed it by prod's auto rule; a build held for an hour
// is installed once a channel administrator placed it early.
func TestHeldPublisher(t *testing.T) {
	b := needBuild(t)
	k := startKista(t, b)
	ctx := context.Background()
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}}
	key := k.publisherKey(t, "held-ci", false)
	// publish on its names tenant-wide: what the catalogue's route takes
	if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "publisher:held-ci", []string{"publish"}, tenants.GrantOn{}); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.SetPublisherHold(ctx, serveAdmin, "acme", "held-ci", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	const demo = "loadable_extension_demo"
	if st := k.publish(t, demo, key); st != 409 {
		t.Fatalf("a held publisher on prod's route: %d", st)
	}
	public := url.Values{"visibility": {"public"}}
	if st := k.publishTo(t, "catalogue/extensions/"+demo+"/builds", demo, key, public); st != 201 {
		t.Fatalf("into the catalogue: %d", st)
	}
	if _, err := k.rel.SetSource(ctx, serveAdmin, "acme", "prod", demo, "publisher:held-ci", store.RuleAuto); err != nil {
		t.Fatal(err)
	}
	placer := &release.Placer{Service: k.rel, Holder: "e2e", Log: slog.New(slog.DiscardHandler)}
	pem := k.pem(t, k.activeKey(t))
	placed := func(name string) bool {
		rs, err := k.rel.List(ctx, serveAdmin, "acme", "prod", name)
		if err != nil {
			t.Fatal(err)
		}
		return len(rs) > 0
	}
	placer.Once(ctx, false)
	if placed(demo) {
		t.Fatal("the pass placed a held build before its hold ended")
	}
	if res := newSession(t, b, nil).exec(createRepo("r", k.httpURL(), pem), "INSTALL "+demo+" FROM r"); res[1].OK {
		t.Fatal("installed a held build")
	}
	for deadline := time.Now().Add(15 * time.Second); !placed(demo); {
		if time.Now().After(deadline) {
			t.Fatal("the pass did not place the build when its hold ended")
		}
		time.Sleep(250 * time.Millisecond)
		placer.Once(ctx, false) // the due work only: the rule waits for the hold's end
	}
	mustOK(t, newSession(t, b, nil).exec(createRepo("r", k.httpURL(), pem),
		"INSTALL "+demo+" FROM r", "LOAD "+demo+" FROM r", "SELECT test_alias_hello()"))

	// held for an hour: placed early by a channel administrator, audited
	if _, err := adm.SetPublisherHold(ctx, serveAdmin, "acme", "held-ci", time.Hour); err != nil {
		t.Fatal(err)
	}
	if st := k.publishTo(t, "catalogue/extensions/kista_demo/builds", "kista_demo", key, public); st != 201 {
		t.Fatalf("kista_demo into the catalogue: %d", st)
	}
	data, err := os.ReadFile(k.b.extensions["kista_demo"])
	if err != nil {
		t.Fatal(err)
	}
	ef, err := extfile.Open(bytes.NewReader(data), int64(len(data)), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	o := release.PlaceOptions{Source: "publisher:held-ci", Version: ef.Metadata.ExtensionVersion}
	if _, _, err := k.rel.Place(ctx, serveAdmin, "acme", "prod", "kista_demo", o); !errors.Is(err, release.ErrHeld) {
		t.Fatalf("a held build placed without early: %v", err)
	}
	o.Early = true
	if _, _, err := k.rel.Place(ctx, serveAdmin, "acme", "prod", "kista_demo", o); err != nil {
		t.Fatalf("placed early: %v", err)
	}
	mustOK(t, newSession(t, b, nil).exec(createRepo("r", k.httpURL(), pem), "INSTALL kista_demo FROM r"))
}
