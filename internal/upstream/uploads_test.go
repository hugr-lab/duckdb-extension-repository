package upstream_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

func hexOf(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// Spec 0014 phase 2: a bundle uploaded in chunks, out of order, each checked by its digest and
// stored once; completed, then imported by a runner as the CLI imports it; its chunks collected.
func TestUploads(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			g.up.Uploads = upstream.UploadSettings{MinChunkSize: 512, MaxUploads: 2}
			g.put(t, "tresor", "1.0", 1)
			sp := g.spec(store.UpstreamEntry{Name: "tresor"})
			sp.Visibility = store.Public
			if _, err := g.up.Add(ctx, admin, "acme", sp); err != nil {
				t.Fatal(err)
			}
			g.sync(t, false)
			g.air(t, "tresor")
			_, data := g.export(t, g.plan(t), upstream.ExportOptions{})
			runner := &upstream.UploadRunner{Service: g.up, Holder: "r1", BundleKeys: func(string) []string { return []string{g.fp} },
				Log: slog.New(slog.DiscardHandler)}

			chunk := int64(4096)
			u, err := g.up.CreateUpload(ctx, admin, "site", int64(len(data)), hexOf(data), chunk, upstream.UploadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if u.Chunks() < 2 {
				t.Fatalf("the test needs several chunks: %d bytes", len(data))
			}
			part := func(n int64) []byte { return data[n*chunk : min(int64(len(data)), (n+1)*chunk)] }
			put := func(n int64, b []byte) error {
				return g.up.PutUploadChunk(ctx, admin, "site", u.ID, n, bytes.NewReader(b), int64(len(b)), hexOf(b))
			}
			// out of order; a retry is a no-op; another digest, a wrong size or number are refused
			for n := u.Chunks() - 1; n >= 1; n-- {
				if err := put(n, part(n)); err != nil {
					t.Fatalf("chunk %d: %v", n, err)
				}
			}
			if err := put(1, part(1)); err != nil {
				t.Fatalf("a retry: %v", err)
			}
			other := bytes.Clone(part(1))
			other[0] ^= 1
			if err := put(1, other); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("another digest: %v", err)
			}
			if err := g.up.PutUploadChunk(ctx, admin, "site", u.ID, 0, bytes.NewReader(part(0)), int64(len(part(0))), hexOf(other)); !errors.Is(err, blob.ErrDigest) {
				t.Fatalf("a digest that does not match: %v", err)
			}
			if err := put(0, part(0)[:10]); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("a wrong size: %v", err)
			}
			if err := put(u.Chunks(), part(0)); !errors.Is(err, upstream.ErrUploadChunk) {
				t.Fatalf("a chunk past the last: %v", err)
			}
			if _, err := g.up.CompleteUpload(ctx, admin, "site", u.ID); !errors.Is(err, store.ErrUploadState) {
				t.Fatalf("complete with a chunk missing: %v", err)
			}
			if err := put(0, part(0)); err != nil {
				t.Fatal(err)
			}
			u, err = g.up.CompleteUpload(ctx, admin, "site", u.ID)
			if err != nil || u.State != store.UploadQueued {
				t.Fatalf("complete: %+v %v", u, err)
			}
			if again, err := g.up.CompleteUpload(ctx, admin, "site", u.ID); err != nil || again.State != store.UploadQueued {
				t.Fatalf("complete again: %+v %v", again, err)
			}
			if err := put(0, part(0)); err == nil {
				t.Fatal("a chunk after complete")
			}

			// the runner imports it as the CLI does
			runner.Once(ctx)
			u, chunks, err := g.up.GetUpload(ctx, admin, "site", u.ID)
			if err != nil || u.State != store.UploadImported || u.InputSHA256 != hexOf(data) || len(chunks) != int(u.Chunks()) {
				t.Fatalf("imported: %+v %v", u, err)
			}
			rec, err := g.up.GetImport(ctx, admin, "site", u.ImportID)
			if err != nil || rec.State != store.ImportDone || rec.Actor != admin.String() {
				t.Fatalf("the import record: %+v %v", rec, err)
			}

			// the collector deletes an imported upload's chunks, then their rows
			col := &blob.Collector{Service: g.blob, Holder: "gc", Grace: 4 * blob.IntakeDeadline, Log: slog.New(slog.DiscardHandler),
				UploadTTL: time.Hour, UploadMaxAge: 2 * time.Hour}
			res, err := col.Pass(ctx, "default", false)
			if err != nil || res.Chunks != int(u.Chunks()) {
				t.Fatalf("collected: %+v %v", res, err)
			}
			if left, _ := g.st.UploadChunks(ctx, u.ID); len(left) != 0 {
				t.Fatalf("chunk rows left: %+v", left)
			}

			// a declared SHA-256 that differs: nothing is imported
			bad, err := g.up.CreateUpload(ctx, admin, "site", int64(len(data)), hexOf([]byte("another")), chunk, upstream.UploadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for n := int64(0); n < bad.Chunks(); n++ {
				if err := g.up.PutUploadChunk(ctx, admin, "site", bad.ID, n, bytes.NewReader(part(n)), int64(len(part(n))), hexOf(part(n))); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := g.up.CompleteUpload(ctx, admin, "site", bad.ID); err != nil {
				t.Fatal(err)
			}
			runner.Once(ctx)
			if b, _, _ := g.up.GetUpload(ctx, admin, "site", bad.ID); b.State != store.UploadFailed || b.Error != "sha256" {
				t.Fatalf("a differing SHA-256: %+v", b)
			}

			// the limits: two live uploads per tenant (the failed one counts)
			if _, err := g.up.CreateUpload(ctx, admin, "site", 10, "", 0, upstream.UploadOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := g.up.CreateUpload(ctx, admin, "site", 10, "", 0, upstream.UploadOptions{}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("a third live upload: %v", err)
			}
			if _, err := g.up.CreateUpload(ctx, admin, "site", 1<<50, "", 0, upstream.UploadOptions{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("over max_size: %v", err)
			}
			// an abort frees the room; a failed upload can be completed again
			if err := g.up.AbortUpload(ctx, admin, "site", bad.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := g.up.CreateUpload(ctx, admin, "site", 10, "", 0, upstream.UploadOptions{}); err != nil {
				t.Fatalf("after an abort: %v", err)
			}
		})
	}
}

// An upload whose targets are all busy goes back to the queue; after three attempts it fails. An
// importing upload whose replica died (its leases expired) is taken again.
func TestUploadRetries(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			g.up.Uploads = upstream.UploadSettings{MinChunkSize: 512}
			g.put(t, "tresor", "1.0", 1)
			sp := g.spec(store.UpstreamEntry{Name: "tresor"})
			sp.Visibility = store.Public
			if _, err := g.up.Add(ctx, admin, "acme", sp); err != nil {
				t.Fatal(err)
			}
			g.sync(t, false)
			g.air(t, "tresor")
			_, data := g.export(t, g.plan(t), upstream.ExportOptions{})
			runner := &upstream.UploadRunner{Service: g.up, Holder: "r1", BundleKeys: func(string) []string { return []string{g.fp} },
				Log: slog.New(slog.DiscardHandler)}
			u, err := g.up.CreateUpload(ctx, admin, "site", int64(len(data)), "", int64(len(data)), upstream.UploadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := g.up.PutUploadChunk(ctx, admin, "site", u.ID, 0, bytes.NewReader(data), int64(len(data)), hexOf(data)); err != nil {
				t.Fatal(err)
			}
			if _, err := g.up.CompleteUpload(ctx, admin, "site", u.ID); err != nil {
				t.Fatal(err)
			}
			air, _ := g.up.Get(ctx, admin, "site", "air")
			if ok, err := g.st.AcquireLease(ctx, "kista/upstream/"+air.ID, "a run", time.Hour); err != nil || !ok {
				t.Fatal(err)
			}
			for attempt := 1; attempt <= 3; attempt++ {
				runner.Once(ctx)
				cur, _, _ := g.up.GetUpload(ctx, admin, "site", u.ID)
				want := store.UploadQueued
				if attempt == 3 {
					want = store.UploadFailed
				}
				if cur.State != want || cur.Attempts != attempt {
					t.Fatalf("attempt %d: %+v", attempt, cur)
				}
			}
			_ = g.st.ReleaseLease(ctx, "kista/upstream/"+air.ID, "a run")
			// completed again, it imports
			if _, err := g.up.CompleteUpload(ctx, admin, "site", u.ID); err != nil {
				t.Fatal(err)
			}
			// a replica took it and died: no lease held, it is taken again
			if err := g.st.InTx(ctx, "", func(tx *store.Tx) error { _, err := tx.TakeUpload(ctx, u.ID, store.NewID()); return err }); err != nil {
				t.Fatal(err)
			}
			runner.Once(ctx) // requeued
			runner.Once(ctx) // imported
			if cur, _, _ := g.up.GetUpload(ctx, admin, "site", u.ID); cur.State != store.UploadImported {
				t.Fatalf("after a dead replica: %+v", cur)
			}
		})
	}
}
