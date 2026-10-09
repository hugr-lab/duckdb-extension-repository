package config

import (
	"strings"
	"testing"
	"time"
)

func TestEventsConfig(t *testing.T) {
	cfg, err := Load(write(t, base), nil)
	if err != nil {
		t.Fatal(err)
	}
	if e := cfg.EventSettings(); e.Retention != 30*24*time.Hour || e.ClientAddresses != "truncated" || e.MaxRowsPerTenant != 1000000 {
		t.Fatalf("defaults: %+v", e)
	}
	for name, c := range map[string]struct{ file, want string }{
		"retention": {base + "events: { retention: 1h }\n", "events.retention"},
		"clients":   {base + "events: { client_addresses: some }\n", "events.client_addresses"},
		"rows":      {base + "events: { max_rows_per_tenant: -1 }\n", "events.max_rows_per_tenant"},
	} {
		if _, err := Load(write(t, c.file), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Load(write(t, base), []string{"KISTA_EVENTS__RETENTION=1h"}); err == nil {
		t.Error("events from the environment")
	}
}
