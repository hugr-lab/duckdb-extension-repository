package tenants_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0040: the structure's reads take audit on the tenant (a tenant-wide admin implies it, a
// server administrator has it), never a distribution admin's distribute; the writes keep admin.
func TestStructureReaders(t *testing.T) {
	ctx := context.Background()
	osAdmin := authz.Actor{Kind: authz.ActorOS, ID: "1:t"}
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			st := e.Open(t)
			svc := &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}
			tn, err := svc.CreateTenant(ctx, osAdmin, "acme", "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			is := store.Issuer{TenantID: tn.ID, Name: "corp", URL: "https://idp.example", JWKSURI: "https://idp.example/jwks",
				Algorithms: []string{"RS256"}, CreatedBy: osAdmin.String()}
			grants := map[string]string{"aud": store.VerbAudit, "tom": store.VerbAdmin, "ivan": store.VerbInstall}
			if err := st.InTx(ctx, "", func(tx *store.Tx) error {
				if err := tx.InsertIssuer(ctx, &is); err != nil {
					return err
				}
				for sub, verb := range grants {
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
			gz := authz.Grants{Store: st, Auths: &auth.TenantAuths{Store: st}}
			adm := &tenants.AuthAdmin{Store: st, Authz: gz, PublicURL: "https://kista.example"}
			if _, err := adm.CreatePackageGroup(ctx, osAdmin, "acme", "platform", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := adm.AddPublisher(ctx, osAdmin, "acme", "own-ci"); err != nil {
				t.Fatal(err)
			}
			server := authz.Actor{Kind: authz.ActorServer, ID: "ops|root"}
			dist := authz.Actor{Kind: authz.ActorDistributionAdmin, ID: "ops|distributor"}
			reads := map[string]func(a authz.Actor) error{
				"issuers": func(a authz.Actor) error { _, err := adm.ReadIssuers(ctx, a, "acme"); return err },
				"console": func(a authz.Actor) error { _, err := adm.IssuerConsoleClients(ctx, a, "acme"); return err },
				"audiences": func(a authz.Actor) error {
					_, err := adm.ReadAudiences(ctx, a, "acme")
					return err
				},
				"grants": func(a authz.Actor) error { _, err := adm.ReadGrants(ctx, a, "acme"); return err },
				"group":  func(a authz.Actor) error { _, _, err := adm.GetPackageGroup(ctx, a, "acme", "platform"); return err },
				"members": func(a authz.Actor) error {
					_, err := adm.PackageGroupMembers(ctx, a, "acme", "platform")
					return err
				},
				"groups": func(a authz.Actor) error {
					_, full, err := adm.ListPackageGroups(ctx, a, "acme", "", "acl")
					if err == nil && !full {
						return errors.New("not the full view")
					}
					return err
				},
				"publishers": func(a authz.Actor) error { _, err := adm.ReadPublishers(ctx, a, "acme"); return err },
				"keys": func(a authz.Actor) error {
					_, err := adm.ReadTenantPublisherKeys(ctx, a, "acme")
					return err
				},
				"publisher keys": func(a authz.Actor) error {
					_, err := adm.ReadPublisherKeys(ctx, a, "acme", "own-ci")
					return err
				},
			}
			for who, a := range map[string]authz.Actor{"auditor": principal("aud"), "tenant admin": principal("tom"), "server": server} {
				for what, read := range reads {
					if err := read(a); err != nil {
						t.Errorf("%s reads %s: %v", who, what, err)
					}
				}
			}
			for who, a := range map[string]authz.Actor{"installer": principal("ivan"), "nobody": principal("nobody"), "distribution admin": dist} {
				for what, read := range reads {
					if who == "distribution admin" && (what == "publishers" || what == "keys") {
						continue
					}
					if err := read(a); !errors.Is(err, authz.ErrDenied) {
						t.Errorf("%s reads %s: %v", who, what, err)
					}
				}
			}
			// the administrators' reads, which the closed routes (the grants' principals) and the
			// writes' answers use, refuse an auditor
			aud := principal("aud")
			closed := map[string]error{}
			_, closed["issuers"] = adm.ListIssuers(ctx, aud, "acme")
			_, closed["audiences"] = adm.ListAudiences(ctx, aud, "acme")
			_, closed["grants"] = adm.ListGrants(ctx, aud, "acme")
			_, closed["publishers"] = adm.ListPublishers(ctx, aud, "acme")
			_, closed["keys"] = adm.TenantPublisherKeys(ctx, aud, "acme")
			_, closed["publisher keys"] = adm.ListPublisherKeys(ctx, aud, "acme", "own-ci")
			for what, err := range closed {
				if !errors.Is(err, authz.ErrDenied) {
					t.Errorf("an auditor's %s through the administrators' read: %v", what, err)
				}
			}
			if _, err := adm.ListPublisherKeys(ctx, principal("tom"), "acme", "own-ci"); err != nil {
				t.Errorf("an administrator's publisher keys: %v", err)
			}
			// a distribution admin: the tenant's own publisher's keys are not its to read
			if _, err := adm.ReadPublisherKeys(ctx, dist, "acme", "own-ci"); !errors.Is(err, authz.ErrDenied) {
				t.Errorf("a distribution admin reads the tenant's own publisher's keys: %v", err)
			}
			// the publishers' reads: a distribution admin's, narrowed to the distributors' (none here)
			if ps, err := adm.ReadPublishers(ctx, dist, "acme"); err != nil || len(ps) != 0 {
				t.Errorf("a distribution admin's publishers: %v %v", ps, err)
			}
			if _, err := adm.ReadPublishers(ctx, principal("ivan"), "acme"); !errors.Is(err, authz.ErrDenied) {
				t.Errorf("an installer's publishers: %v", err)
			}
			// an auditor writes nothing
			writes := map[string]error{}
			_, writes["grant"] = adm.AddGrant(ctx, aud, "acme", "subject:corp|zed", []string{"install"}, tenants.GrantOn{})
			_, writes["group"] = adm.CreatePackageGroup(ctx, aud, "acme", "other", "")
			_, _, writes["members"] = adm.ChangePackageGroupMembers(ctx, aud, "acme", "platform", "", []string{"acl"}, nil)
			_, writes["publisher"] = adm.AddPublisher(ctx, aud, "acme", "ci")
			writes["issuer"] = adm.RemoveIssuer(ctx, aud, "acme", "corp", "")
			for what, err := range writes {
				if !errors.Is(err, authz.ErrDenied) {
					t.Errorf("an auditor's %s write: %v", what, err)
				}
			}
		})
	}
}
