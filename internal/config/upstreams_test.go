package config

import (
	"strings"
	"testing"
	"time"
)

func TestUpstreamsConfig(t *testing.T) {
	cfg, err := Load(write(t, base), nil)
	if err != nil {
		t.Fatal(err)
	}
	if u := cfg.UpstreamLimits(); u.Concurrency != 4 || u.FetchTimeout != 10*time.Minute || u.MinRate != 64<<10 || u.Interval != 6*time.Hour {
		t.Fatalf("defaults: %+v", u)
	}
	cfg, err = Load(write(t, base+"upstreams: { concurrency: 8, fetch_timeout: 30m, min_rate: 1MiB }\n"), nil)
	if err != nil || cfg.UpstreamLimits().Concurrency != 8 || cfg.UpstreamLimits().MinRate != 1<<20 {
		t.Fatalf("set: %+v %v", cfg.Upstreams, err)
	}
	for name, c := range map[string]struct{ file, want string }{
		"concurrency": {base + "upstreams: { concurrency: 64 }\n", "upstreams.concurrency"},
		"timeout":     {base + "upstreams: { fetch_timeout: 5s }\n", "upstreams.fetch_timeout"},
		"rate":        {base + "upstreams: { min_rate: 10 }\n", "upstreams.min_rate"},
		"interval":    {base + "upstreams: { interval: 1m }\n", "upstreams.interval"},
		"negative":    {base + "upstreams: { negative_ttl: 5s }\n", "upstreams.negative_ttl"},
	} {
		if _, err := Load(write(t, c.file), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Load(write(t, base), []string{"KISTA_UPSTREAMS__CONCURRENCY=2"}); err == nil {
		t.Error("upstreams from the environment")
	}
}
