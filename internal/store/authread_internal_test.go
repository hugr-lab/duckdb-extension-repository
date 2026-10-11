package store

import (
	"context"
	"path/filepath"
	"testing"
)

// A version that moves during every read sends GetTenantAuth to the locked read, which a writer
// cannot interleave with.
func TestTenantAuthLockedRead(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tn := Tenant{Name: "acme"}
	if err := s.InTx(ctx, "", func(tx *Tx) error { return tx.CreateTenant(ctx, &tn) }); err != nil {
		t.Fatal(err)
	}
	reads := 0
	afterAuthRead = func() {
		reads++
		if _, err := s.db.ExecContext(ctx, "UPDATE tenants SET auth_version = auth_version + 1 WHERE id = ?", tn.ID); err != nil {
			t.Error(err)
		}
	}
	defer func() { afterAuthRead = nil }()
	ta, err := s.GetTenantAuth(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := s.GetTenant(ctx, "acme")
	if reads != 3 || ta.Version != cur.AuthVersion {
		t.Fatalf("%d unlocked reads; version %d, the tenant's %d", reads, ta.Version, cur.AuthVersion)
	}
}
