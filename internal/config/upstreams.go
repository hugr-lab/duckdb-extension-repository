package config

import "time"

// Upstreams configures upstream runs (spec 0009). The whole block is file-only.
type Upstreams struct {
	Concurrency  int           `yaml:"concurrency"`   // cells fetched at once per replica; default 4
	FetchTimeout time.Duration `yaml:"fetch_timeout"` // one file's download; default 10m
	MinRate      ByteSize      `yaml:"min_rate"`      // a download's least rate; default 64 KiB/s
	Interval     time.Duration `yaml:"interval"`      // between a mirror's scheduled runs; default 6h
}

// Defaults of upstreams:.
const (
	DefaultUpstreamConcurrency  = 4
	DefaultUpstreamFetchTimeout = 10 * time.Minute
	DefaultUpstreamMinRate      = ByteSize(64 << 10)
	DefaultUpstreamInterval     = 6 * time.Hour
)

// UpstreamLimits returns upstreams: with its defaults.
func (c Config) UpstreamLimits() Upstreams {
	u := c.Upstreams
	if u.Concurrency == 0 {
		u.Concurrency = DefaultUpstreamConcurrency
	}
	if u.FetchTimeout == 0 {
		u.FetchTimeout = DefaultUpstreamFetchTimeout
	}
	if u.MinRate == 0 {
		u.MinRate = DefaultUpstreamMinRate
	}
	if u.Interval == 0 {
		u.Interval = DefaultUpstreamInterval
	}
	return u
}

func validateUpstreams(bad func(string, ...any), c Config) {
	u := c.Upstreams
	if u.Concurrency < 0 || u.Concurrency > 32 {
		bad("upstreams.concurrency must be within 1..32")
	}
	if u.FetchTimeout != 0 && (u.FetchTimeout < time.Minute || u.FetchTimeout > 2*time.Hour) {
		bad("upstreams.fetch_timeout must be within 1m..2h")
	}
	if u.Interval != 0 && (u.Interval < 15*time.Minute || u.Interval > 7*24*time.Hour) {
		bad("upstreams.interval must be within 15m..168h")
	}
	if u.MinRate != 0 && u.MinRate < 1<<10 {
		bad("upstreams.min_rate must be at least 1KiB")
	}
}
