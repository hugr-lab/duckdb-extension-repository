package e2e

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0039 PR 1: kista A's distributor's publisher publishes into A's prod; kista B mirrors it with
// an explicit entry, the switch off. A yanks it: B shows the revocation and still serves. Switched
// on, B's next run yanks it in its home channel; lifted, it is served again. B promotes it to
// nightly, and kista C mirrors B's prod with the switch on. A withdraws the version: B's next run
// blocks it for the upstream, so the flat and the versioned INSTALL fail in B's prod and nightly;
// C's next run carries the withdrawal on. B's administrator lifts it and both channels serve it
// again.
func TestUpstreamRevocations(t *testing.T) {
	b := needBuild(t)
	ctx := context.Background()
	const demo = "loadable_extension_demo"

	// A: a distributor's version, placed into prod, public
	a := startKista(t, b)
	adm := &tenants.AuthAdmin{Store: a.st, Authz: authz.ServerAdmin{}}
	if _, err := adm.AddDistributorPublisher(ctx, serveAdmin, "acme", "enterest-ci", "enterest"); err != nil {
		t.Fatal(err)
	}
	if _, err := adm.SetDistributorHold(ctx, serveAdmin, "acme", 0); err != nil {
		t.Fatal(err)
	}
	key := publisherSigner(t)
	if _, err := adm.AddPublisherKey(ctx, serveAdmin, "acme", "enterest-ci", authorizedKey(key), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := a.publishTo(t, "catalogue/extensions/"+demo+"/builds", demo, key, url.Values{"visibility": {"public"}}); st != 201 {
		t.Fatalf("into A's catalogue: %d", st)
	}
	if _, err := a.rel.SetSource(ctx, serveAdmin, "acme", "prod", demo, "publisher:enterest-ci", store.RuleAuto); err != nil {
		t.Fatal(err)
	}
	(&release.Placer{Service: a.rel, Holder: "e2e", Log: slog.New(slog.DiscardHandler)}).Once(ctx, false)
	inA, err := a.rel.List(ctx, serveAdmin, "acme", "prod", demo)
	if err != nil || len(inA) != 1 {
		t.Fatalf("placed in A: %+v %v", inA, err)
	}
	data, err := os.ReadFile(b.extensions[demo])
	if err != nil {
		t.Fatal(err)
	}
	ef, err := extfile.Open(bytes.NewReader(data), int64(len(data)), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	version := ef.Metadata.ExtensionVersion

	// a mirror of another kista's prod, public, through egress to that kista's port only
	mirror := func(k, of *kista, revocations bool) *upstream.Service {
		t.Helper()
		_, port, _ := net.SplitHostPort(of.addr)
		p, _ := strconv.Atoi(port)
		eg, err := egress.New(egress.Config{OwnHost: "127.0.0.1", CAFile: testCA(t).caFile,
			Allow: []egress.Allow{{Prefix: netip.MustParsePrefix("127.0.0.1/32"), Ports: []uint16{uint16(p)}},
				{Prefix: netip.MustParsePrefix("::1/128"), Ports: []uint16{uint16(p)}}}})
		if err != nil {
			t.Fatal(err)
		}
		up := &upstream.Service{Store: k.st, Releases: k.rel, Blob: k.blob, Fetch: eg, Authz: authz.ServerAdmin{}, MaxBody: 256 << 20,
			MaxIngests: 2, TempDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
		if _, err := up.Add(ctx, serveAdmin, "acme", upstream.Spec{Name: "src", Kind: store.UpstreamRepo, Prefix: "https://localhost:" + port + "/acme/prod",
			Channel: "prod", Visibility: store.Public, Keys: []string{of.activeKey(t).Fingerprint}, Platforms: []string{b.platform},
			Entries: []store.UpstreamEntry{{Name: demo}}, Revocations: revocations}); err != nil {
			t.Fatal(err)
		}
		return up
	}
	run := func(up *upstream.Service) {
		t.Helper()
		if _, err := up.Sync(ctx, serveAdmin, "acme", "src", false); err != nil {
			t.Fatal(err)
		}
		(&upstream.Runner{Service: up, Holder: "e2e", Log: slog.New(slog.DiscardHandler)}).Once(ctx)
	}
	stored := func(up *upstream.Service) []store.UpstreamRevocation {
		t.Helper()
		rs, err := up.RevocationsOf(ctx, serveAdmin, "acme", "src")
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	// installs answers whether DuckDB installs the demo from a kista's channel, by the flat path and
	// by the versioned one, each in a fresh session
	installs := func(k *kista, channel string) (flat, versioned bool) {
		t.Helper()
		repo := createRepo("r", "http://"+k.addr+"/acme/"+channel, k.pem(t, activeKeyOf(t, k, channel)))
		f := newSession(t, b, nil).exec(repo, "INSTALL "+demo+" FROM r")
		v := newSession(t, b, nil).exec(repo, "INSTALL "+demo+" FROM r VERSION "+sqlString(version))
		mustOK(t, f[:1])
		mustOK(t, v[:1])
		return f[1].OK, v[1].OK
	}
	mustServe := func(k *kista, channel string, want bool) {
		t.Helper()
		if f, v := installs(k, channel); f != want || v != want {
			t.Fatalf("%s serves the demo: flat %v, versioned %v; want %v", channel, f, v, want)
		}
	}

	// B mirrors A, the switch off; A yanks: B shows it and still serves
	bk := startKista(t, b)
	bUp := mirror(bk, a, false)
	run(bUp)
	mustServe(bk, "prod", true)
	if _, err := a.rel.Apply(ctx, serveAdmin, "acme", "prod", demo, inA[0].ID, release.Yank, 0); err != nil {
		t.Fatal(err)
	}
	run(bUp)
	if rs := stored(bUp); len(rs) != 1 || rs[0].State != store.RevokeYanked || rs[0].AppliedState != "" {
		t.Fatalf("B stores the yank, switched off: %+v", rs)
	}
	mustServe(bk, "prod", true)

	// switched on: the next run yanks it in B's prod; lifted, it is served again
	if _, err := bUp.SetRevocations(ctx, serveAdmin, "acme", "src", true, 0); err != nil {
		t.Fatal(err)
	}
	run(bUp)
	rs := stored(bUp)
	if len(rs) != 1 || rs[0].AppliedState != store.RevokeYanked {
		t.Fatalf("B applies the yank: %+v", rs)
	}
	mustServe(bk, "prod", false)
	if n, err := bUp.LiftRevocation(ctx, serveAdmin, "acme", "src", rs[0].BuildID, 0); err != nil || n != 1 {
		t.Fatalf("B lifts the yank: %d %v", n, err)
	}
	mustServe(bk, "prod", true)
	run(bUp) // a lifted yank is not applied again
	mustServe(bk, "prod", true)

	// B promotes it to nightly; C mirrors B's prod, the switch on
	if _, err := bk.ten.CreateChannel(ctx, serveAdmin, "acme", "nightly", store.ChannelSigned, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := bk.ten.SetChannelVersions(ctx, serveAdmin, "acme", "nightly", []string{b.versionDir}, nil); err != nil {
		t.Fatal(err)
	}
	if err := signer.GenerateKeyFile(filepath.Join(bk.keyDir, "n.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := bk.keys.Add(ctx, serveAdmin, "acme", "nightly", "file:n.pem", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bk.rel.Promote(ctx, serveAdmin, "acme", "nightly", demo, release.PromoteOptions{From: "prod", Version: version}); err != nil {
		t.Fatal(err)
	}
	mustServe(bk, "nightly", true)
	ck := startKista(t, b)
	cUp := mirror(ck, bk, true)
	run(cUp)
	mustServe(ck, "prod", true)

	// A withdraws the version: B blocks it for the upstream, in prod and nightly; C carries it on
	if _, err := a.rel.Withdraw(ctx, serveAdmin, "acme", "", "publisher:enterest-ci", demo, version, "CVE-2026-0001"); err != nil {
		t.Fatal(err)
	}
	run(bUp)
	if rs := stored(bUp); len(rs) != 1 || rs[0].AppliedState != store.RevokeWithdrawn || rs[0].Reason != "CVE-2026-0001" {
		t.Fatalf("B applies the withdrawal: %+v", rs)
	}
	mustServe(bk, "prod", false)
	mustServe(bk, "nightly", false)
	run(cUp)
	if rs := stored(cUp); len(rs) != 1 || rs[0].AppliedState != store.RevokeWithdrawn || rs[0].Reason != "CVE-2026-0001" {
		t.Fatalf("C applies the withdrawal along the chain: %+v", rs)
	}
	mustServe(ck, "prod", false)

	// B's administrator lifts it: both channels serve it again, and a run does not apply it again
	if n, err := bUp.LiftRevocation(ctx, serveAdmin, "acme", "src", rs[0].BuildID, 0); err != nil || n != 2 {
		t.Fatalf("B lifts the withdrawal: %d %v", n, err)
	}
	mustServe(bk, "prod", true)
	mustServe(bk, "nightly", true)
	run(bUp)
	mustServe(bk, "prod", true)

	// C runs after B's lift: B no longer lists the revocation, yet C keeps what it applied (its row
	// kept, unlisted, for C's own administrator to lift)
	run(cUp)
	inC := stored(cUp)
	if len(inC) != 1 || inC[0].AppliedState != store.RevokeWithdrawn || inC[0].UnlistedAt.IsZero() {
		t.Fatalf("C after B's lift: %+v", inC)
	}
	mustServe(ck, "prod", false)
	if n, err := cUp.LiftRevocation(ctx, serveAdmin, "acme", "src", inC[0].BuildID, 0); err != nil || n != 1 {
		t.Fatalf("C lifts: %d %v", n, err)
	}
	mustServe(ck, "prod", true)
}
