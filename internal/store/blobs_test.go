package store_test

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

func blobRec(body, stream string, streamLen int64) store.Blob {
	return store.Blob{Domain: "default", BodyHash: strings.Repeat(body, 64), StreamHash: strings.Repeat(stream, 64),
		BodyLen: 3 << 20, BodyCRC32: 0xfedcba98, StreamLen: streamLen,
		StreamChunks: bytes.Repeat([]byte{0xab}, int((streamLen+(1<<20)-1)>>20)*32)}
}

func putBlob(t *testing.T, s *store.Store, b *store.Blob) error {
	t.Helper()
	return s.InTx(ctx, "", func(tx *store.Tx) error { return tx.PutBlob(ctx, b) })
}

func TestBlobs(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		b := blobRec("a", "1", 2<<20+5)
		if err := putBlob(t, s, &b); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetBlob(ctx, "default", b.BodyHash)
		if err != nil {
			t.Fatal(err)
		}
		if got.StreamHash != b.StreamHash || got.BodyCRC32 != 0xfedcba98 || got.StreamLen != b.StreamLen ||
			got.BodyLen != b.BodyLen || !bytes.Equal(got.StreamChunks, b.StreamChunks) || !got.CorruptAt.IsZero() ||
			!got.CreatedAt.Equal(b.CreatedAt) {
			t.Fatalf("round trip: %+v", got)
		}
		if _, err := s.GetBlob(ctx, "other", b.BodyHash); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("another domain: %v", err)
		}

		// corrupt, then a commit of the same stream clears it and keeps the record
		if err := s.MarkBlobCorrupt(ctx, b); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetBlob(ctx, "default", b.BodyHash); got.CorruptAt.IsZero() {
			t.Fatal("corrupt_at not set")
		}
		time.Sleep(time.Millisecond) // distinct committed_at
		again := blobRec("a", "1", 2<<20+5)
		if err := putBlob(t, s, &again); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetBlob(ctx, "default", b.BodyHash); !got.CorruptAt.IsZero() || !got.CreatedAt.Equal(b.CreatedAt) ||
			!got.CommittedAt.Equal(again.CommittedAt) || got.CommittedAt.Before(b.CommittedAt) {
			t.Fatalf("after recommit: %+v", got)
		}
		// a reader that saw the earlier commit cannot mark the repaired record
		if err := s.MarkBlobCorrupt(ctx, b); err != nil {
			t.Fatal(err)
		}
		if again.CommittedAt.Equal(b.CommittedAt) {
			t.Fatal("two commits share committed_at; the generation check proves nothing")
		}
		if got, _ := s.GetBlob(ctx, "default", b.BodyHash); !got.CorruptAt.IsZero() {
			t.Fatal("a stale mark landed on a newer commit")
		}

		// a new stream for the same body replaces the record; marking the old stream changes nothing
		other := blobRec("a", "2", 100)
		if err := putBlob(t, s, &other); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkBlobCorrupt(ctx, b); err != nil {
			t.Fatal(err)
		}
		got, _ = s.GetBlob(ctx, "default", b.BodyHash)
		if got.StreamHash != other.StreamHash || got.StreamLen != 100 || len(got.StreamChunks) != 32 || !got.CorruptAt.IsZero() {
			t.Fatalf("after a new stream: %+v", got)
		}

		for name, bad := range map[string]func(*store.Blob){
			"upper hash":   func(b *store.Blob) { b.BodyHash = strings.Repeat("A", 64) },
			"short chunks": func(b *store.Blob) { b.StreamChunks = b.StreamChunks[1:] },
			"domain":       func(b *store.Blob) { b.Domain = "Default" },
			"empty stream": func(b *store.Blob) { b.StreamLen = 0; b.StreamChunks = nil },
		} {
			r := blobRec("c", "3", 10)
			bad(&r)
			if err := putBlob(t, s, &r); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}

		// racing commits of one body both succeed
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := blobRec("d", "4", 10)
				errs[i] = putBlob(t, s, &r)
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil && !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrExists) {
				t.Fatal(err)
			}
		}
	})
}

func TestStorageDomainsAndDeployment(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		var id1, id2 string
		err := s.InTx(ctx, "", func(tx *store.Tx) error {
			var err error
			if id1, err = tx.DeploymentID(ctx); err != nil {
				return err
			}
			id2, err = tx.DeploymentID(ctx)
			return err
		})
		if err != nil || id1 == "" || id1 != id2 {
			t.Fatalf("deployment id: %q %q %v", id1, id2, err)
		}
		d := store.StorageDomain{Name: "eu", Kind: "s3", StoreID: strings.Repeat("f", 64)}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.PinStorageDomain(ctx, &d) }); err != nil {
			t.Fatal(err)
		}
		err = s.InTx(ctx, "", func(tx *store.Tx) error {
			got, err := tx.GetStorageDomain(ctx, "eu")
			if err == nil && (got.StoreID != d.StoreID || got.Kind != "s3") {
				t.Errorf("domain: %+v", got)
			}
			if _, err := tx.GetStorageDomain(ctx, "us"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("missing domain: %v", err)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.PinStorageDomain(ctx, &d) }); !errors.Is(err, store.ErrExists) {
			t.Fatalf("pinning twice: %v", err)
		}

		tn := store.Tenant{Name: "acme"}
		cn := store.Tenant{Name: "acme-cn", StorageDomain: "cn"}
		bad := store.Tenant{Name: "bad", StorageDomain: "CN"}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			if err := tx.CreateTenant(ctx, &tn); err != nil {
				return err
			}
			return tx.CreateTenant(ctx, &cn)
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.CreateTenant(ctx, &bad) }); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("invalid domain: %v", err)
		}
		if got, _ := s.GetTenant(ctx, "acme"); got.StorageDomain != store.DefaultDomain {
			t.Fatalf("default domain: %q", got.StorageDomain)
		}
		if got, _ := s.GetTenant(ctx, "acme-cn"); got.StorageDomain != "cn" {
			t.Fatalf("domain: %q", got.StorageDomain)
		}
	})
}
