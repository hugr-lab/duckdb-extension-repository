package e2e

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0033 phase C: the spec's end to end with the pinned DuckDB. A tenant's repository upstream
// (another kista) and a distributor's publisher both bring geo_tools (played by
// loadable_extension_demo: the pin builds no geo_tools, and kista's file checks need the name's
// entry point), each its own body. prod serves the upstream's by its home rule, nightly the
// distributor's by an auto rule once its hold has passed; prod's follow of the distributor takes its
// other name (kista_demo) and lists geo_tools as skipped; a withdrawal stops nightly's install at
// once, and a held version placed early is installed there. The licence part is
// TestLicenseFromDistributor.
func TestCatalogueSources(t *testing.T) {
	b := needBuild(t)
	ctx := context.Background()
	const geo = "loadable_extension_demo"
	src := startKista(t, b)
	src.release(geo, release.AddOptions{})
	k := startKista(t, b)
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

	// the upstream: prod its home channel, whose first release writes prod's rule
	_, port, _ := net.SplitHostPort(src.addr)
	eg, err := egress.New(egress.Config{AllowLoopbackHTTP: true, OwnHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	up := &upstream.Service{Store: k.st, Releases: k.rel, Blob: k.blob, Fetch: eg, Authz: authz.ServerAdmin{},
		MaxBody: 256 << 20, MaxIngests: 2, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	if _, err := up.Add(ctx, serveAdmin, "acme", upstream.Spec{Name: "org", Kind: store.UpstreamRepo, Prefix: "http://localhost:" + port + "/acme/prod",
		Channel: "prod", Keys: []string{src.activeKey(t).Fingerprint}, Platforms: []string{b.platform}, Visibility: store.Public,
		Entries: []store.UpstreamEntry{{Name: geo}}}); err != nil {
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

	// the distributor's publisher, with a hold of the server's smallest unit
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}}
	if _, err := adm.AddDistributorPublisher(ctx, serveAdmin, "acme", "enterest-ci", "enterest"); err != nil {
		t.Fatal(err)
	}
	key := publisherSigner(t)
	if _, err := adm.AddPublisherKey(ctx, serveAdmin, "acme", "enterest-ci", authorizedKey(key), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.SetPublisherHold(ctx, serveAdmin, "acme", "enterest-ci", time.Second); err != nil {
		t.Fatal(err)
	}
	const dist = "publisher:enterest-ci"
	if _, err := k.rel.SetSource(ctx, serveAdmin, "acme", "nightly", geo, dist, store.RuleAuto); err != nil {
		t.Fatal(err)
	}
	if _, err := k.rel.SetFollow(ctx, serveAdmin, "acme", "prod", dist); err != nil {
		t.Fatal(err)
	}
	public := url.Values{"visibility": {"public"}}
	if st := k.publishTo(t, "catalogue/extensions/kista_demo/builds", "kista_demo", key, public); st != 201 {
		t.Fatalf("kista_demo into the catalogue: %d", st)
	}
	if st := k.publishFile(t, "catalogue/extensions/"+geo+"/builds", withVersion(t, b.extensions[geo], "e2e-dist"), key, public); st != 201 {
		t.Fatalf("geo_tools into the catalogue: %d", st)
	}
	placer := &release.Placer{Service: k.rel, Holder: "e2e", Log: slog.New(slog.DiscardHandler)}
	placed := func(channel, name string) bool {
		t.Helper()
		rs, err := k.rel.List(ctx, serveAdmin, "acme", channel, name)
		if err != nil {
			t.Fatal(err)
		}
		return len(rs) > 0
	}
	placer.Once(ctx, false)
	if placed("nightly", geo) { // published last: within its hold still
		t.Fatal("the pass placed a held build before its hold ended")
	}
	for deadline := time.Now().Add(15 * time.Second); !placed("nightly", geo) || !placed("prod", "kista_demo"); {
		if time.Now().After(deadline) {
			t.Fatal("the pass did not place the builds when their hold ended")
		}
		time.Sleep(250 * time.Millisecond)
		placer.Once(ctx, false)
	}

	// the right body each: the upstream's in prod, the distributor's in nightly
	installed := func(channel string) any {
		t.Helper()
		res := newSession(t, b, nil).exec(
			createRepo("r", "http://"+k.addr+"/acme/"+channel, k.pem(t, activeKeyOf(t, k, channel))),
			"INSTALL "+geo+" FROM r",
			"LOAD "+geo+" FROM r",
			"SELECT extension_version FROM duckdb_extensions() WHERE extension_name = '"+geo+"'",
		)
		mustOK(t, res)
		return res[len(res)-1].Rows[0][0]
	}
	if got := installed("prod"); got == "e2e-dist" {
		t.Fatal("prod serves the distributor's geo_tools")
	}
	if got := installed("nightly"); got != "e2e-dist" {
		t.Fatalf("nightly serves %v, not the distributor's", got)
	}
	// prod's follow took kista_demo, and skips the name prod serves from its upstream
	f, err := k.rel.Follow(ctx, serveAdmin, "acme", "prod", dist)
	if err != nil || f.Skips[geo] != "the channel serves it from another source" || len(f.Skips) != 1 {
		t.Fatalf("prod's follow: %+v %v", f, err)
	}
	mustOK(t, newSession(t, b, nil).exec(createRepo("r", k.httpURL(), k.pem(t, k.activeKey(t))), "INSTALL kista_demo FROM r"))

	// withdrawn by the distributor: nightly's install fails at once
	if w, err := k.rel.Withdraw(ctx, serveAdmin, "acme", "", dist, geo, "e2e-dist", "CVE-2026-0001"); err != nil || len(w.Builds) != 1 {
		t.Fatalf("withdrawn: %+v %v", w, err)
	}
	res := newSession(t, b, nil).exec(createRepo("r", "http://"+k.addr+"/acme/nightly", k.pem(t, activeKeyOf(t, k, "nightly"))),
		"INSTALL "+geo+" FROM r")
	mustOK(t, res[:1])
	mustFail(t, res[1], "failed to download")

	// held for an hour: placed early by a channel administrator
	if _, err := adm.SetPublisherHold(ctx, serveAdmin, "acme", "enterest-ci", time.Hour); err != nil {
		t.Fatal(err)
	}
	if st := k.publishFile(t, "catalogue/extensions/"+geo+"/builds", withVersion(t, b.extensions[geo], "e2e-dist-2"), key, public); st != 201 {
		t.Fatalf("the next geo_tools into the catalogue: %d", st)
	}
	o := release.PlaceOptions{Source: dist, Version: "e2e-dist-2"}
	if _, _, err := k.rel.Place(ctx, serveAdmin, "acme", "nightly", geo, o); !errors.Is(err, release.ErrHeld) {
		t.Fatalf("a held build placed without early: %v", err)
	}
	o.Early = true
	if _, _, err := k.rel.Place(ctx, serveAdmin, "acme", "nightly", geo, o); err != nil {
		t.Fatalf("placed early: %v", err)
	}
	if got := installed("nightly"); got != "e2e-dist-2" {
		t.Fatalf("nightly serves %v after the early placement", got)
	}
}
