// Package authz holds the actor of a request and the authorization hook every service calls
// (spec 0003). The CLI's server administrator is allowed everything; over the API (spec 0007)
// server administrators are allowed everything and tenant principals act through their grants.
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
)

// Resource is what an action is on: the server (no tenant), a tenant, a channel, or an extension
// in a channel (or in every channel). Reserved marks an extension name DuckDB owns (spec 0008):
// only grants that name it reach it.
type Resource struct {
	Tenant, Channel, Extension string
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
// everything, and nobody else anything.
type ServerAdmin struct{}

// Allow implements Authorizer.
func (ServerAdmin) Allow(_ context.Context, a Actor, _ Verb, _ Resource) error {
	if a.Kind == ActorOS {
		return nil
	}
	return ErrDenied
}

// Grants is the API's authorizer: OS and server actors (a server administrator; the API makes
// server actors of administrators only) are allowed everything; a principal actor what its grants
// in its own tenant allow.
type Grants struct {
	Store *store.Store
	Auths *auth.TenantAuths
}

// Allow implements Authorizer.
func (g Grants) Allow(ctx context.Context, a Actor, verb Verb, r Resource) error {
	switch a.Kind {
	case ActorOS, ActorServer:
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
	if Covers(a.Principals, ta.Grants, string(verb), r) {
		return nil
	}
	return ErrDenied
}

// Covers reports whether principals hold verb on a resource: a grant without a channel or extension
// covers the tenant and everything in it, one on a channel that channel and everything in it, one on
// an extension that extension's releases, in its channel or every channel (channels are compared
// by name: a tenant's are unique and never renamed). admin implies every verb but publish and
// promote (spec 0008), except on an issuer-wide grant, which never carries admin's rights. For
// publish and promote, a reserved name is reached only by a grant naming it.
func Covers(p auth.Principals, grants []store.Grant, verb string, r Resource) bool {
	explicit := verb == store.VerbPublish || verb == store.VerbPromote
	for _, g := range grants {
		if !p[auth.Key{IssuerID: g.IssuerID, Kind: g.Kind, Value: g.Value}] {
			continue
		}
		if g.Kind == store.PrincipalIssuer && (verb == store.VerbAdmin || explicit || !slices.Contains(g.Verbs, verb)) {
			continue
		}
		if !slices.Contains(g.Verbs, verb) && (explicit || !slices.Contains(g.Verbs, store.VerbAdmin)) {
			continue
		}
		if r.Reserved && explicit && g.Extension == "" {
			continue
		}
		switch {
		case g.ChannelID == "" && g.Extension == "":
			return true
		case g.Extension == "":
			if r.Channel != "" && g.ChannelName == r.Channel {
				return true
			}
		default:
			if r.Extension == g.Extension && (g.ChannelID == "" || r.Channel != "" && g.ChannelName == r.Channel) {
				return true
			}
		}
	}
	return false
}
