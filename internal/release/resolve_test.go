package release_test

import (
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

func cand(id, name, ext, state, vis string, seq int64, abi, dv string, c *store.CAPI, at int) store.Candidate {
	var r store.Candidate
	r.ID, r.Name, r.ExtVersion, r.Platform, r.State, r.Visibility, r.Seq = id, name, ext, "linux_amd64", state, vis, seq
	r.ABI, r.DuckDBVersion, r.CAPI = abi, dv, c
	r.CreatedAt = time.Unix(int64(at), 0)
	return r
}

// The resolution rules the DuckDB routes and the index share.
func TestSnapshotResolution(t *testing.T) {
	ch := store.Channel{ID: "c", Kind: store.ChannelSigned, ServingKeyID: "k"}
	capis := map[string][]store.CAPI{
		"v2.0.0": {{Major: 1, Minor: 5, Patch: 6}, {Major: 2}},
		"v1.9.0": {{Major: 1, Minor: 4}},
	}
	rels := []store.Candidate{
		cand("a", "t", "1.0", store.ReleaseActive, store.Public, 1, store.ABICPP, "v2.0.0", nil, 1),
		cand("b", "t", "1.1", store.ReleaseActive, store.Private, 2, store.ABICPP, "v2.0.0", nil, 2),
		cand("c", "t", "1.2", store.ReleaseActive, store.Public, 0, store.ABICPP, "v2.0.0", nil, 3), // not current
		cand("d", "t", "0.9", store.ReleaseYanked, store.Public, 3, store.ABICPP, "v2.0.0", nil, 4),
		cand("e", "t", "0.8", store.ReleaseDeprecated, store.Public, 4, store.ABICPP, "v2.0.0", nil, 5),
		cand("f", "d", "0.1", store.ReleaseActive, store.Public, 1, store.ABICStruct, "", &store.CAPI{Major: 1, Minor: 2, Patch: 0}, 1),
		cand("g", "d", "0.1", store.ReleaseDeprecated, store.Public, 0, store.ABICStruct, "", &store.CAPI{Major: 2, Minor: 0, Patch: 0}, 2),
		cand("h", "d", "0.1", store.ReleaseYanked, store.Public, 0, store.ABICStruct, "", &store.CAPI{Major: 3, Minor: 0, Patch: 0}, 3),
		cand("i", "d", "0.2", store.ReleaseActive, store.Public, 2, store.ABICStruct, "", &store.CAPI{Major: 1, Minor: 5, Patch: 0}, 4),
	}
	s := release.NewSnapshot(ch, rels, capis, nil)
	id := func(c *store.Candidate) string {
		if c == nil {
			return "-"
		}
		return c.ID
	}
	for _, tc := range []struct {
		name, got, want string
	}{
		{"flat, public view: highest seq among active public", id(s.Flat("v2.0.0", "linux_amd64", "t", release.PublicOnly)), "a"},
		{"flat, everything: the private one", id(s.Flat("v2.0.0", "linux_amd64", "t", release.Everything)), "b"},
		{"flat: another DuckDB version", id(s.Flat("v1.9.0", "linux_amd64", "t", release.Everything)), "-"},
		{"flat: an unknown platform", id(s.Flat("v2.0.0", "osx_arm64", "t", release.Everything)), "-"},
		{"versioned: not current is still served", id(s.Versioned("v2.0.0", "linux_amd64", "t", "1.2", release.PublicOnly)), "c"},
		{"versioned: deprecated is served", id(s.Versioned("v2.0.0", "linux_amd64", "t", "0.8", release.PublicOnly)), "e"},
		{"versioned: yanked is not", id(s.Versioned("v2.0.0", "linux_amd64", "t", "0.9", release.PublicOnly)), "-"},
		{"versioned: private in the public view", id(s.Versioned("v2.0.0", "linux_amd64", "t", "1.1", release.PublicOnly)), "-"},
		{"c_struct: the highest accepted major, deprecated or not", id(s.Versioned("v2.0.0", "linux_amd64", "d", "0.1", release.Everything)), "g"},
		{"c_struct: with yanked, the yanked v3 is not accepted by v2.0.0", id(s.Versioned("v2.0.0", "linux_amd64", "d", "0.1", release.Everything,
			store.ReleaseActive, store.ReleaseDeprecated, store.ReleaseYanked)), "g"},
		{"c_struct: v1.9.0 accepts v1.4 at most", id(s.Versioned("v1.9.0", "linux_amd64", "d", "0.1", release.Everything)), "f"},
		{"c_struct: v1.5.0 build on v1.9.0 (max v1.4.0)", id(s.Versioned("v1.9.0", "linux_amd64", "d", "0.2", release.Everything)), "-"},
		{"c_struct flat on v1.9.0", id(s.Flat("v1.9.0", "linux_amd64", "d", release.Everything)), "f"},
		{"c_struct flat on v2.0.0", id(s.Flat("v2.0.0", "linux_amd64", "d", release.Everything)), "i"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, tc.got, tc.want)
		}
	}
	// without a serving key nothing is served; a passthrough channel has no releases
	noKey := release.NewSnapshot(store.Channel{ID: "c", Kind: store.ChannelSigned}, rels, capis, nil)
	if noKey.Flat("v2.0.0", "linux_amd64", "t", release.Everything) != nil ||
		noKey.Versioned("v2.0.0", "linux_amd64", "t", "1.0", release.Everything) != nil {
		t.Error("a channel without a serving key serves")
	}
	pt := release.NewSnapshot(store.Channel{ID: "c", Kind: store.ChannelPassthrough, ServingKeyID: "k"}, rels, capis, nil)
	if len(pt.Releases) != 0 || pt.Platforms("t") != nil {
		t.Error("a passthrough channel lists releases")
	}
	if got := s.Platforms("t"); len(got) != 1 || got[0] != "linux_amd64" {
		t.Errorf("platforms: %v", got)
	}
	if got := s.Versions(); len(got) != 2 || got[0] != "v1.9.0" {
		t.Errorf("versions: %v", got)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"v1.9.0", "v1.10.0", -1}, {"0.10", "0.9", 1}, {"1.0", "1.0", 0}, {"1.0", "1.0.1", -1},
		{"v2.0.0", "v2.0.0-rc1", -1}, {"01", "1", -1}, {"eb0d9df48e", "v1.0.0", -1}, {"1.2a", "1.2b", -1},
	} {
		if got := release.CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("%s vs %s: %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := release.CompareVersions(tc.b, tc.a); got != -tc.want {
			t.Errorf("%s vs %s: %d, want %d", tc.b, tc.a, got, -tc.want)
		}
	}
}

// Snapshots builds one snapshot for concurrent callers, keeps it while the counters hold, and
// replaces it after a change.
func TestSnapshotsCache(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		if _, _, err := en.add(t, ext(t, 1000, 1, cpp("1.0")), release.AddOptions{Name: "tresor"}); err != nil {
			t.Fatal(err)
		}
		sc, err := en.st.GetServeChannel(ctx, "acme", "prod")
		if err != nil {
			t.Fatal(err)
		}
		snaps := &release.Snapshots{Store: en.st}
		got := make(chan *release.Snapshot, 8)
		for range 8 {
			go func() {
				s, err := snaps.Get(ctx, sc.Channel)
				if err != nil {
					t.Error(err)
				}
				got <- s
			}()
		}
		first := <-got
		for range 7 {
			if s := <-got; s != first {
				t.Fatal("concurrent callers got different snapshots")
			}
		}
		if len(first.Releases) != 1 || first.ServingKey == "" || len(first.Keys) != 1 {
			t.Fatalf("snapshot: %d releases, serving key %q, %d keys", len(first.Releases), first.ServingKey, len(first.Keys))
		}
		if _, _, err := en.add(t, ext(t, 1000, 2, cpp("1.1")), release.AddOptions{Name: "tresor"}); err != nil {
			t.Fatal(err)
		}
		// a caller holding the old row still gets a snapshot at least as new as it
		old, err := snaps.Get(ctx, sc.Channel)
		if err != nil || old != first {
			t.Fatalf("old counters: %v", err)
		}
		sc2, err := en.st.GetServeChannel(ctx, "acme", "prod")
		if err != nil {
			t.Fatal(err)
		}
		s2, err := snaps.Get(ctx, sc2.Channel)
		if err != nil || s2 == first || len(s2.Releases) != 2 {
			t.Fatalf("after a change: %v", err)
		}
		if s, _ := snaps.Get(ctx, sc.Channel); s != s2 {
			t.Fatal("the old row's request rebuilt an older snapshot")
		}
	})
}
