package e2e

import (
	"context"
	"slices"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0030: the version is registered with the core extensions its sources name, legacy_parser
// (new in v2.0.0) among them; a name a version brings is refused to a publish grant that does not
// name it, and published under one that does (spec 0037: tenant reservations are gone).
func TestReservedNames(t *testing.T) {
	b := needBuild(t)
	t.Cleanup(reserved.SetForTest(reserved.Seed(), 0)) // the process's view follows this kista's store
	k := startKista(t, b)
	ctx := context.Background()
	l, err := k.st.ListReservedNames(ctx, pkgtype.DuckDBExtension, "legacy_parser")
	if err != nil || len(l) != 1 || !slices.Contains(l[0].Sources, "version:"+b.versionDir) {
		t.Fatalf("legacy_parser: %+v %v", l, err)
	}
	ten := &tenants.Service{Store: k.st, Authz: authz.ServerAdmin{}}
	key := k.publisherKey(t, "wide-ci", false) // publish on prod, no extension named
	// kista_demo, free: published under the wide grant
	if st := k.publish(t, "kista_demo", key); st != 201 {
		t.Fatalf("a free name under a wide grant: %d", st)
	}
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}}
	// kista_demo, as a later version's core extension (forced: acme has its release now)
	if added, err := ten.AddVersionCore(ctx, serveAdmin, b.versionDir, tenants.CoreExtensions{Names: []string{"kista_demo"}, Force: true}); err != nil ||
		!slices.Equal(added, []string{"kista_demo"}) {
		t.Fatalf("kista_demo reserved: %v %v", added, err)
	}
	if st := k.publish(t, "kista_demo", key); st != 404 {
		t.Fatalf("a core name under a wide grant: %d", st)
	}
	if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "publisher:wide-ci", []string{"publish"},
		tenants.GrantOn{Channel: "prod", Package: "kista_demo"}); err != nil {
		t.Fatal(err)
	}
	if st := k.publish(t, "kista_demo", key); st != 200 { // the same body, released above
		t.Fatalf("a core name under a grant naming it: %d", st)
	}
}
