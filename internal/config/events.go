package config

import "time"

// Events configures the event buffer (spec 0010). The whole block is file-only.
type Events struct {
	Retention        time.Duration `yaml:"retention"`           // default 30 days
	ClientAddresses  string        `yaml:"client_addresses"`    // full, truncated (default) or none
	MaxRowsPerTenant int           `yaml:"max_rows_per_tenant"` // default 1,000,000
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
	// up to 90 days with sinks (spec 0010 phase 1b); without any, the buffer is the only record
	if e.Retention != 0 && (e.Retention < 24*time.Hour || e.Retention > 365*24*time.Hour) {
		bad("events.retention must be within 1..365 days")
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
