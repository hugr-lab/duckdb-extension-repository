package config

import "time"

// GC configures storage garbage collection (spec 0016).
type GC struct {
	Interval time.Duration `yaml:"interval"` // between scheduled passes; 0 (default): none, kista admin blob gc only
	Grace    time.Duration `yaml:"grace"`    // default 24h, at least 4h (four intake deadlines)
}

// DefaultGCGrace is how long what became unreferenced is kept.
const DefaultGCGrace = 24 * time.Hour

// GCSettings returns gc: with its defaults.
func (c Config) GCSettings() GC {
	g := c.GC
	if g.Grace == 0 {
		g.Grace = DefaultGCGrace
	}
	return g
}

func validateGC(bad func(string, ...any), c Config) {
	if g := c.GC.Grace; g != 0 && (g < 4*time.Hour || g > 30*24*time.Hour) {
		bad("gc.grace must be within 4h..720h (four intake deadlines at least)")
	}
	if i := c.GC.Interval; i != 0 && (i < time.Hour || i > 7*24*time.Hour) {
		bad("gc.interval must be 0 or within 1h..168h")
	}
}

// Attachments configures documents about bodies (spec 0011).
type Attachments struct {
	MaxSize int64 `yaml:"max_size"` // bytes; default 4 MiB, at most 16 MiB
	// MaxReads is how many documents are held in memory at once, read and sent (default 8).
	MaxReads int `yaml:"max_reads"`
	// FeedInterval is the time between a feed's scheduled runs (phase 1b; default 6h, at least 5m).
	FeedInterval time.Duration `yaml:"feed_interval"`
}

// FeedIntervalOrDefault is attachments.feed_interval or its default.
func (a Attachments) FeedIntervalOrDefault() time.Duration {
	if a.FeedInterval == 0 {
		return 6 * time.Hour
	}
	return a.FeedInterval
}

func validateAttachments(bad func(string, ...any), c Config) {
	if m := c.Attachments.MaxSize; m != 0 && (m < 64<<10 || m > 16<<20) {
		bad("attachments.max_size must be 0 or within 64 KiB..16 MiB")
	}
	if m := c.Attachments.MaxReads; m < 0 || m > 256 {
		bad("attachments.max_reads must be 0 (8) to 256")
	}
	if i := c.Attachments.FeedInterval; i != 0 && i < 5*time.Minute {
		bad("attachments.feed_interval must be 0 (6h) or at least 5m")
	}
}
