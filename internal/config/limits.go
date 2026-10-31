package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Limits are tenant limits (spec 0038): the configuration's defaults for a tenant that has no values
// of its own (a backstop, not a plan), or the bootstrap's tenant's own values. Each is "unlimited",
// 0 (none, no growth of that kind at all, unlike kista's other settings, where 0 is the default), a
// number up to its ceiling, or for bytes a size (50GiB). A key left out is unlimited in the
// defaults, and left as it is by the bootstrap.
type Limits struct {
	Bytes      string `yaml:"bytes" json:"bytes,omitempty"`
	Channels   string `yaml:"channels" json:"channels,omitempty"`
	Upstreams  string `yaml:"upstreams" json:"upstreams,omitempty"`
	Publishers string `yaml:"publishers" json:"publishers,omitempty"`
	// Ceilings are the server's: the most of each kind any tenant may hold (never a tenant's own; a
	// bootstrap tenant's limits leave them out).
	Ceilings Ceilings `yaml:"ceilings" json:"-"`
}

// Ceilings are the server's ceilings (spec 0038, amended 2026-10-31: no tenant ceiling is fixed in
// code), each left out for its default (store.DefaultCeilings). A ceiling is never 0: a count is at
// least 1 and a duration at least a second (unlike a limit, where 0 means none).
type Ceilings struct {
	Channels              *int           `yaml:"channels"`
	Upstreams             *int           `yaml:"upstreams"`
	Publishers            *int           `yaml:"publishers"`
	DistributorPublishers *int           `yaml:"distributor_publishers"`
	PublisherKeys         *int           `yaml:"publisher_keys"`
	SigningKeys           *int           `yaml:"signing_keys"`
	Issuers               *int           `yaml:"issuers"`
	Grants                *int           `yaml:"grants"`
	PackageGroups         *int           `yaml:"package_groups"`
	GroupMembers          *int           `yaml:"group_members"`
	TenantGroupMembers    *int           `yaml:"tenant_group_members"`
	Feeds                 *int           `yaml:"feeds"`
	FeedKeys              *int           `yaml:"feed_keys"`
	FeedKept              *int           `yaml:"feed_kept"`
	Blocks                *int           `yaml:"blocks"`
	Attachments           *int           `yaml:"attachments"`
	AttachmentHeads       *int           `yaml:"attachment_heads"`
	UpstreamEntries       *int           `yaml:"upstream_entries"`
	EntryVersions         *int           `yaml:"entry_versions"`
	UpstreamKeys          *int           `yaml:"upstream_keys"`
	UpstreamCells         *int           `yaml:"upstream_cells"`
	TenantCells           *int           `yaml:"tenant_cells"`
	DiscoveredNames       *int           `yaml:"discovered_names"`
	MaxHold               *time.Duration `yaml:"max_hold"`
	PublisherKeyLifetime  *time.Duration `yaml:"publisher_key_lifetime"`
	IssuerTokenLifetime   *time.Duration `yaml:"issuer_token_lifetime"`
	EventsRetention       *time.Duration `yaml:"events_retention"`
}

type ceilingCount struct {
	name string
	dst  *int
	v    *int
}

type ceilingDuration struct {
	name string
	dst  *time.Duration
	v    *time.Duration
}

func (c Ceilings) fields(out *store.Ceilings) ([]ceilingCount, []ceilingDuration) {
	return []ceilingCount{{"channels", &out.Channels, c.Channels}, {"upstreams", &out.Upstreams, c.Upstreams},
			{"publishers", &out.Publishers, c.Publishers}, {"distributor_publishers", &out.DistributorPublishers, c.DistributorPublishers},
			{"publisher_keys", &out.PublisherKeys, c.PublisherKeys}, {"signing_keys", &out.SigningKeys, c.SigningKeys},
			{"issuers", &out.Issuers, c.Issuers}, {"grants", &out.Grants, c.Grants},
			{"package_groups", &out.PackageGroups, c.PackageGroups}, {"group_members", &out.GroupMembers, c.GroupMembers},
			{"tenant_group_members", &out.TenantGroupMembers, c.TenantGroupMembers}, {"feeds", &out.Feeds, c.Feeds},
			{"feed_keys", &out.FeedKeys, c.FeedKeys}, {"feed_kept", &out.FeedKept, c.FeedKept}, {"blocks", &out.Blocks, c.Blocks},
			{"attachments", &out.Attachments, c.Attachments}, {"attachment_heads", &out.AttachmentHeads, c.AttachmentHeads},
			{"upstream_entries", &out.UpstreamEntries, c.UpstreamEntries}, {"entry_versions", &out.EntryVersions, c.EntryVersions},
			{"upstream_keys", &out.UpstreamKeys, c.UpstreamKeys}, {"upstream_cells", &out.UpstreamCells, c.UpstreamCells},
			{"tenant_cells", &out.TenantCells, c.TenantCells}, {"discovered_names", &out.DiscoveredNames, c.DiscoveredNames}},
		[]ceilingDuration{{"max_hold", &out.MaxHold, c.MaxHold},
			{"publisher_key_lifetime", &out.PublisherKeyLifetime, c.PublisherKeyLifetime},
			{"issuer_token_lifetime", &out.IssuerTokenLifetime, c.IssuerTokenLifetime},
			{"events_retention", &out.EventsRetention, c.EventsRetention}}
}

// Settings are the ceilings in force: each one set, else its default.
func (c Ceilings) Settings() store.Ceilings {
	out := store.DefaultCeilings()
	counts, durations := c.fields(&out)
	for _, f := range counts {
		if f.v != nil {
			*f.dst = *f.v
		}
	}
	for _, f := range durations {
		if f.v != nil {
			*f.dst = *f.v
		}
	}
	return out
}

// check refuses a ceiling of 0 or below (left out is its default): a count at least 1, a duration at
// least a second.
func (c Ceilings) check() error {
	var out store.Ceilings
	counts, durations := c.fields(&out)
	for _, f := range counts {
		if f.v != nil && *f.v < 1 {
			return fmt.Errorf("%w: limits.ceilings.%s is at least 1 (leave it out for its default)", store.ErrInvalid, f.name)
		}
	}
	for _, f := range durations {
		if f.v != nil && *f.v < time.Second {
			return fmt.Errorf("%w: limits.ceilings.%s is at least 1s (leave it out for its default)", store.ErrInvalid, f.name)
		}
	}
	return c.Settings().Check()
}

// Named returns the limits l names, as the store takes them (never store.LimitDefault: the
// configuration is the default), each at most its ceiling in ceil.
func (l Limits) Named(ceil store.Ceilings) (map[string]int64, error) {
	out := map[string]int64{}
	for _, f := range []struct{ name, value string }{{store.LimitBytes, l.Bytes}, {store.LimitChannels, l.Channels},
		{store.LimitUpstreams, l.Upstreams}, {store.LimitPublishers, l.Publishers}} {
		if f.value == "" {
			continue
		}
		v, err := store.ParseLimitUnder(f.name, f.value, ceil)
		if err == nil && v == store.LimitDefault {
			err = fmt.Errorf("%w: %s: default is not a value here", store.ErrInvalid, f.name)
		}
		if err != nil {
			return nil, err
		}
		out[f.name] = v
	}
	return out, nil
}

// LimitSettings returns limits: as the store takes them, every value unlimited or a limit.
func (c Config) LimitSettings() (store.Limits, error) {
	out := store.UnlimitedLimits()
	named, err := c.Limits.Named(c.Limits.Ceilings.Settings())
	for n, v := range named {
		out.Set(n, v)
	}
	return out, err
}

const limitsRule = "each is unlimited, 0 (none), a number up to its ceiling (limits.ceilings) " +
	"or for bytes a size such as 50GiB"

func validateLimits(bad func(string, ...any), c Config) {
	ceil := c.Limits.Ceilings.Settings()
	if err := c.Limits.Ceilings.check(); err != nil {
		bad("%s", limitError(err))
	}
	if _, err := c.Limits.Named(ceil); err != nil {
		bad("limits: %s (%s)", limitError(err), limitsRule)
	}
	if l := c.Bootstrap.Tenant.Limits; l != nil {
		if l.Ceilings != (Ceilings{}) {
			bad("bootstrap.tenant.limits.ceilings: the ceilings are the server's (limits.ceilings)")
		}
		if _, err := l.Named(ceil); err != nil {
			bad("bootstrap.tenant.limits: %s (%s)", limitError(err), limitsRule)
		}
	}
}

// limitError is a limit's refusal without the store's prefix: it names the limit, never the value.
func limitError(err error) string {
	return strings.TrimPrefix(err.Error(), store.ErrInvalid.Error()+": ")
}
