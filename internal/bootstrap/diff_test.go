package bootstrap

import (
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Each field of an upstream that exists is compared with the section's (spec 0024); more than the
// section lists is the administrator's.
func TestUpstreamDiff(t *testing.T) {
	want := config.BootstrapUpstream{Name: "v", Kind: store.UpstreamRepo, Prefix: "https://up.example/x", Keys: []string{"k1"},
		Channel: "prod", Mode: store.ModeMirror, Visibility: store.Private, Credential: "c", Platforms: []string{"linux_amd64"},
		Extensions: []config.BootstrapUpstreamEntry{{Name: "acl", Versions: []string{"1.0.0"}}}}
	cur := func() store.Upstream {
		return store.Upstream{Kind: store.UpstreamRepo, Prefix: "https://up.example/x", ChannelID: "ch1", Mode: store.ModeMirror, FetchMode: store.FetchEager,
			Visibility: store.Private, Credential: "c", Keys: []string{"k1", "k2"}, Platforms: []string{"linux_amd64", "osx_arm64"},
			Entries: []store.UpstreamEntry{{Name: "acl", Versions: []string{"1.0.0"}}, {Name: "more"}}}
	}
	if d := upstreamDiff(cur(), want, "ch1"); d != "" {
		t.Fatalf("the same, with more: %q", d)
	}
	for name, c := range map[string]struct {
		change func(*store.Upstream)
		ch     string
		want   string
	}{
		"kind":           {func(u *store.Upstream) { u.Kind = store.UpstreamCore }, "ch1", "kind"},
		"prefix":         {func(u *store.Upstream) { u.Prefix += "/y" }, "ch1", "prefix"},
		"channel":        {func(*store.Upstream) {}, "ch2", "channel"},
		"no channel":     {func(*store.Upstream) {}, "", "channel"},
		"mode":           {func(u *store.Upstream) { u.Mode = store.ModePullThrough }, "ch1", "mode"},
		"fetch":          {func(u *store.Upstream) { u.FetchMode = store.FetchLazy }, "ch1", "fetch"},
		"eager":          {func(u *store.Upstream) { u.Entries[0].Eager = true }, "ch1", "allowlist entry acl"},
		"visibility":     {func(u *store.Upstream) { u.Visibility = store.Public }, "ch1", "visibility"},
		"credential":     {func(u *store.Upstream) { u.Credential = "" }, "ch1", "credential"},
		"a key":          {func(u *store.Upstream) { u.Keys = []string{"k2"} }, "ch1", "keys"},
		"a platform":     {func(u *store.Upstream) { u.Platforms = []string{"osx_arm64"} }, "ch1", "platforms"},
		"an entry":       {func(u *store.Upstream) { u.Entries = []store.UpstreamEntry{{Name: "more"}} }, "ch1", "allowlist (acl"},
		"versions":       {func(u *store.Upstream) { u.Entries[0].Versions = nil }, "ch1", "allowlist entry acl"},
		"allow_reserved": {func(u *store.Upstream) { u.Entries[0].AllowReserved = true }, "ch1", "allowlist entry acl"},
	} {
		u := cur()
		c.change(&u)
		if d := upstreamDiff(u, want, c.ch); !strings.HasPrefix(d, c.want) {
			t.Errorf("%s: %q", name, d)
		}
	}
}
