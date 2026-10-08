package store_test

import (
	"errors"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

func TestPublishers(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		p := store.Publisher{TenantID: tn.ID, Name: "acl-ci", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &p) })
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			return tx.InsertPublisher(ctx, &store.Publisher{TenantID: tn.ID, Name: "acl-ci"})
		}); !errors.Is(err, store.ErrExists) {
			t.Fatalf("the same name: %v", err)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			return tx.InsertPublisher(ctx, &store.Publisher{TenantID: tn.ID, Name: "Bad Name"})
		}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a bad name: %v", err)
		}
		c := store.GitHubCredential{PublisherID: p.ID, Provider: "github", OwnerID: "123", RepositoryID: "456",
			Workflow: "hugr-lab/duckdb-acl/.github/workflows/release.yml", Ref: "refs/tags/v*", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertGitHubCredential(ctx, tn.ID, &c) })
		same := c
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertGitHubCredential(ctx, tn.ID, &same) }); !errors.Is(err, store.ErrExists) {
			t.Fatalf("the same credential: %v", err)
		}
		bare := store.Publisher{TenantID: tn.ID, Name: "bare", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &bare) })
		for name, bad := range map[string]store.GitHubCredential{
			"owner name":  {PublisherID: p.ID, Provider: "github", OwnerID: "hugr-lab", RepositoryID: "456", Workflow: "a/b/c.yml"},
			"with a ref":  {PublisherID: p.ID, Provider: "github", OwnerID: "1", RepositoryID: "2", Workflow: "a/b/c.yml@refs/heads/main"},
			"no workflow": {PublisherID: p.ID, Provider: "github", OwnerID: "1", RepositoryID: "2"},
			"not a path":  {PublisherID: p.ID, Provider: "github", OwnerID: "1", RepositoryID: "2", Workflow: "release.yml"},
			"no provider": {PublisherID: p.ID, OwnerID: "1", RepositoryID: "2", Workflow: "a/b/c.yml"},
		} {
			b := bad
			if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertGitHubCredential(ctx, tn.ID, &b) }); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// a publisher grant: no issuer, publish and promote only
		g := store.Grant{TenantID: tn.ID, Kind: store.PrincipalPublisher, PublisherID: p.ID, ChannelID: ch.ID,
			Extension: "acl", Verbs: []string{"publish"}, CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertGrant(ctx, &g) })
		for name, bad := range map[string]store.Grant{
			"install":        {TenantID: tn.ID, Kind: store.PrincipalPublisher, PublisherID: p.ID, Verbs: []string{"install"}},
			"admin":          {TenantID: tn.ID, Kind: store.PrincipalPublisher, PublisherID: p.ID, Verbs: []string{"admin"}},
			"no publisher":   {TenantID: tn.ID, Kind: store.PrincipalPublisher, Verbs: []string{"publish"}},
			"subject with p": {TenantID: tn.ID, Kind: store.PrincipalSubject, Value: "x", PublisherID: p.ID, Verbs: []string{"publish"}},
		} {
			b := bad
			if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertGrant(ctx, &b) }); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		dup := g
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertGrant(ctx, &dup) }); !errors.Is(err, store.ErrExists) {
			t.Fatalf("the same publisher grant: %v", err)
		}
		ta, err := s.GetTenantAuth(ctx, tn.ID)
		if err != nil || len(ta.Publishers) != 2 || len(ta.Publishers[0].GitHub) != 1 || ta.Publishers[0].GitHub[0].Ref != "refs/tags/v*" ||
			ta.Publishers[1].Name != "bare" || len(ta.Publishers[1].GitHub) != 0 {
			t.Fatalf("tenant auth publishers: %+v %v", ta.Publishers, err)
		}
		var pg *store.Grant
		for i := range ta.Grants {
			if ta.Grants[i].Kind == store.PrincipalPublisher {
				pg = &ta.Grants[i]
			}
		}
		if pg == nil || pg.Principal() != "publisher:acl-ci" || pg.Value != p.ID || pg.IssuerID != "" || pg.ChannelName == "" {
			t.Fatalf("the publisher grant read back: %+v", pg)
		}
		// removing the publisher removes its credentials and grants
		inTx(t, s, func(tx *store.Tx) error { return tx.DeletePublisher(ctx, tn.ID, "acl-ci") })
		ta, err = s.GetTenantAuth(ctx, tn.ID)
		if err != nil || len(ta.Publishers) != 1 {
			t.Fatalf("after removal: %+v %v", ta, err)
		}
		for _, g := range ta.Grants {
			if g.Kind == store.PrincipalPublisher {
				t.Fatal("a removed publisher's grant remains")
			}
		}
	})
}
