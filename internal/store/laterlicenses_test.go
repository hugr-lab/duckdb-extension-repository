package store_test

import (
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0037: the licences that make a licence older. A later iat from the same issuer for the same
// holder, verified by the same key, counts, in any entry of the tenant; an equal iat does not, nor
// another issuer, another key, a holder that differs only by a trailing space (compared byte for
// byte: SQL Server ignores trailing spaces), another package type, name or tenant. Whether a later
// licence still counts (its grace, a refused key) is the service's: the licences' API test covers it
// (TestDistributorsLicences).
func TestLaterLicenses(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		unregister := pkgtype.RegisterForTest(testType)
		defer func() { unregister() }()
		s := e.Open(t)
		acme, other := store.Tenant{Name: "acme"}, store.Tenant{Name: "other"}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			if err := tx.CreateTenant(ctx, &acme); err != nil {
				return err
			}
			return tx.CreateTenant(ctx, &other)
		}); err != nil {
			t.Fatal(err)
		}
		const iss, holder, key = "https://issuer.example", "org-1", "kid"
		put := func(tenantID, packageType, entry, name, issuer, holderID, kid string, iat int64) {
			t.Helper()
			l := testLicense(tenantID, name, holderID)
			l.PackageType, l.Entry, l.Issuer, l.Key, l.IssuedAt, l.LicenseID = packageType, store.PublisherSource(entry), issuer, kid, iat, entry
			if err := putLicense(s, l); err != nil {
				t.Fatal(err)
			}
		}
		put(acme.ID, pkgtype.Default, "ci", "tresor", iss, holder, key, 10)
		put(acme.ID, pkgtype.Default, "enterest", "tresor", iss, holder, key, 20)
		put(acme.ID, pkgtype.Default, "other-issuer", "tresor", "https://other.example", holder, key, 30)
		put(acme.ID, pkgtype.Default, "spaced", "tresor", iss, holder+" ", key, 40)
		put(acme.ID, pkgtype.Default, "other-key", "tresor", iss, holder, "other-kid", 45)
		put(acme.ID, pkgtype.Default, "ci", "acl", iss, holder, key, 50)
		put(acme.ID, testType, "typed", "tresor", iss, holder, key, 55)
		put(other.ID, pkgtype.Default, "ci", "tresor", iss, holder, key, 60)
		later := func(iat int64) []string {
			t.Helper()
			var out []string
			if err := s.InTx(ctx, "", func(tx *store.Tx) error {
				ls, err := tx.LaterLicenses(ctx, acme.ID, pkgtype.Default, "tresor", iss, holder, key, iat)
				for _, l := range ls {
					out = append(out, l.LicenseID)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return out
		}
		if got := later(10); len(got) != 1 || got[0] != "enterest" {
			t.Fatalf("later than 10: %v", got)
		}
		if got := later(20); len(got) != 0 {
			t.Fatalf("an equal iat: %v", got)
		}
		if got := later(5); len(got) != 2 || got[0] != "ci" || got[1] != "enterest" {
			t.Fatalf("later than 5, by entry: %v", got)
		}
	})
}
