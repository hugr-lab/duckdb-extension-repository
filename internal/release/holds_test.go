package release_test

import (
	"bytes"
	"context"
	"errors"
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
