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

// Resource is what an action is on: the server (no tenant), a tenant, a channel, an extension in a
// channel (or in every channel), or an extension group (spec 0017: reading it). Reserved marks an
// extension name DuckDB owns (spec 0008): for publish and promote, only grants that name it reach
// it.
type Resource struct {
	Tenant, Channel, Extension string
	ExtensionGroup             string
	Reserved                   bool
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
		if verb != VerbDistribute || r.Channel != "" || r.Extension != "" || r.ExtensionGroup != "" {
			return ErrDenied
		}
		if r.Tenant == "" {
			return nil // the server's reserved names
		}
		return activeTenant(ctx, g.Store, r.Tenant)
	case ActorLicenseManager:
		if verb != VerbLicense || r.Tenant == "" || r.Channel != "" || r.Extension != "" || r.ExtensionGroup != "" {
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
	if Covers(auth.HeldBy(a.Principals, ta), string(verb), r) {
		return nil
	}
	return ErrDenied
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

// Reaching is how a grant reaches an extension (tenant, channel, extension or extension_group) and
// the verbs it gives there, in the grant's own channel scope (spec 0017); no verbs: it does not.
func Reaching(g store.Grant, tenant, ext string, reserved bool) (via string, verbs []string) {
	res := Resource{Tenant: tenant, Channel: g.ChannelName, Extension: ext, Reserved: reserved}
	for _, v := range ExtensionVerbs {
		if GrantCovers(g, v, res) {
			verbs = append(verbs, v)
		}
	}
	switch {
	case g.ExtensionGroupID != "":
		via = "extension_group"
	case g.Extension != "":
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
	if g.ExtensionGroupID != "" {
		switch {
		case verb == store.VerbAudit:
			return false // the tenant's log: a tenant-wide grant only
		case r.ExtensionGroup != "":
			return verb == store.VerbAdmin && g.ExtensionGroupName == r.ExtensionGroup
		case r.Reserved && explicit:
			return false
		}
		return r.Extension != "" && g.Members[r.Extension] && (g.ChannelID == "" || r.Channel != "" && g.ChannelName == r.Channel)
	}
	if r.Reserved && explicit && g.Extension == "" {
		return false
	}
	if verb == store.VerbAudit && (g.ChannelID != "" || g.Extension != "") {
		return false // the tenant's log: a tenant-wide grant only
	}
	if verb == store.VerbFeed && g.ChannelID != "" && !slices.Contains(g.Verbs, store.VerbFeed) {
		return false // spec 0011: only a tenant-wide admin implies feed; on a channel it is granted explicitly
	}
	switch {
	case g.ChannelID == "" && g.Extension == "":
		return true
	case g.Extension == "":
		return r.Channel != "" && g.ChannelName == r.Channel
	default:
		return r.Extension == g.Extension && (g.ChannelID == "" || r.Channel != "" && g.ChannelName == r.Channel)
	}
}
