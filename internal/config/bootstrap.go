package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream/upspec"
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
	// Upstreams are the channels' upstreams (upstream add, spec 0024); omitempty keeps the digest of
	// a section without them as it was.
	Upstreams []BootstrapUpstream `yaml:"upstreams" json:"upstreams,omitempty"`
}

// BootstrapUpstream is an upstream of one of the section's channels (upstream add, spec 0009): a
// mirror or pull-through upstream; bundle mode is an operator's, never the bootstrap's.
type BootstrapUpstream struct {
	Name    string   `yaml:"name" json:"name,omitempty"`
	Kind    string   `yaml:"kind" json:"kind,omitempty"`
	Prefix  string   `yaml:"prefix" json:"prefix,omitempty"`
	Keys    []string `yaml:"keys" json:"keys,omitempty"`
	Channel string   `yaml:"channel" json:"channel,omitempty"`
	Mode    string   `yaml:"mode" json:"mode,omitempty"`
	// Fetch is a mirror's: eager or lazy (spec 0032); "" is eager, never defaulted, so a section
	// applied before it keeps its digest
	Fetch      string                   `yaml:"fetch" json:"fetch,omitempty"`
	Platforms  []string                 `yaml:"platforms" json:"platforms,omitempty"`
	Extensions []BootstrapUpstreamEntry `yaml:"extensions" json:"extensions,omitempty"`
	Visibility string                   `yaml:"visibility" json:"visibility,omitempty"`
	Credential string                   `yaml:"credential" json:"credential,omitempty"`
}

// BootstrapUpstreamEntry is an allowlist entry: a name, the versions taken (none: any), and whether
// a reserved name comes from a repository that is not its own.
type BootstrapUpstreamEntry struct {
	Name          string   `yaml:"name" json:"name,omitempty"`
	Versions      []string `yaml:"versions" json:"versions,omitempty"`
	AllowReserved bool     `yaml:"allow_reserved" json:"allow_reserved,omitempty"`
	Eager         bool     `yaml:"eager" json:"eager,omitempty"` // a lazy mirror fetches it unasked (spec 0032)
}

// StoreEntries are the allowlist as the store holds it.
func (u BootstrapUpstream) StoreEntries() []store.UpstreamEntry {
	out := make([]store.UpstreamEntry, len(u.Extensions))
	for i, e := range u.Extensions {
		out[i] = store.UpstreamEntry{Name: e.Name, Versions: e.Versions, AllowReserved: e.AllowReserved, Eager: e.Eager}
	}
	return out
}

// BootstrapVersion is a server-wide DuckDB version (version add) with its core extensions (spec
// 0030; kista duckdb core-extensions), required for a version the server does not have.
type BootstrapVersion struct {
	Version              string   `yaml:"version" json:"version,omitempty"`
	Kind                 string   `yaml:"kind" json:"kind,omitempty"`
	CAPIs                []string `yaml:"c_apis" json:"c_apis,omitempty"`
	CoreExtensions       []string `yaml:"core_extensions" json:"core_extensions,omitempty"`
	CoreExtensionsCommit string   `yaml:"core_extensions_commit" json:"core_extensions_commit,omitempty"`
}

// BootstrapTenant is the tenant (tenant create) and its audiences (tenant audience add).
type BootstrapTenant struct {
	Name        string   `yaml:"name" json:"name,omitempty"`
	DisplayName string   `yaml:"display_name" json:"display_name,omitempty"`
	Domain      string   `yaml:"domain" json:"domain,omitempty"`
	Audiences   []string `yaml:"audiences" json:"audiences,omitempty"`
	// Limits are the tenant's own limits (spec 0038): those named are set, the others left as they
	// are; omitempty keeps the digest of a section without them as it was
	Limits *Limits `yaml:"limits" json:"limits,omitempty"`
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
	// PackageType is the grant's package type (spec 0034): "" is the default for its shape (an
	// extension: duckdb_extension; none: "*", every type). Never filled in by Defaulted, and left out
	// of Key when it is that default, so a section applied before it keeps its digest and its plan.
	PackageType string `yaml:"package_type" json:"package_type,omitempty"`
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
	if t := g.shownType(); t != "" {
		k += " of " + t
	}
	return k
}

// shownType is the grant's package type when it is not the default for its shape ("" when it is).
func (g BootstrapGrant) shownType() string {
	switch {
	case g.PackageType == "", g.Extension != "" && g.PackageType == pkgtype.Default, g.Extension == "" && g.PackageType == pkgtype.Any:
		return ""
	}
	return g.PackageType
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
	if len(b.Upstreams) > 0 {
		kinds := map[string]string{}
		for _, ch := range b.Channels {
			kinds[ch.Name] = ch.Kind
		}
		ups := make([]BootstrapUpstream, len(b.Upstreams))
		for i, u := range b.Upstreams {
			if u.Mode == "" {
				u.Mode = store.ModeMirror
			}
			if u.Visibility == "" {
				u.Visibility = store.Private
				if kinds[u.Channel] == store.ChannelPassthrough {
					u.Visibility = store.Public
				}
			}
			if p, err := upspec.Prefix(u.Prefix, u.Mode, func(uint16) bool { return true }); err == nil && u.Prefix != "" {
				u.Prefix = p
			}
			u.Keys, u.Platforms = upspec.Dedup(u.Keys), upspec.Dedup(u.Platforms)
			ext := make([]BootstrapUpstreamEntry, len(u.Extensions))
			for j, e := range u.Extensions {
				e.Versions = upspec.Dedup(e.Versions)
				ext[j] = e
			}
			u.Extensions = ext
			ups[i] = u
		}
		b.Upstreams = ups
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
	signerRef   = regexp.MustCompile(`^[a-z][a-z0-9-]*:\S+$`)
	capiRule    = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	coreExtRule = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	commitRule  = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
)

func validateBootstrap(bad func(string, ...any), c Config) {
	b := c.Bootstrap
	if !b.Set() {
		if len(b.Channels) > 0 || b.Issuer != nil || len(b.Grants) > 0 || b.AttestationKey != "" || len(b.Upstreams) > 0 {
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
		for _, n := range v.CoreExtensions {
			if !coreExtRule.MatchString(n) {
				bad("%s.core_extensions: %q is not an extension name", where, n)
			}
		}
		if c := v.CoreExtensionsCommit; c != "" && !commitRule.MatchString(c) {
			bad("%s.core_extensions_commit: %q is not a commit", where, c)
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
		// the bootstrap's channels are duckdb_extension's: a grant on one of that type is "*" there
		// (spec 0034: the store holds it so)
		same := g
		if same.Channel != "" && same.Extension == "" && same.PackageType == pkgtype.DuckDBExtension {
			same.PackageType = ""
		}
		switch t := g.PackageType; {
		case t == pkgtype.Any && g.Extension != "":
			bad("%s.package_type: a grant on an extension names its type, never \"*\"", where)
		case t != "" && t != pkgtype.Any && !pkgtype.Known(t):
			bad("%s.package_type: %q is not a package type this server knows (%s)", where, t, strings.Join(pkgtype.Types(), ", "))
		case same.PackageType != "" && same.PackageType != pkgtype.Any && slices.Contains(g.Verbs, store.VerbAudit):
			bad("%s: audit is granted on the tenant of every package type, never of one (no package_type)", where)
		}
		if g.Channel != "" && !channels[g.Channel] {
			bad("%s.channel %q is none of bootstrap.channels", where, g.Channel)
		}
		once("grant", same.Key())
	}
	validateBootstrapUpstreams(bad, c, once)
}

func issuerNameOr(is *BootstrapIssuer) string {
	if is == nil {
		return "<issuer>"
	}
	return is.Name
}

// validateBootstrapUpstreams checks the section's upstreams with upstream add's own field checks
// (spec 0024); what depends on what exists (collisions, a source's keys) is the bootstrap's check.
func validateBootstrapUpstreams(bad func(string, ...any), c Config, once func(what, v string)) {
	b := c.Bootstrap
	kinds, versions := map[string]string{}, map[string]int{}
	for _, ch := range b.Channels {
		kinds[ch.Name], versions[ch.Name] = ch.Kind, len(ch.Versions)
	}
	if len(b.Upstreams) > store.MaxUpstreams {
		bad("bootstrap.upstreams: at most %d", store.MaxUpstreams)
	}
	portAllowed := func(p uint16) bool {
		if p == 443 || c.loopbackHTTP() {
			return true
		}
		for _, a := range c.Egress.Allow {
			if slices.Contains(a.Ports, p) {
				return true
			}
		}
		return false
	}
	creds := map[string]Credential{}
	for _, cr := range c.Upstreams.Credentials {
		creds[cr.Name] = cr
	}
	tenantCells := 0
	for i, u := range b.Defaulted().Upstreams {
		where := fmt.Sprintf("bootstrap.upstreams[%d]", i)
		if err := store.ValidName(u.Name); err != nil {
			bad("%s.name: %v", where, err)
		}
		once("upstream", u.Name)
		kind, known := kinds[u.Channel]
		if !known {
			bad("%s.channel %q is none of bootstrap.channels", where, u.Channel)
		}
		switch u.Mode {
		case store.ModeMirror, store.ModePullThrough:
		default:
			bad("%s.mode must be %s or %s (bundle mode is an operator's)", where, store.ModeMirror, store.ModePullThrough)
		}
		if u.Visibility != store.Public && u.Visibility != store.Private {
			bad("%s.visibility must be %s or %s", where, store.Public, store.Private)
		}
		switch {
		case u.Mode != store.ModeMirror && u.Fetch != "":
			bad("%s.fetch: only a mirror takes one", where)
		case u.Mode == store.ModeMirror && u.Fetch != "" && u.Fetch != store.FetchEager && u.Fetch != store.FetchLazy:
			bad("%s.fetch must be %s or %s", where, store.FetchEager, store.FetchLazy)
		}
		switch u.Kind {
		case store.UpstreamCore, store.UpstreamCommunity:
			if u.Prefix != "" || len(u.Keys) > 0 {
				bad("%s: a %s upstream takes no prefix or keys", where, u.Kind)
			}
			if u.Credential != "" {
				bad("%s.credential: only a repository upstream takes one", where)
			}
		case store.UpstreamRepo:
			if _, err := upspec.Prefix(u.Prefix, u.Mode, portAllowed); err != nil {
				bad("%s.prefix: %v", where, err)
			} else if strings.HasPrefix(strings.ToLower(u.Prefix), "http://") && (!c.loopbackHTTP() || !loopbackHost(u.Prefix)) {
				// plain http reaches loopback only, in development (egress); a fetch would refuse it
				bad("%s.prefix: not https", where)
			}
			if len(u.Keys) == 0 {
				bad("%s.keys: a repository upstream pins at least one key", where)
			}
		default:
			bad("%s.kind must be %s, %s or %s", where, store.UpstreamCore, store.UpstreamCommunity, store.UpstreamRepo)
		}
		if len(u.Keys) > store.MaxUpstreamKeys {
			bad("%s.keys: at most %d", where, store.MaxUpstreamKeys)
		}
		for _, k := range u.Keys {
			if err := upspec.Fingerprint(k); err != nil {
				bad("%s.keys: %v", where, err)
			}
		}
		if len(u.Platforms) == 0 {
			bad("%s.platforms: at least one", where)
		}
		for _, p := range u.Platforms {
			if err := release.ValidPlatform(p); err != nil {
				bad("%s.platforms: %v", where, err)
			}
		}
		if u.Mode == store.ModeMirror && len(u.Extensions) == 0 {
			bad("%s.extensions: a mirror names at least one", where)
		}
		if len(u.Extensions) > store.MaxUpstreamEntries {
			bad("%s.extensions: at most %d", where, store.MaxUpstreamEntries)
		}
		names := map[string]bool{}
		for j, e := range u.StoreEntries() {
			if err := upspec.Entry(u.Kind, u.Mode, &e); err != nil {
				bad("%s.extensions[%d]: %v", where, j, err)
			}
			if len(e.Versions) > store.MaxEntryVersions {
				bad("%s.extensions[%d].versions: at most %d", where, j, store.MaxEntryVersions)
			}
			if names[e.Name] {
				bad("%s.extensions: %q is listed twice", where, e.Name)
			}
			names[e.Name] = true
			// one source decides a name in a channel ("*" included: two would collide)
			once("extension of channel "+u.Channel, e.Name)
		}
		if kind == store.ChannelPassthrough {
			switch {
			case u.Kind != store.UpstreamCore:
				bad("%s: a passthrough channel takes duckdb-core upstreams only", where)
			case u.Mode != store.ModeMirror:
				bad("%s: a passthrough channel takes mirrors only", where)
			case u.Visibility != store.Public:
				bad("%s: a passthrough channel's releases are public", where)
			case u.Fetch == store.FetchLazy:
				bad("%s: a passthrough channel takes no lazy mirror", where)
			}
		}
		if u.Credential != "" && u.Kind == store.UpstreamRepo && u.Prefix != "" {
			cr, ok := creds[u.Credential]
			switch prefix := u.Prefix; { // Defaulted normalised it
			case !ok:
				bad("%s.credential %q is none of upstreams.credentials", where, u.Credential)
			case !slices.Contains(cr.Tenants, "*") && !slices.Contains(cr.Tenants, b.Tenant.Name):
				bad("%s.credential %q is not for tenant %s (its tenants)", where, u.Credential, b.Tenant.Name)
			case !upspec.Covers(parsePrefixes(cr.Prefixes), prefix):
				bad("%s.credential %q is not sent to %s (its prefixes)", where, u.Credential, prefix)
			case u.Visibility == store.Public && !cr.AllowPublic:
				bad("%s.credential %q does not allow public releases (allow_public)", where, u.Credential)
			}
		}
		// the matrix: the channel's DuckDB versions × platforms × entries, as the store counts them
		cells := versions[u.Channel] * len(u.Platforms) * len(names)
		if cells > store.MaxUpstreamCells {
			bad("%s: its matrix would be %d cells (at most %d)", where, cells, store.MaxUpstreamCells)
		}
		tenantCells += cells
	}
	if tenantCells > store.MaxTenantCells {
		bad("bootstrap.upstreams: the tenant's upstreams would fetch %d cells (at most %d)", tenantCells, store.MaxTenantCells)
	}
}

func parsePrefixes(raw []string) []*url.URL {
	var out []*url.URL
	for _, p := range raw {
		if u, err := url.Parse(p); err == nil {
			out = append(out, u)
		}
	}
	return out
}

// loopbackHost reports whether a URL's host is loopback (localhost, 127.0.0.0/8, ::1), the only
// place egress takes plain http to, in development.
func loopbackHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
