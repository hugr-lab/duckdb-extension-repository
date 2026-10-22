package config

import (
	"net/url"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/duckdbsrc"
)

// Reserved configures the server's reserved extension names (spec 0030).
type Reserved struct {
	CommunitySync CommunitySync `yaml:"community_sync"`
}

// CommunitySync follows duckdb/community-extensions: every interval the server reads the names of
// its extensions/*/description.yml and reserves the new ones as community. Off by default.
type CommunitySync struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"` // default 24h; 1h..168h
	// URL is a gzip tarball of the repository's default branch, as git archive writes it (the
	// commit in its pax header): GitHub's by default, a mirror's otherwise.
	URL string `yaml:"url"`
}

// DefaultCommunitySyncURL is duckdb/community-extensions' default branch, from GitHub.
const DefaultCommunitySyncURL = duckdbsrc.CommunityTarball + "HEAD"

// CommunitySyncSettings returns reserved.community_sync with its defaults.
func (c Config) CommunitySyncSettings() CommunitySync {
	s := c.Reserved.CommunitySync
	if s.Interval == 0 {
		s.Interval = 24 * time.Hour
	}
	if s.URL == "" {
		s.URL = DefaultCommunitySyncURL
	}
	return s
}

func validateReserved(bad func(string, ...any), c Config) {
	s := c.Reserved.CommunitySync
	if i := s.Interval; i != 0 && (i < time.Hour || i > 7*24*time.Hour) {
		bad("reserved.community_sync.interval must be within 1h..168h")
	}
	if s.URL != "" {
		u, err := url.Parse(s.URL)
		plainOK := u != nil && u.Scheme == "http" && c.loopbackHTTP() // loopback in dev only (egress checks the host)
		if err != nil || u.Host == "" || u.User != nil || u.Scheme != "https" && !plainOK {
			bad("reserved.community_sync.url must be an https URL")
		}
	}
}
