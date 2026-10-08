package tenants_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

func TestMain(m *testing.M) {
	code := m.Run()
	storetest.Cleanup()
	os.Exit(code)
}

func TestTenantsService(t *testing.T) {
	ctx := context.Background()
	admin := authz.Actor{Kind: authz.ActorOS, ID: "1:t"}
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			svc := &tenants.Service{Store: e.Open(t), Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}
			if _, err := svc.CreateTenant(ctx, admin, "acme", "", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.CreateTenant(ctx, admin, "Bad Name", "", ""); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("bad name: %v", err)
			}
			if _, err := svc.CreateTenant(ctx, admin, "acme-cn", "", "cn"); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("a storage domain that is not configured: %v", err)
			}
			if tn, err := svc.CreateTenant(ctx, admin, "acme-eu", "", "default"); err != nil || tn.StorageDomain != "default" {
				t.Fatalf("explicit domain: %+v %v", tn, err)
			}
			if _, err := svc.CreateChannel(ctx, admin, "acme", "prod", "mirror"); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("bad kind: %v", err)
			}
			if _, err := svc.CreateChannel(ctx, admin, "nope", "prod", store.ChannelSigned); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("missing tenant: %v", err)
			}
			if _, err := svc.CreateChannel(ctx, admin, "acme", "prod", store.ChannelSigned); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.AddVersion(ctx, admin, "v2.0.0", "nightly", nil); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("bad version kind: %v", err)
			}
			for _, bad := range [][]string{{"1.2.0"}, {"v1.2.0", "v1.3.0"}} {
				if _, err := svc.AddVersion(ctx, admin, "v2.0.0", "release", bad); !errors.Is(err, store.ErrInvalid) {
					t.Fatalf("bad C APIs %v: %v", bad, err)
				}
			}
			if _, err := svc.AddVersion(ctx, admin, "v2.0.0", "release", []string{"v1.5.6"}); err != nil {
				t.Fatal(err)
			}
			v, err := svc.AddVersionCAPI(ctx, admin, "v2.0.0", "v2.0.0")
			if err != nil || len(v.CAPIs) != 2 {
				t.Fatalf("add a major: %+v %v", v, err)
			}
			if _, err := svc.AddVersionCAPI(ctx, admin, "v2.0.0", "v1.9.9"); !errors.Is(err, store.ErrExists) {
				t.Fatalf("change a major: %v", err)
			}
			if vs, err := svc.ListVersions(ctx); err != nil || len(vs) != 1 || len(vs[0].CAPIs) != 2 {
				t.Fatalf("list versions: %+v %v", vs, err)
			}
			// a version from before migration 0003 keeps its legacy maximum when a major is added
			st := svc.Store
			if err := st.InTx(ctx, "", func(tx *store.Tx) error {
				return tx.AddDuckDBVersion(ctx, &store.DuckDBVersion{Name: "eb0d9df48e", Kind: "dev", CAPIVersion: "v1.5.6"})
			}); err != nil {
				t.Fatal(err)
			}
			v, err = svc.AddVersionCAPI(ctx, admin, "eb0d9df48e", "v2.0.0")
			if err != nil || len(v.CAPIs) != 2 || v.CAPIs[0] != (store.CAPI{Major: 1, Minor: 5, Patch: 6}) {
				t.Fatalf("legacy version plus a major: %+v %v", v, err)
			}
			if _, err := svc.SetChannelVersions(ctx, admin, "acme", "prod", []string{"v9.9.9"}, nil); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("unknown version: %v", err)
			}
			vs, err := svc.SetChannelVersions(ctx, admin, "acme", "prod", []string{"v2.0.0"}, nil)
			if err != nil || strings.Join(vs, ",") != "v2.0.0" {
				t.Fatalf("add version: %v %v", vs, err)
			}
			if _, err := svc.SetChannelVersions(ctx, admin, "acme", "prod", []string{"v2.0.0"}, nil); !errors.Is(err, store.ErrExists) {
				t.Fatalf("duplicate version: %v", err)
			}
			if vs, err := svc.SetChannelVersions(ctx, admin, "acme", "prod", nil, []string{"v2.0.0"}); err != nil || len(vs) != 0 {
				t.Fatalf("remove: %v %v", vs, err)
			}
			tn, err := svc.SetTenantState(ctx, admin, "acme", store.TenantSuspended, 0)
			if err != nil || tn.State != store.TenantSuspended {
				t.Fatalf("suspend: %+v %v", tn, err)
			}
			if _, err := svc.SetTenantState(ctx, admin, "acme", "deleted", 0); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("bad state: %v", err)
			}
			if _, err := svc.ListTenants(ctx, authz.Actor{Kind: authz.ActorPrincipal, ID: "x"}); !errors.Is(err, authz.ErrDenied) {
				t.Fatalf("principal: %v", err)
			}
		})
	}
}
