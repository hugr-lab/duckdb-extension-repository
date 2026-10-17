package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Bootstrap names what a first start creates (spec 0020 phase 4a): DuckDB versions, one tenant
// with its audiences, channels, keys, issuer, console client and grants. Each field is a parameter
// of the service call the admin command of the same purpose makes. It is applied once.
type Bootstrap struct {
	Versions       []BootstrapVersion `yaml:"versions" json:"versions,omitempty"`
	Tenant         BootstrapTenant    `yaml:"tenant" json:"tenant,omitempty"`
	Channels       []BootstrapChannel `yaml:"channels" json:"channels,omitempty"`
	AttestationKey string             `yaml:"attestation_key" json:"attestation_key,omitempty"` // a signer reference, added active
	Issuer         *BootstrapIssuer   `yaml:"issuer" json:"issuer,omitempty"`
	Grants         []BootstrapGrant   `yaml:"grants" json:"grants,omitempty"`
}

// BootstrapVersion is a server-wide DuckDB version (version add).
type BootstrapVersion struct {
	Version string   `yaml:"version" json:"version,omitempty"`
	Kind    string   `yaml:"kind" json:"kind,omitempty"`
	CAPIs   []string `yaml:"c_apis" json:"c_apis,omitempty"`
}

// BootstrapTenant is the tenant (tenant create) and its audiences (tenant audience add).
type BootstrapTenant struct {
	Name        string   `yaml:"name" json:"name,omitempty"`
	DisplayName string   `yaml:"display_name" json:"display_name,omitempty"`
	Domain      string   `yaml:"domain" json:"domain,omitempty"`
	Audiences   []string `yaml:"audiences" json:"audiences,omitempty"`
}

// BootstrapChannel is a channel (channel create), its DuckDB versions (added, never removed) and
// its first key, added active.
type BootstrapChannel struct {
	Name     string   `yaml:"name" json:"name,omitempty"`
	Kind     string   `yaml:"kind" json:"kind,omitempty"`
	Versions []string `yaml:"versions" json:"versions,omitempty"`
	Key      string   `yaml:"key" json:"key,omitempty"`
}

// BootstrapIssuer is the tenant's IdP (issuer add) and, optionally, its console client (issuer
// console).
type BootstrapIssuer struct {
	Name           string            `yaml:"name" json:"name,omitempty"`
	URL            string            `yaml:"url" json:"url,omitempty"`
	JWKSURI        string            `yaml:"jwks_uri" json:"jwks_uri,omitempty"`
	Algorithms     []string          `yaml:"algorithms" json:"algorithms,omitempty"`
	RequiredClaims map[string]string `yaml:"required_claims" json:"required_claims,omitempty"`
	RolesClaim     string            `yaml:"roles_claim" json:"roles_claim,omitempty"`
	GroupsClaim    string            `yaml:"groups_claim" json:"groups_claim,omitempty"`
	ClientClaim    string            `yaml:"client_claim" json:"client_claim,omitempty"`
	MaxLifetime    time.Duration     `yaml:"max_lifetime" json:"max_lifetime,omitempty"`
	Console        *BootstrapConsole `yaml:"console" json:"console,omitempty"`
}

// BootstrapConsole is a console client at the bootstrap's issuer.
type BootstrapConsole struct {
	ClientID          string `yaml:"client_id" json:"client_id,omitempty"`
	Scopes            string `yaml:"scopes" json:"scopes,omitempty"`
	Audience          string `yaml:"audience" json:"audience,omitempty"`
	AudienceParameter string `yaml:"audience_parameter" json:"audience_parameter,omitempty"`
}

// BootstrapGrant is a grant (grant add) to a principal of the bootstrap's issuer, tenant-wide or on
// one channel, extension or extension group.
type BootstrapGrant struct {
	Principal      string   `yaml:"principal" json:"principal,omitempty"`
	Verbs          []string `yaml:"verbs" json:"verbs,omitempty"`
	Channel        string   `yaml:"channel" json:"channel,omitempty"`
	Extension      string   `yaml:"extension" json:"extension,omitempty"`
	ExtensionGroup string   `yaml:"extension_group" json:"extension_group,omitempty"`
}

// ScopeList is the console's scopes (commas or spaces), or DefaultConsoleScopes, as issuer console
// takes them.
func (c BootstrapConsole) ScopeList() []string {
	sc := strings.FieldsFunc(c.Scopes, func(r rune) bool { return r == ',' || r == ' ' })
	if len(sc) == 0 {
		return DefaultConsoleScopes
	}
	return sc
}

// Key names a grant by its principal, verbs (sorted, once each) and scope: the bootstrap's plan and
// its sameness compare grants by it.
func (g BootstrapGrant) Key() string {
	verbs := slices.Compact(slices.Sorted(slices.Values(g.Verbs)))
	k := g.Principal + " " + strings.Join(verbs, ",")
	switch {
	case g.Channel != "" && g.Extension != "":
		k += " on " + g.Channel + "/" + g.Extension
	case g.Channel != "":
		k += " on " + g.Channel
	case g.Extension != "":
		k += " on */" + g.Extension
	}
	return k
}

// Defaulted is the section with its defaults filled in: the tenant's domain and the console's
// scopes. Its digest is taken from it, so writing a default out is no change.
func (b Bootstrap) Defaulted() Bootstrap {
	if b.Tenant.Domain == "" {
		b.Tenant.Domain = store.DefaultDomain
	}
	if b.Issuer != nil && b.Issuer.Console != nil {
		is, cc := *b.Issuer, *b.Issuer.Console
		cc.Scopes = strings.Join(cc.ScopeList(), " ")
		is.Console, b.Issuer = &cc, &is
	}
	return b
}

// Set reports whether the section names anything.
func (b Bootstrap) Set() bool { return b.Tenant.Name != "" || len(b.Versions) > 0 }

// IssuerRecord is the bootstrap's issuer as an issuer record (its claim paths parsed).
func (b BootstrapIssuer) IssuerRecord() (store.Issuer, error) {
	is := store.Issuer{Name: b.Name, URL: b.URL, JWKSURI: b.JWKSURI, Algorithms: b.Algorithms, RequiredClaims: b.RequiredClaims,
		MaxTokenLifetime: b.MaxLifetime}
	if is.RequiredClaims == nil {
		is.RequiredClaims = map[string]string{}
	}
	var err error
	for _, c := range []struct {
		raw string
		out *[]string
	}{{b.RolesClaim, &is.RolesClaim}, {b.GroupsClaim, &is.GroupsClaim}, {b.ClientClaim, &is.ClientClaim}} {
		if *c.out, err = auth.ParseClaimPath(c.raw); err != nil {
			return is, err
		}
	}
	return is, nil
}

var (
	signerRef = regexp.MustCompile(`^[a-z][a-z0-9-]*:\S+$`)
	capiRule  = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
)

func validateBootstrap(bad func(string, ...any), c Config) {
	b := c.Bootstrap
	if !b.Set() {
		if len(b.Channels) > 0 || b.Issuer != nil || len(b.Grants) > 0 || b.AttestationKey != "" {
			bad("bootstrap needs a tenant")
		}
		return
	}
	for i, v := range b.Versions {
		where := fmt.Sprintf("bootstrap.versions[%d]", i)
		if err := store.ValidDuckDBVersion(v.Version); err != nil {
			bad("%s.version: %v", where, err)
		}
		if v.Kind != "release" && v.Kind != "dev" {
			bad("%s.kind must be release or dev", where)
		}
		for _, ca := range v.CAPIs {
			if !capiRule.MatchString(ca) {
				bad("%s.c_apis: %q is not a version", where, ca)
			}
		}
	}
	t := b.Tenant
	if err := store.ValidName(t.Name); err != nil {
		bad("bootstrap.tenant.name: %v", err)
	}
	if t.Domain != "" && !c.HasBlobDomain(t.Domain) {
		bad("bootstrap.tenant.domain %q is no blob domain", t.Domain)
	}
	seen := map[string]bool{}
	once := func(what, v string) {
		if seen[what+"\x00"+v] {
			bad("bootstrap: %s %q is listed twice", what, v)
		}
		seen[what+"\x00"+v] = true
	}
	for _, v := range b.Versions {
		once("version", v.Version)
	}
	for _, a := range t.Audiences {
		switch pub := strings.TrimRight(c.Serve.PublicURL, "/"); {
		case strings.TrimSpace(a) != a || a == "":
			bad("bootstrap.tenant.audiences: %q", a)
		case pub != "" && strings.HasPrefix(a, pub+"/"):
			bad("bootstrap.tenant.audiences: %s is under serve.public_url: a tenant's canonical audience is never assigned", a)
		case slices.Contains(c.Auth.ServerAudiences, a):
			bad("bootstrap.tenant.audiences: %s is a server audience", a)
		}
		once("audience", a)
	}
	channels := map[string]bool{}
	for i, ch := range b.Channels {
		where := fmt.Sprintf("bootstrap.channels[%d]", i)
		if err := store.ValidName(ch.Name); err != nil {
			bad("%s.name: %v", where, err)
		} else if channels[ch.Name] {
			bad("%s.name %q is listed twice", where, ch.Name)
		}
		channels[ch.Name] = true
		switch ch.Kind {
		case store.ChannelSigned:
			if !signerRef.MatchString(ch.Key) {
				bad("%s.key must be a signer reference (scheme:…): a signed channel's first key", where)
			}
		case store.ChannelPassthrough:
			if ch.Key != "" {
				bad("%s: a passthrough channel holds no key", where)
			}
		default:
			bad("%s.kind must be %s or %s", where, store.ChannelSigned, store.ChannelPassthrough)
		}
		for _, v := range ch.Versions {
			if err := store.ValidDuckDBVersion(v); err != nil {
				bad("%s.versions: %v", where, err)
			}
			once(ch.Name+" version", v)
		}
		if ch.Key != "" {
			once("signer reference", ch.Key)
		}
	}
	if b.AttestationKey != "" {
		if !signerRef.MatchString(b.AttestationKey) {
			bad("bootstrap.attestation_key must be a signer reference (scheme:…)")
		}
		once("signer reference", b.AttestationKey)
	}
	if is := b.Issuer; is != nil {
		if err := store.ValidIssuerName(is.Name); err != nil {
			bad("bootstrap.issuer.name: %v", err)
		}
		if rec, err := is.IssuerRecord(); err != nil {
			bad("bootstrap.issuer: %v", err)
		} else if err := auth.CheckIssuer(&rec, c.Egress.AllowLoopbackHTTP); err != nil {
			bad("bootstrap.issuer: %v", err)
		}
		if is.MaxLifetime < 0 {
			bad("bootstrap.issuer.max_lifetime must not be negative")
		}
		if cc := is.Console; cc != nil {
			if cc.ClientID == "" {
				bad("bootstrap.issuer.console.client_id is required")
			}
			if cc.Audience != "" && !slices.Contains(t.Audiences, cc.Audience) {
				bad("bootstrap.issuer.console.audience must be one of bootstrap.tenant.audiences")
			}
			// a tenant's console never signs in at a server issuer (spec 0015): found at load, not
			// by every start failing
			for _, s := range c.Auth.ServerIssuers {
				if strings.TrimRight(s.URL, "/") == strings.TrimRight(is.URL, "/") {
					bad("bootstrap.issuer.console: %s is server issuer %s's URL; a tenant's console does not sign in there", is.URL, s.Name)
				}
			}
		}
	}
	for i, g := range b.Grants {
		where := fmt.Sprintf("bootstrap.grants[%d]", i)
		kind, issuer, _, err := auth.ParsePrincipalKey(g.Principal)
		switch {
		case err != nil:
			bad("%s.principal: %v", where, err)
		case kind == store.PrincipalIssuer:
			bad("%s.principal: an issuer-wide principal (every account of the issuer) is never the bootstrap's", where)
		case b.Issuer == nil || issuer != b.Issuer.Name:
			bad("%s.principal must be a principal of bootstrap.issuer (kind:%s|value)", where, issuerNameOr(b.Issuer))
		case kind == store.PrincipalClient && slices.Contains(g.Verbs, "admin"):
			bad("%s: a client principal is never an administrator", where)
		}
		if len(g.Verbs) == 0 {
			bad("%s.verbs is required", where)
		}
		for _, v := range g.Verbs {
			if !slices.Contains(store.Verbs, v) {
				bad("%s.verbs: %q is none of %s", where, v, strings.Join(store.Verbs, ", "))
			}
		}
		if g.ExtensionGroup != "" {
			bad("%s: the bootstrap makes no extension groups; grant on one through the API", where)
		}
		if g.Channel != "" && !channels[g.Channel] {
			bad("%s.channel %q is none of bootstrap.channels", where, g.Channel)
		}
		once("grant", g.Key())
	}
}

func issuerNameOr(is *BootstrapIssuer) string {
	if is == nil {
		return "<issuer>"
	}
	return is.Name
}
