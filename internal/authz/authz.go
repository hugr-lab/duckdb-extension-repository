// Package authz holds the actor of a request and the authorization hook every service calls
// (spec 0003). The CLI's server administrator is allowed everything; over the API (spec 0007)
// server administrators are allowed everything, licence managers every tenant's licences (spec
// 0029), and tenant principals act through their grants.
package authz

import (
	"context"
	"errors"
	"os"
	"os/user"
	"slices"
	"strconv"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// ActorKind says where an action comes from.
type ActorKind string

const (
	ActorOS        ActorKind = "os"        // kista admin: the OS user running the CLI
	ActorPrincipal ActorKind = "principal" // a tenant token's principals (spec 0006)
	ActorServer    ActorKind = "server"    // a server administrator's token (spec 0007)
	ActorPublisher ActorKind = "publisher" // a publisher's credential (spec 0008): publish and promote only
	ActorSystem    ActorKind = "system"    // kista itself
	// ActorLicenseManager is a licence manager's server token (spec 0029): license only
	ActorLicenseManager ActorKind = "license-manager"
	// ActorDistributionAdmin is a distribution admin's server token (spec 0030): distribute only
	ActorDistributionAdmin ActorKind = "distribution-admin"
)

// Actor is who performs an action. Its String form is recorded with every change.
type Actor struct {
	Kind ActorKind
	ID   string
	// Tenant is the tenant whose records verified a principal actor's token.
	Tenant string
	// Principals are a principal actor's principals in Tenant.
	Principals auth.Principals
}

func (a Actor) String() string { return string(a.Kind) + ":" + a.ID }

// OSActor is the OS user running this process, as "os:<uid>:<name>" (the uid, not $USER).
func OSActor() Actor {
	id := strconv.Itoa(os.Getuid())
	if u, err := user.Current(); err == nil {
		return Actor{Kind: ActorOS, ID: id + ":" + u.Username}
	}
	return Actor{Kind: ActorOS, ID: id}
}

// Verb is what an actor wants to do. Reading management data needs admin too (spec 0007).
type Verb string

const (
	VerbAdmin   Verb = "admin"
	VerbPublish Verb = "publish" // spec 0008: never implied by admin
	VerbPromote Verb = "promote"
	VerbAudit   Verb = "audit" // spec 0010: read the tenant's events; admin implies it
	VerbFeed    Verb = "feed"  // spec 0011: take the tenant's feed in a channel; admin implies it
	// VerbLicense manages a tenant's licences (spec 0029): admin on the tenant implies it, licence
	// managers hold it on every tenant and nothing else. No grant gives it (store.Verbs lacks it).
	VerbLicense Verb = "license"
	// VerbDistribute is a distributor's set of calls on a tenant (spec 0030): its reserved names,
	// marked publishers, and grants on extensions; on the server, reading the reserved names.
	// Distribution admins hold it on every active tenant and the server, and nothing else; server
	// administrators hold everything. No grant gives it.
	VerbDistribute Verb = "distribute"
)

// Resource is what an action is on: the server (no tenant), a tenant, a channel, a package (an
// extension) in a channel (or in every channel), or a package group (spec 0017's extension group:
// reading it). Reserved marks an extension name DuckDB owns (spec 0008): for publish and promote, only grants that name it reach
// it.
type Resource struct {
	Tenant, Channel, Package string
	// PackageType is the type of the channel's or the package's packages (spec 0034): the channel's
	// (Grants.Allow reads it when a channel is named and it is not set), or the route's ?type= for a
	// package without a channel (the default type when not set); never a body's. Unused without a
	// channel or a package.
	PackageType  string
	PackageGroup string
	Reserved     bool
}

// Server is the server-wide resource: tenants, DuckDB versions, audiences, signer references, force.
var Server = Resource{}

// ErrDenied is returned when an actor may not act.
var ErrDenied = errors.New("authz: not allowed")

// Authorizer decides whether an actor may perform a verb on a resource.
type Authorizer interface {
	Allow(ctx context.Context, a Actor, verb Verb, r Resource) error
}

// ServerAdmin allows OS actors (the CLI, running as the service user with the service's config)
// and the configuration's bootstrap (spec 0020 phase 4a) everything, and nobody else anything.
type ServerAdmin struct{}

// Bootstrap is the actor kista serve applies the configuration's bootstrap as. No request makes
// a system actor: a token maps to principals of an issuer.
var Bootstrap = Actor{Kind: ActorSystem, ID: "bootstrap"}

// Allow implements Authorizer.
func (ServerAdmin) Allow(_ context.Context, a Actor, _ Verb, _ Resource) error {
	if a.Kind == ActorOS || a.Kind == Bootstrap.Kind && a.ID == Bootstrap.ID {
		return nil
	}
	return ErrDenied
}

// Grants is the API's authorizer: OS and server actors (a server administrator; the API makes
// server actors of administrators only) are allowed everything; a licence manager license on a
// tenant (spec 0029); a principal actor what its grants in its own tenant allow.
type Grants struct {
	Store *store.Store
	Auths *auth.TenantAuths
}

// Allow implements Authorizer.
func (g Grants) Allow(ctx context.Context, a Actor, verb Verb, r Resource) error {
	switch a.Kind {
	case ActorOS, ActorServer:
		return nil
	case ActorDistributionAdmin:
		if verb != VerbDistribute || r.Channel != "" || r.Package != "" || r.PackageGroup != "" {
			return ErrDenied
		}
		if r.Tenant == "" {
			return nil // the server's reserved names
		}
		return activeTenant(ctx, g.Store, r.Tenant)
	case ActorLicenseManager:
		if verb != VerbLicense || r.Tenant == "" || r.Channel != "" || r.Package != "" || r.PackageGroup != "" {
			return ErrDenied
		}
		// an active tenant only: the API refuses the others first, and so does this whatever
		// reaches the service
		t, err := g.Store.GetTenant(ctx, r.Tenant)
		switch {
		case errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrDeleting) || errors.Is(err, store.ErrDeleted):
			return ErrDenied
		case err != nil:
			return err
		case t.State != store.TenantActive:
			return ErrDenied
		}
		return nil
	case ActorPrincipal:
	case ActorPublisher:
		if verb != VerbPublish && verb != VerbPromote {
			return ErrDenied
		}
	default:
		return ErrDenied
	}
	if r.Tenant == "" || r.Tenant != a.Tenant {
		return ErrDenied
	}
	t, err := g.Store.GetTenant(ctx, r.Tenant)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrDenied
		}
		return err
	}
	ta, err := g.Auths.Get(ctx, t)
	if err != nil {
		return err
	}
	r, err = g.typed(ctx, t.ID, r)
	if err != nil {
		return err
	}
	if Covers(auth.HeldBy(a.Principals, ta), string(verb), r) {
		return nil
	}
	return ErrDenied
}

// typed fills a resource's package type (spec 0034): a channel's is the channel's, read from the
// store (a channel that is not there has none, which only a grant of every type covers; one of a
// type this binary does not know is nobody's: ErrDenied); a package's without a channel the default
// when the caller did not name one, and a type that is not known is nobody's.
func (g Grants) typed(ctx context.Context, tenantID string, r Resource) (Resource, error) {
	switch {
	case r.Channel != "":
		t, err := g.Store.ChannelType(ctx, tenantID, r.Channel)
		switch {
		case errors.Is(err, store.ErrNotFound):
			t = ""
		case err != nil:
			return r, err
		case !pkgtype.Known(t):
			return r, ErrDenied
		}
		if r.PackageType != "" && r.PackageType != t {
			t = "" // a caller's type that is not the channel's: covered by nothing typed
		}
		r.PackageType = t
	case r.Package != "" && r.PackageType == "":
		r.PackageType = pkgtype.Default
	case r.Package != "" && !pkgtype.Known(r.PackageType):
		return r, ErrDenied
	}
	return r, nil
}

// activeTenant allows an active tenant only (a server role's calls on a tenant).
func activeTenant(ctx context.Context, st *store.Store, name string) error {
	t, err := st.GetTenant(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrDeleting) || errors.Is(err, store.ErrDeleted):
		return ErrDenied
	case err != nil:
		return err
	case t.State != store.TenantActive:
		return ErrDenied
	}
	return nil
}

// Covers reports whether a caller holds verb on a resource: a grant without a channel or extension
// covers the tenant and everything in it, one on a channel that channel and everything in it, one on
// an extension that extension's releases, in its channel or every channel (channels are compared
// by name: a tenant's are unique and never renamed), one on an extension group its members' as a
// grant on each would, and reading the group. admin implies every verb but publish and promote
// (spec 0008), except on an issuer-wide grant, which never carries admin's rights. For publish and
// promote, a reserved name is reached only by a grant naming it, never through a group.
func Covers(h auth.Held, verb string, r Resource) bool {
	for _, g := range h.Grants {
		if GrantCovers(g, verb, r) {
			return true
		}
	}
	for _, g := range h.GroupGrants {
		if GrantCovers(g, verb, r) {
			return true
		}
	}
	return false
}

// Held is what an actor holds in a tenant (spec 0017: read once to test many resources). OS and
// server actors hold nothing here: Allow admits them before grants.
func (g Grants) Held(ctx context.Context, a Actor, tenant string) (auth.Held, error) {
	if a.Kind != ActorPrincipal && a.Kind != ActorPublisher || a.Tenant != tenant {
		return auth.Held{}, nil
	}
	t, err := g.Store.GetTenant(ctx, tenant)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return auth.Held{}, nil
		}
		return auth.Held{}, err
	}
	ta, err := g.Auths.Get(ctx, t)
	if err != nil {
		return auth.Held{}, err
	}
	return auth.HeldBy(a.Principals, ta), nil
}

// ExtensionVerbs are the verbs a grant can give on an extension (audit is the tenant's log).
var ExtensionVerbs = []string{store.VerbInstall, store.VerbAdmin, store.VerbPublish, store.VerbPromote}

// Reaching is how a grant reaches a package (tenant, channel, extension or extension_group) and the
// verbs it gives there, in the grant's own channel scope (spec 0017); no verbs: it does not. The
// package is of packageType; a channel-scoped grant's channel is of the type it serves.
func Reaching(g store.Grant, tenant, packageType, ext string, reserved bool) (via string, verbs []string) {
	res := Resource{Tenant: tenant, Channel: g.ChannelName, Package: ext, PackageType: packageType, Reserved: reserved}
	for _, v := range ExtensionVerbs {
		if GrantCovers(g, v, res) {
			verbs = append(verbs, v)
		}
	}
	switch {
	case g.PackageGroupID != "":
		via = "extension_group"
	case g.Package != "":
		via = "extension"
	case g.ChannelID != "":
		via = "channel"
	default:
		via = "tenant"
	}
	return via, verbs
}

// GrantCovers reports whether one grant gives verb on a resource (Covers' rule, per grant).
func GrantCovers(g store.Grant, verb string, r Resource) bool {
	if verb == string(VerbDistribute) {
		return false // spec 0030: a server role's, never a grant's (admin does not imply it)
	}
	explicit := verb == store.VerbPublish || verb == store.VerbPromote
	if g.Kind == store.PrincipalIssuer && (verb == store.VerbAdmin || verb == store.VerbAudit || verb == store.VerbFeed || explicit || !slices.Contains(g.Verbs, verb)) {
		return false
	}
	if !slices.Contains(g.Verbs, verb) && (explicit || !slices.Contains(g.Verbs, store.VerbAdmin)) {
		return false
	}
	if g.PackageGroupID != "" {
		switch {
		case verb == store.VerbAudit:
			return false // the tenant's log: a tenant-wide grant only
		case r.PackageGroup != "":
			return verb == store.VerbAdmin && g.PackageGroupName == r.PackageGroup
		case r.Reserved && explicit:
			return false
		}
		// spec 0034: members are (type, name); a resource without a type is no member's
		return r.Package != "" && r.PackageType != "" && g.Members[pkgtype.Key{Type: r.PackageType, Name: r.Package}] &&
			(g.ChannelID == "" || r.Channel != "" && g.ChannelName == r.Channel)
	}
	if r.Reserved && explicit && g.Package == "" {
		return false
	}
	// spec 0034: a grant of a type reaches that type's channels and packages only, nothing
	// tenant-level; a grant that names a package, that (type, name) only; one of a type this binary
	// does not know, nothing
	switch t := g.Type(); {
	case t == pkgtype.Any && g.Package == "":
	case !pkgtype.Known(t) || r.PackageType != t || r.Channel == "" && r.Package == "":
		return false
	}
	if verb == store.VerbAudit && (g.ChannelID != "" || g.Package != "") {
		return false // the tenant's log: a tenant-wide grant only
	}
	if verb == store.VerbFeed && g.ChannelID != "" && !slices.Contains(g.Verbs, store.VerbFeed) {
		return false // spec 0011: only a tenant-wide admin implies feed; on a channel it is granted explicitly
	}
	switch {
	case g.ChannelID == "" && g.Package == "":
		return true
	case g.Package == "":
		return r.Channel != "" && g.ChannelName == r.Channel
	default:
		return r.Package == g.Package && (g.ChannelID == "" || r.Channel != "" && g.ChannelName == r.Channel)
	}
}
