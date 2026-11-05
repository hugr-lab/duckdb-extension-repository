package release_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// rel is a release for the retention computation: id, version, platform, state, visibility, seq,
// and when it was made (minutes after a base); a cpp build for v2.0.0 unless changed.
func rel(id, version string, minute int, seq int64) store.Candidate {
	return store.Candidate{Release: store.Release{ID: id, Name: "tresor", PackageVersion: version, Platform: "linux_amd64", Slot: "v2.0.0",
		State: store.ReleaseActive, Visibility: store.Public, Seq: seq, Source: "tenant",
		CreatedAt: time.Date(2026, 11, 1, 10, minute, 0, 0, time.UTC)}, ABI: store.ABICPP, DuckDBVersion: "v2.0.0"}
}

func with(c store.Candidate, f func(*store.Candidate)) store.Candidate { f(&c); return c }

var lines2 = map[string][]store.CAPI{"v2.0.0": {{Major: 1, Minor: 5, Patch: 6}}}

// Spec 0042: newest N per (source, name, line) by the version's first release in the line, the
// current versions and the pinned ones kept; the rest retired, a release only when no line it serves
// keeps it.
func TestRetentionComputation(t *testing.T) {
	two := map[string][]store.CAPI{"v1.5.2": {{Major: 1, Minor: 2, Patch: 0}}, "v2.0.0": {{Major: 1, Minor: 5, Patch: 6}}}
	cases := []struct {
		name   string
		rels   []store.Candidate
		capis  map[string][]store.CAPI
		pins   []store.RetentionPin
		keep   int
		retire []string
		keptBy map[string][]string
	}{
		{name: "the newest N", rels: []store.Candidate{rel("a", "1.0", 1, 1), rel("b", "1.1", 2, 2), rel("c", "1.2", 3, 3)}, keep: 2,
			retire: []string{"a"}},
		{name: "newest by first release, not by version", rels: []store.Candidate{rel("a", "1.0", 1, 1), rel("b", "1.1", 2, 2),
			rel("c", "1.2", 3, 3), rel("d", "0.9", 4, 0)}, keep: 2, retire: []string{"a", "b"}},
		{name: "a version's platforms count once", rels: []store.Candidate{rel("a", "1.0", 1, 1),
			with(rel("a2", "1.0", 5, 2), func(c *store.Candidate) { c.Platform = "osx_arm64" }), rel("b", "1.1", 2, 3),
			with(rel("b2", "1.1", 3, 4), func(c *store.Candidate) { c.Platform = "osx_arm64" })}, keep: 1, retire: []string{"a", "a2"}},
		{name: "a current older than the newest N", rels: []store.Candidate{rel("a", "1.0", 1, 9), rel("b", "1.1", 2, 2), rel("c", "1.2", 3, 3)},
			keep: 1, retire: []string{"b"}},
		{name: "the private current and the public one", rels: []store.Candidate{rel("a", "1.0", 1, 1),
			with(rel("b", "1.1", 2, 2), func(c *store.Candidate) { c.Visibility = store.Private }), rel("c", "1.2", 3, 0)}, keep: 1},
		{name: "a deprecated version is counted, never current", rels: []store.Candidate{
			with(rel("a", "1.0", 1, 5), func(c *store.Candidate) { c.State = store.ReleaseDeprecated }), rel("b", "1.1", 2, 2), rel("c", "1.2", 3, 3)},
			keep: 2, retire: []string{"a"}},
		{name: "yanked, withdrawn and retired releases are not counted", rels: []store.Candidate{rel("a", "1.0", 1, 1), rel("b", "1.1", 2, 2),
			with(rel("y", "1.5", 3, 0), func(c *store.Candidate) { c.State = store.ReleaseYanked }),
			with(rel("w", "1.6", 4, 0), func(c *store.Candidate) { c.Withdrawn = true }),
			with(rel("r", "1.7", 5, 0), func(c *store.Candidate) { c.State = store.ReleaseRetired })}, keep: 1, retire: []string{"a"}},
		{name: "two sources counted apart, a non-selected source's current kept", rels: []store.Candidate{rel("a", "1.0", 1, 1), rel("b", "1.1", 2, 2),
			with(rel("x", "2.0", 3, 3), func(c *store.Candidate) { c.Source = "upstream:u" }),
			with(rel("y", "2.1", 4, 0), func(c *store.Candidate) { c.Source = "upstream:u" })}, keep: 1, retire: []string{"a"}},
		{name: "a release serving no line is left alone", rels: []store.Candidate{
			with(rel("old", "0.1", 0, 0), func(c *store.Candidate) { c.DuckDBVersion, c.Slot = "v1.4.0", "v1.4.0" }), rel("b", "1.1", 2, 2)}, keep: 1},
		{name: "pins for a line and for every line", rels: []store.Candidate{rel("a", "1.0", 1, 1), rel("b", "1.1", 2, 2), rel("c", "1.2", 3, 3)},
			pins: []store.RetentionPin{{Name: "tresor", PackageVersion: "1.0", DuckDBVersion: "v2.0.0"}, {Name: "tresor", PackageVersion: "1.1"}},
			keep: 1},
		{name: "a pin of another line or name keeps nothing here", rels: []store.Candidate{rel("a", "1.0", 1, 1), rel("c", "1.2", 3, 3)},
			pins: []store.RetentionPin{{Name: "tresor", PackageVersion: "1.0", DuckDBVersion: "v1.5.2"}, {Name: "acl", PackageVersion: "1.0"}},
			keep: 1, retire: []string{"a"}},
		{name: "a C API build serving two lines is kept by one", capis: two, rels: []store.Candidate{
			with(rel("capi", "1.0", 1, 1), func(c *store.Candidate) {
				c.ABI, c.DuckDBVersion, c.CAPI, c.Slot = store.ABICStruct, "", &store.CAPI{Major: 1, Minor: 2}, "capi:1"
			}),
			rel("b", "1.1", 2, 2), rel("c", "1.2", 3, 3)},
			keep: 1, retire: []string{"b"}, keptBy: map[string][]string{"capi": {"v1.5.2"}}},
		{name: "a C API build retired where neither line keeps it", capis: two, rels: []store.Candidate{
			with(rel("capi", "1.0", 1, 1), func(c *store.Candidate) {
				c.ABI, c.DuckDBVersion, c.CAPI, c.Slot = store.ABICStruct, "", &store.CAPI{Major: 1, Minor: 2}, "capi:1"
			}),
			with(rel("lts", "1.1", 2, 2), func(c *store.Candidate) { c.DuckDBVersion, c.Slot = "v1.5.2", "v1.5.2" }), rel("c", "1.2", 3, 3)},
			keep: 1, retire: []string{"capi"}},
		{name: "an unknown C API maximum serves nothing: kept", rels: []store.Candidate{
			with(rel("capi", "1.0", 1, 0), func(c *store.Candidate) { c.ABI, c.DuckDBVersion, c.CAPI = store.ABICStruct, "", nil }), rel("c", "1.2", 3, 3)},
			keep: 1},
	}
	for _, tc := range cases {
		capis := tc.capis
		if capis == nil {
			capis = lines2
		}
		got, _, keptBy := release.Retire(tc.rels, capis, tc.pins, tc.keep)
		slices.Sort(tc.retire)
		if !slices.Equal(got, tc.retire) {
			t.Errorf("%s: retired %v, want %v", tc.name, got, tc.retire)
		}
		for id, ls := range tc.keptBy {
			if !slices.Equal(keptBy[id], ls) {
				t.Errorf("%s: %s kept by %v, want %v", tc.name, id, keptBy[id], ls)
			}
		}
	}
	// the kept versions, newest first by first release: the newest N, then the current and the pins
	_, kept, _ := release.Retire([]store.Candidate{rel("a", "1.0", 1, 9), rel("b", "1.1", 2, 2), rel("c", "1.2", 3, 3)}, lines2, nil, 1)
	if got := strings.Join(kept["tenant/tresor@v2.0.0"], " "); got != "1.2 1.0" {
		t.Fatalf("kept: %q", got)
	}
}

// Spec 0042: each field of the rule in force on its own, from the channel or the tenant; a field
// with no value anywhere is no rule; a channel's none is no rule whatever the tenant's; a grace above
// the ceiling is capped where it is in force.
func TestRuleInForce(t *testing.T) {
	n := func(v int) *int { return &v }
	d := func(v time.Duration) *time.Duration { return &v }
	r := release.InForce(store.RetentionRule{Keep: n(5)}, store.RetentionRule{Keep: n(3), Grace: d(720 * time.Hour)})
	if !r.Active() || r.Keep != 5 || r.KeepFrom != release.FromChannel || r.Grace != 720*time.Hour || r.GraceFrom != release.FromTenant {
		t.Fatalf("keep from the channel, grace from the tenant: %+v", r)
	}
	if r := release.InForce(store.RetentionRule{}, store.RetentionRule{Keep: n(3)}); r.Active() || !slices.Equal(r.Missing(), []string{"grace"}) {
		t.Fatalf("no grace anywhere: %+v", r)
	}
	if r := release.InForce(store.RetentionRule{Keep: n(0)}, store.RetentionRule{Keep: n(3), Grace: d(time.Hour)}); r.Active() || !r.None ||
		r.KeepFrom != release.FromChannel {
		t.Fatalf("none at the channel: %+v", r)
	}
	if r := release.InForce(store.RetentionRule{}, store.RetentionRule{}); r.Active() || len(r.Missing()) != 2 {
		t.Fatalf("no rule anywhere: %+v", r)
	}
	old := store.MaxRetentionGrace
	store.MaxRetentionGrace = time.Hour
	defer func() { store.MaxRetentionGrace = old }()
	if r := release.InForce(store.RetentionRule{Keep: n(1), Grace: d(48 * time.Hour)}, store.RetentionRule{}); r.Grace != time.Hour || !r.Capped {
		t.Fatalf("a lowered ceiling caps the stored grace: %+v", r)
	}
	f := release.InForceFields(r)
	if g := f["grace"].(map[string]any); g["value"] != "720h" || g["from"] != "tenant" {
		t.Fatalf("in_force's grace: %+v", f)
	}
}
