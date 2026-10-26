package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// testLicense is a licence row of a holder, as a verified import would store it.
func testLicense(tenantID, name, holder string) store.License {
	return store.License{TenantID: tenantID, Name: name, Entry: store.PublisherSource("ci"), LicenseID: "lic-" + name, Issuer: "https://issuer.example", HolderID: holder,
		Holder: "Holder", Plan: "pro", Token: "t." + name, Key: "kid", IssuedAt: 1, ExpiresAt: 4102444800, GraceUntil: 4103049600,
		Source: store.LicenseImport, Visibility: store.Private, Actor: "os:1:test"}
}

func putLicense(s *store.Store, l store.License) error {
	return s.InTenantTx(ctx, l.TenantID, []string{store.LicenseLock(l.TenantID)}, func(tx *store.Tx) error {
		return tx.PutLicense(ctx, &l, 0)
	})
}

// Spec 0028: a tenant's expected holder, its rule, and the one place licences are stored refusing
// another holder's.
func TestTenantHolder(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn := store.Tenant{Name: "acme", HolderID: "org-1"}
		// migration 0029 raises the minimum reader to 29: an older binary would not check the holder
		if n, err := store.CountRaw(ctx, s, "schema_migrations", "version = 29 AND min_reader = 29"); err != nil || n != 1 {
			t.Fatalf("0029's min_reader: %d %v", n, err)
		}
		for _, bad := range []string{strings.Repeat("a", 129), "\xff", "org\x001", "\x00"} {
			x := store.Tenant{Name: "bad", HolderID: bad}
			if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.CreateTenant(ctx, &x) }); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("holder %q: %v", bad, err)
			}
		}
		other := store.Tenant{Name: "other"}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			if err := tx.CreateTenant(ctx, &tn); err != nil {
				return err
			}
			return tx.CreateTenant(ctx, &other)
		}); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetTenant(ctx, "acme"); err != nil || got.HolderID != "org-1" {
			t.Fatalf("the column: %+v %v", got, err)
		}
		if got, err := s.GetTenant(ctx, "other"); err != nil || got.HolderID != "" {
			t.Fatalf("no holder: %+v %v", got, err)
		}
		long := strings.Repeat("é", 64) // 128 bytes
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.CreateTenant(ctx, &store.Tenant{Name: "long", HolderID: long}) }); err != nil {
			t.Fatalf("a holder of 128 bytes: %v", err)
		}
		if got, err := s.GetTenant(ctx, "long"); err != nil || got.HolderID != long {
			t.Fatalf("a holder of 128 bytes read back: %q %v", got.HolderID, err)
		}
		// a control character other than U+0000 stays, as a licence's sub may carry it escaped
		ctl := "org\x01\t1"
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.CreateTenant(ctx, &store.Tenant{Name: "ctl", HolderID: ctl}) }); err != nil {
			t.Fatalf("a holder with a control character: %v", err)
		}
		if got, err := s.GetTenant(ctx, "ctl"); err != nil || got.HolderID != ctl {
			t.Fatalf("a holder with a control character read back: %q %v", got.HolderID, err)
		}

		// another holder's licence is refused, and nothing is written; the holder's is stored
		err := putLicense(s, testLicense(tn.ID, "tresor", "org-2"))
		var hm *store.HolderMismatch
		if !errors.Is(err, store.ErrHolderMismatch) || !errors.As(err, &hm) || hm.Expected != "org-1" || hm.Given != "org-2" {
			t.Fatalf("another holder's licence: %v", err)
		}
		if _, err := nameLicense(ctx, s, tn.ID, pkgtype.DuckDBExtension, "tresor"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a refused licence was stored: %v", err)
		}
		// the comparison is exact: no case folding, no trimming
		for _, near := range []string{"ORG-1", "org-1 ", "org-"} {
			if err := putLicense(s, testLicense(tn.ID, "tresor", near)); !errors.Is(err, store.ErrHolderMismatch) {
				t.Fatalf("holder %q: %v", near, err)
			}
		}
		if err := putLicense(s, testLicense(tn.ID, "tresor", "org-1")); err != nil {
			t.Fatal(err)
		}
		// a tenant with no holder takes any
		if err := putLicense(s, testLicense(other.ID, "tresor", "org-2")); err != nil {
			t.Fatal(err)
		}

		// the licences of other holders than one
		err = s.InTxLocks(ctx, nil, func(tx *store.Tx) error {
			ls, err := tx.OtherHolderLicenses(ctx, tn.ID, "org-3")
			if err != nil || len(ls) != 1 || ls[0].Name != "tresor" || ls[0].HolderID != "org-1" {
				t.Errorf("other holders than org-3: %+v %v", ls, err)
			}
			if ls, err := tx.OtherHolderLicenses(ctx, tn.ID, "org-1"); err != nil || len(ls) != 0 {
				t.Errorf("other holders than org-1: %+v %v", ls, err)
			}
			// exact, on every engine (SQL Server's = ignores trailing spaces)
			if ls, err := tx.OtherHolderLicenses(ctx, tn.ID, "org-1 "); err != nil || len(ls) != 1 {
				t.Errorf("other holders than %q: %+v %v", "org-1 ", ls, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		// set and clear, compare-and-set on the tenant's version
		set := func(tn *store.Tenant, holder string) error {
			return s.InTxLocks(ctx, []string{store.LicenseLock(tn.ID)}, func(tx *store.Tx) error { return tx.SetTenantHolder(ctx, tn, holder) })
		}
		stale := tn
		if err := set(&tn, ""); err != nil || tn.HolderID != "" || tn.Version != 2 {
			t.Fatalf("clear: %+v %v", tn, err)
		}
		if err := set(&stale, "org-9"); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a stale version: %v", err)
		}
		if got, _ := s.GetTenant(ctx, "acme"); got.HolderID != "" || got.Version != 2 {
			t.Fatalf("cleared: %+v", got)
		}
		if err := putLicense(s, testLicense(tn.ID, "httpfs", "org-2")); err != nil {
			t.Fatalf("no holder after the clear: %v", err)
		}
		for _, bad := range []string{strings.Repeat("a", 129), "org\x00"} {
			if err := set(&tn, bad); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("holder %q: %v", bad, err)
			}
		}
		if err := set(&tn, "org-2"); err != nil || tn.HolderID != "org-2" || tn.Version != 3 {
			t.Fatalf("set: %+v %v", tn, err)
		}
		if got, _ := s.GetTenant(ctx, "acme"); got.HolderID != "org-2" {
			t.Fatalf("set, read back: %+v", got)
		}
		// a replacement is a store too
		l, err := nameLicense(ctx, s, tn.ID, pkgtype.DuckDBExtension, "httpfs")
		if err != nil {
			t.Fatal(err)
		}
		l.HolderID = "org-1"
		if err := s.InTenantTx(ctx, tn.ID, []string{store.LicenseLock(tn.ID)}, func(tx *store.Tx) error {
			return tx.PutLicense(ctx, &l, l.Version)
		}); !errors.Is(err, store.ErrHolderMismatch) {
			t.Fatalf("a replacement of another holder: %v", err)
		}
	})
}
