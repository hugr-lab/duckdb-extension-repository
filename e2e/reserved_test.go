package e2e

import (
	"context"
	"slices"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0030: the version is registered with the core extensions its sources name, legacy_parser
// (new in v2.0.0) among them; a name reserved in the tenant, and one a version brings, are refused
// to a publish grant that does not name them, and published under one that does.
func TestReservedNames(t *testing.T) {
	b := needBuild(t)
	t.Cleanup(reserved.SetForTest(reserved.Seed(), 0)) // the process's view follows this kista's store
	k := startKista(t, b)
	ctx := context.Background()
	l, err := k.st.ListReservedNames(ctx, "legacy_parser")
	if err != nil || len(l) != 1 || !slices.Contains(l[0].Sources, "version:"+b.versionDir) {
		t.Fatalf("legacy_parser: %+v %v", l, err)
	}
	ten := &tenants.Service{Store: k.st, Authz: authz.ServerAdmin{}}
	key := k.publisherKey(t, "wide-ci", false) // publish on prod, no extension named
	// kista_demo, reserved in the tenant for a holder (spec 0030 part 2), then released
	if _, err := ten.ReserveInTenant(ctx, serveAdmin, "acme", "kista_demo", "enterest"); err != nil {
		t.Fatal(err)
	}
	if st := k.publish(t, "kista_demo", key); st != 404 {
		t.Fatalf("a tenant-reserved name under a wide grant: %d", st)
	}
	adm := &tenants.AuthAdmin{Store: k.st, Authz: authz.ServerAdmin{}}
	named, err := adm.AddGrant(ctx, serveAdmin, "acme", "publisher:wide-ci", []string{"publish"},
		tenants.GrantOn{Channel: "prod", Extension: "kista_demo"})
	if err != nil {
		t.Fatal(err)
	}
	if st := k.publish(t, "kista_demo", key); st != 201 {
		t.Fatalf("a tenant-reserved name under a grant naming it: %d", st)
	}
	if err := adm.RemoveGrant(ctx, serveAdmin, "acme", named.ID); err != nil {
		t.Fatal(err)
	}
	if err := ten.ReleaseInTenant(ctx, serveAdmin, "acme", "kista_demo"); err != nil {
		t.Fatal(err)
	}
	// kista_demo, as a later version's core extension (forced: acme has its release now)
	if added, err := ten.AddVersionCore(ctx, serveAdmin, b.versionDir, tenants.CoreExtensions{Names: []string{"kista_demo"}, Force: true}); err != nil ||
		!slices.Equal(added, []string{"kista_demo"}) {
		t.Fatalf("kista_demo reserved: %v %v", added, err)
	}
	if st := k.publish(t, "kista_demo", key); st != 404 {
		t.Fatalf("a core name under a wide grant: %d", st)
	}
	if _, err := adm.AddGrant(ctx, serveAdmin, "acme", "publisher:wide-ci", []string{"publish"},
		tenants.GrantOn{Channel: "prod", Extension: "kista_demo"}); err != nil {
		t.Fatal(err)
	}
	if st := k.publish(t, "kista_demo", key); st != 200 { // the same body, released above
		t.Fatalf("a core name under a grant naming it: %d", st)
	}
}
