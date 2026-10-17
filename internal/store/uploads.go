package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Bundle uploads (spec 0014 phase 2): a bundle sent over the API in chunks, each its own object,
// imported by the server once complete.

// Upload states.
const (
	UploadUploading = "uploading"
	UploadQueued    = "queued"
	UploadImporting = "importing"
	UploadImported  = "imported"
	UploadFailed    = "failed"
	UploadAborted   = "aborted"
	UploadExpired   = "expired"
)

// BundleUpload is an upload's record.
type BundleUpload struct {
	ID, TenantID    string
	Size, ChunkSize int64
	SHA256          string // declared; "" when not given
	Options         string // JSON: the import's options
	State           string
	ImportID        string // the latest attempt's
	InputSHA256     string
	Attempts        int
	Error           string // a class
	CreatedBy       string
	CompletedBy     string
	CreatedAt       time.Time
	LastChunkAt     time.Time
	UpdatedAt       time.Time
}

// Chunks is the number of chunks of the upload.
func (u BundleUpload) Chunks() int64 {
	if u.ChunkSize <= 0 {
		return 0
	}
	return (u.Size + u.ChunkSize - 1) / u.ChunkSize
}

// ChunkSizeOf is chunk n's size.
func (u BundleUpload) ChunkSizeOf(n int64) int64 {
	if n == u.Chunks()-1 {
		return u.Size - n*u.ChunkSize
	}
	return u.ChunkSize
}

// UploadChunk is a chunk received.
type UploadChunk struct {
	UploadID   string
	N          int64
	Key        string // its object, uploads/<uuid>
	Size       int64
	SHA256     string // hex
	ReceivedAt time.Time
}

// ErrUploadState is an act an upload's state does not allow.
var ErrUploadState = errors.New("store: the upload's state does not allow it")

// liveUploads are the states that hold chunks and count against the limits.
var liveUploads = []any{UploadUploading, UploadQueued, UploadImporting, UploadFailed}

const uploadCols = `id, tenant_id, size, sha256, chunk_size, options, state, import_id, input_sha256, attempts, error, created_by,
completed_by, created_at, last_chunk_at, updated_at`

func scanUpload(r interface{ Scan(...any) error }) (BundleUpload, error) {
	var u BundleUpload
	var sha, imp, input, errText, completed sql.NullString
	err := r.Scan(&u.ID, &u.TenantID, &u.Size, &sha, &u.ChunkSize, &u.Options, &u.State, &imp, &input, &u.Attempts, &errText,
		&u.CreatedBy, &completed, scanTime{&u.CreatedAt}, scanTime{&u.LastChunkAt}, scanTime{&u.UpdatedAt})
	u.SHA256, u.ImportID, u.InputSHA256, u.Error, u.CompletedBy = sha.String, imp.String, input.String, errText.String, completed.String
	return u, err
}

// InsertBundleUpload records a new upload, within the tenant's count (maxUploads live uploads) and
// the server's bytes (maxBytes of live uploads' declared sizes).
func (t *Tx) InsertBundleUpload(ctx context.Context, u *BundleUpload, maxUploads int, maxBytes int64) error {
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM bundle_uploads WHERE tenant_id = ? AND state IN (?, ?, ?, ?)",
		append([]any{u.TenantID}, liveUploads...)...).Scan(&n); err != nil {
		return err
	}
	if n >= maxUploads {
		return fmt.Errorf("%w: %d uploads of the tenant are not imported yet (at most %d)", ErrConflict, n, maxUploads)
	}
	var total sql.NullInt64
	if err := t.queryRow(ctx, "SELECT SUM(size) FROM bundle_uploads WHERE state IN (?, ?, ?, ?)", liveUploads...).Scan(&total); err != nil {
		return err
	}
	if total.Int64+u.Size > maxBytes {
		return fmt.Errorf("%w: the server's uploads would hold %d bytes (at most %d)", ErrConflict, total.Int64+u.Size, maxBytes)
	}
	now := t.Now()
	if u.ID == "" {
		u.ID = NewID()
	}
	u.State, u.CreatedAt, u.UpdatedAt = UploadUploading, now, now
	_, err := t.exec(ctx, "INSERT INTO bundle_uploads ("+uploadCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		u.ID, u.TenantID, u.Size, nullable(u.SHA256), u.ChunkSize, u.Options, u.State, nil, nil, 0, nil, u.CreatedBy, nil,
		t.s.d.timeArg(now), nil, t.s.d.timeArg(now))
	return t.s.mapErr(err, "bundle upload")
}

// GetBundleUpload reads a tenant's upload.
func (t *Tx) GetBundleUpload(ctx context.Context, tenantID, id string) (BundleUpload, error) {
	u, err := scanUpload(t.queryRow(ctx, "SELECT "+uploadCols+" FROM bundle_uploads WHERE tenant_id = ? AND id = ?", tenantID, id))
	return u, notFound(err, "bundle upload")
}

// GetBundleUploadByID reads an upload by id (the runner's).
func (t *Tx) GetBundleUploadByID(ctx context.Context, id string) (BundleUpload, error) {
	u, err := scanUpload(t.queryRow(ctx, "SELECT "+uploadCols+" FROM bundle_uploads WHERE id = ?", id))
	return u, notFound(err, "bundle upload")
}

// ListBundleUploads lists a tenant's uploads, newest first.
func (s *Store) ListBundleUploads(ctx context.Context, tenantID string, limit int) ([]BundleUpload, error) {
	var out []BundleUpload
	err := s.eachRow(ctx, "SELECT "+uploadCols+" FROM bundle_uploads WHERE tenant_id = ? ORDER BY created_at DESC, id DESC"+s.limit(limit),
		[]any{tenantID}, func(sc func(...any) error) error {
			u, err := scanUpload(scanner(sc))
			if err == nil {
				out = append(out, u)
			}
			return err
		})
	return out, err
}

type scanner func(...any) error

func (f scanner) Scan(dest ...any) error { return f(dest...) }

// UploadChunks lists an upload's chunks by number.
func (s *Store) UploadChunks(ctx context.Context, uploadID string) ([]UploadChunk, error) {
	var out []UploadChunk
	err := s.eachRow(ctx, "SELECT upload_id, n, object_key, size, sha256, received_at FROM bundle_upload_chunks WHERE upload_id = ? ORDER BY n",
		[]any{uploadID}, func(sc func(...any) error) error {
			var c UploadChunk
			if err := sc(&c.UploadID, &c.N, &c.Key, &c.Size, &c.SHA256, scanTime{&c.ReceivedAt}); err != nil {
				return err
			}
			out = append(out, c)
			return nil
		})
	return out, err
}

// Chunk commit outcomes.
const (
	ChunkStored   = "stored"   // recorded now
	ChunkRetry    = "retry"    // recorded before with the same digest
	ChunkConflict = "conflict" // recorded before with another digest
)

// CommitChunk records a chunk stored as key, only while the upload is uploading and the chunk is
// not recorded: a chunk's object is never replaced.
func (t *Tx) CommitChunk(ctx context.Context, c UploadChunk) (string, error) {
	var state string
	if err := t.queryRow(ctx, "SELECT state FROM bundle_uploads WHERE id = ?", c.UploadID).Scan(&state); err != nil {
		return "", notFound(err, "bundle upload")
	}
	var have string
	err := t.queryRow(ctx, "SELECT sha256 FROM bundle_upload_chunks WHERE upload_id = ? AND n = ?", c.UploadID, c.N).Scan(&have)
	switch {
	case err == nil && have == c.SHA256:
		return ChunkRetry, nil
	case err == nil:
		return ChunkConflict, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	case state != UploadUploading:
		return "", fmt.Errorf("%w: the upload is %s", ErrUploadState, state)
	}
	now := t.Now()
	if _, err := t.exec(ctx, "INSERT INTO bundle_upload_chunks (upload_id, n, object_key, size, sha256, received_at) VALUES (?, ?, ?, ?, ?, ?)",
		c.UploadID, c.N, c.Key, c.Size, c.SHA256, t.s.d.timeArg(now)); err != nil {
		if errors.Is(t.s.mapErr(err, "chunk"), ErrExists) {
			return ChunkConflict, nil // a concurrent commit of the same chunk won: compare again on retry
		}
		return "", err
	}
	_, err = t.exec(ctx, "UPDATE bundle_uploads SET last_chunk_at = ?, updated_at = ? WHERE id = ?", t.s.d.timeArg(now), t.s.d.timeArg(now), c.UploadID)
	return ChunkStored, err
}

// setUploadState moves an upload from one of from to state, answering whether it moved.
func (t *Tx) setUploadState(ctx context.Context, id, state string, from []string, set string, args ...any) (bool, error) {
	q := "UPDATE bundle_uploads SET state = ?, updated_at = ?" + set + " WHERE id = ? AND state IN ("
	a := []any{state, t.s.d.timeArg(t.Now())}
	a = append(a, args...)
	a = append(a, id)
	for i, f := range from {
		if i > 0 {
			q += ", "
		}
		q += "?"
		a = append(a, f)
	}
	res, err := t.exec(ctx, q+")", a...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// CompleteUpload queues an upload whose chunks are all there (from uploading, or again from
// failed), recording who completed it; a queued, importing or imported one is left as it is.
func (t *Tx) CompleteUpload(ctx context.Context, u BundleUpload, actor string) (BundleUpload, error) {
	switch u.State {
	case UploadQueued, UploadImporting, UploadImported:
		return u, nil
	case UploadUploading, UploadFailed:
	default:
		return u, fmt.Errorf("%w: the upload is %s", ErrUploadState, u.State)
	}
	var n int64
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM bundle_upload_chunks WHERE upload_id = ?", u.ID).Scan(&n); err != nil {
		return u, err
	}
	if n != u.Chunks() {
		return u, fmt.Errorf("%w: %d of %d chunks received", ErrUploadState, n, u.Chunks())
	}
	ok, err := t.setUploadState(ctx, u.ID, UploadQueued, []string{u.State}, ", completed_by = ?, attempts = 0, error = NULL", actor)
	if err != nil {
		return u, err
	}
	if !ok {
		return u, fmt.Errorf("%w: it changed meanwhile", ErrUploadState)
	}
	return t.GetBundleUploadByID(ctx, u.ID)
}

// AbortUpload aborts an upload not importing.
func (t *Tx) AbortUpload(ctx context.Context, id string) (bool, error) {
	return t.setUploadState(ctx, id, UploadAborted, []string{UploadUploading, UploadQueued, UploadFailed}, "")
}

// TakeUpload moves a queued upload to importing with a new import id, counting the attempt.
func (t *Tx) TakeUpload(ctx context.Context, id, importID string) (bool, error) {
	return t.setUploadState(ctx, id, UploadImporting, []string{UploadQueued}, ", import_id = ?, attempts = attempts + 1", importID)
}

// RequeueUpload puts an importing upload back in the queue (an interrupted import); retake is a
// lost-lease upload's: from importing too.
func (t *Tx) RequeueUpload(ctx context.Context, id string) (bool, error) {
	return t.setUploadState(ctx, id, UploadQueued, []string{UploadImporting}, "")
}

// FinishUpload ends an importing upload: imported or failed, with an error class and the input's
// SHA-256.
func (t *Tx) FinishUpload(ctx context.Context, id, state, errClass, inputSHA string) (bool, error) {
	return t.setUploadState(ctx, id, state, []string{UploadImporting}, ", error = ?, input_sha256 = ?", nullable(errClass), nullable(inputSHA))
}

// FailUpload fails an upload in one of from (a refusal, attempts used up).
func (t *Tx) FailUpload(ctx context.Context, id, errClass string, from ...string) (bool, error) {
	return t.setUploadState(ctx, id, UploadFailed, from, ", error = ?", nullable(errClass))
}

// UploadsIn lists the ids of uploads in a state, oldest first (the runner's).
func (s *Store) UploadsIn(ctx context.Context, state string, limit int) ([]BundleUpload, error) {
	var out []BundleUpload
	err := s.eachRow(ctx, "SELECT "+uploadCols+" FROM bundle_uploads WHERE state = ? ORDER BY updated_at, id"+s.limit(limit),
		[]any{state}, func(sc func(...any) error) error {
			u, err := scanUpload(scanner(sc))
			if err == nil {
				out = append(out, u)
			}
			return err
		})
	return out, err
}

// ExpireUploads expires, by conditional updates, the uploads of a domain's tenants still
// uploading past ttl from their creation or last chunk, or maxAge from their creation, and the
// failed ones past ttl from their failure; it answers how many.
func (s *Store) ExpireUploads(ctx context.Context, domain string, now time.Time, ttl, maxAge time.Duration) (int64, error) {
	var n int64
	err := s.tx(ctx, nil, func(t *Tx) error {
		ofDomain := "tenant_id IN (SELECT id FROM tenants WHERE storage_domain = ?)"
		res, err := t.exec(ctx, `UPDATE bundle_uploads SET state = ?, updated_at = ? WHERE `+ofDomain+` AND state = ?
AND (created_at < ? OR (created_at < ? AND (last_chunk_at IS NULL OR last_chunk_at < ?)))`, UploadExpired, t.s.d.timeArg(now), domain,
			UploadUploading, t.s.d.timeArg(now.Add(-maxAge)), t.s.d.timeArg(now.Add(-ttl)), t.s.d.timeArg(now.Add(-ttl)))
		if err != nil {
			return err
		}
		a, _ := res.RowsAffected()
		res, err = t.exec(ctx, `UPDATE bundle_uploads SET state = ?, updated_at = ? WHERE `+ofDomain+` AND state = ? AND updated_at < ?`,
			UploadExpired, t.s.d.timeArg(now), domain, UploadFailed, t.s.d.timeArg(now.Add(-ttl)))
		if err != nil {
			return err
		}
		b, _ := res.RowsAffected()
		n = a + b
		return nil
	})
	return n, err
}

// DoneChunks lists up to limit chunks of a domain's uploads that are imported, aborted or expired:
// their objects go, then their rows.
func (s *Store) DoneChunks(ctx context.Context, domain string, limit int) ([]UploadChunk, error) {
	var out []UploadChunk
	err := s.eachRow(ctx, `SELECT c.upload_id, c.n, c.object_key, c.size, c.sha256, c.received_at FROM bundle_upload_chunks c
JOIN bundle_uploads u ON u.id = c.upload_id JOIN tenants t ON t.id = u.tenant_id WHERE t.storage_domain = ? AND u.state IN (?, ?, ?)
ORDER BY c.upload_id, c.n`+s.limit(limit), []any{domain, UploadImported, UploadAborted, UploadExpired}, func(sc func(...any) error) error {
		var c UploadChunk
		if err := sc(&c.UploadID, &c.N, &c.Key, &c.Size, &c.SHA256, scanTime{&c.ReceivedAt}); err != nil {
			return err
		}
		out = append(out, c)
		return nil
	})
	return out, err
}

// DeleteChunkRow deletes a chunk's row once its object is gone.
func (s *Store) DeleteChunkRow(ctx context.Context, uploadID string, n int64) error {
	return s.tx(ctx, nil, func(t *Tx) error {
		_, err := t.exec(ctx, "DELETE FROM bundle_upload_chunks WHERE upload_id = ? AND n = ?", uploadID, n)
		return err
	})
}

// ChunkKeyKnown reports whether a chunk's row names an object (the collector's orphan check).
func (s *Store) ChunkKeyKnown(ctx context.Context, key string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT COUNT(*) FROM bundle_upload_chunks WHERE object_key = ?"), key).Scan(&n)
	return n > 0, err
}

// AbortTenantUploads aborts a deleting tenant's uploads that are not importing.
func (t *Tx) AbortTenantUploads(ctx context.Context, tenantID string) error {
	_, err := t.exec(ctx, "UPDATE bundle_uploads SET state = ?, updated_at = ? WHERE tenant_id = ? AND state IN (?, ?, ?)",
		UploadAborted, t.s.d.timeArg(t.Now()), tenantID, UploadUploading, UploadQueued, UploadFailed)
	return err
}
