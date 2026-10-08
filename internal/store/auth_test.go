package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

func TestAuthRecords(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		other := store.Tenant{Name: "other"}
		otherCh := store.Channel{Name: "prod", Kind: store.ChannelSigned}
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.CreateTenant(ctx, &other); err != nil {
				return err
			}
			otherCh.TenantID = other.ID
			return tx.CreateChannel(ctx, &otherCh)
		})
		is := store.Issuer{TenantID: tn.ID, Name: "corp", URL: "https://idp.example", Algorithms: []string{"RS256", "ES256"},
			RequiredClaims: map[string]string{"tid": "t1"}, RolesClaim: []string{"https://x/roles"}, MaxTokenLifetime: 2 * time.Hour, CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertIssuer(ctx, &is) })
		for name, bad := range map[string]store.Issuer{
			"same name": {TenantID: tn.ID, Name: "corp", URL: "https://b.example"},
			"same url":  {TenantID: tn.ID, Name: "corp2", URL: "https://idp.example"},
		} {
			b := bad
			if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertIssuer(ctx, &b) }); !errors.Is(err, store.ErrExists) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			return tx.InsertIssuer(ctx, &store.Issuer{TenantID: tn.ID, Name: "Corp", URL: "https://c.example"})
		}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("bad name: %v", err)
		}
		// at most 16 records per tenant
		for i := 1; i < store.MaxIssuers; i++ {
			x := store.Issuer{TenantID: tn.ID, Name: "x" + string(rune('a'+i)), URL: "https://x" + string(rune('a'+i)) + ".example"}
			inTx(t, s, func(tx *store.Tx) error { return tx.InsertIssuer(ctx, &x) })
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			return tx.InsertIssuer(ctx, &store.Issuer{TenantID: tn.ID, Name: "one-more", URL: "https://more.example"})
		}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a 17th issuer: %v", err)
		}
		for i := 1; i < store.MaxIssuers; i++ {
			name := "x" + string(rune('a'+i))
			inTx(t, s, func(tx *store.Tx) error { return tx.DeleteIssuer(ctx, tn.ID, name) })
		}
		// the same issuer URL in another tenant is another record
		ois := store.Issuer{TenantID: other.ID, Name: "corp", URL: "https://idp.example"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertIssuer(ctx, &ois) })

		inTx(t, s, func(tx *store.Tx) error { return tx.AddAudience(ctx, tn.ID, "api://kista-acme", "os:1:t") })
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.AddAudience(ctx, other.ID, "api://kista-acme", "os:1:t") }); !errors.Is(err, store.ErrExists) {
			t.Fatalf("an audience in two tenants: %v", err)
		}
		g := store.Grant{TenantID: tn.ID, IssuerID: is.ID, Kind: store.PrincipalSubject, Value: "alice", ChannelID: ch.ID,
			Extension: "tresor", Verbs: []string{"install"}, CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertGrant(ctx, &g) })
		for name, bad := range map[string]store.Grant{
			"another tenant's issuer":  {TenantID: tn.ID, IssuerID: ois.ID, Kind: "subject", Value: "x", Verbs: []string{"install"}},
			"another tenant's channel": {TenantID: tn.ID, IssuerID: is.ID, Kind: "subject", Value: "x", ChannelID: otherCh.ID, Verbs: []string{"install"}},
			"bad kind":                 {TenantID: tn.ID, IssuerID: is.ID, Kind: "server", Value: "x", Verbs: []string{"install"}},
			"bad verb":                 {TenantID: tn.ID, IssuerID: is.ID, Kind: "subject", Value: "x", Verbs: []string{"publish"}},
			"issuer with a value":      {TenantID: tn.ID, IssuerID: is.ID, Kind: "issuer", Value: "x", Verbs: []string{"install"}},
		} {
			b := bad
			if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertGrant(ctx, &b) }); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		ta, err := s.GetTenantAuth(ctx, tn.ID)
		if err != nil || len(ta.Issuers) != 1 || ta.Issuers[0].RequiredClaims["tid"] != "t1" || ta.Issuers[0].RolesClaim[0] != "https://x/roles" ||
			ta.Issuers[0].MaxTokenLifetime != 2*time.Hour || len(ta.Audiences) != 1 || len(ta.Grants) != 1 ||
			ta.Grants[0].Principal() != "subject:corp|alice" || ta.Grants[0].ChannelName != "prod" {
			t.Fatalf("tenant auth: %+v %v", ta, err)
		}
		before, _ := s.GetTenant(ctx, "acme")
		inTx(t, s, func(tx *store.Tx) error { return tx.DeleteIssuer(ctx, tn.ID, "corp") })
		after, _ := s.GetTenant(ctx, "acme")
		if ta, _ := s.GetTenantAuth(ctx, tn.ID); len(ta.Grants) != 0 || len(ta.Issuers) != 0 {
			t.Fatalf("after removing the issuer: %+v", ta)
		}
		if after.AuthVersion <= before.AuthVersion {
			t.Fatal("auth_version not bumped")
		}
	})
}
