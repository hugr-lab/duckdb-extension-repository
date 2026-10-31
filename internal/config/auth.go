package config

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Auth configures server identity (spec 0007): the server issuers, their audiences, the server
// administrators, the licence managers (spec 0029). The whole block is file-only, so no API caller
// can widen a server role; changes take effect at restart.
type Auth struct {
	ServerIssuers     []ServerIssuer `yaml:"server_issuers"`
	ServerAudiences   []string       `yaml:"server_audiences"`    // default: [ serve.public_url ]
	ServerEgressAllow []EgressAllow  `yaml:"server_egress_allow"` // reached only by the server issuers' fetcher
	ServerAdmins      []string       `yaml:"server_admins"`       // principals of the server issuers; issuer: is refused
	// ServerLicenseAdmins are the licence managers (spec 0029): principals of the server issuers
	// (issuer: is refused) who manage every tenant's licences and nothing else
	ServerLicenseAdmins []string `yaml:"server_license_admins"`
	// ServerDistributionAdmins are the distribution admins (spec 0030): principals of the server
	// issuers (issuer: is refused) who, on every tenant, create, read and manage distributors'
	// publishers (their keys, licence-key marks and holds; never the tenant's own: spec 0037), read
	// and act on their catalogue entries, and read the tenant's state and expected holder; nothing
	// else
	ServerDistributionAdmins []string      `yaml:"server_distribution_admins"`
	AdminTokenMaxAge         time.Duration `yaml:"admin_token_max_age"` // default 1h
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

func (s ServerIssuer) issuer(allowHTTP bool, maxLifetime time.Duration) (store.Issuer, error) {
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
	if err := auth.CheckIssuerUnder(&is, allowHTTP, maxLifetime); err != nil {
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
		if is, err := s.issuer(c.loopbackHTTP(), c.Limits.Ceilings.Settings().IssuerTokenLifetime); err == nil {
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
func (c Config) ServerAdmins() auth.Principals { return serverPrincipals(c.Auth.ServerAdmins) }

// ServerLicenseAdmins returns the licence managers' principals (validated; spec 0029).
func (c Config) ServerLicenseAdmins() auth.Principals {
	return serverPrincipals(c.Auth.ServerLicenseAdmins)
}

// ServerDistributionAdmins returns the distribution admins' principals (validated; spec 0030).
func (c Config) ServerDistributionAdmins() auth.Principals {
	return serverPrincipals(c.Auth.ServerDistributionAdmins)
}

func serverPrincipals(list []string) auth.Principals {
	p := auth.Principals{}
	for _, s := range list {
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
		if _, err := s.issuer(c.loopbackHTTP(), c.Limits.Ceilings.Settings().IssuerTokenLifetime); err != nil {
			bad("auth.server_issuers[%d]: %s", i, strings.TrimPrefix(err.Error(), store.ErrInvalid.Error()+": "))
		}
		if names[s.Name] {
			bad("auth.server_issuers[%d]: the name %s is used twice", i, s.Name)
		}
		names[s.Name] = true
		if auth.RefusedIssuer(s.URL) {
			bad("auth.server_issuers[%d]: %s is a CI token issuer's: it issues tokens to any workflow for any audience", i, s.URL)
		}
	}
	// the claim each principal kind is read from: without it on its issuer, it never matches
	claimOf := map[string]func(ServerIssuer) (string, string){
		store.PrincipalRole:   func(s ServerIssuer) (string, string) { return s.RolesClaim, "roles_claim" },
		store.PrincipalGroup:  func(s ServerIssuer) (string, string) { return s.GroupsClaim, "groups_claim" },
		store.PrincipalClient: func(s ServerIssuer) (string, string) { return s.ClientClaim, "client_claim" },
	}
	byName := map[string]ServerIssuer{}
	for _, s := range a.ServerIssuers {
		if _, ok := byName[s.Name]; !ok {
			byName[s.Name] = s
		}
	}
	for _, l := range []struct {
		name, role string
		list       []string
	}{{"server_admins", "an administrator", a.ServerAdmins}, {"server_license_admins", "a licence manager", a.ServerLicenseAdmins},
		{"server_distribution_admins", "a distribution admin", a.ServerDistributionAdmins}} {
		for i, s := range l.list {
			kind, issuer, _, err := auth.ParsePrincipalKey(s)
			is, known := byName[issuer]
			switch {
			case err != nil:
				bad("auth.%s[%d]: %q is kind:issuer|value", l.name, i, s)
			case kind == store.PrincipalIssuer:
				bad("auth.%s[%d]: issuer: is refused (every account of the issuer would be %s)", l.name, i, l.role)
			case kind == store.PrincipalPublisher:
				bad("auth.%s[%d]: publisher: is refused (a tenant's publisher, not a principal of a server issuer)", l.name, i)
			case !known:
				bad("auth.%s[%d]: %s is not a server issuer", l.name, i, issuer)
			case claimOf[kind] != nil:
				if claim, setting := claimOf[kind](is); claim == "" {
					bad("auth.%s[%d]: %s never matches: the server issuer %s has no %s", l.name, i, s, issuer, setting)
				}
			}
		}
	}
	// a token acts in one narrow role: a principal is a licence manager or a distribution admin
	for i, s := range a.ServerDistributionAdmins {
		if slices.Contains(a.ServerLicenseAdmins, s) {
			bad("auth.server_distribution_admins[%d]: %s is also a licence manager; a principal holds one of the two roles", i, s)
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
	// a minute at least, in code: the console renews a management token before iat plus the max age
	// (spec 0015), and clocks drift; the most is the server's (limits.ceilings.admin_token_max_age,
	// 24h by default; spec 0038 amended)
	if a.AdminTokenMaxAge != 0 && a.AdminTokenMaxAge < MinAdminTokenMaxAge {
		bad("auth.admin_token_max_age must be at least 1m")
	}
	if m, ceil := c.AdminTokenMaxAge(), c.Limits.Ceilings.Setting().AdminTokenMaxAge; ceil >= MinAdminTokenMaxAge && m > ceil {
		bad("auth.admin_token_max_age is at most limits.ceilings.admin_token_max_age (%s)", durationText(ceil))
	}
	// below the longest token any issuer may mint: else a token as old as a token can be still
	// manages, and the stale-token gate (spec 0007) never refuses one (spec 0038 amended, security)
	if m, life := c.AdminTokenMaxAge(), c.Limits.Ceilings.Settings().IssuerTokenLifetime; life > MinAdminTokenMaxAge && m >= life {
		bad("auth.admin_token_max_age must be below limits.ceilings.issuer_token_lifetime (%s): "+
			"a management token must be fresher than the longest token", durationText(life))
	}
}
