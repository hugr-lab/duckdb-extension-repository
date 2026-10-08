package store_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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

func TestAPIKeyRecords(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		p := store.Publisher{TenantID: tn.ID, Name: "ext-ci", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &p) })
		hash := strings.Repeat("ab", 32)
		k := store.APIKey{PublisherID: p.ID, Prefix: "0a1b2c3d", Hash: hash, ExpiresAt: time.Now().Add(24 * time.Hour), CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertAPIKey(ctx, &k) })
		dup := k
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertAPIKey(ctx, &dup) }); !errors.Is(err, store.ErrExists) {
			t.Fatalf("the same hash: %v", err)
		}
		got, gp, err := s.APIKeyByHash(ctx, hash)
		if err != nil || got.ID != k.ID || gp.Name != "ext-ci" || gp.TenantID != tn.ID || !got.LastUsedAt.IsZero() {
			t.Fatalf("by hash: %+v %+v %v", got, gp, err)
		}
		if err := s.TouchAPIKey(ctx, k.ID); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := s.APIKeyByHash(ctx, hash); got.LastUsedAt.IsZero() {
			t.Fatal("last used not recorded")
		}
		if _, _, err := s.APIKeyByHash(ctx, strings.Repeat("cd", 32)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("an unknown hash: %v", err)
		}
		// another publisher's key id is not found under this one
		q := store.Publisher{TenantID: tn.ID, Name: "other-ci", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &q) })
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.DeleteAPIKey(ctx, q.ID, k.ID) }); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a key under another publisher: %v", err)
		}
		// the cap counts unexpired keys; expired ones are removed when a key is added
		mk := func(i int, exp time.Time) error {
			n := store.APIKey{PublisherID: q.ID, Prefix: "0000000" + string(rune('0'+i%10)), Hash: fmt.Sprintf("%064x", 1000+i), ExpiresAt: exp, CreatedBy: "t"}
			return s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertAPIKey(ctx, &n) })
		}
		for i := range store.MaxAPIKeys {
			if err := mk(i, time.Now().Add(-time.Minute*time.Duration(i+1)).Add(time.Hour*time.Duration(i%2))); err != nil {
				t.Fatal(err)
			}
		}
		if err := mk(20, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("a key beside expired ones: %v", err)
		}
		if ks, _ := s.APIKeys(ctx, q.ID); len(ks) != store.MaxAPIKeys/2+1 {
			t.Fatalf("expired keys kept: %d", len(ks))
		}
		for i := 30; i < 30+store.MaxAPIKeys/2-1; i++ {
			if err := mk(i, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		if err := mk(40, time.Now().Add(time.Hour)); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("an eleventh key: %v", err)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.DeletePublisher(ctx, tn.ID, "ext-ci") })
		if _, _, err := s.APIKeyByHash(ctx, hash); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a removed publisher's key: %v", err)
		}
	})
}
