package config

import (
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Auth configures server identity (spec 0007): the server issuers, their audiences, the server
// administrators. The whole block is file-only; changes take effect at restart.
type Auth struct {
	ServerIssuers     []ServerIssuer `yaml:"server_issuers"`
	ServerAudiences   []string       `yaml:"server_audiences"`    // default: [ serve.public_url ]
	ServerEgressAllow []EgressAllow  `yaml:"server_egress_allow"` // reached only by the server issuers' fetcher
	ServerAdmins      []string       `yaml:"server_admins"`       // principals of the server issuers; issuer: is refused
	AdminTokenMaxAge  time.Duration  `yaml:"admin_token_max_age"` // default 1h
}

// ServerIssuer is an issuer record's fields; required_claims is mandatory.
type ServerIssuer struct {
	Name             string            `yaml:"name"`
	URL              string            `yaml:"url"`
	JWKSURI          string            `yaml:"jwks_uri"`
	Algorithms       []string          `yaml:"algorithms"`
	RequiredClaims   map[string]string `yaml:"required_claims"`
	RolesClaim       string            `yaml:"roles_claim"` // a claim path: keys joined by dots, or a JSON array
	GroupsClaim      string            `yaml:"groups_claim"`
	ClientClaim      string            `yaml:"client_claim"`
	MaxTokenLifetime time.Duration     `yaml:"max_token_lifetime"`
}

// DefaultAdminTokenMaxAge is how recently a management token must have been issued.
const DefaultAdminTokenMaxAge = time.Hour

func (s ServerIssuer) issuer(allowHTTP bool) (store.Issuer, error) {
	is := store.Issuer{ID: auth.ServerIssuerID(s.Name), Name: s.Name, URL: s.URL, JWKSURI: s.JWKSURI,
		Algorithms: s.Algorithms, RequiredClaims: s.RequiredClaims, MaxTokenLifetime: s.MaxTokenLifetime}
	var err error
	for _, c := range []struct {
		raw string
		out *[]string
	}{{s.RolesClaim, &is.RolesClaim}, {s.GroupsClaim, &is.GroupsClaim}, {s.ClientClaim, &is.ClientClaim}} {
		if *c.out, err = auth.ParseClaimPath(c.raw); err != nil {
			return is, err
		}
	}
	if err := auth.CheckIssuer(&is, allowHTTP); err != nil {
		return is, err
	}
	if len(is.RequiredClaims) == 0 {
		return is, fmt.Errorf("%w: a server issuer needs required_claims (any account of a shared issuer could sign in)", store.ErrInvalid)
	}
	return is, nil
}

// ServerIssuers returns the server issuers as issuer records (validated).
func (c Config) ServerIssuers() []store.Issuer {
	var out []store.Issuer
	for _, s := range c.Auth.ServerIssuers {
		if is, err := s.issuer(c.loopbackHTTP()); err == nil {
			out = append(out, is)
		}
	}
	return out
}

func (c Config) loopbackHTTP() bool { return c.Profile == ProfileDev && c.Egress.AllowLoopbackHTTP }

// ServerAudiences returns the server tokens' audiences: auth.server_audiences, or serve.public_url.
func (c Config) ServerAudiences() []string {
	if len(c.Auth.ServerAudiences) > 0 {
		return c.Auth.ServerAudiences
	}
	if c.Serve.PublicURL != "" {
		return []string{strings.TrimSuffix(c.Serve.PublicURL, "/")}
	}
	return nil
}

// ServerAdmins returns the server administrators' principals (validated).
func (c Config) ServerAdmins() auth.Principals {
	p := auth.Principals{}
	for _, s := range c.Auth.ServerAdmins {
		kind, issuer, value, err := auth.ParsePrincipalKey(s)
		if err == nil && kind != store.PrincipalIssuer {
			p[auth.Key{IssuerID: auth.ServerIssuerID(issuer), Kind: kind, Value: value}] = true
		}
	}
	return p
}

// AdminTokenMaxAge is how recently a management token must have been issued.
func (c Config) AdminTokenMaxAge() time.Duration {
	if c.Auth.AdminTokenMaxAge == 0 {
		return DefaultAdminTokenMaxAge
	}
	return c.Auth.AdminTokenMaxAge
}

func validateAuth(bad func(string, ...any), c Config) {
	a := c.Auth
	names, urls := map[string]bool{}, map[string]bool{}
	for i, s := range a.ServerIssuers {
		if urls[s.URL] {
			bad("auth.server_issuers[%d]: the URL %s is used twice", i, s.URL)
		}
		urls[s.URL] = true
		if _, err := s.issuer(c.loopbackHTTP()); err != nil {
			bad("auth.server_issuers[%d]: %s", i, strings.TrimPrefix(err.Error(), store.ErrInvalid.Error()+": "))
		}
		if names[s.Name] {
			bad("auth.server_issuers[%d]: the name %s is used twice", i, s.Name)
		}
		names[s.Name] = true
	}
	for i, s := range a.ServerAdmins {
		kind, issuer, _, err := auth.ParsePrincipalKey(s)
		switch {
		case err != nil:
			bad("auth.server_admins[%d]: %q is kind:issuer|value", i, s)
		case kind == store.PrincipalIssuer:
			bad("auth.server_admins[%d]: issuer: is refused (every account of the issuer would be an administrator)", i)
		case !names[issuer]:
			bad("auth.server_admins[%d]: %s is not a server issuer", i, issuer)
		}
	}
	p := strings.TrimSuffix(c.Serve.PublicURL, "/")
	if len(a.ServerIssuers) > 0 && len(c.ServerAudiences()) == 0 {
		bad("auth.server_audiences (or serve.public_url) is required with server issuers")
	}
	for i, aud := range a.ServerAudiences {
		switch {
		case aud == "" || len(aud) > 400 || strings.ContainsAny(aud, " \t\r\n"):
			bad("auth.server_audiences[%d] is not an audience", i)
		case p != "" && strings.HasPrefix(aud, p+"/"):
			bad("auth.server_audiences[%d]: audiences under serve.public_url are the tenants' canonical ones", i)
		}
	}
	for i, e := range a.ServerEgressAllow {
		if _, err := netip.ParsePrefix(e.CIDR); err != nil {
			bad("auth.server_egress_allow[%d].cidr %q is not a CIDR", i, e.CIDR)
		}
		for _, port := range e.Ports {
			if port == 0 {
				bad("auth.server_egress_allow[%d].ports: 0 is not a port", i)
			}
		}
	}
	if a.AdminTokenMaxAge != 0 && (a.AdminTokenMaxAge < time.Minute || a.AdminTokenMaxAge > 24*time.Hour) {
		bad("auth.admin_token_max_age must be within 1m..24h")
	}
}
