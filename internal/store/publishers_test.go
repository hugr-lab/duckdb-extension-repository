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
		bare := store.Publisher{TenantID: tn.ID, Name: "bare", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &bare) })
		k := testKey(tn.ID, p.ID, 1)
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisherKey(ctx, &k) })
		// a publisher grant: no issuer, publish and promote only
		g := store.Grant{TenantID: tn.ID, Kind: store.PrincipalPublisher, PublisherID: p.ID, ChannelID: ch.ID,
			Package: "acl", Verbs: []string{"publish"}, CreatedBy: "os:1:t"}
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
		if err != nil || len(ta.Publishers) != 2 || ta.Publishers[0].Name != "acl-ci" || ta.Publishers[1].Name != "bare" {
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
		// removing the publisher removes its keys and grants
		inTx(t, s, func(tx *store.Tx) error { return tx.DeletePublisher(ctx, tn.ID, "acl-ci") })
		ta, err = s.GetTenantAuth(ctx, tn.ID)
		if err != nil || len(ta.Publishers) != 1 {
			t.Fatalf("after removal: %+v %v", ta, err)
		}
		if ks, err := s.PublisherKeys(ctx, tn.ID, ""); err != nil || len(ks) != 0 {
			t.Fatalf("a removed publisher's keys: %+v %v", ks, err)
		}
		for _, g := range ta.Grants {
			if g.Kind == store.PrincipalPublisher {
				t.Fatal("a removed publisher's grant remains")
			}
		}
	})
}

// testKey is a key record with a fingerprint of its own (the store does not parse keys).
func testKey(tenantID, publisherID string, i int) store.PublisherKey {
	fp := fmt.Sprintf("SHA256:%043d", i)
	return store.PublisherKey{TenantID: tenantID, PublisherID: publisherID, Type: "ssh-ed25519",
		PublicKey: fmt.Sprintf("ssh-ed25519 AAAA%d", i), Fingerprint: fp, Title: "ci", CreatedBy: "os:1:t"}
}

func TestPublisherKeyRecords(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		p := store.Publisher{TenantID: tn.ID, Name: "ext-ci", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &p) })
		k := testKey(tn.ID, p.ID, 1)
		k.ExpiresAt = time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisherKey(ctx, &k) })
		got, gp, err := s.PublisherKeyByFingerprint(ctx, tn.ID, k.Fingerprint)
		if err != nil || got.ID != k.ID || gp.ID != p.ID || gp.Name != "ext-ci" || gp.TenantID != tn.ID || !got.LastUsedAt.IsZero() ||
			!got.ExpiresAt.Equal(k.ExpiresAt) || got.PublicKey != k.PublicKey || got.Title != "ci" || got.Type != "ssh-ed25519" {
			t.Fatalf("by fingerprint: %+v %+v %v", got, gp, err)
		}
		if err := s.TouchPublisherKey(ctx, k.ID); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := s.PublisherKeyByFingerprint(ctx, tn.ID, k.Fingerprint); got.LastUsedAt.IsZero() {
			t.Fatal("last used not recorded")
		}
		if _, _, err := s.PublisherKeyByFingerprint(ctx, tn.ID, testKey("", "", 2).Fingerprint); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("an unknown fingerprint: %v", err)
		}
		// a fingerprint is unique in the tenant, across its publishers
		q := store.Publisher{TenantID: tn.ID, Name: "other-ci", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &q) })
		same := testKey(tn.ID, q.ID, 1)
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertPublisherKey(ctx, &same) }); !errors.Is(err, store.ErrExists) {
			t.Fatalf("the same key on another publisher: %v", err)
		}
		// but not across tenants
		other := store.Tenant{Name: "other", DisplayName: "Other"}
		inTx(t, s, func(tx *store.Tx) error { return tx.CreateTenant(ctx, &other) })
		op := store.Publisher{TenantID: other.ID, Name: "ext-ci", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisher(ctx, &op) })
		elsewhere := testKey(other.ID, op.ID, 1)
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisherKey(ctx, &elsewhere) })
		if got, _, err := s.PublisherKeyByFingerprint(ctx, tn.ID, k.Fingerprint); err != nil || got.ID != k.ID {
			t.Fatalf("the lookup crossed tenants: %+v %v", got, err)
		}
		// another publisher's key id is not found under this one
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { _, err := tx.DeletePublisherKey(ctx, q.ID, k.ID); return err }); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a key under another publisher: %v", err)
		}
		for name, bad := range map[string]store.PublisherKey{
			"fingerprint": {TenantID: tn.ID, PublisherID: q.ID, Type: "ssh-ed25519", PublicKey: "x", Fingerprint: "MD5:aa"},
			"no key":      {TenantID: tn.ID, PublisherID: q.ID, Type: "ssh-ed25519", Fingerprint: testKey("", "", 3).Fingerprint},
		} {
			b := bad
			if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertPublisherKey(ctx, &b) }); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// at most ten keys a publisher, expired ones included
		for i := range store.MaxPublisherKeys {
			n := testKey(tn.ID, q.ID, 100+i)
			if i%2 == 0 {
				n.ExpiresAt = time.Now().Add(-time.Hour)
			}
			inTx(t, s, func(tx *store.Tx) error { return tx.InsertPublisherKey(ctx, &n) })
		}
		eleventh := testKey(tn.ID, q.ID, 200)
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertPublisherKey(ctx, &eleventh) }); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("an eleventh key: %v", err)
		}
		if ks, err := s.PublisherKeys(ctx, tn.ID, q.ID); err != nil || len(ks) != store.MaxPublisherKeys {
			t.Fatalf("keys: %d %v", len(ks), err)
		}
		inTx(t, s, func(tx *store.Tx) error { _, err := tx.DeletePublisherKey(ctx, p.ID, k.ID); return err })
		if _, _, err := s.PublisherKeyByFingerprint(ctx, tn.ID, k.Fingerprint); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a removed key: %v", err)
		}
	})
}

// Migration 0028 (spec 0023) drops GitHub credentials and API keys and keeps publishers and their
// grants; it raises the minimum reader to 28, since an older binary would read the dropped tables.
func TestMigrationPublisherKeys(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Fresh(t, 1)[0]
		if err := store.MigrateTo(ctx, s, 27); err != nil {
			t.Fatal(err)
		}
		now := store.TimeArg(s, time.Now())
		const tid, pid = "t0000000-0000-0000-0000-000000000000", "p0000000-0000-0000-0000-000000000000"
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{"INSERT INTO tenants (id, name, display_name, state, created_at, version) VALUES (?, 'acme', '', 'active', ?, 1)", []any{tid, now}},
			{"INSERT INTO publishers (id, tenant_id, name, created_at, created_by) VALUES (?, ?, 'ci', ?, 'os:1:t')", []any{pid, tid, now}},
			{"INSERT INTO publisher_github (id, publisher_id, provider, owner_id, repository_id, workflow, created_at, created_by) " +
				"VALUES ('g0000000-0000-0000-0000-000000000000', ?, 'github', '1', '2', 'o/r/.github/workflows/w.yml', ?, 'os:1:t')", []any{pid, now}},
			{"INSERT INTO api_keys (id, publisher_id, prefix, hash, expires_at, created_at, created_by) " +
				"VALUES ('k0000000-0000-0000-0000-000000000000', ?, '0a1b2c3d', ?, ?, ?, 'os:1:t')", []any{pid, strings.Repeat("ab", 32), now, now}},
		} {
			if err := store.ExecRaw(ctx, s, q.sql, q.args...); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		ta, err := s.GetTenantAuth(ctx, tid)
		if err != nil || len(ta.Publishers) != 1 || ta.Publishers[0].Name != "ci" {
			t.Fatalf("publishers after 0028: %+v %v", ta.Publishers, err)
		}
		for _, table := range []string{"publisher_github", "api_keys"} {
			if _, err := store.CountRaw(ctx, s, table, "1 = 1"); err == nil {
				t.Errorf("%s is still there", table)
			}
		}
		if n, err := store.CountRaw(ctx, s, "schema_migrations", "version = 28 AND min_reader = 28"); err != nil || n != 1 {
			t.Fatalf("0028's min_reader: %d %v", n, err)
		}
		if _, err := store.CountRaw(ctx, s, "publisher_keys", "1 = 1"); err != nil {
			t.Fatalf("publisher_keys: %v", err)
		}
		// a key's tenant is its publisher's, even in another existing tenant
		const other = "o0000000-0000-0000-0000-000000000000"
		if err := store.ExecRaw(ctx, s, "INSERT INTO tenants (id, name, display_name, state, created_at, version) VALUES (?, 'other', '', 'active', ?, 1)",
			other, now); err != nil {
			t.Fatal(err)
		}
		bad := testKey(other, pid, 1)
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertPublisherKey(ctx, &bad) }); err == nil {
			t.Fatal("a key in another tenant than its publisher's")
		}
	})
}
