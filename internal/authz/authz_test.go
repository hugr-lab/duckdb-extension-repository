package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

func TestCovers(t *testing.T) {
	me := auth.Principals{{IssuerID: "i1", Kind: store.PrincipalSubject, Value: "me"}: true, {IssuerID: "i1", Kind: store.PrincipalIssuer}: true}
	g := func(kind, value, channel, ext string, verbs ...string) store.Grant {
		ch := ""
		if channel != "" {
			ch = "id-" + channel
		}
		return store.Grant{IssuerID: "i1", Kind: kind, Value: value, ChannelID: ch, ChannelName: channel, Extension: ext, Verbs: verbs}
	}
	tenant := authz.Resource{Tenant: "acme"}
	prod := authz.Resource{Tenant: "acme", Channel: "prod"}
	tresor := authz.Resource{Tenant: "acme", Channel: "prod", Extension: "tresor"}
	for _, tc := range []struct {
		name  string
		grant store.Grant
		verb  string
		r     authz.Resource
		want  bool
	}{
		{"tenant admin on the tenant", g("subject", "me", "", "", "admin"), "admin", tenant, true},
		{"tenant admin on an extension", g("subject", "me", "", "", "admin"), "admin", tresor, true},
		{"channel admin on the tenant", g("subject", "me", "prod", "", "admin"), "admin", tenant, false},
		{"channel admin on its channel", g("subject", "me", "prod", "", "admin"), "admin", prod, true},
		{"channel admin on another channel", g("subject", "me", "staging", "", "admin"), "admin", prod, false},
		{"extension admin on its channel", g("subject", "me", "", "tresor", "admin"), "admin", prod, false},
		{"extension admin on its extension", g("subject", "me", "", "tresor", "admin"), "admin", tresor, true},
		{"extension admin in another channel", g("subject", "me", "staging", "tresor", "admin"), "admin", tresor, false},
		{"install is not admin", g("subject", "me", "", "", "install"), "admin", tenant, false},
		{"admin implies install", g("subject", "me", "", "", "admin"), "install", tresor, true},
		{"issuer-wide admin is ignored", g("issuer", "", "", "", "admin"), "admin", tenant, false},
		{"issuer-wide admin does not imply install", g("issuer", "", "", "", "admin"), "install", tresor, false},
		{"issuer-wide install", g("issuer", "", "", "", "install"), "install", tresor, true},
		{"another principal", g("subject", "you", "", "", "admin"), "admin", tenant, false},
		// spec 0011: feed, in the channels a grant covers; admin implies it, never issuer-wide or by an extension's grant
		{"feed on its channel", g("subject", "me", "prod", "", "feed"), "feed", prod, true},
		{"feed on another channel", g("subject", "me", "staging", "", "feed"), "feed", prod, false},
		{"tenant feed in a channel", g("subject", "me", "", "", "feed"), "feed", prod, true},
		{"feed is not install", g("subject", "me", "", "", "feed"), "install", tresor, false},
		{"tenant admin implies feed", g("subject", "me", "", "", "admin"), "feed", prod, true},
		{"channel admin does not imply feed", g("subject", "me", "prod", "", "admin"), "feed", prod, false},
		{"extension admin does not feed", g("subject", "me", "", "tresor", "admin"), "feed", prod, false},
		{"issuer-wide feed is ignored", g("issuer", "", "", "", "feed"), "feed", prod, false},
	} {
		if got := authz.Covers(auth.HeldBy(me, store.TenantAuth{Grants: []store.Grant{tc.grant}}), tc.verb, tc.r); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	// a server actor is allowed everything, a principal actor nothing outside its tenant, others nothing
	var gz authz.Grants
	ctx := context.Background()
	if err := gz.Allow(ctx, authz.Actor{Kind: authz.ActorServer}, authz.VerbAdmin, authz.Server); err != nil {
		t.Error(err)
	}
	if err := gz.Allow(ctx, authz.Actor{Kind: authz.ActorPrincipal, Tenant: "acme"}, authz.VerbAdmin, authz.Server); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("a principal on the server: %v", err)
	}
	if err := gz.Allow(ctx, authz.Actor{Kind: authz.ActorPrincipal, Tenant: "acme"}, authz.VerbAdmin, authz.Resource{Tenant: "other"}); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("a principal in another tenant: %v", err)
	}
	if err := gz.Allow(ctx, authz.Actor{Kind: authz.ActorSystem}, authz.VerbAdmin, tenant); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("a system actor: %v", err)
	}
}

// A grant on an extension group (spec 0017) covers its members as grants on each would, never the
// tenant or a channel; never a reserved name for publish or promote; never audit; and reads the
// group with admin.
func TestCoversGroups(t *testing.T) {
	me := auth.Principals{{IssuerID: "i1", Kind: store.PrincipalSubject, Value: "me"}: true, {IssuerID: "i1", Kind: store.PrincipalIssuer}: true}
	members := map[string]bool{"tresor": true, "httpfs": true}
	g := func(kind, channel string, verbs ...string) store.Grant {
		ch := ""
		if channel != "" {
			ch = "id-" + channel
		}
		value := "me"
		if kind == store.PrincipalIssuer {
			value = ""
		}
		return store.Grant{IssuerID: "i1", Kind: kind, Value: value, ChannelID: ch, ChannelName: channel, ExtensionGroupID: "g1",
			ExtensionGroupName: "platform", Members: members, Verbs: verbs}
	}
	tenant := authz.Resource{Tenant: "acme"}
	prod := authz.Resource{Tenant: "acme", Channel: "prod"}
	tresor := authz.Resource{Tenant: "acme", Channel: "prod", Extension: "tresor"}
	tresorStaging := authz.Resource{Tenant: "acme", Channel: "staging", Extension: "tresor"}
	acl := authz.Resource{Tenant: "acme", Channel: "prod", Extension: "acl"}
	httpfs := authz.Resource{Tenant: "acme", Channel: "prod", Extension: "httpfs", Reserved: true}
	group := authz.Resource{Tenant: "acme", ExtensionGroup: "platform"}
	for _, tc := range []struct {
		name  string
		grant store.Grant
		verb  string
		r     authz.Resource
		want  bool
	}{
		{"admin on a member", g("subject", "", "admin"), "admin", tresor, true},
		{"admin implies install on a member", g("subject", "", "admin"), "install", tresor, true},
		{"not on another name", g("subject", "", "admin"), "admin", acl, false},
		{"never the tenant", g("subject", "", "admin"), "admin", tenant, false},
		{"never a channel", g("subject", "", "admin"), "admin", prod, false},
		{"in its channel", g("subject", "prod", "install"), "install", tresor, true},
		{"not in another channel", g("subject", "prod", "install"), "install", tresorStaging, false},
		{"publish on a member", g("subject", "", "publish"), "publish", tresor, true},
		{"publish never reaches a reserved member", g("subject", "", "publish"), "publish", httpfs, false},
		{"promote never reaches a reserved member", g("subject", "", "promote"), "promote", httpfs, false},
		{"admin on a reserved member", g("subject", "", "admin"), "admin", httpfs, true},
		{"admin does not imply publish", g("subject", "", "admin"), "publish", tresor, false},
		{"audit never", g("subject", "", "audit"), "audit", tenant, false},
		{"reads the group with admin", g("subject", "prod", "admin"), "admin", group, true},
		{"install does not read the group", g("subject", "", "install"), "admin", group, false},
		{"issuer-wide install on a member", g("issuer", "", "install"), "install", tresor, true},
		{"issuer-wide admin is ignored", g("issuer", "", "admin"), "admin", tresor, false},
		{"issuer-wide admin does not read the group", g("issuer", "", "admin"), "admin", group, false},
	} {
		h := auth.HeldBy(me, store.TenantAuth{GroupGrants: []store.Grant{tc.grant}})
		if got := authz.Covers(h, tc.verb, tc.r); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	// a tenant-wide admin reads any group; a channel's or an extension's does not
	for _, tc := range []struct {
		grant store.Grant
		want  bool
	}{
		{store.Grant{IssuerID: "i1", Kind: "subject", Value: "me", Verbs: []string{"admin"}}, true},
		{store.Grant{IssuerID: "i1", Kind: "subject", Value: "me", ChannelID: "id-prod", ChannelName: "prod", Verbs: []string{"admin"}}, false},
		{store.Grant{IssuerID: "i1", Kind: "subject", Value: "me", Extension: "tresor", Verbs: []string{"admin"}}, false},
	} {
		if got := authz.Covers(auth.HeldBy(me, store.TenantAuth{Grants: []store.Grant{tc.grant}}), "admin", group); got != tc.want {
			t.Errorf("%+v on the group: %v", tc.grant, got)
		}
	}
	// an empty group is readable by its administrator, and covers no name
	empty := g("subject", "", "admin")
	empty.Members = map[string]bool{}
	if h := auth.HeldBy(me, store.TenantAuth{GroupGrants: []store.Grant{empty}}); !authz.Covers(h, "admin", group) || authz.Covers(h, "admin", tresor) {
		t.Error("an empty group")
	}
	// a removed member is no longer covered
	h := auth.HeldBy(me, store.TenantAuth{GroupGrants: []store.Grant{g("subject", "", "admin")}})
	delete(members, "tresor")
	if authz.Covers(h, "admin", tresor) {
		t.Error("a removed member is covered")
	}
}

// Spec 0020 phase 4a: the CLI's OS actors and the configuration's bootstrap are server
// administrators; no other system actor is, nor a principal spelled like the bootstrap.
func TestServerAdmin(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		a    authz.Actor
		want bool
	}{
		{authz.Actor{Kind: authz.ActorOS, ID: "0:root"}, true},
		{authz.Bootstrap, true},
		{authz.Actor{Kind: authz.ActorSystem, ID: "upstream:acme/core"}, false},
		{authz.Actor{Kind: authz.ActorPrincipal, ID: "bootstrap"}, false},
		{authz.Actor{Kind: authz.ActorServer, ID: "bootstrap"}, false},
	} {
		if err := (authz.ServerAdmin{}).Allow(ctx, c.a, authz.VerbAdmin, authz.Server); (err == nil) != c.want {
			t.Errorf("%s: %v", c.a, err)
		}
	}
}
