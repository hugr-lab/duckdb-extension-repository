package store_test

import (
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0037: the licences that make a licence older. A later iat from the same issuer for the same
// holder counts, in any entry of the tenant; an equal iat does not, nor another issuer, nor a holder
// that differs only by a trailing space (compared byte for byte: SQL Server ignores trailing spaces),
// nor another package type, name or tenant.
func TestLaterLicenses(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
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
		const iss, holder = "https://issuer.example", "org-1"
		put := func(tenantID, entry, name, issuer, holderID string, iat int64) {
			t.Helper()
			l := testLicense(tenantID, name, holderID)
			l.Entry, l.Issuer, l.IssuedAt, l.LicenseID = store.PublisherSource(entry), issuer, iat, entry
			if err := putLicense(s, l); err != nil {
				t.Fatal(err)
			}
		}
		put(acme.ID, "ci", "tresor", iss, holder, 10)
		put(acme.ID, "enterest", "tresor", iss, holder, 20)
		put(acme.ID, "other-issuer", "tresor", "https://other.example", holder, 30)
		put(acme.ID, "spaced", "tresor", iss, holder+" ", 40)
		put(acme.ID, "ci", "acl", iss, holder, 50)
		put(other.ID, "ci", "tresor", iss, holder, 60)
		later := func(iat int64) []string {
			t.Helper()
			var out []string
			if err := s.InTx(ctx, "", func(tx *store.Tx) error {
				ls, err := tx.LaterLicenses(ctx, acme.ID, pkgtype.Default, "tresor", iss, holder, iat)
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
