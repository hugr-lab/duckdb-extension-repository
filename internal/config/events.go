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
	return e
}

func validateEvents(bad func(string, ...any), c Config) {
	e := c.Events
	// up to 90 days with sinks (spec 0010); without any, the buffer is the only record
	// two days at least: the installers of a day are counted from its events after it ends
	if e.Retention != 0 && (e.Retention < 48*time.Hour || e.Retention > 365*24*time.Hour) {
		bad("events.retention must be within 2..365 days")
	}
	if len(e.Sinks) > 0 && e.Retention > 90*24*time.Hour {
		bad("events.retention is at most 90 days when sinks are configured")
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

// Statistics configures download statistics (spec 0010 phase 2a).
type Statistics struct {
	Retention time.Duration `yaml:"retention"` // default 3 years
}

// DefaultStatisticsRetention is how long counts and installers are kept.
const DefaultStatisticsRetention = 3 * 365 * 24 * time.Hour

// StatisticsSettings returns statistics: with its defaults.
func (c Config) StatisticsSettings() Statistics {
	s := c.Statistics
	if s.Retention == 0 {
		s.Retention = DefaultStatisticsRetention
	}
	return s
}

func validateStatistics(bad func(string, ...any), c Config) {
	if r := c.Statistics.Retention; r != 0 && (r < 30*24*time.Hour || r > 10*365*24*time.Hour) {
		bad("statistics.retention must be within 30 days..10 years")
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
