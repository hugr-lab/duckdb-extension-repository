package app

import (
	"net/netip"
	"net/url"
	"slices"

	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
)

// Egress builds the outbound client for requests made on a tenant's behalf (spec 0006).
func Egress(cfg config.Config) (*egress.Client, error) { return egressWith(cfg, nil) }

// ServerEgress builds the server issuers' outbound client: egress.allow plus
// auth.server_egress_allow, which tenants' issuer records never reach (spec 0007).
func ServerEgress(cfg config.Config) (*egress.Client, error) {
	return egressWith(cfg, cfg.Auth.ServerEgressAllow)
}

func egressWith(cfg config.Config, extra []config.EgressAllow) (*egress.Client, error) {
	var allow []egress.Allow
	for _, a := range append(slices.Clone(cfg.Egress.Allow), extra...) {
		p, err := netip.ParsePrefix(a.CIDR)
		if err != nil {
			continue // validated by config
		}
		allow = append(allow, egress.Allow{Prefix: p.Masked(), Ports: a.Ports})
	}
	var proxy *url.URL
	if cfg.Egress.Proxy != "" {
		proxy, _ = url.Parse(cfg.Egress.Proxy)
	}
	own := ""
	if u, err := url.Parse(cfg.Serve.PublicURL); err == nil {
		own = u.Host
	}
	return egress.New(egress.Config{Allow: allow, AllowLoopbackHTTP: cfg.Egress.AllowLoopbackHTTP, OwnHost: own,
		Proxy: proxy, CAFile: cfg.Egress.CAFile, Timeout: cfg.Egress.Timeout})
}
