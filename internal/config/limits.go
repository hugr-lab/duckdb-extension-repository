package config

import (
	"fmt"
	"strconv"
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
	// The ceilings on the server's own settings (SettingCeilings).
	EventsRetention          *time.Duration `yaml:"events_retention"`
	EventsRetentionWithSinks *time.Duration `yaml:"events_retention_with_sinks"`
	EventsRowsPerTenant      *int           `yaml:"events_rows_per_tenant"`
	StatisticsRetention      *time.Duration `yaml:"statistics_retention"`
	AdminTokenMaxAge         *time.Duration `yaml:"admin_token_max_age"`
}

// The least a setting may be: what it needs to work, so in code (spec 0038, amended 2026-10-31), and
// the least its ceiling may be.
const (
	// MinEventRetention: a day's installers are counted from its events after it ends (spec 0010).
	MinEventRetention = 48 * time.Hour
	// MinStatisticsRetention: the API answers each name's downloads in the last 30 days.
	MinStatisticsRetention = 30 * 24 * time.Hour
	// MinAdminTokenMaxAge: the console renews a token before iat plus the age, and clocks drift.
	MinAdminTokenMaxAge = time.Minute
)

// SettingCeilings are the ceilings on the server's own settings (spec 0038, amended 2026-10-31): not
// a tenant's room, so never in force in the store; the configuration checks its settings against
// them as it loads. Each is at least its setting's least (MinEventRetention, …).
type SettingCeilings struct {
	EventsRetention          time.Duration // the longest events.retention
	EventsRetentionWithSinks time.Duration // the longest events.retention once a sink is configured
	EventsRowsPerTenant      int           // the most events.max_rows_per_tenant
	StatisticsRetention      time.Duration // the longest statistics.retention
	AdminTokenMaxAge         time.Duration // the longest auth.admin_token_max_age
}

// DefaultSettingCeilings are the setting ceilings a configuration that sets none has.
func DefaultSettingCeilings() SettingCeilings {
	return SettingCeilings{EventsRetention: 365 * 24 * time.Hour, EventsRetentionWithSinks: 90 * 24 * time.Hour,
		EventsRowsPerTenant: 100000000, StatisticsRetention: 10 * 365 * 24 * time.Hour, AdminTokenMaxAge: 24 * time.Hour}
}

// settingDuration is a setting ceiling with the least it may be and the setting it bounds.
type settingDuration struct {
	ceilingDuration
	least   time.Duration
	setting string
}

func (c Ceilings) settingFields(out *SettingCeilings) ([]ceilingCount, []settingDuration) {
	return []ceilingCount{{"events_rows_per_tenant", &out.EventsRowsPerTenant, c.EventsRowsPerTenant}},
		[]settingDuration{
			{ceilingDuration{"events_retention", &out.EventsRetention, c.EventsRetention}, MinEventRetention, "events.retention"},
			{ceilingDuration{"events_retention_with_sinks", &out.EventsRetentionWithSinks, c.EventsRetentionWithSinks}, MinEventRetention, "events.retention"},
			{ceilingDuration{"statistics_retention", &out.StatisticsRetention, c.StatisticsRetention}, MinStatisticsRetention, "statistics.retention"},
			{ceilingDuration{"admin_token_max_age", &out.AdminTokenMaxAge, c.AdminTokenMaxAge}, MinAdminTokenMaxAge, "auth.admin_token_max_age"}}
}

// Setting are the setting ceilings in force: each one set, else its default.
func (c Ceilings) Setting() SettingCeilings {
	out := DefaultSettingCeilings()
	counts, durations := c.settingFields(&out)
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
			{"issuer_token_lifetime", &out.IssuerTokenLifetime, c.IssuerTokenLifetime}}
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

// Set are the ceilings this configuration sets, by key, as the docs write them: what server.start
// digests (a ceiling left out is not named, so a new one does not show as a change).
func (c Ceilings) Set() map[string]string {
	out := map[string]string{}
	counts, durations := c.fields(&store.Ceilings{})
	settingCounts, settingDurations := c.settingFields(&SettingCeilings{})
	for _, f := range append(counts, settingCounts...) {
		if f.v != nil {
			out[f.name] = strconv.Itoa(*f.v)
		}
	}
	for _, f := range settingDurations {
		durations = append(durations, f.ceilingDuration)
	}
	for _, f := range durations {
		if f.v != nil {
			out[f.name] = durationText(*f.v)
		}
	}
	return out
}

// Warnings are the settings that loosen a guard below its default: each is allowed, and each start
// says so (spec 0038 amended, security).
func (c Config) Warnings() []string {
	var out []string
	if f := c.Rotation.Floor(); f < DefaultRotationFloor {
		out = append(out, fmt.Sprintf("rotation.min_floor is %s, below its default %s", durationText(f), durationText(DefaultRotationFloor)))
	}
	if f := c.Tenants.Floor(); f < DefaultMinSuspendedFloor {
		out = append(out, fmt.Sprintf("tenants.min_suspended_floor is %s, below its default %s", durationText(f), durationText(DefaultMinSuspendedFloor)))
	}
	if m, def := c.AdminTokenMaxAge(), DefaultSettingCeilings().AdminTokenMaxAge; m > def {
		out = append(out, fmt.Sprintf("auth.admin_token_max_age is %s, above %s", durationText(m), durationText(def)))
	}
	return out
}

// check refuses a ceiling of 0 or below (left out is its default): a count at least 1, a duration at
// least a second, a setting's ceiling at least what the setting needs.
func (c Ceilings) check() error {
	var out store.Ceilings
	counts, durations := c.fields(&out)
	settingCounts, settingDurations := c.settingFields(&SettingCeilings{})
	counts = append(counts, settingCounts...)
	for _, f := range settingDurations {
		if f.v != nil && *f.v < f.least {
			return fmt.Errorf("%w: limits.ceilings.%s is at least %s, the least %s may be (leave it out for its default)",
				store.ErrInvalid, f.name, durationText(f.least), f.setting)
		}
	}
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

// durationText is d as the docs write it: 8760h, 30m, 1m30s (never 8760h0m0s).
func durationText(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// limitError is a limit's refusal without the store's prefix: it names the limit, never the value.
func limitError(err error) string {
	return strings.TrimPrefix(err.Error(), store.ErrInvalid.Error()+": ")
}
