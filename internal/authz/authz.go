// Package authz holds the actor of a request and the authorization hook every service calls
// (spec 0003). The CLI's server administrator is allowed everything; spec 0005 adds grants.
package authz

import (
	"context"
	"errors"
	"os"
	"os/user"
	"strconv"
)

// ActorKind says where an action comes from.
type ActorKind string

const (
	ActorOS        ActorKind = "os"        // kista admin: the OS user running the CLI
	ActorPrincipal ActorKind = "principal" // the HTTP API: issuer record and subject (spec 0005)
	ActorSystem    ActorKind = "system"    // kista itself
)

// Actor is who performs an action. Its String form is recorded with every change.
type Actor struct {
	Kind ActorKind
	ID   string
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

// Verb is what an actor wants to do.
type Verb string

const (
	VerbRead  Verb = "read"
	VerbAdmin Verb = "admin"
)

// ErrDenied is returned when an actor may not act.
var ErrDenied = errors.New("authz: not allowed")

// Authorizer decides whether an actor may perform a verb on a tenant (and channel; empty for
// tenant-wide or server-wide actions).
type Authorizer interface {
	Allow(ctx context.Context, a Actor, verb Verb, tenant, channel string) error
}

// ServerAdmin allows OS actors (the CLI, running as the service user with the service's config)
// everything, and nobody else anything.
type ServerAdmin struct{}

// Allow implements Authorizer.
func (ServerAdmin) Allow(_ context.Context, a Actor, _ Verb, _, _ string) error {
	if a.Kind == ActorOS {
		return nil
	}
	return ErrDenied
}
