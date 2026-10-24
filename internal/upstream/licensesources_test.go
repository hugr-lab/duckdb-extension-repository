package upstream_test

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0033 phase B part 2: licences per (name, source), on one engine (the store suite covers the
// three). A mirror relays tresor's licence; a marked publisher's build of tresor in another channel,
// carrying a copy of the mirror's key, gets no licence through it and takes none away; a key of the
// publisher's own gives it a licence of its own beside the mirror's; each is served by the
// channel that selects its source; a tenant administrator gives the mirror's key to the publisher,
// the licence moving along, and the mirror's relay of it is refused from then on.
func TestLicenseSources(t *testing.T) {
	en, tokenFile := privateEnv(t, storetest.Engines(t)[0])
	en.repo.token = "T-sub"
	if err := os.WriteFile(tokenFile, []byte("T-sub\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lic := newLicensor(t)
	en.repo.put("v2.0.0", "linux_amd64", "tresor", signedWith(t, en.key, cpp("1.0", "v2.0.0"), lic.block, "tresor_duckdb_cpp_init"))
	sp := en.spec(store.UpstreamEntry{Name: "tresor"})
	sp.Credential = "sub"
	u, err := en.up.Add(ctx, admin, "acme", sp)
	if err != nil {
		t.Fatal(err)
	}
	if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
		t.Fatalf("a run: %+v", r)
	}
	svc := &licenses.Service{Store: en.st, Authz: authz.ServerAdmin{}, Enabled: true, Fetch: en.up.Fetch,
		Credentials: en.up.Credentials, Interval: time.Hour, Log: slog.New(slog.DiscardHandler)}
	tid, ext := en.tenantID(t), pkgtype.DuckDBExtension
	now := en.st.Now().Unix()
	day := int64(24 * 3600)
	relayed := lic.sign("tresor", "pro", now-60, now+30*day)
	en.answer(t, relayed)
	if res, err := svc.RunUpstream(ctx, u.ID); err != nil || res.Counts["stored"] != 1 {
		t.Fatalf("the relay: %+v %v", res, err)
	}
	us, ul := store.UpstreamSource(u.ID), "upstream:acme-repo"

	// a marked publisher's build of tresor in channel other, with a copy of the mirror's key
	p := store.Publisher{TenantID: tid, Name: "copycat", CreatedBy: "os:1:t"}
	ps := ""
	other, err := en.st.GetChannel(ctx, "acme", "other")
	if err != nil {
		t.Fatal(err)
	}
	record := func(b store.Build, l *licensor) {
		t.Helper()
		pemKey, err := extfile.MarshalPublicKeyPEM(&l.key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		must(t, func() error {
			return en.st.InTx(ctx, "", func(tx *store.Tx) error {
				_, err := tx.RecordLicenseKeys(ctx, tid, b.ID, store.KeySourcePublisher, p.ID,
					[]store.LicenseKey{{Kid: extfile.Fingerprint(&l.key.PublicKey), PEM: pemKey}})
				return err
			})
		})
	}
	var b store.Build
	must(t, func() error {
		return en.st.InTx(ctx, "", func(tx *store.Tx) error {
			if err := tx.InsertPublisher(ctx, &p); err != nil {
				return err
			}
			if _, err := tx.SetPublisherLicenseKeys(ctx, tid, "copycat", true); err != nil {
				return err
			}
			ps = store.PublisherSource(p.ID)
			b = store.Build{TenantID: tid, Source: ps, Name: "tresor", PackageVersion: "1.0", Platform: "linux_amd64", ABI: store.ABICPP,
				DuckDBVersion: "v2.0.0", BodyHash: strings.Repeat("c", 64), Origin: store.OriginPublication, CreatedBy: "os:1:t", Checked: true}
			if err := tx.FindOrInsertBuild(ctx, &b); err != nil {
				return err
			}
			r := store.Release{TenantID: tid, ChannelID: other.ID, BuildID: b.ID, Name: "tresor", Source: ps, PackageVersion: "1.0",
				Platform: "linux_amd64", Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public, Origin: store.OriginPublication}
			if err := tx.InsertRelease(ctx, &r, "os:1:t"); err != nil {
				return err
			}
			_, err := tx.SetChannelSource(ctx, other.ID, "tresor", ps, "os:1:t")
			return err
		})
	})
	record(b, lic)
	if evs, err := en.st.ListEvents(ctx, tid, store.EventFilter{Kind: "license.key_conflict"}); err != nil || len(evs) != 1 ||
		!strings.Contains(evs[0].Data, `"source":"publisher:copycat"`) || !strings.Contains(evs[0].Data, `"owner":"`+ul+`"`) {
		t.Fatalf("the conflict: %+v %v", evs, err)
	}
	// nothing for the copy: an import of a licence signed with the key is the mirror's, which holds one
	if _, err := svc.Import(ctx, admin, "acme", "", lic.sign("tresor", "max", now, now+day), licenses.Create); !errors.Is(err, licenses.ErrExists) {
		t.Fatalf("an import with the copied key: %v", err)
	}
	served := func(src string) string {
		t.Helper()
		l, ok, err := svc.Serve(ctx, tid, ext, "tresor", src)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return ""
		}
		return l.Token
	}
	if served(ps) != "" || served(us) != relayed {
		t.Fatalf("served: the copy's %q, the mirror's %q", served(ps), served(us))
	}

	// a key of its own: a licence of its own, beside the mirror's
	own := &licensor{key: otherKey(t)}
	record(b, own)
	v, err := svc.Import(ctx, admin, "acme", "", own.sign("tresor", "ent", now, now+day), licenses.Create)
	if err != nil || v.Owner != "publisher:copycat" || !v.Created {
		t.Fatalf("its own licence: %+v %v", v, err)
	}
	if _, err := svc.Get(ctx, admin, "acme", "", "tresor", ""); !errors.Is(err, licenses.ErrSources) {
		t.Fatalf("a name of two sources without one: %v", err)
	}
	for src, plan := range map[string]string{"publisher:copycat": "ent", ul: "pro", "upstream-id:" + u.ID: "pro"} {
		if v, err := svc.Get(ctx, admin, "acme", "", "tresor", src); err != nil || v.Plan != plan {
			t.Fatalf("%s: %+v %v", src, v, err)
		}
	}
	if vs, err := svc.List(ctx, admin, "acme", "", ul); err != nil || len(vs) != 1 || vs[0].Owner != ul {
		t.Fatalf("the mirror's licences: %+v %v", vs, err)
	}
	if served(ps) == "" || served(us) != relayed {
		t.Fatal("each source's licence, served by its channel")
	}

	// the key given to the publisher: refused while it holds a licence of the name, then the mirror's
	// licence moves; the mirror's relay of it is refused, and what moved stays
	kid := extfile.Fingerprint(&lic.key.PublicKey)
	if _, err := svc.SetKeyOwner(ctx, admin, "acme", "", "tresor", kid, "publisher:copycat"); !errors.Is(err, licenses.ErrOwnerHolds) {
		t.Fatalf("over its own licence: %v", err)
	}
	if err := svc.Remove(ctx, admin, "acme", "", "tresor", "publisher:copycat", 0); err != nil {
		t.Fatal(err)
	}
	ko, err := svc.SetKeyOwner(ctx, admin, "acme", "", "tresor", kid, "publisher:copycat")
	if err != nil || ko.Owner != "publisher:copycat" || ko.Before != ul || len(ko.Moved) != 1 {
		t.Fatalf("the key given: %+v %v", ko, err)
	}
	if served(ps) != relayed || served(us) != "" {
		t.Fatal("the moved licence, served by the publisher's channel")
	}
	res, err := svc.RunUpstream(ctx, u.ID)
	if err != nil || len(res.Refused) != 1 || res.Refused[0].Rule != licenses.RuleKeyOwner || res.Counts["removed"] != 0 {
		t.Fatalf("the mirror's relay of a key it no longer owns: %+v %v", res, err)
	}
	if served(ps) != relayed {
		t.Fatal("the moved licence after the relay")
	}
	// nor does the mirror's revocation take it: it is the publisher's now
	en.answer(t)
	if res, err := svc.RunUpstream(ctx, u.ID); err != nil || res.Counts["removed"] != 0 || served(ps) != relayed {
		t.Fatalf("an answer without it: %+v %v", res, err)
	}
}
