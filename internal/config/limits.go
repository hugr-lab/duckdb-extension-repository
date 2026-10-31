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

// Ceilings are the server's ceilings (spec 0038, amended 2026-10-31: no limit is fixed in code),
// each 0 or left out for its default (store.DefaultCeilings).
type Ceilings struct {
	Channels              int           `yaml:"channels"`
	Upstreams             int           `yaml:"upstreams"`
	Publishers            int           `yaml:"publishers"`
	DistributorPublishers int           `yaml:"distributor_publishers"`
	PublisherKeys         int           `yaml:"publisher_keys"`
	SigningKeys           int           `yaml:"signing_keys"`
	Issuers               int           `yaml:"issuers"`
	Grants                int           `yaml:"grants"`
	PackageGroups         int           `yaml:"package_groups"`
	GroupMembers          int           `yaml:"group_members"`
	TenantGroupMembers    int           `yaml:"tenant_group_members"`
	Feeds                 int           `yaml:"feeds"`
	FeedKeys              int           `yaml:"feed_keys"`
	FeedKept              int           `yaml:"feed_kept"`
	Blocks                int           `yaml:"blocks"`
	Attachments           int           `yaml:"attachments"`
	AttachmentHeads       int           `yaml:"attachment_heads"`
	UpstreamEntries       int           `yaml:"upstream_entries"`
	EntryVersions         int           `yaml:"entry_versions"`
	UpstreamKeys          int           `yaml:"upstream_keys"`
	UpstreamCells         int           `yaml:"upstream_cells"`
	TenantCells           int           `yaml:"tenant_cells"`
	DiscoveredNames       int           `yaml:"discovered_names"`
	MaxHold               time.Duration `yaml:"max_hold"`
	PublisherKeyLifetime  time.Duration `yaml:"publisher_key_lifetime"`
}

func (c *Ceilings) counts(out *store.Ceilings) []struct{ dst, v *int } {
	return []struct{ dst, v *int }{{&out.Channels, &c.Channels}, {&out.Upstreams, &c.Upstreams},
		{&out.Publishers, &c.Publishers}, {&out.DistributorPublishers, &c.DistributorPublishers},
		{&out.PublisherKeys, &c.PublisherKeys}, {&out.SigningKeys, &c.SigningKeys}, {&out.Issuers, &c.Issuers},
		{&out.Grants, &c.Grants}, {&out.PackageGroups, &c.PackageGroups}, {&out.GroupMembers, &c.GroupMembers},
		{&out.TenantGroupMembers, &c.TenantGroupMembers}, {&out.Feeds, &c.Feeds}, {&out.FeedKeys, &c.FeedKeys},
		{&out.FeedKept, &c.FeedKept}, {&out.Blocks, &c.Blocks}, {&out.Attachments, &c.Attachments},
		{&out.AttachmentHeads, &c.AttachmentHeads}, {&out.UpstreamEntries, &c.UpstreamEntries},
		{&out.EntryVersions, &c.EntryVersions}, {&out.UpstreamKeys, &c.UpstreamKeys},
		{&out.UpstreamCells, &c.UpstreamCells}, {&out.TenantCells, &c.TenantCells},
		{&out.DiscoveredNames, &c.DiscoveredNames}}
}

// Settings are the ceilings in force: each one set, else its default.
func (c Ceilings) Settings() store.Ceilings {
	out := store.DefaultCeilings()
	for _, f := range c.counts(&out) {
		if *f.v != 0 {
			*f.dst = *f.v
		}
	}
	if c.MaxHold != 0 {
		out.MaxHold = c.MaxHold
	}
	if c.PublisherKeyLifetime != 0 {
		out.PublisherKeyLifetime = c.PublisherKeyLifetime
	}
	return out
}

// check refuses a negative ceiling (0 is its default).
func (c Ceilings) check() error {
	var out store.Ceilings
	for _, f := range c.counts(&out) {
		if *f.v < 0 {
			return fmt.Errorf("%w: limits.ceilings: a count is positive, or 0 for its default", store.ErrInvalid)
		}
	}
	if c.MaxHold < 0 || c.PublisherKeyLifetime < 0 {
		return fmt.Errorf("%w: limits.ceilings: a duration is positive, or 0 for its default", store.ErrInvalid)
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
		if (l.Ceilings != Ceilings{}) {
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
