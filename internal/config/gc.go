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
