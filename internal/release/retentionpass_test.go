package release_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// clock moves the store's time by hand from now on.
type clock struct{ now time.Time }

func (en *env) clock() *clock {
	c := &clock{now: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)}
	en.st.Now = func() time.Time { c.now = c.now.Add(time.Millisecond); return c.now }
	return c
}

func ip(v int) *int                     { return &v }
func dp(v time.Duration) *time.Duration { return &v }
func rule(k int, g time.Duration) store.RetentionRule {
	return store.RetentionRule{Keep: ip(k), Grace: dp(g)}
}

// states lists prod's releases of a name as version:state, oldest first.
func (en *env) states(t *testing.T, channel, name string) string {
	t.Helper()
	rs, err := en.st.ListReleases(ctx, mustChannel(t, en, channel).ID, name)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(rs)
	var out []string
	for _, r := range rs {
		out = append(out, r.PackageVersion+":"+r.State)
	}
	return strings.Join(out, " ")
}

func (en *env) events(t *testing.T, kind string) []store.Event {
	t.Helper()
	tn, err := en.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := en.st.ListEvents(ctx, tn.ID, store.EventFilter{Kind: kind, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func (en *env) retain(t *testing.T) release.RetentionResult {
	t.Helper()
	res, err := en.rel.RetainChannel(ctx, mustChannel(t, en, "prod").ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Spec 0042: the pass retires nothing without a rule in force; with one it retires what falls out of
// the newest N (one release.retire per version, the actor system:release-retention, retention.run
// once), again nothing (idempotent); after the grace it purges through spec 0016's purge
// (release.purge with reason retention), but not what a pin keeps nor while purges are off.
func TestRetentionPass(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		clk := en.clock()
		for i, v := range []string{"1.0", "1.1", "1.2"} {
			if _, _, err := en.add(t, ext(t, 1000, uint64(i+1), cpp(v)), release.AddOptions{Unchecked: true, Name: "tresor"}); err != nil {
				t.Fatal(err)
			}
			clk.now = clk.now.Add(time.Minute)
		}
		if res := en.retain(t); !res.NoRule || res.Retired != 0 {
			t.Fatalf("no rule: %+v", res)
		}
		// keep from the tenant, the grace missing: no rule in force
		if _, err := en.rel.SetTenantRetention(ctx, admin, "acme", store.RetentionRule{Keep: ip(1)}, ""); err != nil {
			t.Fatal(err)
		}
		if res := en.retain(t); !res.NoRule {
			t.Fatalf("a grace missing: %+v", res)
		}
		if _, err := en.rel.SetChannelRetention(ctx, admin, "acme", "prod", store.RetentionRule{Grace: dp(time.Hour)}, ""); err != nil {
			t.Fatal(err)
		}
		res := en.retain(t)
		if res.Retired != 2 || res.Purged != 0 || res.Failed != 0 || en.states(t, "prod", "tresor") != "1.0:retired 1.1:retired 1.2:active" {
			t.Fatalf("the first pass: %+v %s", res, en.states(t, "prod", "tresor"))
		}
		evs := en.events(t, "release.retire")
		if len(evs) != 2 || evs[0].Actor != "system:release-retention" || evs[0].Subject != "channel:prod/ext:tresor" ||
			!strings.Contains(evs[0].Data, `"by":"retention"`) || !strings.Contains(evs[0].Data, `"lines":["v2.0.0"]`) {
			t.Fatalf("release.retire: %+v", evs)
		}
		if evs := en.events(t, "retention.run"); len(evs) != 1 || !strings.Contains(evs[0].Data, `"retired":2`) || evs[0].Subject != "channel:prod" {
			t.Fatalf("retention.run: %+v", evs)
		}
		if _, checked, _ := en.st.ChannelRetention(ctx, mustChannel(t, en, "prod").ID); checked.IsZero() {
			t.Fatal("the look is not recorded")
		}
		// idempotent: nothing more, no new summary
		if res := en.retain(t); res.Retired+res.Purged+res.Failed != 0 || len(en.events(t, "retention.run")) != 1 {
			t.Fatalf("the second pass: %+v", res)
		}
		// within the grace nothing is purged; past it, 1.1 is, 1.0 is kept by its pin
		var v10 string
		for _, c := range must2(en.st.ListReleases(ctx, mustChannel(t, en, "prod").ID, "tresor")) {
			if c.PackageVersion == "1.0" {
				v10 = c.ID
			}
		}
		pin, existed, err := en.rel.Pin(ctx, admin, "acme", "prod", store.RetentionPin{Name: "tresor", PackageVersion: "1.0", Reason: "LTS nodes"})
		if err != nil || existed {
			t.Fatalf("pin: %+v %v %v", pin, existed, err)
		}
		clk.now = clk.now.Add(2 * time.Hour)
		en.rel.PurgeOff = true // gc.interval is 0: no purge
		if res := en.retain(t); res.Purged != 0 || !res.PurgeOff {
			t.Fatalf("purges off: %+v", res)
		}
		en.rel.PurgeOff = false
		if res := en.retain(t); res.Purged != 1 || en.states(t, "prod", "tresor") != "1.0:retired 1.2:active" {
			t.Fatalf("the purge: %+v %s", res, en.states(t, "prod", "tresor"))
		}
		if evs := en.events(t, "release.purge"); len(evs) != 1 || !strings.Contains(evs[0].Data, `"reason":"retention"`) ||
			evs[0].Actor != "system:release-retention" {
			t.Fatalf("release.purge: %+v", evs)
		}
		if _, _, err := en.add(t, ext(t, 1000, 2, cpp("1.1")), release.AddOptions{Unchecked: true, Name: "tresor"}); !errors.Is(err, release.ErrSlot) {
			t.Fatalf("the purged slot stays taken: %v", err)
		}
		// the pinned version restored, then unpinned and purged once retired again
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "", v10, release.Restore, 0); err != nil {
			t.Fatal(err)
		}
		if err := en.rel.Unpin(ctx, admin, "acme", "prod", pin.ID); err != nil {
			t.Fatal(err)
		}
		en.retain(t)
		clk.now = clk.now.Add(2 * time.Hour)
		if res := en.retain(t); res.Purged != 1 || en.states(t, "prod", "tresor") != "1.2:active" {
			t.Fatalf("unpinned: %+v %s", res, en.states(t, "prod", "tresor"))
		}
		// with keep 1 a yank of the current leaves the line's flat path empty (decision Q-H)
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "", must2(en.st.ListReleases(ctx, mustChannel(t, en, "prod").ID, "tresor"))[0].ID,
			release.Yank, 0); err != nil {
			t.Fatal(err)
		}
		if c := en.snapshot(t, "prod").Flat("v2.0.0", "linux_amd64", "tresor", release.Everything); c != nil {
			t.Fatalf("the flat path after the yank: %+v", c)
		}
	})
}

// Spec 0042: the pass re-checks under the lock what its look found: a release made current or
// pinned meanwhile is left; the lease keeps two replicas apart; suspended tenants are skipped.
func TestRetentionPassRecheck(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		clk := en.clock()
		var ids []string
		for i, v := range []string{"1.0", "1.1", "1.2", "1.3"} {
			r, _, err := en.add(t, ext(t, 1000, uint64(i+1), cpp(v)), release.AddOptions{Unchecked: true, Name: "tresor"})
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, r.ID)
			clk.now = clk.now.Add(time.Minute)
		}
		if _, err := en.rel.SetChannelRetention(ctx, admin, "acme", "prod", rule(1, time.Hour), ""); err != nil {
			t.Fatal(err)
		}
		calls := 0
		release.SetBeforeRetire(en.rel, func() {
			calls++
			if calls > 1 {
				return
			}
			// meanwhile: 1.0 made current, 1.1 pinned for its line
			if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "", ids[0], release.MakeCurrent, 0); err != nil {
				t.Error(err)
			}
			if _, _, err := en.rel.Pin(ctx, admin, "acme", "prod", store.RetentionPin{Name: "tresor", PackageVersion: "1.1",
				DuckDBVersion: "v2.0.0", Reason: "x"}); err != nil {
				t.Error(err)
			}
		})
		res := en.retain(t)
		release.SetBeforeRetire(en.rel, nil)
		if res.Retired != 1 || en.states(t, "prod", "tresor") != "1.0:active 1.1:active 1.2:retired 1.3:active" {
			t.Fatalf("re-checked under the lock: %+v %s", res, en.states(t, "prod", "tresor"))
		}

		// the lease: another replica holding it, the pass does not run there
		r := &release.Retainer{Service: en.rel, Holder: "a", Interval: time.Hour, Log: slog.New(slog.DiscardHandler)}
		ch := mustChannel(t, en, "prod")
		if ok, err := en.st.AcquireLease(ctx, "kista/retention/"+ch.ID, "b", time.Minute); err != nil || !ok {
			t.Fatalf("lease: %v %v", ok, err)
		}
		if _, held, err := r.Channel(ctx, ch.ID); err != nil || held {
			t.Fatalf("a lease another replica holds: %v %v", held, err)
		}
		if err := en.st.ReleaseLease(ctx, "kista/retention/"+ch.ID, "b"); err != nil {
			t.Fatal(err)
		}
		// a suspended tenant's channels are skipped (1.2 past its grace stays); resumed, a channel is
		// looked at once its last look is an Interval old
		clk.now = clk.now.Add(2 * time.Hour)
		if _, err := en.ten.SetTenantState(ctx, admin, "acme", store.TenantSuspended, 0); err != nil {
			t.Fatal(err)
		}
		r.Once(ctx, true)
		if got := en.states(t, "prod", "tresor"); got != "1.0:active 1.1:active 1.2:retired 1.3:active" {
			t.Fatalf("a suspended tenant's channel: %s", got)
		}
		if _, err := en.ten.SetTenantState(ctx, admin, "acme", store.TenantActive, 0); err != nil {
			t.Fatal(err)
		}
		r.Once(ctx, false)
		if got := en.states(t, "prod", "tresor"); got != "1.0:active 1.1:active 1.3:active" {
			t.Fatalf("due: %s", got)
		}
		if err := store.ExecRaw(ctx, en.st, "DELETE FROM retention_pins WHERE channel_id = ?", ch.ID); err != nil { // 1.1 retirable
			t.Fatal(err)
		}
		r.Once(ctx, false) // looked at just now: not due
		if got := en.states(t, "prod", "tresor"); got != "1.0:active 1.1:active 1.3:active" {
			t.Fatalf("not due: %s", got)
		}
		clk.now = clk.now.Add(2 * time.Hour)
		r.Once(ctx, false)
		if got := en.states(t, "prod", "tresor"); got != "1.0:active 1.1:retired 1.3:active" {
			t.Fatalf("due again: %s", got)
		}
	})
}

// Spec 0042: pins: a version with a release in the channel, a line one of its releases serves; the
// same pin twice is the existing one; a pin is dropped by a purge only when no release of its version
// is left in its line; another channel's pin id is not found; a passthrough channel takes none.
func TestRetentionPins(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		r, _, err := en.add(t, ext(t, 1000, 1, cpp("1.0")), release.AddOptions{Unchecked: true, Name: "tresor"})
		if err != nil {
			t.Fatal(err)
		}
		pin := func(version, line string) (store.RetentionPin, bool, error) {
			return en.rel.Pin(ctx, admin, "acme", "prod", store.RetentionPin{Name: "tresor", PackageVersion: version, DuckDBVersion: line, Reason: "LTS"})
		}
		for name, tc := range map[string]struct{ version, line string }{"no release": {"9.9", ""}, "a line it does not serve": {"1.0", "v1.4.0"}} {
			if _, _, err := pin(tc.version, tc.line); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, _, err := en.rel.Pin(ctx, admin, "acme", "prod", store.RetentionPin{Name: "tresor", PackageVersion: "1.0", Reason: "a\x01"}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("an unprintable reason: %v", err)
		}
		if _, _, err := en.rel.Pin(ctx, admin, "acme", "mirror", store.RetentionPin{Name: "json", PackageVersion: "1.0", Reason: "x"}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a passthrough channel: %v", err)
		}
		p1, existed, err := pin("1.0", "v2.0.0")
		if err != nil || existed {
			t.Fatal(err)
		}
		p2, existed, err := pin("1.0", "v2.0.0")
		if err != nil || !existed || p2.ID != p1.ID {
			t.Fatalf("the same pin: %+v %v %v", p2, existed, err)
		}
		all, _, err := pin("1.0", "")
		if err != nil {
			t.Fatal(err)
		}
		if ps, err := en.rel.Pins(ctx, admin, "acme", "prod", "", "", 10); err != nil || len(ps) != 2 || ps[0].ID != all.ID {
			t.Fatalf("pins: %+v %v", ps, err)
		}
		if ps, err := en.rel.Pins(ctx, admin, "acme", "prod", "", store.PinKey(all), 10); err != nil || len(ps) != 1 || ps[0].ID != p1.ID {
			t.Fatalf("the next page: %+v %v", ps, err)
		}
		en.staging(t)
		if err := en.rel.Unpin(ctx, admin, "acme", "staging", p1.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("another channel's pin: %v", err)
		}
		if len(en.events(t, "retention.pin")) != 2 {
			t.Fatalf("retention.pin: %+v", en.events(t, "retention.pin"))
		}
		// a purge by hand of the version's last release drops its pins
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "", r.ID, release.Yank, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := en.rel.Purge(ctx, admin, "acme", "prod", "", r.ID, 0); err != nil {
			t.Fatal(err)
		}
		if ps, err := en.rel.Pins(ctx, admin, "acme", "prod", "", "", 10); err != nil || len(ps) != 0 {
			t.Fatalf("pins after the purge: %+v %v", ps, err)
		}
		// an auditor reads, writes nothing; a stranger reads nothing
		if _, err := en.rel.Pins(ctx, authz.Actor{Kind: authz.ActorPrincipal, ID: "x"}, "acme", "prod", "", "", 10); !errors.Is(err, authz.ErrDenied) {
			t.Fatalf("a stranger: %v", err)
		}
	})
}

// Spec 0042: the line act retires what only that line keeps, current and pinned versions included
// (its pins removed; a pin for every line keeps a cpp build from the purge), keeps a C API build
// another line keeps, skips and counts withdrawn entries' releases; a line the channel no longer
// serves goes by the version's own maxima (a C API build) and the slot (a cpp build); a passthrough
// channel is refused.
func TestRetireLine(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		clk := en.clock()
		must(t, func() error {
			_, _, err := en.ten.AddVersion(ctx, admin, "v1.5.2", "release", []string{"v0.9.0", "v1.2.0"}, tenants.CoreExtensions{Names: []string{"json"}})
			return err
		})
		must(t, func() error {
			_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "prod", []string{"v1.5.2"}, nil)
			return err
		})
		lts := extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v1.5.2", ExtensionVersion: "1.0", ABI: extfile.ABICPP}
		add := func(seed uint64, m extfile.Metadata, name string) store.Release {
			t.Helper()
			r, _, err := en.add(t, ext(t, 1000, seed, m), release.AddOptions{Unchecked: true, Name: name})
			if err != nil {
				t.Fatal(err)
			}
			clk.now = clk.now.Add(time.Minute)
			return r
		}
		ltsR := add(1, lts, "tresor")                   // only v1.5.2
		mainR := add(2, cpp("1.0"), "tresor")           // only v2.0.0
		both := add(3, capi("1.1", "v1.2.0"), "tresor") // both lines
		zero := add(4, capi("0.5", "v0.9.0"), "acl")    // only v1.5.2 (C API major 0)
		gone := add(5, lts, "geo")                      // withdrawn below
		if err := store.ExecRaw(ctx, en.st, "UPDATE builds SET withdrawn_at = ? WHERE id = ?", store.TimeArg(en.st, clk.now), gone.BuildID); err != nil {
			t.Fatal(err)
		}
		pinAll, _, err := en.rel.Pin(ctx, admin, "acme", "prod", store.RetentionPin{Name: "tresor", PackageVersion: "1.0", Reason: "LTS"})
		if err != nil {
			t.Fatal(err)
		}
		pinLine, _, err := en.rel.Pin(ctx, admin, "acme", "prod", store.RetentionPin{Name: "tresor", PackageVersion: "1.1", DuckDBVersion: "v1.5.2", Reason: "LTS"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := en.rel.RetireLine(ctx, admin, "acme", "mirror", "v1.5.2", ""); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a passthrough channel: %v", err)
		}
		if _, err := en.rel.RetireLine(ctx, admin, "acme", "prod", "v9.9.9", ""); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("an unknown DuckDB version: %v", err)
		}
		res, err := en.rel.RetireLine(ctx, admin, "acme", "prod", "v1.5.2", "tresor")
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Retired) != 1 || res.Retired[0].ID != ltsR.ID || len(res.Kept) != 1 || res.Kept[0].ID != both.ID ||
			!slices.Equal(res.Kept[0].Lines, []string{"v2.0.0"}) || res.PinsRemoved != 1 || res.Withdrawn != 0 {
			t.Fatalf("retire-line v1.5.2 tresor: %+v", res)
		}
		if ps, _ := en.rel.Pins(ctx, admin, "acme", "prod", "", "", 10); len(ps) != 1 || ps[0].ID != pinAll.ID {
			t.Fatalf("the line's pin removed, the every-line pin kept: %+v (removed %s)", ps, pinLine.ID)
		}
		if evs := en.events(t, "retention.line"); len(evs) != 1 || !strings.Contains(evs[0].Data, `"retired":1`) ||
			!strings.Contains(evs[0].Data, `"name":"tresor"`) || !strings.Contains(evs[0].Data, `"pins_removed":1`) {
			t.Fatalf("retention.line: %+v", evs)
		}
		if evs := en.events(t, "release.retire"); len(evs) != 1 || !strings.Contains(evs[0].Data, `"by":"line"`) {
			t.Fatalf("release.retire: %+v", evs)
		}
		// mainR, the same version in another line, untouched; the pass (grace 0) does not purge the pinned lts build
		if got := en.states(t, "prod", "tresor"); got != "1.0:retired 1.0:active 1.1:active" {
			t.Fatalf("tresor: %s (main %s)", got, mainR.ID)
		}
		if _, err := en.rel.SetChannelRetention(ctx, admin, "acme", "prod", rule(5, 0), ""); err != nil {
			t.Fatal(err)
		}
		if res := en.retain(t); res.Purged != 0 {
			t.Fatalf("a pinned retired release purged: %+v", res)
		}
		if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "", ltsR.ID, release.Restore, 0); err != nil {
			t.Fatalf("restorable while pinned: %v", err)
		}
		// the line removed from the channel: what it left behind goes, by the version's maxima and slot
		must(t, func() error {
			_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "prod", nil, []string{"v1.5.2"})
			return err
		})
		res, err = en.rel.RetireLine(ctx, admin, "acme", "prod", "v1.5.2", "")
		if err != nil {
			t.Fatal(err)
		}
		var retired []string
		for _, r := range res.Retired {
			retired = append(retired, r.ID)
		}
		slices.Sort(retired)
		want := []string{ltsR.ID, zero.ID}
		slices.Sort(want)
		if !slices.Equal(retired, want) || res.Withdrawn != 1 || len(res.Kept) != 1 || res.Kept[0].ID != both.ID {
			t.Fatalf("a line no longer served: %+v", res)
		}
	})
}

// Spec 0042: a PUT is the whole rule (a field left out is unset), nothing changed writes nothing; a
// grace above the ceiling is refused on change, an unchanged one sent again passes; the ETag guards a
// PUT; keep none at a channel is no retention whatever the tenant's; a passthrough channel takes none.
func TestRetentionRules(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		if _, err := en.rel.SetTenantRetention(ctx, admin, "acme", rule(3, 720*time.Hour), ""); err != nil {
			t.Fatal(err)
		}
		r, err := en.rel.TenantRetention(ctx, admin, "acme")
		if err != nil || *r.Keep != 3 || *r.Grace != 720*time.Hour {
			t.Fatalf("the tenant's: %+v %v", r, err)
		}
		if _, err := en.rel.SetTenantRetention(ctx, admin, "acme", rule(3, 720*time.Hour), release.RetentionTag(r)); err != nil {
			t.Fatal(err)
		}
		if len(en.events(t, "retention.set")) != 1 {
			t.Fatalf("an unchanged rule wrote: %+v", en.events(t, "retention.set"))
		}
		if _, err := en.rel.SetTenantRetention(ctx, admin, "acme", store.RetentionRule{Keep: ip(3)}, `"stale"`); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a stale If-Match: %v", err)
		}
		if _, err := en.rel.SetTenantRetention(ctx, admin, "acme", store.RetentionRule{Keep: ip(3)}, ""); err != nil {
			t.Fatal(err)
		}
		if r, _ := en.rel.TenantRetention(ctx, admin, "acme"); r.Grace != nil {
			t.Fatalf("a field left out is unset: %+v", r)
		}
		// the ceiling, on change only
		old := store.MaxRetentionGrace
		defer func() { store.MaxRetentionGrace = old }()
		if _, err := en.rel.SetChannelRetention(ctx, admin, "acme", "prod", rule(5, 48*time.Hour), ""); err != nil {
			t.Fatal(err)
		}
		store.MaxRetentionGrace = 24 * time.Hour
		v, err := en.rel.SetChannelRetention(ctx, admin, "acme", "prod", rule(5, 48*time.Hour), "")
		if err != nil || v.InForce.Grace != 24*time.Hour || !v.InForce.Capped || *v.Rule.Grace != 48*time.Hour {
			t.Fatalf("an unchanged grace above a lowered ceiling: %+v %v", v, err)
		}
		if _, err := en.rel.SetChannelRetention(ctx, admin, "acme", "prod", rule(5, 36*time.Hour), ""); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a changed grace above the ceiling: %v", err)
		}
		// none at the channel
		v, err = en.rel.SetChannelRetention(ctx, admin, "acme", "prod", store.RetentionRule{Keep: ip(0)}, "")
		if err != nil || v.InForce.Active() || !v.InForce.None {
			t.Fatalf("none: %+v %v", v, err)
		}
		if _, err := en.rel.SetChannelRetention(ctx, admin, "acme", "prod", store.RetentionRule{Keep: ip(-1)}, ""); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a negative keep: %v", err)
		}
		if _, err := en.rel.SetChannelRetention(ctx, admin, "acme", "mirror", rule(1, 0), ""); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a passthrough channel: %v", err)
		}
		if evs := en.events(t, "retention.set"); len(evs) != 4 || !strings.Contains(evs[0].Data, `"in_force"`) || evs[0].Subject != "channel:prod" {
			t.Fatalf("retention.set: %+v", evs)
		}
	})
}

// Spec 0042 review: a pin of a DuckDB version the channel no longer serves still keeps a retired
// release of it from the purge, and a purge of one of its releases drops the pin only once none is
// left in that version; a pass whose lease is lost sums up what it did and records no look.
func TestRetentionPinsAndLease(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.clock()
		must(t, func() error {
			_, _, err := en.ten.AddVersion(ctx, admin, "v1.5.2", "release", []string{"v1.2.0"}, tenants.CoreExtensions{Names: []string{"json"}})
			return err
		})
		must(t, func() error {
			_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "prod", []string{"v1.5.2"}, nil)
			return err
		})
		var lts []store.Release
		for i, p := range []string{"linux_amd64", "linux_arm64"} {
			m := extfile.Metadata{Platform: p, DuckDBVersion: "v1.5.2", ExtensionVersion: "1.0", ABI: extfile.ABICPP}
			r, _, err := en.add(t, ext(t, 1000, uint64(i+1), m), release.AddOptions{Unchecked: true, Name: "tresor"})
			if err != nil {
				t.Fatal(err)
			}
			en.retire(t, "prod", r.ID)
			lts = append(lts, r)
		}
		if _, _, err := en.rel.Pin(ctx, admin, "acme", "prod", store.RetentionPin{Name: "tresor", PackageVersion: "1.0", DuckDBVersion: "v1.5.2",
			Reason: "LTS"}); err != nil {
			t.Fatal(err)
		}
		must(t, func() error {
			_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "prod", nil, []string{"v1.5.2"})
			return err
		})
		if _, err := en.rel.SetChannelRetention(ctx, admin, "acme", "prod", rule(1, 0), ""); err != nil {
			t.Fatal(err)
		}
		if res := en.retain(t); res.Purged != 0 {
			t.Fatalf("a pin of a line no longer served did not keep it: %+v", res)
		}
		if _, err := en.rel.Purge(ctx, admin, "acme", "prod", "", lts[0].ID, 0); err != nil {
			t.Fatal(err)
		}
		if ps, _ := en.rel.Pins(ctx, admin, "acme", "prod", "", "", 10); len(ps) != 1 {
			t.Fatalf("the pin dropped while a release of its line is left: %+v", ps)
		}
		if _, err := en.rel.Purge(ctx, admin, "acme", "prod", "", lts[1].ID, 0); err != nil {
			t.Fatal(err)
		}
		if ps, _ := en.rel.Pins(ctx, admin, "acme", "prod", "", "", 10); len(ps) != 0 {
			t.Fatalf("the pin kept with no release of its line: %+v", ps)
		}

		// the lease lost after the first retirement
		for i, v := range []string{"2.0", "2.1", "2.2"} {
			if _, _, err := en.add(t, ext(t, 1000, uint64(10+i), cpp(v)), release.AddOptions{Unchecked: true, Name: "acl"}); err != nil {
				t.Fatal(err)
			}
		}
		_, before, _ := en.st.ChannelRetention(ctx, mustChannel(t, en, "prod").ID)
		calls := 0
		lost := errors.New("lost")
		_, err := en.rel.RetainChannel(ctx, mustChannel(t, en, "prod").ID, func(context.Context) error {
			if calls++; calls > 1 {
				return lost
			}
			return nil
		})
		if !errors.Is(err, lost) || en.states(t, "prod", "acl") != "2.0:retired 2.1:active 2.2:active" {
			t.Fatalf("the lease lost: %v %s", err, en.states(t, "prod", "acl"))
		}
		if evs := en.events(t, "retention.run"); len(evs) == 0 || !strings.Contains(evs[0].Data, `"retired":1`) {
			t.Fatalf("no summary of what the pass did: %+v", evs)
		}
		if _, checked, _ := en.st.ChannelRetention(ctx, mustChannel(t, en, "prod").ID); before.IsZero() || !checked.Equal(before) {
			t.Fatalf("an unfinished look recorded: %v, before %v", checked, before)
		}
	})
}
