package upstream_test

import (
	"errors"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0040: Read and ReadAll take audit on the tenant (a tenant-wide admin implies it), never a
// distribution admin's distribute; Get and List, which the closed routes (an upstream's sub-routes)
// and the writes use, and the writes keep admin.
func TestUpstreamReaders(t *testing.T) {
	osAdmin := authz.Actor{Kind: authz.ActorOS, ID: "1:t"}
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			st := e.Open(t)
			ten := &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}
			tn, err := ten.CreateTenant(ctx, osAdmin, "acme", "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			is := store.Issuer{TenantID: tn.ID, Name: "corp", URL: "https://idp.example", JWKSURI: "https://idp.example/jwks",
				Algorithms: []string{"RS256"}, CreatedBy: osAdmin.String()}
			if err := st.InTx(ctx, "", func(tx *store.Tx) error {
				if err := tx.InsertIssuer(ctx, &is); err != nil {
					return err
				}
				for sub, verb := range map[string]string{"aud": store.VerbAudit, "tom": store.VerbAdmin} {
					g := store.Grant{TenantID: tn.ID, IssuerID: is.ID, Kind: store.PrincipalSubject, Value: sub, PackageType: pkgtype.Any,
						Verbs: []string{verb}, CreatedBy: osAdmin.String()}
					if err := tx.InsertGrant(ctx, &g); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			principal := func(sub string) authz.Actor {
				return authz.Actor{Kind: authz.ActorPrincipal, ID: "acme/" + is.ID + "|" + sub, Tenant: "acme",
					Principals: auth.Principals{{IssuerID: is.ID, Kind: store.PrincipalSubject, Value: sub}: true}}
			}
			svc := &upstream.Service{Store: st, Authz: authz.Grants{Store: st, Auths: &auth.TenantAuths{Store: st}}}
			dist := authz.Actor{Kind: authz.ActorDistributionAdmin, ID: "ops|distributor"}
			for who, c := range map[string]struct {
				a    authz.Actor
				want error
			}{
				"auditor":            {principal("aud"), nil},
				"tenant admin":       {principal("tom"), nil},
				"server":             {authz.Actor{Kind: authz.ActorServer, ID: "ops|root"}, nil},
				"nobody":             {principal("nobody"), authz.ErrDenied},
				"distribution admin": {dist, authz.ErrDenied},
			} {
				if _, err := svc.ReadAll(ctx, c.a, "acme"); !errors.Is(err, c.want) {
					t.Errorf("%s lists: %v", who, err)
				}
				want := c.want
				if want == nil {
					want = store.ErrNotFound // allowed: the upstream is missing
				}
				if _, err := svc.Read(ctx, c.a, "acme", "core"); !errors.Is(err, want) {
					t.Errorf("%s reads one: %v", who, err)
				}
			}
			// the administrators' paths refuse an auditor: List, Get (an upstream's sub-routes: its
			// entries, platforms, keys, names, usage), its revocations, its cells
			aud := principal("aud")
			closed := map[string]error{}
			_, closed["list"] = svc.List(ctx, aud, "acme")
			_, closed["get"] = svc.Get(ctx, aud, "acme", "core")
			_, closed["revocations"] = svc.RevocationsOf(ctx, aud, "acme", "core")
			_, closed["cells"] = svc.Cells(ctx, aud, "acme", "core", "", [3]string{}, 10)
			for what, err := range closed {
				if !errors.Is(err, authz.ErrDenied) {
					t.Errorf("an auditor's %s: %v", what, err)
				}
			}
			if _, err := svc.Get(ctx, principal("tom"), "acme", "core"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("an administrator's get: %v", err)
			}
			// an auditor writes nothing
			if err := svc.Remove(ctx, principal("aud"), "acme", "core", 0); !errors.Is(err, authz.ErrDenied) {
				t.Errorf("an auditor removes an upstream: %v", err)
			}
			if _, err := svc.Add(ctx, principal("aud"), "acme", upstream.Spec{Name: "core", Kind: "duckdb-core", Channel: "prod"}); !errors.Is(err, authz.ErrDenied) {
				t.Errorf("an auditor adds an upstream: %v", err)
			}
			// PutEntry checks admin before it reads the upstream or asks its remote anything
			if _, err := svc.PutEntry(ctx, principal("aud"), "acme", "core", store.UpstreamEntry{Name: "*"}); !errors.Is(err, authz.ErrDenied) {
				t.Errorf("an auditor puts an entry: %v", err)
			}
		})
	}
}
