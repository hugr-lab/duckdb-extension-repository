package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

var (
	sinkName   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	tenantName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// Events configures the event buffer (spec 0010). The whole block is file-only.
type Events struct {
	Retention        time.Duration `yaml:"retention"`           // default 30 days
	ClientAddresses  string        `yaml:"client_addresses"`    // full, truncated (default) or none
	MaxRowsPerTenant int           `yaml:"max_rows_per_tenant"` // default 1,000,000
	// Settle is how far behind now a reader following the events stops (spec 0040: the writer
	// commits each replica's events in batches, so an event can commit after a later one is
	// readable); default 30s.
	Settle time.Duration `yaml:"settle"`
	// Resource are attributes every OTLP export carries (deployment.environment.name, …).
	Resource map[string]string `yaml:"resource"`
	Sinks    []Sink            `yaml:"sinks"`
}

// Sink is where events are delivered (spec 0010): an OTLP/HTTP logs endpoint or JSON lines.
type Sink struct {
	Name    string   `yaml:"name"`
	Kind    string   `yaml:"kind"`    // otlp | jsonl
	Tenants []string `yaml:"tenants"` // tenant names, or "*" for every tenant; none by default
	Server  bool     `yaml:"server"`  // the server's own events too
	// Unmasked sends server administrators' and the CLI's identities on tenants' events as they
	// are; by default they show as "server", as to the tenants' readers.
	Unmasked bool `yaml:"unmasked"`
	// otlp
	URL         string        `yaml:"url"`
	Allow       []EgressAllow `yaml:"allow"`
	HeadersFile string        `yaml:"headers_file"` // "Name: value" lines
	Timeout     time.Duration `yaml:"timeout"`      // default 10s
	// jsonl
	Path    string   `yaml:"path"`     // "-": stdout
	MaxSize ByteSize `yaml:"max_size"` // a file's size before it rotates; default 100MiB
	Keep    int      `yaml:"keep"`     // rotated files kept; 0: 5
}

// Defaults of events:.
const (
	DefaultEventRetention     = 30 * 24 * time.Hour
	DefaultEventRowsPerTenant = 1000000
	DefaultEventSettle        = 30 * time.Second
)

// EventSettings returns events: with its defaults.
func (c Config) EventSettings() Events {
	e := c.Events
	if e.Retention == 0 {
		e.Retention = DefaultEventRetention
	}
	if e.ClientAddresses == "" {
		e.ClientAddresses = "truncated"
	}
	if e.MaxRowsPerTenant == 0 {
		e.MaxRowsPerTenant = DefaultEventRowsPerTenant
	}
	if e.Settle == 0 {
		e.Settle = DefaultEventSettle
	}
	return e
}

func validateEvents(bad func(string, ...any), c Config) {
	e := c.Events
	// Two days at least, in code: the installers of a day are counted from its events after it
	// ends (spec 0010 phase 2a), so a shorter buffer loses them. The most is the server's
	// (limits.ceilings.events_retention, 365 days by default; spec 0038 amended), and less with
	// sinks (events_retention_with_sinks, 90 days): without any, the buffer is the only record.
	// A ceiling below 1s is refused as the ceilings are checked, not here again.
	ceil := c.Limits.Ceilings.Setting()
	if e.Retention != 0 && e.Retention < 48*time.Hour {
		bad("events.retention must be at least 48h (a day's installers are counted from its events after it ends)")
	}
	if r := c.EventSettings().Retention; ceil.EventsRetention >= time.Second && r > ceil.EventsRetention {
		bad("events.retention is at most limits.ceilings.events_retention (%s)", ceil.EventsRetention)
	}
	// a second at least: the writer's batches
	if e.Settle != 0 && e.Settle < time.Second {
		bad("events.settle must be at least 1s")
	}
	// the retention in force, the default included: a lowered ceiling refuses the 30-day default too
	if w := ceil.EventsRetentionWithSinks; len(e.Sinks) > 0 && w >= time.Second && c.EventSettings().Retention > w {
		bad("events.retention is at most limits.ceilings.events_retention_with_sinks (%s) when sinks are configured", w)
	}
	if len(e.Sinks) > 16 {
		bad("events.sinks holds at most 16 sinks")
	}
	names, paths := map[string]bool{}, map[string]bool{}
	for i, s := range e.Sinks {
		where := fmt.Sprintf("events.sinks[%d]", i)
		if !sinkName.MatchString(s.Name) || names[s.Name] {
			bad("%s.name must match [a-z][a-z0-9-]{0,31} and be unique", where)
		}
		names[s.Name] = true
		for _, t := range s.Tenants {
			if t != "*" && !tenantName.MatchString(t) {
				bad("%s.tenants: %q is not a tenant name or *", where, t)
			}
		}
		switch s.Kind {
		case "otlp":
			u, err := url.Parse(s.URL)
			devHTTP := u != nil && u.Scheme == "http" && c.Profile == ProfileDev && c.Egress.AllowLoopbackHTTP // egress checks loopback
			if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.Scheme != "https" && !devHTTP {
				bad("%s.url must be an https URL (http to loopback only in profile dev with egress.allow_loopback_http)", where)
			}
			for j, a := range s.Allow {
				if _, err := netip.ParsePrefix(a.CIDR); err != nil {
					bad("%s.allow[%d].cidr %q is not a CIDR", where, j, a.CIDR)
				}
				if slices.Contains(a.Ports, 0) {
					bad("%s.allow[%d].ports: 0 is not a port", where, j)
				}
			}
			if s.HeadersFile != "" && !filepath.IsAbs(s.HeadersFile) {
				bad("%s.headers_file must be an absolute path", where)
			}
			if s.Timeout != 0 && (s.Timeout < time.Second || s.Timeout > time.Minute) {
				bad("%s.timeout must be within 1s..60s", where)
			}
		case "jsonl":
			if s.Path != "-" && !filepath.IsAbs(s.Path) {
				bad("%s.path must be - (stdout) or an absolute path", where)
			}
			if s.Path != "-" && paths[filepath.Clean(s.Path)] {
				bad("%s.path is another sink's file", where)
			}
			paths[filepath.Clean(s.Path)] = true
			if s.Keep < 0 || s.Keep > 100 {
				bad("%s.keep must be within 0..100", where)
			}
		default:
			bad("%s.kind must be otlp or jsonl", where)
		}
	}
	switch e.ClientAddresses {
	case "", "full", "truncated", "none":
	default:
		bad("events.client_addresses must be full, truncated or none")
	}
	if e.MaxRowsPerTenant < 0 || e.MaxRowsPerTenant > 100000000 {
		bad("events.max_rows_per_tenant must be within 1..100000000 (0: the default)")
	}
}

// Statistics configures download statistics (spec 0010 phase 2a) and usage reports (spec 0012
// phase 1b).
type Statistics struct {
	Retention time.Duration `yaml:"retention"` // default 3 years
	// UsageAccept false answers usage reports and keeps nothing; default true.
	UsageAccept *bool `yaml:"usage_accept"`
	// DistributorMinTenants is how many non-public tenants a distributor's row needs before their
	// part is answered (spec 0036 phase 2); default 3, at least 2.
	DistributorMinTenants int `yaml:"distributor_min_tenants"`
	// PublicTenants are the tenants, by name, whose downloads always count in distributor statistics
	// (spec 0036 phase 2); none by default.
	PublicTenants []string `yaml:"public_tenants"`
}

// DefaultDistributorMinTenants is statistics.distributor_min_tenants' default.
const DefaultDistributorMinTenants = 3

// UsageAccepted reports whether usage reports are kept (statistics.usage_accept, default true).
func (s Statistics) UsageAccepted() bool { return s.UsageAccept == nil || *s.UsageAccept }

// DefaultStatisticsRetention is how long counts and installers are kept.
const DefaultStatisticsRetention = 3 * 365 * 24 * time.Hour

// StatisticsSettings returns statistics: with its defaults.
func (c Config) StatisticsSettings() Statistics {
	s := c.Statistics
	if s.Retention == 0 {
		s.Retention = DefaultStatisticsRetention
	}
	if s.DistributorMinTenants == 0 {
		s.DistributorMinTenants = DefaultDistributorMinTenants
	}
	return s
}

func validateStatistics(bad func(string, ...any), c Config) {
	// 30 days at least, in code: the API answers every name's downloads in the last 30 days and a
	// 30-day range by default from the statistics kept. The most is the server's
	// (limits.ceilings.statistics_retention, 10 years by default; spec 0038 amended).
	if r := c.Statistics.Retention; r != 0 && r < 30*24*time.Hour {
		bad("statistics.retention must be at least 720h (30 days: the API answers the last 30 days' downloads)")
	}
	if r, ceil := c.StatisticsSettings().Retention, c.Limits.Ceilings.Setting().StatisticsRetention; ceil >= time.Second && r > ceil {
		bad("statistics.retention is at most limits.ceilings.statistics_retention (%s)", ceil)
	}
	if n := c.Statistics.DistributorMinTenants; n != 0 && n < 2 {
		bad("statistics.distributor_min_tenants must be at least 2 (0: the default, 3)")
	}
	for _, t := range c.Statistics.PublicTenants {
		if !tenantName.MatchString(t) {
			bad("statistics.public_tenants: %q is not a tenant name", t)
		}
	}
}

// Telemetry configures OpenTelemetry metrics (spec 0010 phase 2b).
type Telemetry struct {
	Metrics Metrics `yaml:"metrics"`
}

// Metrics are what metrics may name.
type Metrics struct {
	Tenants   []string `yaml:"tenants"`    // tenant names, or "*": whose ids, channels and extensions appear; none by default
	Labels    []string `yaml:"labels"`     // kista.downloads' optional labels: version, platform, duckdb_version
	MaxSeries int      `yaml:"max_series"` // per instrument; default 10,000
}

// metricLabels are the optional labels of kista.downloads.
var metricLabels = []string{"version", "platform", "duckdb_version"}

// DefaultMaxSeries is how many series an instrument holds before its overflow series counts.
const DefaultMaxSeries = 10000

// TelemetrySettings returns telemetry: with its defaults.
func (c Config) TelemetrySettings() Telemetry {
	t := c.Telemetry
	if t.Metrics.MaxSeries == 0 {
		t.Metrics.MaxSeries = DefaultMaxSeries
	}
	return t
}

func validateTelemetry(bad func(string, ...any), c Config) {
	m := c.Telemetry.Metrics
	for _, t := range m.Tenants {
		if t != "*" && !tenantName.MatchString(t) {
			bad("telemetry.metrics.tenants: %q is not a tenant name or *", t)
		}
	}
	for _, l := range m.Labels {
		if !slices.Contains(metricLabels, l) {
			bad("telemetry.metrics.labels: %q is not version, platform or duckdb_version", l)
		}
	}
	if m.MaxSeries != 0 && (m.MaxSeries < 100 || m.MaxSeries > 1000000) {
		bad("telemetry.metrics.max_series must be within 100..1,000,000")
	}
}
