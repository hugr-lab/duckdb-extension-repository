package config

import (
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0038: the default limits: a key left out is unlimited, 0 is none, bytes take a size, a count
// above its ceiling (or a value that is none of these) refuses to start; the bootstrap's tenant's
// limits are checked the same way.
func TestLimits(t *testing.T) {
	cfg, err := Load(write(t, base), nil)
	if err != nil {
		t.Fatal(err)
	}
	if l, err := cfg.LimitSettings(); err != nil || l != store.UnlimitedLimits() {
		t.Fatalf("none set: %+v %v", l, err)
	}
	cfg, err = Load(write(t, base+"limits: { bytes: 50GiB, channels: 0, upstreams: unlimited }\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if l, err := cfg.LimitSettings(); err != nil || l != (store.Limits{Bytes: 50 << 30, Channels: 0, Upstreams: store.LimitUnlimited,
		Publishers: store.LimitUnlimited}) {
		t.Fatalf("set: %+v %v", l, err)
	}
	for _, bad := range []string{"limits: { channels: 1001 }\n", "limits: { publishers: -1 }\n", "limits: { bytes: lots }\n",
		"limits: { upstreams: default }\n", "bootstrap: { tenant: { name: acme, limits: { channels: 5000 } } }\n"} {
		if _, err := Load(write(t, base+bad), nil); err == nil || !strings.Contains(err.Error(), "limits") {
			t.Errorf("%s: %v", bad, err)
		}
	}
}
