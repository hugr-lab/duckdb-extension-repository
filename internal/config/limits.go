package config

import (
	"fmt"

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
}

// Named returns the limits l names, as the store takes them (never store.LimitDefault: the
// configuration is the default).
func (l Limits) Named() (map[string]int64, error) {
	out := map[string]int64{}
	for _, f := range []struct{ name, value string }{{store.LimitBytes, l.Bytes}, {store.LimitChannels, l.Channels},
		{store.LimitUpstreams, l.Upstreams}, {store.LimitPublishers, l.Publishers}} {
		if f.value == "" {
			continue
		}
		v, err := store.ParseLimit(f.name, f.value)
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
	named, err := c.Limits.Named()
	for n, v := range named {
		out.Set(n, v)
	}
	return out, err
}

const limitsRule = "each is unlimited, 0 (none), a number up to its ceiling (channels 1000, upstreams 100, publishers 200) " +
	"or for bytes a size such as 50GiB"

func validateLimits(bad func(string, ...any), c Config) {
	if _, err := c.Limits.Named(); err != nil {
		bad("limits: " + limitsRule)
	}
	if l := c.Bootstrap.Tenant.Limits; l != nil {
		if _, err := l.Named(); err != nil {
			bad("bootstrap.tenant.limits: " + limitsRule)
		}
	}
}
