package config

import (
	"net/url"
	"regexp"
	"strings"
)

var providerName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Publish configures publication (spec 0008). The whole block is file-only.
type Publish struct {
	MaxPerPrincipal int        `yaml:"max_per_principal"` // uploads at once per principal; default 2
	MaxPerTenant    int        `yaml:"max_per_tenant"`    // uploads at once per tenant; default 8
	Providers       []Provider `yaml:"providers"`         // trusted-publishing issuers; default: GitHub Actions
}

// Provider is a trusted-publishing issuer: GitHub Actions, or a GitHub Enterprise Server's.
type Provider struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

// GitHubActions is the default provider.
var GitHubActions = Provider{Name: "github", URL: "https://token.actions.githubusercontent.com"}

// PublishProviders returns the providers: GitHub Actions alone when the key is absent, none for
// an empty list (trusted publishing off).
func (c Config) PublishProviders() []Provider {
	if c.Publish.Providers == nil {
		return []Provider{GitHubActions}
	}
	return c.Publish.Providers
}

// PublishLimits returns the upload limits, or their defaults.
func (c Config) PublishLimits() (perPrincipal, perTenant int) {
	perPrincipal, perTenant = c.Publish.MaxPerPrincipal, c.Publish.MaxPerTenant
	if perPrincipal == 0 {
		perPrincipal = 2
	}
	if perTenant == 0 {
		perTenant = 8
	}
	return perPrincipal, perTenant
}

func validatePublish(bad func(string, ...any), c Config) {
	p := c.Publish
	if p.MaxPerPrincipal < 0 || p.MaxPerPrincipal > 64 {
		bad("publish.max_per_principal must be within 1..64")
	}
	if p.MaxPerTenant < 0 || p.MaxPerTenant > 256 {
		bad("publish.max_per_tenant must be within 1..256")
	}
	if p.MaxPerPrincipal > 0 && p.MaxPerTenant > 0 && p.MaxPerPrincipal > p.MaxPerTenant {
		bad("publish.max_per_principal cannot exceed publish.max_per_tenant")
	}
	names, urls := map[string]bool{}, map[string]bool{}
	for i, pr := range p.Providers {
		u, err := url.Parse(pr.URL)
		switch {
		case !providerName.MatchString(pr.Name):
			bad("publish.providers[%d].name must match [a-z][a-z0-9-]{0,31}", i)
		case err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
			strings.HasSuffix(pr.URL, "/"):
			bad("publish.providers[%d].url must be an https issuer URL without a trailing slash", i)
		case names[pr.Name] || urls[pr.URL]:
			bad("publish.providers[%d]: a name or URL is used twice", i)
		}
		names[pr.Name], urls[pr.URL] = true, true
	}
}
