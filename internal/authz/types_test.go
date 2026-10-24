package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

const otherType = "test_package"

// Spec 0034: a grant's package type. A grant that names a package covers that (type, name) only; a
// typed one without a name that type's channels and packages, nothing tenant-level; "*" what it
// covered before; members are (type, name); a grant of a type this binary does not know nothing.
func TestCoversTypes(t *testing.T) {
	defer pkgtype.RegisterForTest(otherType)()
	me := auth.Principals{{IssuerID: "i1", Kind: store.PrincipalSubject, Value: "me"}: true}
	g := func(typ, channel, ext string, verbs ...string) store.Grant {
		ch := ""
		if channel != "" {
			ch = "id-" + channel
		}
		return store.Grant{IssuerID: "i1", Kind: store.PrincipalSubject, Value: "me", ChannelID: ch, ChannelName: channel, ChannelType: chType(channel),
			Package: ext, PackageType: typ, Verbs: verbs}
	}
	res := func(typ, channel, ext string) authz.Resource {
		return authz.Resource{Tenant: "acme", Channel: channel, Package: ext, PackageType: typ}
	}
	ext, drv := pkgtype.DuckDBExtension, otherType
	for _, tc := range []struct {
		name  string
		grant store.Grant
		verb  string
		r     authz.Resource
		want  bool
	}{
		{"named on its type", g(ext, "", "snowflake", "install"), "install", res(ext, "prod", "snowflake"), true},
		{"named on another type", g(ext, "", "snowflake", "install"), "install", res(drv, "drivers", "snowflake"), false},
		{"named of the other type", g(drv, "", "snowflake", "install"), "install", res(drv, "drivers", "snowflake"), true},
		{"named, a resource without a type", g(ext, "", "snowflake", "install"), "install", res("", "prod", "snowflake"), false},
		{"typed on its channel", g(drv, "", "", "admin"), "admin", res(drv, "drivers", ""), true},
		{"typed on its package", g(drv, "", "", "install"), "install", res(drv, "", "snowflake"), true},
		{"typed on another type's channel", g(drv, "", "", "admin"), "admin", res(ext, "prod", ""), false},
		{"typed on another type's package", g(drv, "", "", "install"), "install", res(ext, "prod", "snowflake"), false},
		{"typed on the tenant", g(drv, "", "", "admin"), "admin", authz.Resource{Tenant: "acme"}, false},
		{"typed does not read the log", g(drv, "", "", "admin"), "audit", authz.Resource{Tenant: "acme"}, false},
		{"typed on a group", g(drv, "", "", "admin"), "admin", authz.Resource{Tenant: "acme", PackageGroup: "platform"}, false},
		{"any type on the tenant", g(pkgtype.Any, "", "", "admin"), "admin", authz.Resource{Tenant: "acme"}, true},
		{"any type on each type", g(pkgtype.Any, "", "", "install"), "install", res(drv, "drivers", "snowflake"), true},
		{"the shape's default: no name, any type", g("", "", "", "install"), "install", res(drv, "drivers", "snowflake"), true},
		{"the shape's default: a name, DuckDB's", g("", "", "snowflake", "install"), "install", res(drv, "drivers", "snowflake"), false},
		{"a channel's grant", g(drv, "drivers", "", "install"), "install", res(drv, "drivers", "snowflake"), true},
		{"an unknown type's grant", g("nope", "", "", "admin"), "admin", res("nope", "x", ""), false},
		{"named any type", g(pkgtype.Any, "", "snowflake", "install"), "install", res(ext, "prod", "snowflake"), false},
		// a channel's grant reaches its channel's type only, whatever its column holds
		{"any type on its channel", g(pkgtype.Any, "prod", "", "install"), "install", res(ext, "prod", "snowflake"), true},
		{"any type on its channel, another type asked", g(pkgtype.Any, "prod", "", "install"), "install", res(drv, "prod", "snowflake"), false},
		{"named on its channel, another type asked", g(drv, "prod", "snowflake", "install"), "install", res(drv, "prod", "snowflake"), false},
		{"a channel whose type was not read", func() store.Grant { x := g(pkgtype.Any, "prod", "", "install"); x.ChannelType = ""; return x }(),
			"install", res(ext, "prod", "snowflake"), false},
		// audit and feed: the tenant's, of every type
		{"typed audit", g(drv, "", "", "audit"), "audit", authz.Resource{Tenant: "acme"}, false},
		{"any type audit", g(pkgtype.Any, "", "", "audit"), "audit", authz.Resource{Tenant: "acme"}, true},
		{"typed admin implies no feed", g(drv, "", "", "admin"), "feed", res(drv, "drivers", ""), false},
		{"typed feed granted", g(drv, "", "", "feed"), "feed", res(drv, "drivers", ""), true},
		{"any type admin implies feed", g(pkgtype.Any, "", "", "admin"), "feed", res(drv, "drivers", ""), true},
	} {
		if got := authz.Covers(auth.HeldBy(me, store.TenantAuth{Grants: []store.Grant{tc.grant}}), tc.verb, tc.r); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	// a group's members are (type, name)
	gg := store.Grant{IssuerID: "i1", Kind: store.PrincipalSubject, Value: "me", PackageGroupID: "g1", PackageGroupName: "warehouse",
		Members: map[pkgtype.Key]bool{{Type: drv, Name: "snowflake"}: true}, Verbs: []string{"install"}}
	h := auth.HeldBy(me, store.TenantAuth{GroupGrants: []store.Grant{gg}})
	if !authz.Covers(h, "install", res(drv, "drivers", "snowflake")) || authz.Covers(h, "install", res(ext, "prod", "snowflake")) ||
		authz.Covers(h, "install", res("", "prod", "snowflake")) {
		t.Error("a group's members by type")
	}
	if via, verbs := authz.Reaching(g(drv, "", "", "install"), "acme", ext, "snowflake", false); len(verbs) != 0 {
		t.Errorf("a typed grant reaches another type's package: %s %v", via, verbs)
	}
	if via, verbs := authz.Reaching(g(drv, "", "", "install"), "acme", drv, "snowflake", false); via != "tenant" || len(verbs) != 1 {
		t.Errorf("a typed grant on its type's package: %s %v", via, verbs)
	}
	// a channel's grant of every type reaches only packages of its channel's type
	if via, verbs := authz.Reaching(g(pkgtype.Any, "prod", "", "install"), "acme", drv, "snowflake", false); len(verbs) != 0 {
		t.Errorf("a channel's grant reaches another type's package: %s %v", via, verbs)
	}
	if via, verbs := authz.Reaching(g(pkgtype.Any, "prod", "", "install"), "acme", ext, "snowflake", false); via != "channel" || len(verbs) != 1 {
		t.Errorf("a channel's grant on its type's package: %s %v", via, verbs)
	}
	cg := gg
	cg.ChannelID, cg.ChannelName, cg.ChannelType = "id-prod", "prod", ext
	if via, verbs := authz.Reaching(cg, "acme", drv, "snowflake", false); len(verbs) != 0 {
		t.Errorf("a channel's group grant reaches another type's member: %s %v", via, verbs)
	}
}

// Spec 0034: the API's authorizer reads a channel's type, and a caller's type that is not the
// channel's is covered by no typed grant; a type it does not know is refused.
func TestAllowTypes(t *testing.T) {
	defer pkgtype.RegisterForTest(otherType)()
	ctx := context.Background()
	cli := authz.Actor{Kind: authz.ActorOS, ID: "1:t"}
	st := storetest.Engines(t)[0].Open(t)
	ten := &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}
	if _, err := ten.CreateTenant(ctx, cli, "acme", "", "", ""); err != nil {
		t.Fatal(err)
	}
	for name, typ := range map[string]string{"prod": "", "drivers": otherType} {
		if _, err := ten.CreateChannel(ctx, cli, "acme", name, store.ChannelSigned, typ); err != nil {
			t.Fatal(err)
		}
	}
	tn, err := st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	is := store.Issuer{TenantID: tn.ID, Name: "corp", URL: "https://idp.example", CreatedBy: "os:1:t"}
	if err := st.InTx(ctx, "", func(tx *store.Tx) error {
		if err := tx.InsertIssuer(ctx, &is); err != nil {
			return err
		}
		gr := store.Grant{TenantID: tn.ID, IssuerID: is.ID, Kind: store.PrincipalRole, Value: "drivers", PackageType: otherType,
			Verbs: []string{"admin"}, CreatedBy: "os:1:t"}
		return tx.InsertGrant(ctx, &gr)
	}); err != nil {
		t.Fatal(err)
	}
	gz := authz.Grants{Store: st, Auths: &auth.TenantAuths{Store: st}}
	a := authz.Actor{Kind: authz.ActorPrincipal, ID: "acme/x", Tenant: "acme",
		Principals: auth.Principals{{IssuerID: is.ID, Kind: store.PrincipalRole, Value: "drivers"}: true}}
	for _, c := range []struct {
		r    authz.Resource
		want bool
	}{
		{authz.Resource{Tenant: "acme", Channel: "drivers"}, true},
		{authz.Resource{Tenant: "acme", Channel: "drivers", Package: "snowflake"}, true},
		{authz.Resource{Tenant: "acme", Channel: "drivers", PackageType: otherType}, true},
		{authz.Resource{Tenant: "acme", Channel: "drivers", PackageType: pkgtype.DuckDBExtension}, false},
		{authz.Resource{Tenant: "acme", Channel: "prod"}, false},
		{authz.Resource{Tenant: "acme", Channel: "nope"}, false},
		{authz.Resource{Tenant: "acme", Package: "snowflake", PackageType: otherType}, true},
		{authz.Resource{Tenant: "acme", Package: "snowflake"}, false}, // the default type
		{authz.Resource{Tenant: "acme", Package: "snowflake", PackageType: "nope"}, false},
		{authz.Resource{Tenant: "acme"}, false},
	} {
		if err := gz.Allow(ctx, a, authz.VerbAdmin, c.r); (err == nil) != c.want || err != nil && !errors.Is(err, authz.ErrDenied) {
			t.Errorf("%+v: %v", c.r, err)
		}
	}
}
