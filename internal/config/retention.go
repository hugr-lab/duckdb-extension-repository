package config

import "time"

// Retention schedules the channels' version retention pass (spec 0042). The rules are the tenants'
// and channels' own: kista has no retention by default.
type Retention struct {
	// Interval is the time between scheduled passes; left out, DefaultRetentionInterval; 0: none
	// (kista admin retention run only).
	Interval *time.Duration `yaml:"interval" json:",omitempty"`
}

// DefaultRetentionInterval is retention.interval when it is left out.
const DefaultRetentionInterval = 24 * time.Hour

// IntervalOrDefault is retention.interval in force.
func (r Retention) IntervalOrDefault() time.Duration {
	if r.Interval != nil {
		return *r.Interval
	}
	return DefaultRetentionInterval
}

func validateRetention(bad func(string, ...any), c Config) {
	if i := c.Retention.Interval; i != nil && *i != 0 && *i < time.Minute {
		bad("retention.interval must be 0 (no scheduled pass) or at least 1m")
	}
}
