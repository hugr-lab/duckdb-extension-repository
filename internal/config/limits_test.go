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

// Spec 0038, amended 2026-10-31: the server's ceilings are configured (limits.ceilings), each left
// out at its default; a tenant default above the default ceiling is accepted under a higher
// configured one; a negative ceiling, a default above its ceiling, or ceilings in the bootstrap
// tenant's limits refuse to start.
func TestCeilings(t *testing.T) {
	cfg, err := Load(write(t, base), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Limits.Ceilings.Settings(); got != store.DefaultCeilings() {
		t.Fatalf("none set: %+v", got)
	}
	cfg, err = Load(write(t, base+"limits: { channels: 1500, ceilings: { channels: 2000, issuers: 4, max_hold: 240h } }\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Limits.Ceilings.Settings()
	if c.Channels != 2000 || c.Issuers != 4 || c.MaxHold.Hours() != 240 || c.Upstreams != store.DefaultCeilings().Upstreams {
		t.Fatalf("set: %+v", c)
	}
	if l, err := cfg.LimitSettings(); err != nil || l.Channels != 1500 {
		t.Fatalf("a default under a raised ceiling: %+v %v", l, err)
	}
	for _, bad := range []string{"limits: { ceilings: { grants: -1 } }\n", "limits: { ceilings: { max_hold: -1h } }\n",
		"limits: { channels: 30, ceilings: { channels: 20 } }\n",
		"bootstrap: { tenant: { name: acme, limits: { ceilings: { channels: 5 } } } }\n"} {
		if _, err := Load(write(t, base+bad), nil); err == nil || !strings.Contains(err.Error(), "limits") {
			t.Errorf("%s: %v", bad, err)
		}
	}
}
