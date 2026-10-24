package auth

import (
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0034: what a caller may install in a channel is of the channel's package type: a typed grant
// without a name installs everything in its type's channels only, a named grant and a group's
// member its (type, name).
func TestInstallScopeTypes(t *testing.T) {
	const other = "test_package"
	defer pkgtype.RegisterForTest(other)()
	p := Principals{{"i1", "role", "team"}: true}
	g := func(typ, ext string) store.Grant {
		return store.Grant{IssuerID: "i1", Kind: "role", Value: "team", Package: ext, PackageType: typ, Verbs: []string{"install"}}
	}
	for name, tc := range map[string]struct {
		grants         []store.Grant
		groups         []store.Grant
		channelType    string
		all, snowflake bool
	}{
		"typed, its type":                  {grants: []store.Grant{g(other, "")}, channelType: other, all: true, snowflake: true},
		"typed, another type":              {grants: []store.Grant{g(other, "")}, channelType: pkgtype.DuckDBExtension},
		"any type":                         {grants: []store.Grant{g(pkgtype.Any, "")}, channelType: other, all: true, snowflake: true},
		"named, its type":                  {grants: []store.Grant{g(other, "snowflake")}, channelType: other, snowflake: true},
		"named, another type":              {grants: []store.Grant{g(pkgtype.DuckDBExtension, "snowflake")}, channelType: other},
		"an unknown type's":                {grants: []store.Grant{g("nope", "")}, channelType: "nope"},
		"a member of the channel's type":   {groups: []store.Grant{group(pkgtype.Key{Type: other, Name: "snowflake"})}, channelType: other, snowflake: true},
		"a member of another type":         {groups: []store.Grant{group(pkgtype.Of("snowflake"))}, channelType: other},
		"a member of the default, default": {groups: []store.Grant{group(pkgtype.Of("snowflake"))}, channelType: pkgtype.DuckDBExtension, snowflake: true},
	} {
		h := HeldBy(p, store.TenantAuth{Grants: tc.grants, GroupGrants: tc.groups})
		all, may := InstallScope(h, "c1", tc.channelType)
		if all != tc.all || may("snowflake") != tc.snowflake || Allows(h, "c1", tc.channelType, "snowflake", "install") != tc.snowflake {
			t.Errorf("%s: all %v, snowflake %v", name, all, may("snowflake"))
		}
	}
}

func group(members ...pkgtype.Key) store.Grant {
	m := map[pkgtype.Key]bool{}
	for _, k := range members {
		m[k] = true
	}
	return store.Grant{IssuerID: "i1", Kind: "role", Value: "team", PackageGroupID: "g1", Members: m, Verbs: []string{"install"}}
}
