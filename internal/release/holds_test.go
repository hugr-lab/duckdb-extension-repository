package release_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// noAdmin allows every verb but admin: a holder of promote who administers no channel.
type noAdmin struct{}

func (noAdmin) Allow(_ context.Context, _ authz.Actor, v authz.Verb, _ authz.Resource) error {
	if v == authz.VerbAdmin {
		return authz.ErrDenied
	}
	return nil
}

// pastClock is a store clock half a day back: a test moves it forward by hours, and a blob commit's
// claim, which lasts until a deadline on the real clock, stays live.
func pastClock() time.Time { return time.Now().UTC().Add(-12 * time.Hour).Truncate(time.Second) }

// hold sets ci's hold.
func (p placing) hold(t *testing.T, d time.Duration) {
	t.Helper()
	must(t, func() error {
		return p.st.InTx(ctx, "", func(tx *store.Tx) error { _, err := tx.SetPublisherHold(ctx, p.ci.TenantID, "ci", d); return err })
	})
}

// toCatalogue publishes a version of tresor into the catalogue only, as ci.
func (p placing) toCatalogue(t *testing.T, m extfile.Metadata) store.Build {
	t.Helper()
	b, _, err := p.pubs.AddToCatalogue(ctx, p.pub, "acme", bytes.NewReader(built(t, m, "tresor_duckdb_cpp_init")),
		publish("tresor", m.ExtensionVersion, m.Platform))
	if err != nil {
		t.Fatalf("publish %s into the catalogue: %v", m.ExtensionVersion, err)
	}
	return b
}

// Spec 0033 phase A2: a publisher with a hold is refused on a channel's route and publishes into the
// catalogue; an auto rule places its entry only when the hold ends (the pass wakes then), and an
// older version whose hold ends later does not overtake a newer one; a channel's hold adds to it.
func TestPlaceHolds(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		clock := pastClock()
		en.st.Now = func() time.Time { return clock }
		p := newPlacing(t, en)
		p.hold(t, time.Hour)
		_, _, err := p.pubs.Add(ctx, p.pub, "acme", "staging", bytes.NewReader(built(t, cpp("1.0"), "tresor_duckdb_cpp_init")),
			publish("tresor", "1.0", "linux_amd64"))
		if !errors.Is(err, release.ErrHeld) || !strings.Contains(err.Error(), "catalogue") {
			t.Fatalf("a held publisher on a channel's route: %v", err)
		}
		if b := p.toCatalogue(t, cpp("1.0")); !b.Held(clock) || !b.AvailableAt.Equal(clock.Add(time.Hour)) {
			t.Fatalf("the entry: %+v", b)
		}
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "tresor", "publisher:ci", "auto"); err != nil {
			t.Fatal(err)
		}
		p.pass(t, false)
		if got := p.prod(t); got != "" {
			t.Fatalf("placed while held: %q", got)
		}
		clock = clock.Add(61 * time.Minute)
		p.pass(t, false) // due: the hold it waited for ended
		if got := p.prod(t); got != "1.0@v2.0.0:active" {
			t.Fatalf("after the hold: %q", got)
		}
		// 1.1 held for an hour, then 1.2 with no hold: 1.2 is placed, and 1.1 never
		p.toCatalogue(t, cpp("1.1"))
		p.hold(t, 0)
		p.toCatalogue(t, cpp("1.2"))
		clock = clock.Add(time.Minute)
		p.pass(t, false)
		clock = clock.Add(2 * time.Hour)
		p.pass(t, true)
		if got := p.prod(t); got != "1.0@v2.0.0:active 1.2@v2.0.0:active" {
			t.Fatalf("an older version whose hold ended later: %q", got)
		}
		// the channel's hold: an automatic placement waits for it
		if _, err := p.rel.SetChannelHold(ctx, admin, "acme", "prod", 3*time.Hour); err != nil {
			t.Fatal(err)
		}
		if d, err := p.rel.ChannelHold(ctx, admin, "acme", "prod"); err != nil || d != 3*time.Hour {
			t.Fatalf("the channel's hold: %s %v", d, err)
		}
		p.toCatalogue(t, cpp("1.3"))
		p.pass(t, false)
		if got := p.prod(t); got != "1.0@v2.0.0:active 1.2@v2.0.0:active" {
			t.Fatalf("placed within the channel's hold: %q", got)
		}
		clock = clock.Add(3*time.Hour + time.Minute)
		p.pass(t, false)
		if got := p.prod(t); got != "1.0@v2.0.0:active 1.2@v2.0.0:active 1.3@v2.0.0:active" {
			t.Fatalf("after the channel's hold: %q", got)
		}
		if _, err := p.rel.SetChannelHold(ctx, admin, "acme", "mirror", time.Hour); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a passthrough channel's hold: %v", err)
		}
	})
}

// Spec 0033 phase A2: placing a held entry by hand takes early, a channel administrator's (audited:
// release.place with early); a promotion refuses a held entry.
func TestPlaceEarly(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		clock := pastClock()
		en.st.Now = func() time.Time { return clock }
		p := newPlacing(t, en)
		p.hold(t, time.Hour)
		p.toCatalogue(t, cpp("1.0"))
		o := release.PlaceOptions{Source: "publisher:ci", Version: "1.0"}
		if _, _, err := p.rel.Place(ctx, admin, "acme", "staging", "tresor", o); !errors.Is(err, release.ErrHeld) {
			t.Fatalf("a held entry placed without early: %v", err)
		}
		holder := *p.rel // promote everywhere, administering nothing
		holder.Authz = noAdmin{}
		early := o
		early.Early = true
		if _, _, err := holder.Place(ctx, admin, "acme", "staging", "tresor", early); !errors.Is(err, authz.ErrDenied) {
			t.Fatalf("early by someone who administers no channel: %v", err)
		}
		rs, _, err := p.rel.Place(ctx, admin, "acme", "staging", "tresor", early)
		if err != nil || len(rs) != 1 {
			t.Fatalf("placed early: %+v %v", rs, err)
		}
		tn, _ := p.st.GetTenant(ctx, "acme")
		evs, err := p.st.ListEvents(ctx, tn.ID, store.EventFilter{Kind: "release.place"})
		if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Data, `"early":true`) {
			t.Fatalf("the audit of an early placement: %+v %v", evs, err)
		}
		// a promotion of the held entry, placed early in staging, is refused
		if _, _, err := p.rel.Promote(ctx, admin, "acme", "prod", "tresor", release.PromoteOptions{From: "staging", Version: "1.0"}); !errors.Is(err, release.ErrHeld) {
			t.Fatalf("a held entry promoted: %v", err)
		}
		clock = clock.Add(2 * time.Hour)
		if _, _, err := p.rel.Promote(ctx, admin, "acme", "prod", "tresor", release.PromoteOptions{From: "staging", Version: "1.0"}); err != nil {
			t.Fatalf("promoted once available: %v", err)
		}
		// early on an entry that is not held places it as without
		if _, _, err := p.rel.Place(ctx, admin, "acme", "empty", "tresor", early); err == nil || errors.Is(err, release.ErrHeld) {
			t.Fatalf("a channel without the DuckDB version: %v", err)
		}
	})
}

// hookReader runs once before its first read: a change made while an upload is in flight.
type hookReader struct {
	r    io.Reader
	once func()
}

func (h *hookReader) Read(b []byte) (int, error) {
	if f := h.once; f != nil {
		h.once = nil
		f()
	}
	return h.r.Read(b)
}

// key adds a key to ci and returns its id.
func (p placing) key(t *testing.T, n int) string {
	t.Helper()
	k := store.PublisherKey{TenantID: p.ci.TenantID, PublisherID: p.ci.ID, Type: "ssh-ed25519", PublicKey: fmt.Sprintf("ssh-ed25519 AAAA%d", n),
		Fingerprint: "SHA256:" + strings.Repeat(strconv.Itoa(n%10), 43)}
	must(t, func() error {
		return p.st.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertPublisherKey(ctx, &k) })
	})
	return k.ID
}

// revoke does what revoking ci's key with purge_held does in the store, under the locks the
// tenant's administration takes (keyID "": removing ci), and returns the entries the purge kept.
func (p placing) revoke(t *testing.T, keyID string) []store.KeptEntry {
	t.Helper()
	var kept []store.KeptEntry
	must(t, func() error {
		return p.st.InTxLocks(ctx, []string{store.TenantUpstreamLock(p.ci.TenantID), store.PublisherAuthLock(p.ci.TenantID)}, func(tx *store.Tx) error {
			var err error
			if keyID != "" {
				if _, err := tx.DeletePublisherKey(ctx, p.ci.ID, keyID); err != nil {
					return err
				}
			}
			if _, kept, err = tx.PurgeHeld(ctx, p.ci.TenantID, store.PublisherSource(p.ci.ID), keyID); err != nil || keyID != "" {
				return err
			}
			return tx.DeletePublisher(ctx, p.ci.TenantID, "ci", "os:1:t")
		})
	})
	return kept
}

// Spec 0033 phase A2: an upload in flight when its key is revoked with purge_held, or its publisher
// removed, lands nothing and reaches no channel (the build's transaction checks both under the
// lock the removal takes); a channel's route too.
func TestHeldIntakeRevoked(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		clock := pastClock()
		en.st.Now = func() time.Time { return clock }
		p := newPlacing(t, en)
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "tresor", "publisher:ci", "auto"); err != nil {
			t.Fatal(err)
		}
		entries := func() int {
			bs, err := p.st.SourceBuilds(ctx, p.ci.TenantID, "duckdb_extension", store.PublisherSource(p.ci.ID), "tresor")
			if err != nil {
				t.Fatal(err)
			}
			return len(bs)
		}
		// without a hold, on the channel's route: the key revoked while the upload is in flight
		k1 := p.key(t, 1)
		o := publish("tresor", "1.0", "linux_amd64")
		o.Publish.KeyID = k1
		hr := &hookReader{r: bytes.NewReader(built(t, cpp("1.0"), "tresor_duckdb_cpp_init")), once: func() { p.revoke(t, k1) }}
		if _, _, err := p.pubs.Add(ctx, p.pub, "acme", "staging", hr, o); !errors.Is(err, store.ErrSourceGone) {
			t.Fatalf("a channel's route, the key revoked in flight: %v", err)
		}
		// with a hold, into the catalogue: the key revoked with purge_held in flight
		p.hold(t, 24*time.Hour)
		k2 := p.key(t, 2)
		o.Publish.KeyID = k2
		hr = &hookReader{r: bytes.NewReader(built(t, cpp("1.0"), "tresor_duckdb_cpp_init")), once: func() { p.revoke(t, k2) }}
		if _, _, err := p.pubs.AddToCatalogue(ctx, p.pub, "acme", hr, o); !errors.Is(err, store.ErrSourceGone) {
			t.Fatalf("the catalogue, the key revoked in flight: %v", err)
		}
		if n := entries(); n != 0 {
			t.Fatalf("entries after a revoked key's upload: %d", n)
		}
		// the publisher removed in flight
		o.Publish.KeyID = p.key(t, 3)
		hr = &hookReader{r: bytes.NewReader(built(t, cpp("1.0"), "tresor_duckdb_cpp_init")), once: func() { p.revoke(t, "") }}
		if _, _, err := p.pubs.AddToCatalogue(ctx, p.pub, "acme", hr, o); !errors.Is(err, store.ErrSourceGone) {
			t.Fatalf("the catalogue, the publisher removed in flight: %v", err)
		}
		clock = clock.Add(25 * time.Hour)
		p.pass(t, true)
		if n, got := entries(), p.prod(t); n != 0 || got != "" {
			t.Fatalf("after the hold: %d entries, prod %q", n, got)
		}
	})
}

// Spec 0033 phase A2: an entry placed early survives its key's revocation with purge_held, revoked:
// no rule takes it when its hold ends, nor does a promotion or a placement without early; a channel
// administrator still places it early. The catalogue shows it.
func TestRevokedEntry(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		clock := pastClock()
		en.st.Now = func() time.Time { return clock }
		p := newPlacing(t, en)
		p.hold(t, time.Hour)
		k := p.key(t, 1)
		o := publish("tresor", "1.0", "linux_amd64")
		o.Publish.KeyID = k
		if _, _, err := p.pubs.AddToCatalogue(ctx, p.pub, "acme", bytes.NewReader(built(t, cpp("1.0"), "tresor_duckdb_cpp_init")), o); err != nil {
			t.Fatal(err)
		}
		early := release.PlaceOptions{Source: "publisher:ci", Version: "1.0", Early: true}
		if _, _, err := p.rel.Place(ctx, admin, "acme", "staging", "tresor", early); err != nil {
			t.Fatal(err)
		}
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "tresor", "publisher:ci", "auto"); err != nil {
			t.Fatal(err)
		}
		kept := p.revoke(t, k)
		if len(kept) != 1 || kept[0].PackageVersion != "1.0" || kept[0].Channels != 1 || !kept[0].Revoked() {
			t.Fatalf("kept: %+v", kept)
		}
		clock = clock.Add(2 * time.Hour)
		p.pass(t, true)
		if got := p.prod(t); got != "" {
			t.Fatalf("a revoked entry placed by an auto rule after its hold: %q", got)
		}
		// nor is it the rule's newest available version (spec 0033 phase C: what the pass would place)
		if v, err := p.rel.Source(ctx, admin, "acme", "prod", "tresor"); err != nil || v.Newest != "" {
			t.Fatalf("a revoked entry as the newest: %+v %v", v, err)
		}
		if _, _, err := p.rel.Promote(ctx, admin, "acme", "prod", "tresor", release.PromoteOptions{From: "staging", Version: "1.0"}); !errors.Is(err, release.ErrHeld) {
			t.Fatalf("a revoked entry promoted: %v", err)
		}
		if _, _, err := p.rel.Place(ctx, admin, "acme", "prod", "tresor", release.PlaceOptions{Source: "publisher:ci", Version: "1.0"}); !errors.Is(err, release.ErrHeld) {
			t.Fatalf("a revoked entry placed without early: %v", err)
		}
		es, err := p.rel.Catalogue(ctx, admin, "acme", "duckdb_extension", "publisher:ci", "tresor")
		if err != nil || len(es) != 1 || strings.Join(es[0].Revoked, " ") != "1.0" {
			t.Fatalf("the catalogue: %+v %v", es, err)
		}
		if _, _, err := p.rel.Place(ctx, admin, "acme", "prod", "tresor", early); err != nil {
			t.Fatalf("a revoked entry placed early: %v", err)
		}
		if got := p.prod(t); got != "1.0@v2.0.0:active" {
			t.Fatalf("prod: %q", got)
		}
	})
}

// Spec 0033 phase A2: on a channel's route a publication's release is written after its build, its
// signatures made between them: a key revoked, or a publisher removed, in between releases nothing
// (insert checks both under its locks), and a publication finding its release already made is
// refused once its key is gone.
func TestReleaseAfterRevocation(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p := newPlacing(t, en)
		staging := func() int {
			ch, err := p.st.GetChannel(ctx, "acme", "staging")
			if err != nil {
				t.Fatal(err)
			}
			rs, err := p.st.ListReleases(ctx, ch.ID, "tresor")
			if err != nil {
				t.Fatal(err)
			}
			return len(rs)
		}
		publishWith := func(key string, v string) error {
			o := publish("tresor", v, "linux_amd64")
			o.Publish.KeyID = key
			_, _, err := p.pubs.Add(ctx, p.pub, "acme", "staging", bytes.NewReader(built(t, cpp(v), "tresor_duckdb_cpp_init")), o)
			return err
		}
		// the release already made, then its key revoked: the same publication is refused
		k1 := p.key(t, 1)
		if err := publishWith(k1, "1.0"); err != nil {
			t.Fatal(err)
		}
		p.revoke(t, k1)
		if err := publishWith(k1, "1.0"); !errors.Is(err, store.ErrSourceGone) {
			t.Fatalf("a revoked key's publication finding its release: %v", err)
		}
		// the key revoked between the build and the release
		k2 := p.key(t, 2)
		release.SetAfterBuild(p.pubs, func(store.Build) { p.revoke(t, k2) })
		if err := publishWith(k2, "1.1"); !errors.Is(err, store.ErrSourceGone) {
			t.Fatalf("the key revoked before the release: %v", err)
		}
		// the publisher removed between the build and the release
		k3 := p.key(t, 3)
		release.SetAfterBuild(p.pubs, func(store.Build) { p.revoke(t, "") })
		if err := publishWith(k3, "1.2"); !errors.Is(err, store.ErrSourceGone) {
			t.Fatalf("the publisher removed before the release: %v", err)
		}
		if n := staging(); n != 1 {
			t.Fatalf("staging's releases: %d", n)
		}
	})
}
