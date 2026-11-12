package config

import (
	"errors"
	"net/netip"
	"net/url"
	"path/filepath"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
)

// Egress configures the outbound client for requests made on a tenant's behalf (spec 0006). The
// whole block is file-only: it decides what kista may reach.
type Egress struct {
	Allow             []EgressAllow `yaml:"allow"`
	AllowLoopbackHTTP bool          `yaml:"allow_loopback_http"` // profile dev only
	Proxy             string        `yaml:"proxy"`               // an explicit proxy; never from the environment
	CAFile            string        `yaml:"ca_file"`             // trust only this CA bundle (a private IdP)
	Timeout           time.Duration `yaml:"timeout"`
	// HostsEnforce turns the host list on (spec 0044): the tenants' client then reaches only Hosts.
	HostsEnforce bool `yaml:"hosts_enforce"`
	// Hosts are the host names (exact, or *.<domain>) and IP addresses the tenants' client may
	// reach, with HostsEnforce; [] refuses every host. Ports stay with Allow.
	Hosts []string `yaml:"hosts"`
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

// EgressHosts is egress.hosts as a matcher (spec 0044): nil when hosts_enforce is off, never nil
// (an empty list included) when it is on. The configuration is validated.
func (c Config) EgressHosts() *egress.Hosts {
	if !c.Egress.HostsEnforce {
		return nil
	}
	h, err := egress.NewHosts(c.Egress.Hosts)
	if err != nil { // validated at load; a list that is no list refuses everything
		h, _ = egress.NewHosts(nil)
	}
	return h
}

// egressHostRefused is a URL's host the tenants' client would refuse by egress.hosts ("" when it
// is listed, or the list is off).
func (c Config) egressHostRefused(raw string) string {
	var he *egress.HostError
	if errors.As(c.EgressHosts().Check(raw), &he) {
		return he.Host
	}
	return ""
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
	// spec 0044: the switch is explicit, so the narrowing never fails open by accident
	switch {
	case e.HostsEnforce && e.Hosts == nil:
		bad("egress.hosts_enforce is true but egress.hosts is missing or null: list the hosts the tenants' egress may reach ([] for none)")
	case !e.HostsEnforce && e.Hosts != nil:
		bad("egress.hosts is set without egress.hosts_enforce: true: turn the host list on, or remove it")
	}
	for i, h := range e.Hosts {
		if err := egress.CheckHostEntry(h); err != nil {
			bad("egress.hosts[%d] %q: %v", i, h, err)
		}
	}
}
