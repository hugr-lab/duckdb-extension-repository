package config

import (
	"strings"
	"testing"
	"time"
)

// Spec 0016: gc's defaults and bounds.
func TestGCConfig(t *testing.T) {
	cfg, err := Load(write(t, base), nil)
	if err != nil {
		t.Fatal(err)
	}
	if g := cfg.GCSettings(); g.Interval != 0 || g.Grace != DefaultGCGrace {
		t.Fatalf("defaults: %+v", g)
	}
	for name, c := range map[string]struct{ file, want string }{
		"short grace":    {base + "gc: { grace: 1h }\n", "gc.grace"},
		"short interval": {base + "gc: { interval: 5m }\n", "gc.interval"},
	} {
		if _, err := Load(write(t, c.file), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if cfg, err := Load(write(t, base+"gc: { interval: 24h, grace: 48h }\n"), nil); err != nil || cfg.GCSettings().Grace != 48*time.Hour {
		t.Fatalf("set: %v", err)
	}
	if _, err := Load(write(t, base), []string{"KISTA_GC__INTERVAL=1h"}); err == nil {
		t.Error("gc from the environment")
	}
}
