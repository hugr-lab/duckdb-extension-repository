package config

import (
	"net/netip"
	"net/url"
	"path/filepath"
	"time"
)

// Egress configures the outbound client for requests made on a tenant's behalf (spec 0006). The
// whole block is file-only: it decides what kista may reach.
type Egress struct {
	Allow             []EgressAllow `yaml:"allow"`
	AllowLoopbackHTTP bool          `yaml:"allow_loopback_http"` // profile dev only
	Proxy             string        `yaml:"proxy"`               // an explicit proxy; never from the environment
	CAFile            string        `yaml:"ca_file"`             // trust only this CA bundle (a private IdP)
	Timeout           time.Duration `yaml:"timeout"`
}

// EgressAllow lets a private network be reached: CIDRs, never host names.
type EgressAllow struct {
	CIDR  string   `yaml:"cidr"`
	Ports []uint16 `yaml:"ports"` // empty: 443
}

// EgressAllowPrefixes parses egress.allow (validated).
func (c Config) EgressAllowPrefixes() []netip.Prefix {
	var out []netip.Prefix
	for _, a := range c.Egress.Allow {
		if p, err := netip.ParsePrefix(a.CIDR); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}

func validateEgress(bad func(string, ...any), c Config) {
	e := c.Egress
	for i, a := range e.Allow {
		if _, err := netip.ParsePrefix(a.CIDR); err != nil {
			bad("egress.allow[%d].cidr %q is not a CIDR (host names are never allowlisted)", i, a.CIDR)
		}
		for _, p := range a.Ports {
			if p == 0 {
				bad("egress.allow[%d].ports: 0 is not a port", i)
			}
		}
	}
	if e.AllowLoopbackHTTP && c.Profile != ProfileDev {
		bad("egress.allow_loopback_http is allowed only with profile dev")
	}
	if e.Proxy != "" {
		u, err := url.Parse(e.Proxy)
		if err != nil || u.Scheme != "http" || u.Host == "" || u.Port() == "" || u.Path != "" && u.Path != "/" || u.User != nil {
			bad("egress.proxy must be http://host:port")
		}
	}
	if e.CAFile != "" && !filepath.IsAbs(e.CAFile) {
		bad("egress.ca_file must be an absolute path")
	}
	if e.Timeout != 0 && (e.Timeout < time.Second || e.Timeout > time.Minute) {
		bad("egress.timeout must be within 1s..60s")
	}
}
