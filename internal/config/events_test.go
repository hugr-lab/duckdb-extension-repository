package config

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestEventsConfig(t *testing.T) {
	cfg, err := Load(write(t, base), nil)
	if err != nil {
		t.Fatal(err)
	}
	if e := cfg.EventSettings(); e.Retention != 30*24*time.Hour || e.ClientAddresses != "truncated" || e.MaxRowsPerTenant != 1000000 || e.Settle != 30*time.Second {
		t.Fatalf("defaults: %+v", e)
	}
	// events.settle has no ceiling: a deployment chooses its window
	if c, err := Load(write(t, base+"events: { settle: 48h }\n"), nil); err != nil || c.EventSettings().Settle != 48*time.Hour {
		t.Fatalf("a long settle: %v", err)
	}
	// spec 0038 amended: the cap with sinks is limits.ceilings.events_retention (90 days by default)
	sink := "events: { retention: %s, sinks: [{ name: a, kind: jsonl, path: '-' }] }\n"
	if _, err := Load(write(t, base+fmt.Sprintf(sink, "2160h")), nil); err != nil {
		t.Fatalf("90 days with a sink under the default ceiling: %v", err)
	}
	if _, err := Load(write(t, base+fmt.Sprintf(sink, "4320h")+"limits: { ceilings: { events_retention: 4320h } }\n"), nil); err != nil {
		t.Fatalf("180 days with a sink under a raised ceiling: %v", err)
	}
	if _, err := Load(write(t, base+"events: { retention: 4320h }\nlimits: { ceilings: { events_retention: 168h } }\n"), nil); err != nil {
		t.Fatalf("without sinks the ceiling does not apply: %v", err)
	}
	for name, c := range map[string]struct{ file, want string }{
		"retention":    {base + "events: { retention: 1h }\n", "events.retention"},
		"settle short": {base + "events: { settle: 500ms }\n", "events.settle"},
		"settle zero":  {base + "events: { settle: -1s }\n", "events.settle"},
		"clients":      {base + "events: { client_addresses: some }\n", "events.client_addresses"},
		"rows":         {base + "events: { max_rows_per_tenant: -1 }\n", "events.max_rows_per_tenant"},
		"sink kind":    {base + "events: { sinks: [{ name: a, kind: kafka }] }\n", "events.sinks[0].kind"},
		"sink name":    {base + "events: { sinks: [{ name: A, kind: jsonl, path: '-' }] }\n", "events.sinks[0].name"},
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
		"long retention": {base + "events: { retention: 2400h, sinks: [{ name: a, kind: jsonl, path: '-' }] }\n", "limits.ceilings.events_retention"},
		// a lowered ceiling refuses a retention above it, the 30-day default included
		"over a lowered ceiling": {base + "events: { retention: 240h, sinks: [{ name: a, kind: jsonl, path: '-' }] }\nlimits: { ceilings: { events_retention: 168h } }\n",
			"events.retention is at most limits.ceilings.events_retention"},
		"default over a lowered ceiling": {base + "events: { sinks: [{ name: a, kind: jsonl, path: '-' }] }\nlimits: { ceilings: { events_retention: 168h } }\n",
			"limits.ceilings.events_retention"},
		"ceiling 0":        {base + "limits: { ceilings: { events_retention: 0s } }\n", "limits.ceilings.events_retention is at least 1s"},
		"ceiling negative": {base + "limits: { ceilings: { events_retention: -1h } }\n", "limits.ceilings.events_retention is at least 1s"},
	} {
		if _, err := Load(write(t, c.file), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, c := range map[string]struct{ file, want string }{
		"one day":          {base + "events: { retention: 24h }\n", "events.retention"},
		"statistics short": {base + "statistics: { retention: 240h }\n", "statistics.retention"},
		"statistics long":  {base + "statistics: { retention: 100000h }\n", "statistics.retention"},
		// spec 0036 phase 2: the minimum is never below 2, and public tenants are tenant names
		"min tenants one":      {base + "statistics: { distributor_min_tenants: 1 }\n", "statistics.distributor_min_tenants"},
		"min tenants negative": {base + "statistics: { distributor_min_tenants: -3 }\n", "statistics.distributor_min_tenants"},
		"public tenant":        {base + "statistics: { public_tenants: ['Acme!'] }\n", "statistics.public_tenants"},
		"metric label":         {base + "telemetry: { metrics: { labels: [principal] } }\n", "telemetry.metrics.labels"},
		"metric tenant":        {base + "telemetry: { metrics: { tenants: ['Acme!'] } }\n", "telemetry.metrics.tenants"},
		"metric series":        {base + "telemetry: { metrics: { max_series: 5 } }\n", "telemetry.metrics.max_series"},
	} {
		if _, err := Load(write(t, c.file), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if s := cfg.StatisticsSettings(); s.Retention != DefaultStatisticsRetention || !s.UsageAccepted() || s.DistributorMinTenants != 3 ||
		len(s.PublicTenants) != 0 {
		t.Errorf("statistics default: %+v", s)
	}
	if c, err := Load(write(t, base+"statistics: { distributor_min_tenants: 2, public_tenants: [free] }\n"), nil); err != nil ||
		c.StatisticsSettings().DistributorMinTenants != 2 || !slices.Equal(c.StatisticsSettings().PublicTenants, []string{"free"}) {
		t.Errorf("statistics: the minimum and public tenants: %v", err)
	}
	if c, err := Load(write(t, base+"statistics: { usage_accept: false }\n"), nil); err != nil || c.StatisticsSettings().UsageAccepted() {
		t.Errorf("statistics.usage_accept false: %v", err)
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

func TestTelemetryConfig(t *testing.T) {
	cfg, err := Load(write(t, base+"telemetry: { metrics: { tenants: ['*', acme], labels: [version, platform, duckdb_version], max_series: 100 } }\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := cfg.TelemetrySettings().Metrics; len(m.Tenants) != 2 || len(m.Labels) != 3 || m.MaxSeries != 100 {
		t.Fatalf("telemetry: %+v", m)
	}
	cfg, _ = Load(write(t, base), nil)
	if m := cfg.TelemetrySettings().Metrics; m.MaxSeries != DefaultMaxSeries || len(m.Tenants) != 0 {
		t.Fatalf("defaults: %+v", m)
	}
	if _, err := Load(write(t, base), []string{"KISTA_TELEMETRY__METRICS__MAX_SERIES=200"}); err == nil {
		t.Error("telemetry from the environment")
	}
}
