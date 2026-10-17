package config

import (
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
)

// Licenses configures licensing (spec 0012): the issuers' licence keys kista verifies licences
// against, and the relay's interval. Without keys licensing is off: imports and the relay are
// refused, and no licence is served.
type Licenses struct {
	// Keys are the licence public keys trusted, SPKI PEM (or base64 DER): RSA-2048, e = 65537.
	Keys []string `yaml:"keys"`
	// Refresh is the time between an upstream's licence relay runs (default 6h, within 1h..24h).
	Refresh time.Duration `yaml:"refresh"`
}

// RefreshOrDefault is licenses.refresh or its default.
func (l Licenses) RefreshOrDefault() time.Duration {
	if l.Refresh == 0 {
		return 6 * time.Hour
	}
	return l.Refresh
}

// TrustedKeys are the keys, parsed (validation has checked them).
func (l Licenses) TrustedKeys() license.Keys {
	ks, err := license.ParseKeys(l.Keys)
	if err != nil {
		return license.Keys{}
	}
	return ks
}

func validateLicenses(bad func(string, ...any), c Config) {
	if _, err := license.ParseKeys(c.Licenses.Keys); err != nil {
		bad("licenses.keys: %v", err)
	}
	if r := c.Licenses.Refresh; r != 0 && (r < time.Hour || r > 24*time.Hour) {
		bad("licenses.refresh must be 0 (6h) or within 1h..24h")
	}
}
