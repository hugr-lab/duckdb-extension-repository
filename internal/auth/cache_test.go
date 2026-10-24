package auth_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// One cached entry per tenant (spec 0017): served at the caller's version or later, replaced only by
// a later one, so a caller holding an older tenant row neither re-reads nor rolls the entry back.
func TestTenantAuthsCache(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tn := store.Tenant{Name: "acme"}
	g := store.PackageGroup{Name: "platform", CreatedBy: "os:1:t"}
	if err := st.InTx(ctx, "", func(tx *store.Tx) error {
		if err := tx.CreateTenant(ctx, &tn); err != nil {
			return err
		}
		g.TenantID = tn.ID
		return tx.InsertPackageGroup(ctx, &g)
	}); err != nil {
		t.Fatal(err)
	}
	old, _ := st.GetTenant(ctx, "acme")
	c := &auth.TenantAuths{Store: st}
	first, err := c.Get(ctx, old)
	if err != nil || first.Version != old.AuthVersion || len(first.Groups) != 1 {
		t.Fatalf("first read %+v %v", first, err)
	}
	if err := st.InTx(ctx, "", func(tx *store.Tx) error {
		_, _, err := tx.ChangeGroupMembers(ctx, g, []pkgtype.Key{pkgtype.Of("acl")}, nil, "os:1:t")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ta, _ := c.Get(ctx, old); ta.Version != old.AuthVersion {
		t.Fatalf("an older row re-read: %d", ta.Version)
	}
	cur, _ := st.GetTenant(ctx, "acme")
	ta, err := c.Get(ctx, cur)
	if err != nil || ta.Version != cur.AuthVersion || !ta.Groups[g.ID].Members[pkgtype.Of("acl")] {
		t.Fatalf("a newer row %+v %v", ta, err)
	}
	if ta, _ := c.Get(ctx, old); ta.Version != cur.AuthVersion {
		t.Fatalf("an older row rolled the entry back: %d", ta.Version)
	}
}
