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
		"sink kind": {base + "events: { sinks: [{ name: a, kind: kafka }] }\n", "events.sinks[0].kind"},
		"sink name": {base + "events: { sinks: [{ name: A, kind: jsonl, path: '-' }] }\n", "events.sinks[0].name"},
		"sink twice": {base + "events: { sinks: [{ name: a, kind: jsonl, path: '-' }, { name: a, kind: jsonl, path: '-' }] }\n",
			"events.sinks[1].name"},
		"relative path": {base + "events: { sinks: [{ name: a, kind: jsonl, path: e.jsonl }] }\n", "events.sinks[0].path"},
		"http":          {base + "events: { sinks: [{ name: a, kind: otlp, url: 'http://c:4318/v1/logs' }] }\n", "events.sinks[0].url"},
		"userinfo":      {base + "events: { sinks: [{ name: a, kind: otlp, url: 'https://u:p@c/v1/logs' }] }\n", "events.sinks[0].url"},
		"cidr":          {base + "events: { sinks: [{ name: a, kind: otlp, url: 'https://c/v1/logs', allow: [{ cidr: x }] }] }\n", "allow[0].cidr"},
		"headers":       {base + "events: { sinks: [{ name: a, kind: otlp, url: 'https://c/v1/logs', headers_file: h }] }\n", "headers_file"},
		"tenant":        {base + "events: { sinks: [{ name: a, kind: jsonl, path: '-', tenants: ['Acme!'] }] }\n", "tenants"},
		"port 0": {base + "events: { sinks: [{ name: a, kind: otlp, url: 'https://c/v1/logs', allow: [{ cidr: 10.0.0.0/8, ports: [0] }] }] }\n",
			"0 is not a port"},
		"same file":      {base + "events: { sinks: [{ name: a, kind: jsonl, path: /e }, { name: b, kind: jsonl, path: /e }] }\n", "another sink's file"},
		"long retention": {base + "events: { retention: 2400h, sinks: [{ name: a, kind: jsonl, path: '-' }] }\n", "at most 90 days"},
	} {
		if _, err := Load(write(t, c.file), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, c := range map[string]struct{ file, want string }{
		"one day":          {base + "events: { retention: 24h }\n", "events.retention"},
		"statistics short": {base + "statistics: { retention: 240h }\n", "statistics.retention"},
		"statistics long":  {base + "statistics: { retention: 100000h }\n", "statistics.retention"},
	} {
		if _, err := Load(write(t, c.file), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if s := cfg.StatisticsSettings(); s.Retention != DefaultStatisticsRetention {
		t.Errorf("statistics default: %v", s.Retention)
	}
	if _, err := Load(write(t, base), []string{"KISTA_EVENTS__RETENTION=1h"}); err == nil {
		t.Error("events from the environment")
	}
}

func TestEventSinksConfig(t *testing.T) {
	cfg, err := Load(write(t, base+`events:
  resource: { deployment.environment.name: prod }
  sinks:
    - { name: collector, kind: otlp, url: "https://otel.internal:4318/v1/logs", tenants: ["*"], server: true,
        allow: [{ cidr: 10.0.0.0/8, ports: [4318] }], headers_file: /run/secrets/otel-headers }
    - { name: stdout, kind: jsonl, path: "-", tenants: [acme] }
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := cfg.Events.Sinks; len(s) != 2 || !s[0].Server || s[1].Tenants[0] != "acme" || cfg.Events.Resource["deployment.environment.name"] != "prod" {
		t.Fatalf("sinks: %+v", cfg.Events)
	}
}
