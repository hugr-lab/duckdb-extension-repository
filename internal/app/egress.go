package app

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/netip"
	"net/url"
	"slices"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream/upspec"
)

// Egress builds the outbound client for requests made on a tenant's behalf (spec 0006), narrowed by
// egress.hosts when hosts_enforce is on (spec 0044).
func Egress(cfg config.Config) (*egress.Client, error) {
	return egressWith(cfg, nil, 0, cfg.EgressHosts())
}

// ServerEgress builds the server issuers' outbound client: egress.allow plus
// auth.server_egress_allow, which tenants' issuer records never reach (spec 0007). egress.hosts does
// not narrow it: its destinations are the configuration's own (spec 0044).
func ServerEgress(cfg config.Config) (*egress.Client, error) {
	return egressWith(cfg, cfg.Auth.ServerEgressAllow, 0, nil)
}

// CommunityEgress builds the community sync's client (spec 0030 part 3): egress.allow, no host list,
// so its host need not be on a list every tenant reaches (spec 0044).
func CommunityEgress(cfg config.Config) (*egress.Client, error) { return egressWith(cfg, nil, 0, nil) }

// warnEgressHosts says at start, once per host (its normalised form), which stored records name a
// host off egress.hosts (spec 0044), with how many records and tenants: their fetches are refused
// until the operator lists it. issuers is serve's one read of Store.IssuerURLs (nil when it failed).
// A discovered jwks_uri is not stored (it shows when it is fetched); bundle-mode upstreams and feeds
// contact nothing.
func warnEgressHosts(ctx context.Context, st *store.Store, hosts *egress.Hosts, issuers [][4]string, log *slog.Logger) {
	type use struct{ records, tenants map[string]bool }
	uses := map[string]*use{}
	note := func(tenant, record, raw string) {
		var he *egress.HostError
		if raw == "" || !errors.As(hosts.Check(raw), &he) {
			return
		}
		key := egress.HostKey(he.Host)
		u := uses[key]
		if u == nil {
			u = &use{records: map[string]bool{}, tenants: map[string]bool{}}
			uses[key] = u
		}
		u.records[tenant+"/"+record], u.tenants[tenant] = true, true
	}
	for _, r := range issuers {
		note(r[0], "issuer:"+r[1], r[2])
		note(r[0], "issuer:"+r[1], r[3])
	}
	if rs, err := st.UpstreamPrefixes(ctx); err != nil {
		log.Error("serve: reading upstreams", "error", err)
	} else {
		for _, r := range rs {
			if r[3] != store.ModeBundle {
				note(r[0], "upstream:"+r[1], cmp.Or(upspec.BuiltIn(r[2]), r[4]))
			}
		}
	}
	if rs, err := st.FeedURLs(ctx); err != nil {
		log.Error("serve: reading feeds", "error", err)
	} else {
		for _, r := range rs {
			if r[2] != store.FeedBundle {
				note(r[0], "feed:"+r[1], r[3])
			}
		}
	}
	for _, h := range slices.Sorted(maps.Keys(uses)) {
		log.Warn("serve: stored records name a host not on egress.hosts; their fetches are refused until the server's operator lists it",
			"host", h, "records", len(uses[h].records), "tenants", len(uses[h].tenants))
	}
}

// egressWith builds a client over egress.allow plus extra; timeout 0 is egress.timeout's; hosts nil
// narrows nothing.
func egressWith(cfg config.Config, extra []config.EgressAllow, timeout time.Duration, hosts *egress.Hosts) (*egress.Client, error) {
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
		Proxy: proxy, CAFile: cfg.Egress.CAFile, Timeout: cmp.Or(timeout, cfg.Egress.Timeout), Hosts: hosts})
}
