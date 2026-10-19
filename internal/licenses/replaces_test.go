package licenses

import (
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0012's replacement rules with spec 0021's condition: a licence from another source replaces
// a stored one only when both are signed with the same key, or the stored one's grace has ended.
func TestReplaces(t *testing.T) {
	const now = 1000
	stored := func(source, upstream, key string) store.License {
		return store.License{Source: source, UpstreamID: upstream, Key: key, IssuedAt: 100, ExpiresAt: 2000, GraceUntil: 3000}
	}
	relayed := func(upstream, key string, iat, exp int64) store.License {
		return store.License{Source: store.LicenseUpstream, UpstreamID: upstream, Key: key, IssuedAt: iat, ExpiresAt: exp}
	}
	for _, c := range []struct {
		name     string
		old, l   store.License
		replaces bool
	}{
		{"same upstream, later iat", stored(store.LicenseUpstream, "u", "A"), relayed("u", "A", 200, 1500), true},
		{"same upstream, new key, later iat", stored(store.LicenseUpstream, "u", "A"), relayed("u", "B", 200, 1500), true},
		{"same upstream, older", stored(store.LicenseUpstream, "u", "A"), relayed("u", "A", 50, 1500), false},
		{"another upstream, same key, longer", stored(store.LicenseUpstream, "u", "A"), relayed("v", "A", 200, 2500), true},
		{"another upstream, same key, shorter", stored(store.LicenseUpstream, "u", "A"), relayed("v", "A", 200, 1500), false},
		{"another upstream, its own key, longer", stored(store.LicenseUpstream, "u", "A"), relayed("v", "B", 200, 9000), false},
		{"an import, same key, longer", stored(store.LicenseImport, "", "A"), relayed("u", "A", 200, 2500), true},
		{"an import, another key, longer", stored(store.LicenseImport, "", "A"), relayed("u", "B", 200, 9000), false},
		{"past its grace, another key", store.License{Source: store.LicenseImport, Key: "A", GraceUntil: now}, relayed("u", "B", 200, 1500), true},
	} {
		if got := replaces(c.old, c.l, now); got != c.replaces {
			t.Errorf("%s: %v, want %v", c.name, got, c.replaces)
		}
	}
}
