package config

import (
	"net/url"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// UI configures the administration console (spec 0015).
type UI struct {
	Enabled        *bool            `yaml:"enabled"`         // default true
	Environment    string           `yaml:"environment"`     // a badge in the console's header
	AllowedOrigins []string         `yaml:"allowed_origins"` // shells on other origins (phase 1b)
	FrameAncestors []string         `yaml:"frame_ancestors"` // who may frame the standalone console; default none
	ConnectSrc     []string         `yaml:"connect_src"`     // extra connect-src origins
	ServerClients  []UIServerClient `yaml:"server_clients"`  // the console's clients at server issuers
}

// UIServerClient is the console's public client (PKCE) at a server issuer.
type UIServerClient struct {
	Issuer            string   `yaml:"issuer"` // a server issuer's name
	ClientID          string   `yaml:"client_id"`
	Scopes            []string `yaml:"scopes"`
	AudienceParameter string   `yaml:"audience_parameter"`
}

// UIEnabled reports whether the console and its routes are served.
func (c Config) UIEnabled() bool { return c.UI.Enabled == nil || *c.UI.Enabled }

// DefaultConsoleScopes are a console client's scopes when none are configured.
var DefaultConsoleScopes = []string{"openid", "profile", "offline_access"}

// UIServerClients returns the server clients with their default scopes.
func (c Config) UIServerClients() []UIServerClient {
	out := make([]UIServerClient, 0, len(c.UI.ServerClients))
	for _, s := range c.UI.ServerClients {
		if len(s.Scopes) == 0 {
			s.Scopes = DefaultConsoleScopes
		}
		out = append(out, s)
	}
	return out
}

// ValidOrigin reports whether o is https://host[:port] (http only on loopback), with no path.
func ValidOrigin(o string) bool {
	u, err := url.Parse(o)
	if got, ok := auth.OriginOf(o); !ok || got != o || err != nil {
		return false
	}
	h := u.Hostname()
	loopback := h == "127.0.0.1" || h == "::1" || h == "localhost"
	return u.Scheme == "https" || u.Scheme == "http" && loopback
}

func validateUI(bad func(string, ...any), c Config) {
	for name, list := range map[string][]string{"allowed_origins": c.UI.AllowedOrigins, "frame_ancestors": c.UI.FrameAncestors,
		"connect_src": c.UI.ConnectSrc} {
		for _, o := range list {
			if !ValidOrigin(o) {
				bad("ui.%s: %q is not an origin (https://host[:port], http only on loopback, no path, no *)", name, o)
			}
		}
	}
	if len(c.UI.Environment) > 32 {
		bad("ui.environment is at most 32 characters")
	}
	issuers := map[string]bool{}
	for _, s := range c.Auth.ServerIssuers {
		issuers[s.Name] = true
	}
	seen := map[string]bool{}
	for i, s := range c.UI.ServerClients {
		if !issuers[s.Issuer] {
			bad("ui.server_clients[%d]: %q is not a server issuer", i, s.Issuer)
		}
		if seen[s.Issuer] {
			bad("ui.server_clients[%d]: issuer %s has two clients", i, s.Issuer)
		}
		seen[s.Issuer] = true
		cc := store.ConsoleClient{ClientID: s.ClientID, Scopes: s.Scopes, AudienceParameter: s.AudienceParameter}
		if len(cc.Scopes) == 0 {
			cc.Scopes = DefaultConsoleScopes
		}
		if err := cc.Check(); err != nil {
			bad("ui.server_clients[%d]: %v", i, err)
		}
	}
}
