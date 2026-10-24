package auth

import (
	"slices"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Held is what a caller holds in a tenant: its own grants, and its grants on extension groups
// (spec 0017) with their groups' members. The evaluators take it rather than a tenant's grants, so
// none reads only the plain grants: a group grant has no extension, and read as a plain grant it
// would cover the tenant or the channel.
type Held struct {
	Grants      []store.Grant
	GroupGrants []store.Grant
}

// HeldBy narrows a tenant's grants to those of principals.
func HeldBy(p Principals, ta store.TenantAuth) Held {
	var h Held
	for _, g := range ta.Grants {
		if p[Key{g.IssuerID, g.Kind, g.Value}] {
			h.Grants = append(h.Grants, g)
		}
	}
	for _, g := range ta.GroupGrants {
		if p[Key{g.IssuerID, g.Kind, g.Value}] {
			h.GroupGrants = append(h.GroupGrants, g)
		}
	}
	return h
}

// gives reports whether a grant gives verb: by naming it, or by admin, which an issuer-wide grant
// never carries (spec 0007).
func gives(g store.Grant, verb string) bool {
	return slices.Contains(g.Verbs, verb) || slices.Contains(g.Verbs, store.VerbAdmin) && g.Kind != store.PrincipalIssuer
}

// Allows reports whether a caller holds verb (or admin) on (channel, extension): a grant on the
// tenant, on the channel, on the extension in every channel or in this one, or on a group holding
// the extension in every channel or in this one.
func Allows(h Held, channelID, extension, verb string) bool {
	for _, g := range h.Grants {
		if gives(g, verb) && (g.ChannelID == "" || g.ChannelID == channelID) && (g.Package == "" || g.Package == extension) {
			return true
		}
	}
	for _, g := range h.GroupGrants {
		if gives(g, verb) && (g.ChannelID == "" || g.ChannelID == channelID) && g.Members[extension] {
			return true
		}
	}
	return false
}

// InstallScope returns what a caller may install in a channel: every extension (a grant on the
// tenant or the channel), or the names may reports (grants on extensions or on groups, in every
// channel or in this one), looked up in the groups' member sets without copying them.
func InstallScope(h Held, channelID string) (all bool, may func(name string) bool) {
	names := map[string]bool{}
	var groups []map[string]bool
	for _, g := range h.Grants {
		if !gives(g, store.VerbInstall) || g.ChannelID != "" && g.ChannelID != channelID {
			continue
		}
		if g.Package == "" {
			return true, func(string) bool { return true }
		}
		names[g.Package] = true
	}
	for _, g := range h.GroupGrants {
		if gives(g, store.VerbInstall) && (g.ChannelID == "" || g.ChannelID == channelID) {
			groups = append(groups, g.Members)
		}
	}
	return false, func(name string) bool {
		return names[name] || slices.ContainsFunc(groups, func(m map[string]bool) bool { return m[name] })
	}
}
