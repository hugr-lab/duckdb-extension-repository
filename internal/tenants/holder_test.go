package tenants_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// tenantOnly allows what is a tenant's and nothing server-wide: a tenant administrator.
type tenantOnly struct{}

func (tenantOnly) Allow(_ context.Context, _ authz.Actor, _ authz.Verb, r authz.Resource) error {
	if r == authz.Server {
		return authz.ErrDenied
	}
	return nil
}

func storeLicense(ctx context.Context, st *store.Store, tenantID, name, holder string) error {
	l := store.License{TenantID: tenantID, Name: name, LicenseID: "lic-" + name, Issuer: "https://issuer.example", HolderID: holder,
		Holder: "Holder", Plan: "pro", Token: "t." + name, Key: "kid", IssuedAt: 1, ExpiresAt: 4102444800, GraceUntil: 4103049600,
		Source: store.LicenseImport, Visibility: store.Private, Actor: "os:1:test"}
	return st.InTenantTx(ctx, tenantID, []string{store.LicenseLock(tenantID)}, func(tx *store.Tx) error { return tx.PutLicense(ctx, &l, 0) })
}

// Spec 0028: the expected holder, set by server administrators only, never over another holder's
// licences, and ordered with the licence writes.
func TestTenantHolder(t *testing.T) {
	ctx := context.Background()
	admin := authz.Actor{Kind: authz.ActorOS, ID: "1:t"}
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			st := e.Open(t)
			svc := &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}
			if _, err := svc.CreateTenant(ctx, admin, "bad", "", "", strings.Repeat("a", 129)); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("a holder of 129 bytes: %v", err)
			}
			tn, err := svc.CreateTenant(ctx, admin, "acme", "", "", "org-1")
			if err != nil || tn.HolderID != "org-1" {
				t.Fatalf("create: %+v %v", tn, err)
			}
			if n, err := store.CountRaw(ctx, st, "events", "kind = ? AND data LIKE ?", "tenant.create", `%"holder_id":"org-1"%`); err != nil || n != 1 {
				t.Fatalf("tenant.create with the holder: %d %v", n, err)
			}

			// a tenant administrator reads it, and may not change or clear it
			ta := &tenants.Service{Store: st, Authz: tenantOnly{}, HasDomain: svc.HasDomain}
			if got, err := ta.GetTenant(ctx, admin, "acme"); err != nil || got.HolderID != "org-1" {
				t.Fatalf("a tenant administrator's read: %+v %v", got, err)
			}
			for _, h := range []string{"", "org-2"} {
				if _, err := ta.SetTenantHolder(ctx, admin, "acme", h, 0); !errors.Is(err, authz.ErrDenied) {
					t.Fatalf("a tenant administrator sets %q: %v", h, err)
				}
			}

			// the version, and the rule
			if _, err := svc.SetTenantHolder(ctx, admin, "acme", "org-2", tn.Version+1); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("a stale version: %v", err)
			}
			if _, err := svc.SetTenantHolder(ctx, admin, "acme", strings.Repeat("a", 129), 0); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("a holder of 129 bytes: %v", err)
			}
			if _, err := svc.SetTenantHolder(ctx, admin, "nope", "org-1", 0); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("no tenant: %v", err)
			}

			// clear, then a licence of another holder is stored; setting it again is refused, naming it
			if tn, err = svc.SetTenantHolder(ctx, admin, "acme", "", tn.Version); err != nil || tn.HolderID != "" {
				t.Fatalf("clear: %+v %v", tn, err)
			}
			if err := storeLicense(ctx, st, tn.ID, "tresor", "org-2"); err != nil {
				t.Fatal(err)
			}
			if err := storeLicense(ctx, st, tn.ID, "httpfs", "org-1"); err != nil {
				t.Fatal(err)
			}
			_, err = svc.SetTenantHolder(ctx, admin, "acme", "org-1", 0)
			var rf *tenants.Refusal
			if !errors.As(err, &rf) || !strings.Contains(rf.Reason, "tresor (lic-tresor, holder org-2)") || strings.Contains(rf.Reason, "httpfs") {
				t.Fatalf("over another holder's licence: %v", err)
			}
			if got, _ := st.GetTenant(ctx, "acme"); got.HolderID != "" || got.Version != tn.Version {
				t.Fatalf("a refused set changed the tenant: %+v", got)
			}
			// the holder of every licence stored is taken
			if err := st.InTenantTx(ctx, tn.ID, []string{store.LicenseLock(tn.ID)}, func(tx *store.Tx) error {
				l, err := tx.GetLicense(ctx, tn.ID, "tresor")
				if err != nil {
					return err
				}
				return tx.DeleteLicense(ctx, l, 0)
			}); err != nil {
				t.Fatal(err)
			}
			if tn, err = svc.SetTenantHolder(ctx, admin, "acme", "org-1", 0); err != nil || tn.HolderID != "org-1" {
				t.Fatalf("set: %+v %v", tn, err)
			}
			v := tn.Version
			if tn, err = svc.SetTenantHolder(ctx, admin, "acme", "org-1", 0); err != nil || tn.Version != v {
				t.Fatalf("set again: %+v %v", tn, err)
			}
			for _, ev := range []string{`%"new":""%"old":"org-1"%`, `%"new":"org-1"%"old":""%`} {
				if n, err := store.CountRaw(ctx, st, "events", "kind = ? AND tenant_id = ? AND data LIKE ?", "tenant.holder", tn.ID, ev); err != nil || n != 1 {
					t.Errorf("tenant.holder %s: %d %v", ev, n, err)
				}
			}
			if n, _ := store.CountRaw(ctx, st, "events", "kind = ?", "tenant.holder"); n != 2 {
				t.Errorf("tenant.holder events: %d", n)
			}

			// a licence write under way when the holder is set: the set waits for it, and sees it
			if tn, err = svc.SetTenantHolder(ctx, admin, "acme", "", 0); err != nil {
				t.Fatal(err)
			}
			holding, release := make(chan struct{}), make(chan struct{})
			wrote := make(chan error, 1)
			go func() {
				first := true
				wrote <- st.InTenantTx(ctx, tn.ID, []string{store.LicenseLock(tn.ID)}, func(tx *store.Tx) error {
					l := store.License{TenantID: tn.ID, Name: "late", LicenseID: "lic-late", Issuer: "https://issuer.example", HolderID: "org-3",
						Holder: "Holder", Plan: "pro", Token: "t.late", Key: "kid", IssuedAt: 1, ExpiresAt: 4102444800, GraceUntil: 4103049600,
						Source: store.LicenseImport, Visibility: store.Private, Actor: "os:1:test"}
					if err := tx.PutLicense(ctx, &l, 0); err != nil {
						return err
					}
					if first {
						first = false
						close(holding)
						<-release
					}
					return nil
				})
			}()
			<-holding
			set := make(chan error, 1)
			go func() { _, err := svc.SetTenantHolder(ctx, admin, "acme", "org-1", 0); set <- err }()
			select {
			case err := <-set:
				t.Fatalf("setting the holder did not wait for the licence write: %v", err)
			case <-time.After(300 * time.Millisecond):
			}
			close(release)
			if err := <-wrote; err != nil {
				t.Fatal(err)
			}
			if err := <-set; !errors.As(err, &rf) || !strings.Contains(rf.Reason, "late") {
				t.Fatalf("the holder set over a licence written meanwhile: %v", err)
			}
		})
	}
}
