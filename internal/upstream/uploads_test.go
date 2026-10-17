package upstream_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
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
				if cur.State != want || cur.Attempts != attempt || attempt == 3 && cur.Error != "busy" {
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

			// taken three times by replicas that died: failed, interrupted; an older attempt's end
			// changes nothing
			u2, err := g.up.CreateUpload(ctx, admin, "site", int64(len(data)), "", int64(len(data)), upstream.UploadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := g.up.PutUploadChunk(ctx, admin, "site", u2.ID, 0, bytes.NewReader(data), int64(len(data)), hexOf(data)); err != nil {
				t.Fatal(err)
			}
			if _, err := g.up.CompleteUpload(ctx, admin, "site", u2.ID); err != nil {
				t.Fatal(err)
			}
			var first string
			for i := range 3 {
				id := store.NewID()
				if i == 0 {
					first = id
				}
				if err := g.st.InTx(ctx, "", func(tx *store.Tx) error {
					if _, err := tx.TakeUpload(ctx, u2.ID, id); err != nil || i == 2 {
						return err
					}
					_, err := tx.RequeueUpload(ctx, u2.ID, id)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := g.st.InTx(ctx, "", func(tx *store.Tx) error {
				if ok, err := tx.FinishUpload(ctx, u2.ID, first, store.UploadImported, "", ""); err != nil || ok {
					t.Errorf("an older attempt's end: %v %v", ok, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			runner.Once(ctx)
			if cur, _, _ := g.up.GetUpload(ctx, admin, "site", u2.ID); cur.State != store.UploadFailed || cur.Error != "interrupted" || cur.Attempts != 3 {
				t.Fatalf("attempts used up by dead replicas: %+v", cur)
			}
		})
	}
}

// An upload's life past its import: a chunk that cannot be read for now puts it back in the
// queue, one gone fails it; a bundle refused before its record keeps no import; idle and failed
// uploads expire (each an event) and a completed one does not; the collector deletes the chunks
// of aborted and expired uploads and the objects no chunk names; max_bytes and a suspended tenant
// refuse new uploads, and a suspended tenant's queued upload waits.
func TestUploadLifecycle(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			g.up.Uploads = upstream.UploadSettings{MinChunkSize: 512, MaxUploads: 20, TTL: time.Hour, MaxAge: 2 * time.Hour}
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
			const chunk = int64(4096)
			part := func(b []byte, n int64) []byte { return b[n*chunk : min(int64(len(b)), (n+1)*chunk)] }
			create := func(b []byte, sha string) store.BundleUpload {
				t.Helper()
				u, err := g.up.CreateUpload(ctx, admin, "site", int64(len(b)), sha, chunk, upstream.UploadOptions{})
				if err != nil {
					t.Fatal(err)
				}
				return u
			}
			putN := func(u store.BundleUpload, b []byte, n int64) {
				t.Helper()
				if err := g.up.PutUploadChunk(ctx, admin, "site", u.ID, n, bytes.NewReader(part(b, n)), int64(len(part(b, n))), hexOf(part(b, n))); err != nil {
					t.Fatal(err)
				}
			}
			full := func(b []byte, sha string) store.BundleUpload {
				t.Helper()
				u := create(b, sha)
				for n := int64(0); n < u.Chunks(); n++ {
					putN(u, b, n)
				}
				if _, err := g.up.CompleteUpload(ctx, admin, "site", u.ID); err != nil {
					t.Fatal(err)
				}
				return u
			}
			get := func(u store.BundleUpload) (store.BundleUpload, []store.UploadChunk) {
				t.Helper()
				cur, chunks, err := g.up.GetUpload(ctx, admin, "site", u.ID)
				if err != nil {
					t.Fatal(err)
				}
				return cur, chunks
			}

			// a chunk that cannot be read for now: back in the queue, the attempt counted
			if os.Geteuid() != 0 {
				tr := full(data, "")
				_, chunks := get(tr)
				p := filepath.Join(g.blobDir, chunks[1].Key)
				if err := os.Chmod(p, 0); err != nil {
					t.Fatal(err)
				}
				runner.Once(ctx)
				if cur, _ := get(tr); cur.State != store.UploadQueued || cur.Attempts != 1 || cur.Error != "" {
					t.Fatalf("a chunk not readable for now: %+v", cur)
				}
				if err := os.Chmod(p, 0o600); err != nil {
					t.Fatal(err)
				}
				runner.Once(ctx)
				if cur, _ := get(tr); cur.State != store.UploadImported || cur.Attempts != 2 {
					t.Fatalf("readable again: %+v", cur)
				}
			}
			// a chunk gone: failed, storage
			gone := full(data, "")
			_, chunks := get(gone)
			if err := g.blob.DeleteChunk(ctx, "default", chunks[0].Key); err != nil {
				t.Fatal(err)
			}
			runner.Once(ctx)
			if cur, _ := get(gone); cur.State != store.UploadFailed || cur.Error != "storage" {
				t.Fatalf("a chunk gone: %+v", cur)
			}
			// not a bundle: refused before its record, whose id the upload does not show as one
			junk := bytes.Repeat([]byte("not a bundle "), 100)
			bad := full(junk, "")
			runner.Once(ctx)
			cur, _ := get(bad)
			if cur.State != store.UploadFailed || cur.Error == "" || cur.Error == "storage" {
				t.Fatalf("not a bundle: %+v", cur)
			}
			if _, err := g.up.GetImport(ctx, admin, "site", cur.ImportID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("the import of a refusal before its record: %v", err)
			}

			// expiry: idle, too old, failed; a failed one completed again stays
			real := time.Now().UTC()
			at := func(d time.Duration) { g.st.Now = func() time.Time { return real.Add(d).Truncate(time.Microsecond) } }
			at(0)
			idle := create(data[:10], "")
			old := create(data, "")
			putN(old, data, 0)
			aborted := create(data, "")
			putN(aborted, data, 0)
			if err := g.up.AbortUpload(ctx, admin, "site", aborted.ID); err != nil {
				t.Fatal(err)
			}
			again := full(junk, "")
			runner.Once(ctx) // fails it
			if _, err := g.up.CompleteUpload(ctx, admin, "site", again.ID); err != nil {
				t.Fatal(err)
			}
			orphan, err := g.blob.PutChunk(ctx, "default", bytes.NewReader(junk), int64(len(junk)), hexOf(junk))
			if err != nil {
				t.Fatal(err)
			}
			at(90 * time.Minute)
			putN(old, data, 1) // its last chunk an hour before the pass, its creation over max_age
			at(150 * time.Minute)
			col := &blob.Collector{Service: g.blob, Holder: "gc", Grace: 4 * blob.IntakeDeadline, Log: slog.New(slog.DiscardHandler),
				UploadTTL: time.Hour, UploadMaxAge: 2 * time.Hour}
			if _, err := col.Pass(ctx, "default", false); err != nil {
				t.Fatal(err)
			}
			for _, c := range []struct {
				u     store.BundleUpload
				state string
			}{{idle, store.UploadExpired}, {old, store.UploadExpired}, {gone, store.UploadExpired}, {bad, store.UploadExpired},
				{aborted, store.UploadAborted}, {again, store.UploadQueued}} {
				cur, chunks := get(c.u)
				if cur.State != c.state {
					t.Errorf("%s: %+v, want %s", c.u.ID, cur, c.state)
				}
				if c.state != store.UploadQueued && len(chunks) != 0 {
					t.Errorf("%s: chunks left %+v", c.u.ID, chunks)
				}
			}
			// an object no chunk names: kept within the grace, deleted past it
			if _, err := g.blob.OpenChunk(ctx, "default", orphan); err != nil {
				t.Errorf("an orphan chunk within the grace: %v", err)
			}
			at(5 * time.Hour)
			if _, err := col.Pass(ctx, "default", false); err != nil {
				t.Fatal(err)
			}
			if _, err := g.blob.OpenChunk(ctx, "default", orphan); !errors.Is(err, blob.ErrNotFound) {
				t.Errorf("an orphan chunk: %v", err)
			}
			if cur, chunks := get(again); cur.State != store.UploadQueued || len(chunks) == 0 {
				t.Errorf("a queued upload past the grace: %+v", cur)
			}
			tn, _ := g.st.GetTenant(ctx, "site")
			if evs, _ := g.st.ListEvents(ctx, tn.ID, store.EventFilter{Kind: "bundle.upload_expire", Limit: 100}); len(evs) != 4 {
				t.Errorf("expire events: %d", len(evs))
			}

			// max_bytes: the queued upload takes the room
			g.up.Uploads.MaxBytes = int64(len(junk)) + 5
			if _, err := g.up.CreateUpload(ctx, admin, "site", 10, "", 0, upstream.UploadOptions{}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("over max_bytes: %v", err)
			}
			g.up.Uploads.MaxBytes = 0
			// a suspended tenant takes no upload; its queued one waits
			if _, err := g.ten.SetTenantState(ctx, admin, "site", store.TenantSuspended, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := g.up.CreateUpload(ctx, admin, "site", 10, "", 0, upstream.UploadOptions{}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("a suspended tenant's upload: %v", err)
			}
			runner.Once(ctx)
			if cur, _ := get(again); cur.State != store.UploadQueued || cur.Attempts != 0 {
				t.Fatalf("a suspended tenant's queued upload: %+v", cur)
			}
		})
	}
}
