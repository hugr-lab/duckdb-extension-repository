package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// DefaultDomain is the storage domain of tenants created without one.
const DefaultDomain = "default"

var domainRule = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)

// ValidDomain checks a storage domain name: [a-z][a-z0-9-]{0,15}.
func ValidDomain(name string) error {
	if !domainRule.MatchString(name) {
		return fmt.Errorf("%w: storage domain %q must match [a-z][a-z0-9-]{0,15}", ErrInvalid, name)
	}
	return nil
}

var hexHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Blob is the record of one stored body in a domain (spec 0005): the trust root for its stream.
type Blob struct {
	Domain       string
	BodyHash     string // lowercase hex of the composite body hash
	StreamHash   string // lowercase hex of the composite stream hash; the object is streams/<StreamHash>
	BodyLen      int64
	BodyCRC32    uint32
	StreamLen    int64
	StreamChunks []byte    // 32 bytes per MiB of stream
	CorruptAt    time.Time // zero unless serving found the object changed
	CreatedAt    time.Time
	CommittedAt  time.Time // the latest commit; a corruption mark applies only to the commit it saw
}

func (b *Blob) valid() error {
	switch {
	case ValidDomain(b.Domain) != nil:
		return ValidDomain(b.Domain)
	case !hexHash.MatchString(b.BodyHash) || !hexHash.MatchString(b.StreamHash):
		return fmt.Errorf("%w: blob hashes must be 64 lowercase hex", ErrInvalid)
	case b.BodyLen < 0 || b.StreamLen <= 0:
		return fmt.Errorf("%w: blob lengths", ErrInvalid)
	case int64(len(b.StreamChunks)) != (b.StreamLen+(1<<20)-1)>>20*32:
		return fmt.Errorf("%w: %d bytes of chunk digests for a %d-byte stream", ErrInvalid, len(b.StreamChunks), b.StreamLen)
	}
	return nil
}

const blobCols = "domain, body_hash, stream_hash, body_len, body_crc32, stream_len, stream_chunks, corrupt_at, created_at, committed_at"

func scanBlob(r interface{ Scan(...any) error }) (Blob, error) {
	var b Blob
	var crc int64
	err := r.Scan(&b.Domain, &b.BodyHash, &b.StreamHash, &b.BodyLen, &crc, &b.StreamLen, &b.StreamChunks,
		scanTime{&b.CorruptAt}, scanTime{&b.CreatedAt}, scanTime{&b.CommittedAt})
	if err == nil && (crc < 0 || crc > 0xffffffff) {
		err = errors.New("store: blob crc32 out of range")
	}
	b.BodyCRC32 = uint32(crc)
	if err == nil {
		err = b.valid()
	}
	return b, err
}

// GetBlob returns the record of a body in a domain.
func (s *Store) GetBlob(ctx context.Context, domain, bodyHash string) (Blob, error) {
	b, err := scanBlob(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+blobCols+" FROM blobs WHERE domain = ? AND body_hash = ?"), domain, bodyHash))
	return b, notFound(err, "blob")
}

// MarkBlobCorrupt records that the object of rec's stream did not match. It changes nothing if the
// record has been committed again since rec was read (the commit uploaded the stream anew).
func (s *Store) MarkBlobCorrupt(ctx context.Context, rec Blob) error {
	return s.tx(ctx, nil, func(t *Tx) error {
		_, err := t.exec(ctx, "UPDATE blobs SET corrupt_at = ? WHERE domain = ? AND body_hash = ? AND stream_hash = ? AND committed_at = ? AND corrupt_at IS NULL",
			t.s.d.timeArg(t.Now()), rec.Domain, rec.BodyHash, rec.StreamHash, t.s.d.timeArg(rec.CommittedAt))
		return err
	})
}

// PutBlob records a committed stream. If the body already has a record, it keeps the record when it
// names the same stream and replaces it otherwise; either way corrupt_at is cleared, since the
// caller has just uploaded the stream. b is updated to what is stored.
func (t *Tx) PutBlob(ctx context.Context, b *Blob) error {
	if err := b.valid(); err != nil {
		return err
	}
	now := t.Now()
	cur, err := scanBlob(t.queryRow(ctx, "SELECT "+blobCols+" FROM blobs WHERE domain = ? AND body_hash = ?", b.Domain, b.BodyHash))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		b.CorruptAt, b.CreatedAt, b.CommittedAt = time.Time{}, now, now
		_, err := t.exec(ctx, "INSERT INTO blobs ("+blobCols+") VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)",
			b.Domain, b.BodyHash, b.StreamHash, b.BodyLen, int64(b.BodyCRC32), b.StreamLen, b.StreamChunks,
			t.s.d.timeArg(now), t.s.d.timeArg(now))
		return t.s.mapErr(err, "blob")
	case err != nil:
		return err
	case cur.StreamHash == b.StreamHash:
		_, err := t.exec(ctx, "UPDATE blobs SET corrupt_at = NULL, committed_at = ? WHERE domain = ? AND body_hash = ?",
			t.s.d.timeArg(now), b.Domain, b.BodyHash)
		cur.CorruptAt, cur.CommittedAt = time.Time{}, now
		*b = cur
		return err
	}
	b.CorruptAt, b.CreatedAt, b.CommittedAt = time.Time{}, cur.CreatedAt, now
	_, err = t.exec(ctx, "UPDATE blobs SET stream_hash = ?, body_len = ?, body_crc32 = ?, stream_len = ?, stream_chunks = ?, corrupt_at = NULL, committed_at = ? WHERE domain = ? AND body_hash = ?",
		b.StreamHash, b.BodyLen, int64(b.BodyCRC32), b.StreamLen, b.StreamChunks, t.s.d.timeArg(now), b.Domain, b.BodyHash)
	return err
}

// StorageDomain is a domain pinned to its store.
type StorageDomain struct {
	Name, Kind, StoreID string
	CreatedAt           time.Time
}

// GetStorageDomain reads a pinned domain.
func (t *Tx) GetStorageDomain(ctx context.Context, name string) (StorageDomain, error) {
	var d StorageDomain
	err := t.queryRow(ctx, "SELECT name, kind, store_id, created_at FROM storage_domains WHERE name = ?", name).
		Scan(&d.Name, &d.Kind, &d.StoreID, scanTime{&d.CreatedAt})
	return d, notFound(err, "storage domain "+name)
}

// PinStorageDomain inserts a domain's pin.
func (t *Tx) PinStorageDomain(ctx context.Context, d *StorageDomain) error {
	if err := ValidDomain(d.Name); err != nil {
		return err
	}
	d.CreatedAt = t.Now()
	_, err := t.exec(ctx, "INSERT INTO storage_domains (name, kind, store_id, created_at) VALUES (?, ?, ?, ?)",
		d.Name, d.Kind, d.StoreID, t.s.d.timeArg(d.CreatedAt))
	return t.s.mapErr(err, "storage domain "+d.Name)
}

// DeploymentID returns this deployment's id, creating it on first use.
func (t *Tx) DeploymentID(ctx context.Context) (string, error) {
	var id string
	err := t.queryRow(ctx, "SELECT id FROM deployment WHERE one = 1").Scan(&id)
	if !errors.Is(err, sql.ErrNoRows) {
		return id, err
	}
	id = NewID()
	_, err = t.exec(ctx, "INSERT INTO deployment (one, id, created_at) VALUES (1, ?, ?)", id, t.s.d.timeArg(t.Now()))
	return id, t.s.mapErr(err, "deployment")
}
