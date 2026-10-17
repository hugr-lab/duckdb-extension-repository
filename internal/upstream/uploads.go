package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Bundles uploaded over the API (spec 0014 phase 2): a session, its chunks (each its own object),
// completion, and a runner that imports a completed upload as kista admin bundle import does.

// UploadSettings are the bundles block's limits.
type UploadSettings struct {
	MaxSize, ChunkSize, MaxBytes int64
	MaxUploads                   int
	TTL, MaxAge                  time.Duration // an upload's expiry (the collector's)
	MinChunkSize                 int64         // the smallest chunk a client may ask for (default 1 MiB; tests lower it)
}

// UploadExpiresAt is when an upload expires: an uploading one TTL after its creation or last
// chunk, at the latest MaxAge after its creation; a failed one TTL after its failure; zero for
// any other.
func (s *Service) UploadExpiresAt(u store.BundleUpload) time.Time {
	set := s.uploadSettings()
	switch u.State {
	case store.UploadFailed: // completed again or expired, ttl from its failure
		return u.UpdatedAt.Add(set.TTL)
	case store.UploadUploading:
	default:
		return time.Time{}
	}
	last := u.CreatedAt
	if u.LastChunkAt.After(last) {
		last = u.LastChunkAt
	}
	if at, latest := last.Add(set.TTL), u.CreatedAt.Add(set.MaxAge); at.Before(latest) {
		return at
	} else {
		return latest
	}
}

// UploadOptions are an upload's import options: the CLI's flags.
type UploadOptions struct {
	DryRun    bool     `json:"dry_run,omitempty"`
	Upstreams []string `json:"upstreams,omitempty"`
	Feeds     []string `json:"feeds,omitempty"`
	AnyTenant bool     `json:"any_tenant,omitempty"`
}

// Upload errors.
var (
	// ErrUploadBusy is a chunk past the upload's parallel chunks, or no ingest slot free: retry.
	ErrUploadBusy = errors.New("upstream: too many chunks at once; send it again")
	// ErrUploadChunk is a chunk the upload does not have (its number, its size).
	ErrUploadChunk = errors.New("upstream: no such chunk")
)

const maxParallelChunks = 4

var hexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// uploadsInFlight counts each upload's chunks in flight on this replica.
var uploadsInFlight = struct {
	sync.Mutex
	n map[string]int
}{n: map[string]int{}}

func (s *Service) uploadSettings() UploadSettings {
	u := s.Uploads
	if u.MaxSize == 0 {
		u.MaxSize = 20 << 30
	}
	if u.ChunkSize == 0 {
		u.ChunkSize = 16 << 20
	}
	if u.MaxBytes == 0 {
		u.MaxBytes = max(100<<30, u.MaxSize)
	}
	if u.MaxUploads == 0 {
		u.MaxUploads = 2
	}
	if u.TTL == 0 {
		u.TTL = 24 * time.Hour
	}
	if u.MaxAge == 0 {
		u.MaxAge = 7 * 24 * time.Hour
	}
	if u.MinChunkSize == 0 {
		u.MinChunkSize = 1 << 20
	}
	return u
}

// writableTenant is a server administrator's tenant that takes writes: an active one.
func (s *Service) writableTenant(ctx context.Context, a authz.Actor, tenant string) (store.Tenant, error) {
	t, err := s.serverTenant(ctx, a, tenant)
	if err != nil {
		return t, err
	}
	if !t.SuspendedAt.IsZero() {
		return t, fmt.Errorf("%w: tenant %s is suspended", store.ErrConflict, tenant)
	}
	return t, nil
}

// CreateUpload starts an upload of size bytes (sha256: its declared SHA-256, hex, or ""); chunkSize
// is the client's wish (0: the server's).
func (s *Service) CreateUpload(ctx context.Context, a authz.Actor, tenant string, size int64, sha string, chunkSize int64, o UploadOptions) (store.BundleUpload, error) {
	t, err := s.writableTenant(ctx, a, tenant)
	if err != nil {
		return store.BundleUpload{}, err
	}
	set := s.uploadSettings()
	switch {
	case size <= 0 || size > set.MaxSize:
		return store.BundleUpload{}, fmt.Errorf("%w: size %d (1 to %d bytes)", store.ErrInvalid, size, set.MaxSize)
	case sha != "" && !hexRE.MatchString(sha):
		return store.BundleUpload{}, fmt.Errorf("%w: sha256 is 64 lowercase hex", store.ErrInvalid)
	case chunkSize != 0 && (chunkSize < set.MinChunkSize || chunkSize > set.ChunkSize):
		return store.BundleUpload{}, fmt.Errorf("%w: chunk_size is %d to %d bytes", store.ErrInvalid, set.MinChunkSize, set.ChunkSize)
	}
	if chunkSize == 0 {
		chunkSize = set.ChunkSize
	}
	opts, _ := json.Marshal(o)
	u := store.BundleUpload{TenantID: t.ID, Size: size, SHA256: sha, ChunkSize: chunkSize, Options: string(opts), CreatedBy: a.String()}
	err = s.Store.InTenantTx(ctx, t.ID, []string{uploadsLock}, func(tx *store.Tx) error {
		if err := tx.InsertBundleUpload(ctx, &u, set.MaxUploads, set.MaxBytes); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "bundle.upload", "tenant:"+t.Name, map[string]any{"upload": u.ID, "size": size,
			"sha256": sha, "chunk_size": chunkSize, "dry_run": o.DryRun, "upstreams": o.Upstreams, "feeds": o.Feeds, "any_tenant": o.AnyTenant})
	})
	return u, err
}

// uploadsLock orders the creation of uploads across the server: the byte cap is the server's.
const uploadsLock = "kista/bundle-uploads"

// GetUpload reads an upload and its chunks.
func (s *Service) GetUpload(ctx context.Context, a authz.Actor, tenant, id string) (store.BundleUpload, []store.UploadChunk, error) {
	t, err := s.serverTenant(ctx, a, tenant)
	if err != nil {
		return store.BundleUpload{}, nil, err
	}
	var u store.BundleUpload
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		u, err = tx.GetBundleUpload(ctx, t.ID, id)
		return err
	}); err != nil {
		return u, nil, err
	}
	chunks, err := s.Store.UploadChunks(ctx, u.ID)
	return u, chunks, err
}

// ListUploads lists a tenant's uploads, newest first.
func (s *Service) ListUploads(ctx context.Context, a authz.Actor, tenant string, limit int) ([]store.BundleUpload, error) {
	t, err := s.serverTenant(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	return s.Store.ListBundleUploads(ctx, t.ID, limit)
}

// PutUploadChunk stores chunk n: exactly its size, its SHA-256 (hex) the digest the request gave.
// A chunk recorded with the same digest is a retry; another digest is a conflict.
func (s *Service) PutUploadChunk(ctx context.Context, a authz.Actor, tenant, id string, n int64, r io.Reader, length int64, digest string) error {
	t, err := s.writableTenant(ctx, a, tenant)
	if err != nil {
		return err
	}
	if s.Blob == nil {
		return errors.New("upstream: uploads need a blob service")
	}
	var u store.BundleUpload
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		u, err = tx.GetBundleUpload(ctx, t.ID, id)
		return err
	}); err != nil {
		return err
	}
	switch {
	case n < 0 || n >= u.Chunks():
		return fmt.Errorf("%w: %d of %d", ErrUploadChunk, n, u.Chunks())
	case length != u.ChunkSizeOf(n):
		return fmt.Errorf("%w: chunk %d is %d bytes, not %d", store.ErrInvalid, n, u.ChunkSizeOf(n), length)
	case !hexRE.MatchString(digest):
		return fmt.Errorf("%w: a chunk needs its Content-Digest (sha-256)", store.ErrInvalid)
	case u.State != store.UploadUploading:
		return fmt.Errorf("%w: the upload is %s", store.ErrUploadState, u.State)
	}
	uploadsInFlight.Lock()
	if uploadsInFlight.n[u.ID] >= maxParallelChunks {
		uploadsInFlight.Unlock()
		return ErrUploadBusy
	}
	uploadsInFlight.n[u.ID]++
	uploadsInFlight.Unlock()
	defer func() {
		uploadsInFlight.Lock()
		if uploadsInFlight.n[u.ID]--; uploadsInFlight.n[u.ID] <= 0 {
			delete(uploadsInFlight.n, u.ID)
		}
		uploadsInFlight.Unlock()
	}()
	key, err := s.Blob.PutChunk(ctx, t.StorageDomain, r, length, digest)
	if errors.Is(err, blob.ErrBusy) {
		return ErrUploadBusy
	}
	if err != nil {
		return err
	}
	var outcome string
	err = s.Store.InTenantTx(ctx, t.ID, nil, func(tx *store.Tx) error {
		outcome, err = tx.CommitChunk(ctx, store.UploadChunk{UploadID: u.ID, N: n, Key: key, Size: length, SHA256: digest})
		return err
	})
	if err != nil || outcome != store.ChunkStored {
		_ = s.Blob.DeleteChunk(context.WithoutCancel(ctx), t.StorageDomain, key)
	}
	switch {
	case err != nil:
		return err
	case outcome == store.ChunkConflict:
		return fmt.Errorf("%w: chunk %d was received with another digest", store.ErrConflict, n)
	}
	return nil
}

// CompleteUpload queues an upload whose chunks are all there; the administrator completing it is
// the import's actor. A second call queues nothing more.
func (s *Service) CompleteUpload(ctx context.Context, a authz.Actor, tenant, id string) (store.BundleUpload, error) {
	t, err := s.writableTenant(ctx, a, tenant)
	if err != nil {
		return store.BundleUpload{}, err
	}
	var u store.BundleUpload
	err = s.Store.InTenantTx(ctx, t.ID, nil, func(tx *store.Tx) error {
		cur, err := tx.GetBundleUpload(ctx, t.ID, id)
		if err != nil {
			return err
		}
		before := cur.State
		if u, err = tx.CompleteUpload(ctx, cur, a.String()); err != nil || u.State == before {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "bundle.upload_complete", "tenant:"+t.Name, map[string]any{"upload": u.ID, "retry": before == store.UploadFailed})
	})
	if err == nil {
		select {
		case s.WakeUploads() <- struct{}{}:
		default:
		}
	}
	return u, err
}

// AbortUpload aborts an upload not importing; its chunks go with the collector.
func (s *Service) AbortUpload(ctx context.Context, a authz.Actor, tenant, id string) error {
	t, err := s.serverTenant(ctx, a, tenant)
	if err != nil {
		return err
	}
	return s.Store.InTenantTx(ctx, t.ID, nil, func(tx *store.Tx) error {
		u, err := tx.GetBundleUpload(ctx, t.ID, id)
		if err != nil {
			return err
		}
		ok, err := tx.AbortUpload(ctx, u.ID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: the upload is %s", store.ErrUploadState, u.State)
		}
		return tx.Event(ctx, t.ID, a.String(), "bundle.upload_abort", "tenant:"+t.Name, map[string]any{"upload": u.ID})
	})
}

// --- the runner ---

// UploadRunner imports completed uploads: every replica runs one; leases keep each upload on one.
type UploadRunner struct {
	Service *Service
	Holder  string
	// BundleKeys are the bundle keys pinned for a tenant (upstreams.bundle_keys).
	BundleKeys func(tenant string) []string
	Log        *slog.Logger
	// Poll is the time between looks (default 10s).
	Poll time.Duration
}

const maxUploadAttempts = 3

// WakeUploads is the channel a completed upload wakes this replica's runner with.
func (s *Service) WakeUploads() chan struct{} {
	s.wakeOnce.Do(func() { s.wakeUploads = make(chan struct{}, 1) })
	return s.wakeUploads
}

// Run looks for queued uploads until ctx ends.
func (r *UploadRunner) Run(ctx context.Context) {
	poll := r.Poll
	if poll <= 0 {
		poll = 10 * time.Second
	}
	wake := r.Service.WakeUploads()
	tk := time.NewTicker(poll)
	defer tk.Stop()
	for {
		r.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		case <-wake:
		}
	}
}

// Once takes back the uploads whose import was interrupted, then imports the queued ones.
func (r *UploadRunner) Once(ctx context.Context) {
	s := r.Service
	importing, err := s.Store.UploadsIn(ctx, store.UploadImporting, 100)
	if err != nil {
		r.Log.Warn("upstream: uploads", "error", err)
		return
	}
	for _, u := range importing { // a replica died: its leases expired
		r.retake(ctx, u)
	}
	queued, err := s.Store.UploadsIn(ctx, store.UploadQueued, 100)
	if err != nil {
		r.Log.Warn("upstream: uploads", "error", err)
		return
	}
	for _, u := range queued {
		if ctx.Err() != nil {
			return
		}
		r.take(ctx, u)
	}
}

// retake puts back in the queue, under its lease, an importing upload whose replica died, or
// fails it when its attempts are used up.
func (r *UploadRunner) retake(ctx context.Context, u store.BundleUpload) {
	s := r.Service
	held, err := s.Store.HeldLeases(ctx, []string{uploadLease(u.ID), store.ImportLease(u.ImportID)})
	if err != nil || len(held) > 0 {
		return // its replica, or its last import, still holds one
	}
	if ok, err := s.Store.AcquireLease(ctx, uploadLease(u.ID), r.Holder, leaseTTL); err != nil || !ok {
		return
	}
	defer func() { _ = s.Store.ReleaseLease(context.WithoutCancel(ctx), uploadLease(u.ID), r.Holder) }()
	var tn store.Tenant
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error { tn, err = tx.GetTenantByID(ctx, u.TenantID); return err }); err != nil {
		return
	}
	r.finish(ctx, tn, u, store.UploadQueued, "interrupted", "")
}

// finish ends an attempt: the upload goes back to the queue (its attempts not used up), or is
// imported or failed; a failure is an event. It changes nothing when the upload is no longer at
// that attempt.
func (r *UploadRunner) finish(ctx context.Context, tn store.Tenant, u store.BundleUpload, state, class, input string) {
	s := r.Service
	if state == store.UploadQueued && u.Attempts >= maxUploadAttempts {
		state = store.UploadFailed // class says why: interrupted, or busy
	}
	err := s.Store.InTenantTx(ctx, tn.ID, nil, func(tx *store.Tx) error {
		var ok bool
		var err error
		if state == store.UploadQueued {
			ok, err = tx.RequeueUpload(ctx, u.ID, u.ImportID)
		} else {
			ok, err = tx.FinishUpload(ctx, u.ID, u.ImportID, state, class, input)
		}
		if err != nil || !ok {
			if err == nil {
				r.Log.Warn("upstream: an upload moved on meanwhile", "upload", u.ID, "import", u.ImportID, "state", state)
			}
			return err
		}
		if state == store.UploadFailed {
			return tx.Event(ctx, tn.ID, "system:bundle-upload", "bundle.upload_fail", "tenant:"+tn.Name,
				map[string]any{"upload": u.ID, "import": u.ImportID, "error": class})
		}
		return nil
	})
	if err != nil {
		r.Log.Warn("upstream: an upload's state", "upload", u.ID, "error", err)
	}
}

func uploadLease(id string) string             { return "kista/bundle-upload/" + id }
func uploadTenantLease(tenantID string) string { return "kista/bundle-upload-tenant/" + tenantID }

// take imports one queued upload, under its tenant's lease and its own.
func (r *UploadRunner) take(ctx context.Context, u store.BundleUpload) {
	s := r.Service
	var tn store.Tenant
	var err error
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error { tn, err = tx.GetTenantByID(ctx, u.TenantID); return err }); err != nil {
		if errors.Is(err, store.ErrDeleting) {
			_ = s.Store.InTx(ctx, "", func(tx *store.Tx) error { return tx.AbortTenantUploads(ctx, u.TenantID) })
		}
		return
	}
	if !tn.SuspendedAt.IsZero() {
		return // it waits
	}
	for _, l := range []string{uploadTenantLease(u.TenantID), uploadLease(u.ID)} {
		ok, err := s.Store.AcquireLease(ctx, l, r.Holder, leaseTTL)
		if err != nil || !ok {
			return
		}
		defer func() { _ = s.Store.ReleaseLease(context.WithoutCancel(ctx), l, r.Holder) }()
	}
	importID := store.NewID()
	var took bool
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error { took, err = tx.TakeUpload(ctx, u.ID, importID); return err }); err != nil || !took {
		return
	}
	u.ImportID, u.Attempts = importID, u.Attempts+1
	ictx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() { // the upload's leases are renewed while it imports; losing them cancels it
		tk := time.NewTicker(30 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ictx.Done():
				return
			case <-tk.C:
			}
			for _, l := range []string{uploadTenantLease(u.TenantID), uploadLease(u.ID)} {
				if ok, err := s.Store.RenewLease(ictx, l, r.Holder, leaseTTL); err != nil || !ok {
					cancel(errLeaseLost)
					return
				}
			}
		}
	}()
	state, class, input := r.importUpload(ictx, tn, u)
	r.finish(context.WithoutCancel(ctx), tn, u, state, class, input)
}

// importUpload reads the chunks (once for the declared SHA-256, then as the import's input) and
// imports them; it answers the upload's next state, an error class (for queued: why, should its
// attempts be used up), and the input's SHA-256. A chunk that cannot be read for now (the store,
// the lease) is never a refusal: the upload goes back to the queue.
func (r *UploadRunner) importUpload(ctx context.Context, t store.Tenant, u store.BundleUpload) (string, string, string) {
	s := r.Service
	chunks, err := s.Store.UploadChunks(ctx, u.ID)
	if err != nil {
		return store.UploadQueued, "interrupted", ""
	}
	if int64(len(chunks)) != u.Chunks() {
		return store.UploadFailed, "storage", ""
	}
	if u.SHA256 != "" {
		cr := &chunkReader{ctx: ctx, s: s, domain: t.StorageDomain, chunks: chunks}
		h := sha256.New()
		_, err := io.Copy(h, cr)
		cr.Close()
		if err != nil {
			if cr.bad {
				return store.UploadFailed, "storage", ""
			}
			return store.UploadQueued, "interrupted", ""
		}
		if hex.EncodeToString(h.Sum(nil)) != u.SHA256 {
			return store.UploadFailed, "sha256", ""
		}
	}
	var o UploadOptions
	_ = json.Unmarshal([]byte(u.Options), &o)
	kind, id, _ := strings.Cut(u.CompletedBy, ":")
	if k := authz.ActorKind(kind); k != authz.ActorServer && k != authz.ActorOS {
		// server administrators complete uploads: a stored actor of any other kind (a system
		// actor above all) is never taken as one
		return store.UploadFailed, "refused", ""
	}
	actor := authz.Actor{Kind: authz.ActorKind(kind), ID: id}
	cr := &chunkReader{ctx: ctx, s: s, domain: t.StorageDomain, chunks: chunks}
	defer cr.Close()
	var keys []string
	if r.BundleKeys != nil {
		keys = r.BundleKeys(t.Name)
	}
	res, err := s.Import(ctx, actor, t.Name, cr, ImportOptions{Upstreams: o.Upstreams, Feeds: o.Feeds, DryRun: o.DryRun,
		AnyTenant: o.AnyTenant, BundleKeys: keys, Holder: r.Holder, ID: u.ImportID, Trusted: true})
	switch {
	case cr.bad:
		return store.UploadFailed, "storage", ""
	case cr.err != nil, ctx.Err() != nil, err == nil && res.Stop == "interrupted":
		return store.UploadQueued, "interrupted", "" // whatever the import made of it
	case err != nil: // a refusal before the record
		return store.UploadFailed, refusalClass(err), ""
	}
	busy := len(res.Targets) > 0
	for _, tr := range res.Targets {
		busy = busy && tr.Status == TargetBusy
	}
	if busy {
		return store.UploadQueued, "busy", ""
	}
	if u.SHA256 != "" && res.Import.InputSHA256 != "" && res.Import.InputSHA256 != u.SHA256 {
		return store.UploadFailed, "sha256", res.Import.InputSHA256
	}
	return store.UploadImported, "", res.Import.InputSHA256
}

// refusalClass names an import's refusal for an upload's record: never a store's raw error.
func refusalClass(err error) string {
	switch {
	case errors.Is(err, ErrBundleTenant):
		return "another tenant"
	case errors.Is(err, store.ErrInvalid):
		return "refused"
	case errors.Is(err, store.ErrNotFound):
		return "not found"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not pinned"):
		return "bundle key not pinned"
	case strings.Contains(msg, "bundle:"):
		return "malformed"
	}
	return "failed"
}

// chunkReader reads an upload's chunks in order as one stream, each checked against its recorded
// digest at its end; a chunk missing or differing marks the reader bad, any other failure to read
// is its err (the import's own errors are not).
type chunkReader struct {
	ctx    context.Context
	s      *Service
	domain string
	chunks []store.UploadChunk
	i      int
	rc     io.ReadCloser
	h      hash.Hash
	n      int64
	bad    bool
	err    error
}

// Close closes the chunk open, if any.
func (c *chunkReader) Close() {
	if c.rc != nil {
		_ = c.rc.Close()
		c.rc = nil
	}
}

func (c *chunkReader) Read(p []byte) (int, error) {
	for {
		if c.rc == nil {
			if c.i >= len(c.chunks) {
				return 0, io.EOF
			}
			rc, err := c.s.Blob.OpenChunk(c.ctx, c.domain, c.chunks[c.i].Key)
			if err != nil {
				if errors.Is(err, blob.ErrNotFound) {
					c.bad = true
				} else {
					c.err = err
				}
				return 0, err
			}
			c.rc, c.h, c.n = rc, sha256.New(), 0
		}
		n, err := c.rc.Read(p)
		if n > 0 {
			c.h.Write(p[:n])
			c.n += int64(n)
		}
		if err == io.EOF {
			_ = c.rc.Close()
			c.rc = nil
			ch := c.chunks[c.i]
			if c.n != ch.Size || hex.EncodeToString(c.h.Sum(nil)) != ch.SHA256 {
				c.bad = true
				return n, fmt.Errorf("upstream: chunk %d does not match its digest", ch.N)
			}
			c.i++
			if n > 0 {
				return n, nil
			}
			continue
		}
		if err != nil {
			c.err = err
		}
		return n, err
	}
}
