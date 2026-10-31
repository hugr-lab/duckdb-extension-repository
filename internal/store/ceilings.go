package store

import (
	"fmt"
	"time"
)

// Ceilings are the most of each kind a tenant (or one of its objects) may hold, and the longest hold
// and publisher key lifetime: product limits an operator tunes in the configuration
// (limits.ceilings, spec 0038 as amended on 2026-10-31), never fixed in code. A tenant's own limits
// (spec 0038) are set at or below these. The bounds that keep one request or one remote answer small
// (page and body sizes, a statement's bytes) stay in code.
type Ceilings struct {
	Channels              int
	Upstreams             int
	Publishers            int // the tenant's own
	DistributorPublishers int // distributors' publishers, counted apart (spec 0037)
	PublisherKeys         int // per publisher, expired ones included
	SigningKeys           int // non-retired keys per channel, and attestation keys per tenant
	Issuers               int
	Grants                int
	PackageGroups         int
	GroupMembers          int // per group
	TenantGroupMembers    int // per tenant, every group
	Feeds                 int
	FeedKeys              int // per feed
	FeedKept              int // refusals and statements a tenant's export feeds keep
	Blocks                int
	Attachments           int
	AttachmentHeads       int // per (body, kind, origin)
	UpstreamEntries       int // per upstream
	EntryVersions         int // per entry
	UpstreamKeys          int // per upstream
	UpstreamCells         int // one upstream's matrix
	TenantCells           int // every upstream of a tenant
	DiscoveredNames       int // a mirror's "*" (spec 0039)
	MaxHold               time.Duration
	PublisherKeyLifetime  time.Duration
}

// DefaultCeilings are the ceilings a configuration that sets none has.
func DefaultCeilings() Ceilings {
	return Ceilings{
		Channels: 1000, Upstreams: 100, Publishers: 200, DistributorPublishers: 10, PublisherKeys: 10,
		SigningKeys: 16, Issuers: 16, Grants: 1000, PackageGroups: 100, GroupMembers: 1000,
		TenantGroupMembers: 10000, Feeds: 50, FeedKeys: 10, FeedKept: 100000, Blocks: 10000,
		Attachments: 100000, AttachmentHeads: 20, UpstreamEntries: 1000, EntryVersions: 100,
		UpstreamKeys: 10, UpstreamCells: 20000, TenantCells: 100000, DiscoveredNames: 1000,
		MaxHold: 90 * 24 * time.Hour, PublisherKeyLifetime: 366 * 24 * time.Hour,
	}
}

// MaxSigningKeys is the most non-retired keys a channel may hold, and attestation keys a tenant.
var MaxSigningKeys = 16

// MaxKeyLifetime is the furthest a publisher key's expiry may be.
var MaxKeyLifetime = 366 * 24 * time.Hour

// Check refuses a ceiling below 1 (a duration: below a second).
func (c Ceilings) Check() error {
	for _, f := range c.counts() {
		if *f.v < 1 {
			return fmt.Errorf("%w: limits.ceilings.%s must be at least 1", ErrInvalid, f.name)
		}
	}
	if c.MaxHold < time.Second || c.PublisherKeyLifetime < time.Second {
		return fmt.Errorf("%w: limits.ceilings.max_hold and publisher_key_lifetime must be at least 1s", ErrInvalid)
	}
	return nil
}

func (c *Ceilings) counts() []struct {
	name string
	v    *int
} {
	return []struct {
		name string
		v    *int
	}{
		{"channels", &c.Channels}, {"upstreams", &c.Upstreams}, {"publishers", &c.Publishers},
		{"distributor_publishers", &c.DistributorPublishers}, {"publisher_keys", &c.PublisherKeys},
		{"signing_keys", &c.SigningKeys}, {"issuers", &c.Issuers}, {"grants", &c.Grants},
		{"package_groups", &c.PackageGroups}, {"group_members", &c.GroupMembers},
		{"tenant_group_members", &c.TenantGroupMembers}, {"feeds", &c.Feeds}, {"feed_keys", &c.FeedKeys},
		{"feed_kept", &c.FeedKept}, {"blocks", &c.Blocks}, {"attachments", &c.Attachments},
		{"attachment_heads", &c.AttachmentHeads}, {"upstream_entries", &c.UpstreamEntries},
		{"entry_versions", &c.EntryVersions}, {"upstream_keys", &c.UpstreamKeys},
		{"upstream_cells", &c.UpstreamCells}, {"tenant_cells", &c.TenantCells},
		{"discovered_names", &c.DiscoveredNames},
	}
}

// SetCeilings puts c in force for this process: the server's configuration, set once as the store
// opens (tests set and restore it). It is not synchronised: never call it while requests run.
func SetCeilings(c Ceilings) {
	MaxChannels, MaxUpstreams, MaxPublishers, MaxDistributorPublishers = c.Channels, c.Upstreams, c.Publishers, c.DistributorPublishers
	MaxPublisherKeys, MaxSigningKeys, MaxIssuers, MaxGrants = c.PublisherKeys, c.SigningKeys, c.Issuers, c.Grants
	MaxPackageGroups, MaxGroupMembers, MaxTenantMembers = c.PackageGroups, c.GroupMembers, c.TenantGroupMembers
	MaxFeeds, MaxFeedKeys, MaxFeedKept, MaxBlocks = c.Feeds, c.FeedKeys, c.FeedKept, c.Blocks
	MaxTenantAttachments, MaxAttachmentHeads = c.Attachments, c.AttachmentHeads
	MaxUpstreamEntries, MaxEntryVersions, MaxUpstreamKeys = c.UpstreamEntries, c.EntryVersions, c.UpstreamKeys
	MaxUpstreamCells, MaxTenantCells, MaxDiscoveredNames = c.UpstreamCells, c.TenantCells, c.DiscoveredNames
	MaxHold, MaxKeyLifetime = c.MaxHold, c.PublisherKeyLifetime
}

// CurrentCeilings are the ceilings in force.
func CurrentCeilings() Ceilings {
	return Ceilings{
		Channels: MaxChannels, Upstreams: MaxUpstreams, Publishers: MaxPublishers,
		DistributorPublishers: MaxDistributorPublishers, PublisherKeys: MaxPublisherKeys,
		SigningKeys: MaxSigningKeys, Issuers: MaxIssuers, Grants: MaxGrants, PackageGroups: MaxPackageGroups,
		GroupMembers: MaxGroupMembers, TenantGroupMembers: MaxTenantMembers, Feeds: MaxFeeds,
		FeedKeys: MaxFeedKeys, FeedKept: MaxFeedKept, Blocks: MaxBlocks, Attachments: MaxTenantAttachments,
		AttachmentHeads: MaxAttachmentHeads, UpstreamEntries: MaxUpstreamEntries,
		EntryVersions: MaxEntryVersions, UpstreamKeys: MaxUpstreamKeys, UpstreamCells: MaxUpstreamCells,
		TenantCells: MaxTenantCells, DiscoveredNames: MaxDiscoveredNames, MaxHold: MaxHold,
		PublisherKeyLifetime: MaxKeyLifetime,
	}
}
