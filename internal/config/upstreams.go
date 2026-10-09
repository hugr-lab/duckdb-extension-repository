package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Upstreams configures upstream runs (spec 0009). The whole block is file-only.
type Upstreams struct {
	Concurrency  int           `yaml:"concurrency"`   // cells fetched at once per replica; default 4
	FetchTimeout time.Duration `yaml:"fetch_timeout"` // one file's download; default 10m
	MinRate      ByteSize      `yaml:"min_rate"`      // a download's least rate; default 64 KiB/s
	Interval     time.Duration `yaml:"interval"`      // between a mirror's scheduled runs; default 6h
	NegativeTTL  time.Duration `yaml:"negative_ttl"`  // a pull-through miss is not retried within it; default 1h
	// Credentials are the named credentials private upstreams use (phase 3).
	Credentials []Credential `yaml:"credentials"`
}

// Credential is how kista obtains a token for a private upstream (spec 0009 phase 3): no secret
// in the database, none in the API.
type Credential struct {
	Name        string   `yaml:"name"`
	Tenants     []string `yaml:"tenants"`      // tenant names, or "*"; none by default
	Prefixes    []string `yaml:"prefixes"`     // https upstream prefixes it is sent to
	AllowPublic bool     `yaml:"allow_public"` // releases it brings may be made public
	Kind        string   `yaml:"kind"`         // client_credentials | token_file
	// token_file
	TokenFile string `yaml:"token_file"`
	// client_credentials
	TokenURL          string        `yaml:"token_url"`
	ClientID          string        `yaml:"client_id"`
	Scope             string        `yaml:"scope"`
	Audience          string        `yaml:"audience"`
	ClientAuth        string        `yaml:"client_auth"`        // azure | file | key_file | secret
	AssertionFile     string        `yaml:"assertion_file"`     // file
	AssertionAudience string        `yaml:"assertion_audience"` // key_file: the JWT's aud (default token_url)
	KeyFile           string        `yaml:"key_file"`           // key_file: ZITADEL's JSON or a PEM key
	KeyID             string        `yaml:"key_id"`             // key_file with a PEM key
	ClientSecretFile  string        `yaml:"client_secret_file"` // secret
	ClientSecretEnv   string        `yaml:"client_secret_env"`  // secret
	Allow             []EgressAllow `yaml:"allow"`              // the token endpoint's addresses, beside egress.allow
}

// Defaults of upstreams:.
const (
	DefaultUpstreamConcurrency  = 4
	DefaultUpstreamFetchTimeout = 10 * time.Minute
	DefaultUpstreamMinRate      = ByteSize(64 << 10)
	DefaultUpstreamInterval     = 6 * time.Hour
	DefaultUpstreamNegativeTTL  = time.Hour
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
	if u.NegativeTTL == 0 {
		u.NegativeTTL = DefaultUpstreamNegativeTTL
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
	if u.NegativeTTL != 0 && (u.NegativeTTL < time.Minute || u.NegativeTTL > 24*time.Hour) {
		bad("upstreams.negative_ttl must be within 1m..24h")
	}
	if u.MinRate != 0 && u.MinRate < 1<<10 {
		bad("upstreams.min_rate must be at least 1KiB")
	}
	names := map[string]bool{}
	for i, cr := range u.Credentials {
		where := fmt.Sprintf("upstreams.credentials[%d]", i)
		if !sinkName.MatchString(cr.Name) || names[cr.Name] {
			bad("%s.name must match [a-z][a-z0-9-]{0,31} and be unique", where)
		}
		names[cr.Name] = true
		if len(cr.Tenants) == 0 {
			bad("%s.tenants names the tenants that may use it (or *)", where)
		}
		for _, t := range cr.Tenants {
			if t != "*" && !tenantName.MatchString(t) {
				bad("%s.tenants: %q is not a tenant name or *", where, t)
			}
		}
		if len(cr.Prefixes) == 0 {
			bad("%s.prefixes names the upstream prefixes it is sent to", where)
		}
		for j, p := range cr.Prefixes {
			pu, err := url.Parse(p)
			if err != nil || pu.Scheme != "https" || pu.Host == "" || pu.User != nil || pu.RawQuery != "" || pu.Fragment != "" ||
				strings.Contains(pu.Path, "..") {
				bad("%s.prefixes[%d] must be an https URL without user info, query or fragment", where, j)
			}
		}
		abs := func(field, v string) {
			if v == "" || !filepath.IsAbs(v) {
				bad("%s.%s must be an absolute path", where, field)
			}
		}
		switch cr.Kind {
		case "token_file":
			abs("token_file", cr.TokenFile)
			if len(cr.Allow) > 0 {
				bad("%s.allow is for a token endpoint (client_credentials)", where)
			}
		case "client_credentials":
			tu, err := url.Parse(cr.TokenURL)
			devHTTP := tu != nil && tu.Scheme == "http" && c.Profile == ProfileDev && c.Egress.AllowLoopbackHTTP // egress checks loopback
			if err != nil || tu.Host == "" || tu.User != nil || tu.Scheme != "https" && !devHTTP {
				bad("%s.token_url must be an https URL", where)
			}
			if cr.ClientID == "" {
				bad("%s.client_id is required", where)
			}
			switch cr.ClientAuth {
			case "azure":
				if c.Azure.Identity.Kind == "" {
					bad("%s.client_auth azure needs azure.identity", where)
				}
			case "file":
				abs("assertion_file", cr.AssertionFile)
			case "key_file":
				abs("key_file", cr.KeyFile)
			case "secret":
				if (cr.ClientSecretFile == "") == (cr.ClientSecretEnv == "") {
					bad("%s: client_auth secret takes one of client_secret_file and client_secret_env", where)
				}
				if cr.ClientSecretFile != "" {
					abs("client_secret_file", cr.ClientSecretFile)
				}
			default:
				bad("%s.client_auth must be azure, file, key_file or secret", where)
			}
			for j, a := range cr.Allow {
				if _, err := netip.ParsePrefix(a.CIDR); err != nil || slices.Contains(a.Ports, 0) {
					bad("%s.allow[%d] is not a CIDR with ports", where, j)
				}
			}
		default:
			bad("%s.kind must be client_credentials or token_file", where)
		}
	}
}
