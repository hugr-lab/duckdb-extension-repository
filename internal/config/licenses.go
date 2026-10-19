package config

import (
	"regexp"
	"time"
)

// Licenses configures licensing (spec 0012, spec 0021): whether it is on, the licence keys the
// server refuses, and the relay's interval. A licence verifies against the keys its extension's live
// builds declare, recorded from trusted sources; nothing here names a key to trust.
type Licenses struct {
	// Enabled turns licensing on (default true): off, the licence routes answer as for a missing
	// path, and imports and the relay are refused.
	Enabled *bool `yaml:"enabled"`
	// RefuseKeys are key ids ("sha256:<hex>") that count nowhere: a leaked key while builds that
	// declare it are still live.
	RefuseKeys []string `yaml:"refuse_keys"`
	// Keys is spec 0012's trusted keys, replaced by the keys in the builds (spec 0021): accepted and
	// ignored, with a warning at start.
	Keys []string `yaml:"keys"`
	// Refresh is the time between an upstream's licence relay runs (default 6h, within 1h..24h).
	Refresh time.Duration `yaml:"refresh"`
}

// On reports whether licensing is on.
func (l Licenses) On() bool { return l.Enabled == nil || *l.Enabled }

// Refused is licenses.refuse_keys as a set.
func (l Licenses) Refused() map[string]bool {
	out := map[string]bool{}
	for _, k := range l.RefuseKeys {
		out[k] = true
	}
	return out
}

// RefreshOrDefault is licenses.refresh or its default.
func (l Licenses) RefreshOrDefault() time.Duration {
	if l.Refresh == 0 {
		return 6 * time.Hour
	}
	return l.Refresh
}

var kidPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func validateLicenses(bad func(string, ...any), c Config) {
	for i, k := range c.Licenses.RefuseKeys {
		if !kidPattern.MatchString(k) {
			bad("licenses.refuse_keys: entry %d is not a key id (sha256:<64 lowercase hex>, as kista ext inspect prints)", i+1)
		}
	}
	if r := c.Licenses.Refresh; r != 0 && (r < time.Hour || r > 24*time.Hour) {
		bad("licenses.refresh must be 0 (6h) or within 1h..24h")
	}
}
