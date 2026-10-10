package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0015: an issuer record's console client is set, replaced, listed with its record, and goes
// with it.
func TestConsoleClients(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		a := store.Issuer{TenantID: tn.ID, Name: "corp", URL: "https://a.example", CreatedBy: "os:1:t"}
		b := store.Issuer{TenantID: tn.ID, Name: "bare", URL: "https://b.example", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.InsertIssuer(ctx, &a); err != nil {
				return err
			}
			return tx.InsertIssuer(ctx, &b)
		})
		set := func(c store.ConsoleClient) error {
			return s.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetConsoleClient(ctx, &c) })
		}
		if err := set(store.ConsoleClient{IssuerID: a.ID, ClientID: "x", Scopes: []string{"profile"}}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("scopes without openid: %v", err)
		}
		long := []string{"openid"}
		for range 19 {
			long = append(long, strings.Repeat("s", 100))
		}
		if err := set(store.ConsoleClient{IssuerID: a.ID, ClientID: "x", Scopes: long}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("scopes longer than their column: %v", err)
		}
		if err := set(store.ConsoleClient{IssuerID: a.ID, ClientID: "has space", Scopes: []string{"openid"}}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a client id with a space: %v", err)
		}
		noErr(t, set(store.ConsoleClient{IssuerID: a.ID, ClientID: "one", Scopes: []string{"openid"}, CreatedBy: "os:1:t"}))
		noErr(t, set(store.ConsoleClient{IssuerID: a.ID, ClientID: "two", Scopes: []string{"openid", "offline_access"},
			Audience: "api://acme", AudienceParameter: "audience", CreatedBy: "os:1:t"}))
		iss, ccs, err := s.ConsoleClients(ctx, tn.ID)
		if err != nil || len(iss) != 1 || iss[0].Name != "corp" || ccs[0].ClientID != "two" || len(ccs[0].Scopes) != 2 ||
			ccs[0].Audience != "api://acme" || ccs[0].AudienceParameter != "audience" {
			t.Fatalf("console clients: %+v %+v %v", iss, ccs, err)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.RemoveConsoleClient(ctx, b.ID) }); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("removing a client never set: %v", err)
		}
		// the client goes with its record: one re-added under the name starts without one
		inTx(t, s, func(tx *store.Tx) error { return tx.DeleteIssuer(ctx, tn.ID, "corp") })
		again := store.Issuer{TenantID: tn.ID, Name: "corp", URL: "https://a.example", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertIssuer(ctx, &again) })
		if iss, _, err := s.ConsoleClients(ctx, tn.ID); err != nil || len(iss) != 0 {
			t.Fatalf("a re-added record inherited a client: %+v %v", iss, err)
		}
	})
}
