package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Attachments (spec 0011): documents about a body in a tenant. The statement's columns (issuer,
// issuer id, key, signature, ...) are the feed's (phase 1b); a tenant's own document has issuer ""
// and its own id as issuer id.

// Attachment kinds, origins and visibilities.
const (
	KindSBOM            = "sbom"
	KindVulnerabilities = "vulnerabilities"
	KindReview          = "review"
	KindProvenance      = "provenance"
	KindNotes           = "notes"

	AttachedByTenant    = "tenant"
	AttachedByPublisher = "publisher"
	AttachedByFeed      = "feed"

	VisibleAdministrators = "administrators"
	VisibleReaders        = "readers"
)

// Limits on attachments (spec 0011).
const (
	MaxAttachmentHeads   = 20 // per (body, kind, origin): chain heads only
	MaxTenantAttachments = 100000
)

// Attachment is a document's record. Its content is the object attachments/<ID> in the tenant's
// storage domain, checked against ContentHash on every read.
type Attachment struct {
	ID, TenantID, BodyHash, Kind, MediaType, Title string
	Size                                           int64
	ContentHash                                    string // hex sha256 of the content
	Summary                                        string // json, computed at intake
	Origin, AttachedBy, Visibility                 string
	Feedable                                       bool
	Version                                        int64
	RemovedAt, CreatedAt                           time.Time
	Issuer, IssuerID, KeyFP, SourceOrigin          string
	IssuedAt, ValidUntil                           time.Time
	Supersedes                                     string // an issuer id of the same issuer
	WithdrawnAt                                    time.Time
	Statement, Signature                           string
}

const attachmentCols = `id, tenant_id, body_hash, kind, media_type, title, size, content_hash, summary, origin, attached_by,
visibility, feedable, version, removed_at, created_at, issuer, issuer_id, key_fp, source_origin, issued_at, valid_until,
supersedes, withdrawn_at, statement, signature`

func scanAttachment(scan func(...any) error) (Attachment, error) {
	var a Attachment
	var key, src, sup, stmt, sig sql.NullString
	err := scan(&a.ID, &a.TenantID, &a.BodyHash, &a.Kind, &a.MediaType, &a.Title, &a.Size, &a.ContentHash, &a.Summary, &a.Origin,
		&a.AttachedBy, &a.Visibility, &a.Feedable, &a.Version, scanTime{&a.RemovedAt}, scanTime{&a.CreatedAt}, &a.Issuer, &a.IssuerID,
		&key, &src, scanTime{&a.IssuedAt}, scanTime{&a.ValidUntil}, &sup, scanTime{&a.WithdrawnAt}, &stmt, &sig)
	a.KeyFP, a.SourceOrigin, a.Supersedes, a.Statement, a.Signature = key.String, src.String, sup.String, stmt.String, sig.String
	return a, err
}

// TouchBuild reports whether the tenant holds a Build of name with the body, and refreshes its
// used_at in the transaction (spec 0016's intake rule: the collector cannot remove it meanwhile).
func (t *Tx) TouchBuild(ctx context.Context, tenantID, name, bodyHash string) (bool, error) {
	res, err := t.exec(ctx, "UPDATE builds SET used_at = ? WHERE tenant_id = ? AND name = ? AND body_hash = ?",
		t.s.d.timeArg(t.Now()), tenantID, name, bodyHash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PublishedBy reports whether an actor's publication released the body under name in the tenant.
func (t *Tx) PublishedBy(ctx context.Context, tenantID, name, bodyHash, actor string) (bool, error) {
	var n int
	err := t.queryRow(ctx, `SELECT COUNT(*) FROM releases r JOIN builds b ON b.id = r.build_id WHERE b.tenant_id = ? AND b.name = ?
AND b.body_hash = ? AND r.origin = ? AND r.created_by = ?`, tenantID, name, bodyHash, OriginPublication, actor).Scan(&n)
	return n > 0, err
}

// InsertAttachment adds a document's record (its object is already stored). The caller holds the
// tenant's attachment lock: the limits are counted here.
func (t *Tx) InsertAttachment(ctx context.Context, a *Attachment) error {
	var total int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM attachments WHERE tenant_id = ? AND removed_at IS NULL", a.TenantID).Scan(&total); err != nil {
		return err
	}
	if total >= MaxTenantAttachments {
		return fmt.Errorf("%w: a tenant holds at most %d attachments", ErrInvalid, MaxTenantAttachments)
	}
	if a.Supersedes != "" {
		prev, err := t.attachmentByIssuer(ctx, a.TenantID, a.Issuer, a.Supersedes)
		if err != nil {
			return fmt.Errorf("%w: the document it supersedes: %w", ErrInvalid, err)
		}
		if prev.BodyHash != a.BodyHash || prev.Kind != a.Kind || prev.Origin != a.Origin || !prev.RemovedAt.IsZero() {
			return fmt.Errorf("%w: a document supersedes one of the same body, kind and origin", ErrInvalid)
		}
		var later int
		if err := t.queryRow(ctx, "SELECT COUNT(*) FROM attachments WHERE tenant_id = ? AND issuer = ? AND supersedes = ? AND removed_at IS NULL",
			a.TenantID, a.Issuer, a.Supersedes).Scan(&later); err != nil {
			return err
		}
		if later > 0 {
			return fmt.Errorf("%w: the document it supersedes is superseded already", ErrInvalid)
		}
	} else {
		// a new chain: count the heads of (body, kind, origin)
		var heads int
		if err := t.queryRow(ctx, `SELECT COUNT(*) FROM attachments a WHERE a.tenant_id = ? AND a.body_hash = ? AND a.kind = ? AND a.origin = ?
AND a.removed_at IS NULL AND NOT EXISTS (SELECT 1 FROM attachments n WHERE n.tenant_id = a.tenant_id AND n.issuer = a.issuer
AND n.supersedes = a.issuer_id AND n.removed_at IS NULL)`, a.TenantID, a.BodyHash, a.Kind, a.Origin).Scan(&heads); err != nil {
			return err
		}
		if heads >= MaxAttachmentHeads {
			return fmt.Errorf("%w: at most %d documents of a kind per body and origin: supersede one", ErrInvalid, MaxAttachmentHeads)
		}
	}
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.IssuerID == "" {
		a.IssuerID = a.ID
	}
	a.CreatedAt, a.Version = t.Now(), 1
	if a.IssuedAt.IsZero() {
		a.IssuedAt = a.CreatedAt
	}
	_, err := t.exec(ctx, "INSERT INTO attachments ("+attachmentCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		a.ID, a.TenantID, a.BodyHash, a.Kind, a.MediaType, a.Title, a.Size, a.ContentHash, a.Summary, a.Origin, a.AttachedBy,
		a.Visibility, a.Feedable, a.Version, t.s.d.nullTimeArg(a.RemovedAt), t.s.d.timeArg(a.CreatedAt), a.Issuer, a.IssuerID,
		nullable(a.KeyFP), nullable(a.SourceOrigin), t.s.d.timeArg(a.IssuedAt), t.s.d.nullTimeArg(a.ValidUntil), nullable(a.Supersedes),
		t.s.d.nullTimeArg(a.WithdrawnAt), nullable(a.Statement), nullable(a.Signature))
	return t.s.mapErr(err, "attachment")
}

func (t *Tx) attachmentByIssuer(ctx context.Context, tenantID, issuer, issuerID string) (Attachment, error) {
	a, err := scanAttachment(t.queryRow(ctx, "SELECT "+attachmentCols+" FROM attachments WHERE tenant_id = ? AND issuer = ? AND issuer_id = ?",
		tenantID, issuer, issuerID).Scan)
	return a, notFound(err, "attachment")
}

// GetAttachment reads a tenant's document (a removed one too: its tombstone).
func (s *Store) GetAttachment(ctx context.Context, tenantID, id string) (Attachment, error) {
	a, err := scanAttachment(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+attachmentCols+" FROM attachments WHERE tenant_id = ? AND id = ?"),
		tenantID, id).Scan)
	return a, notFound(err, "attachment")
}

// GetAttachment reads a tenant's document in a transaction.
func (t *Tx) GetAttachment(ctx context.Context, tenantID, id string) (Attachment, error) {
	a, err := scanAttachment(t.queryRow(ctx, "SELECT "+attachmentCols+" FROM attachments WHERE tenant_id = ? AND id = ?", tenantID, id).Scan)
	return a, notFound(err, "attachment")
}

// ExtensionAttachments lists the documents (not removed) of the bodies the tenant holds under name,
// by body hash, kind and creation.
func (s *Store) ExtensionAttachments(ctx context.Context, tenantID, name string) ([]Attachment, error) {
	return s.listAttachments(ctx, `SELECT `+prefixed("a.", attachmentCols)+` FROM attachments a WHERE a.tenant_id = ? AND a.removed_at IS NULL
AND EXISTS (SELECT 1 FROM builds b WHERE b.tenant_id = a.tenant_id AND b.name = ? AND b.body_hash = a.body_hash)
ORDER BY a.body_hash, a.kind, a.created_at, a.id`, tenantID, name)
}

// BodyAttachments lists the documents (not removed) of one body in the tenant.
func (s *Store) BodyAttachments(ctx context.Context, tenantID, bodyHash string) ([]Attachment, error) {
	return s.listAttachments(ctx, "SELECT "+attachmentCols+" FROM attachments WHERE tenant_id = ? AND body_hash = ? AND removed_at IS NULL ORDER BY kind, created_at, id",
		tenantID, bodyHash)
}

func (s *Store) listAttachments(ctx context.Context, q string, args ...any) ([]Attachment, error) {
	var out []Attachment
	err := s.eachRow(ctx, q, args, func(sc func(...any) error) error {
		a, err := scanAttachment(sc)
		if err != nil {
			return err
		}
		out = append(out, a)
		return nil
	})
	return out, err
}

func prefixed(p, cols string) string {
	fs := strings.Split(cols, ",")
	for i, f := range fs {
		fs[i] = p + strings.TrimSpace(f)
	}
	return strings.Join(fs, ", ")
}

// SetAttachmentFlags changes a document's visibility and feedable (compare-and-set on its version;
// expected 0 takes the record's).
func (t *Tx) SetAttachmentFlags(ctx context.Context, a *Attachment, visibility string, feedable bool, expected int64) error {
	if visibility != VisibleAdministrators && visibility != VisibleReaders {
		return fmt.Errorf("%w: visibility %q", ErrInvalid, visibility)
	}
	if expected == 0 {
		expected = a.Version
	}
	return t.cas(ctx, "UPDATE attachments SET visibility = ?, feedable = ?, version = version + 1 WHERE id = ? AND version = ? AND removed_at IS NULL",
		[]any{visibility, feedable, a.ID, expected}, func() { a.Visibility, a.Feedable, a.Version = visibility, feedable, expected+1 })
}

// RemoveAttachment removes a document: a feed's leaves a tombstone (removed_at), so the feed does
// not bring it back; the tenant's own and a publisher's go. It answers whether the object is to be
// deleted.
func (t *Tx) RemoveAttachment(ctx context.Context, a Attachment) (deleteObject bool, err error) {
	if a.Origin == AttachedByFeed {
		return true, t.cas(ctx, "UPDATE attachments SET removed_at = ? WHERE id = ? AND removed_at IS NULL",
			[]any{t.s.d.timeArg(t.Now()), a.ID}, func() {})
	}
	// its successor takes over what it superseded: the chain stays one chain, and an older version
	// never becomes a head again (spec 0011)
	if _, err := t.exec(ctx, "UPDATE attachments SET supersedes = ? WHERE tenant_id = ? AND issuer = ? AND supersedes = ?",
		nullable(a.Supersedes), a.TenantID, a.Issuer, a.IssuerID); err != nil {
		return false, err
	}
	return true, t.cas(ctx, "DELETE FROM attachments WHERE id = ?", []any{a.ID}, func() {})
}

// SupersededBefore lists up to limit documents of a domain's tenants superseded before a time (spec
// 0011: superseded versions older than a year are collected).
func (s *Store) SupersededBefore(ctx context.Context, domain string, before time.Time, limit int) ([]Attachment, error) {
	return s.listAttachments(ctx, `SELECT `+prefixed("a.", attachmentCols)+` FROM attachments a JOIN tenants t ON t.id = a.tenant_id
WHERE t.storage_domain = ? AND a.removed_at IS NULL AND EXISTS (SELECT 1 FROM attachments n WHERE n.tenant_id = a.tenant_id
AND n.issuer = a.issuer AND n.supersedes = a.issuer_id AND n.removed_at IS NULL AND n.created_at < ?) ORDER BY a.id`+s.limit(limit),
		domain, s.d.timeArg(before))
}

// RemoveSuperseded removes a document the collector found superseded before a time, if it still is.
func (s *Store) RemoveSuperseded(ctx context.Context, a Attachment, before time.Time) (bool, error) {
	var ok bool
	err := s.tx(ctx, []string{"kista/attachments/" + a.TenantID}, func(t *Tx) error {
		ok = false
		var n int
		if err := t.queryRow(ctx, `SELECT COUNT(*) FROM attachments WHERE tenant_id = ? AND issuer = ? AND supersedes = ? AND removed_at IS NULL
AND created_at < ?`, a.TenantID, a.Issuer, a.IssuerID, t.s.d.timeArg(before)).Scan(&n); err != nil || n == 0 {
			return err
		}
		cur, err := t.GetAttachment(ctx, a.TenantID, a.ID)
		if errors.Is(err, ErrNotFound) || err == nil && !cur.RemovedAt.IsZero() {
			return nil // removed meanwhile
		}
		if err != nil {
			return err
		}
		if _, err := t.RemoveAttachment(ctx, cur); err != nil {
			return err
		}
		ok = true
		return nil
	})
	return ok, err
}

// AttachmentsOfGoneBodies lists up to limit documents of a domain's tenants whose body the tenant no
// longer holds (no Build) and has not blocked (spec 0011: they go with the last Build, but stay with
// a block).
func (s *Store) AttachmentsOfGoneBodies(ctx context.Context, domain string, limit int) ([]Attachment, error) {
	return s.listAttachments(ctx, `SELECT `+prefixed("a.", attachmentCols)+` FROM attachments a JOIN tenants t ON t.id = a.tenant_id
WHERE t.storage_domain = ? AND NOT EXISTS (SELECT 1 FROM builds b WHERE b.tenant_id = a.tenant_id AND b.body_hash = a.body_hash)
AND NOT EXISTS (SELECT 1 FROM blocks k WHERE k.tenant_id = a.tenant_id AND k.body_hash = a.body_hash) ORDER BY a.id`+s.limit(limit), domain)
}

// DeleteAttachmentRecord deletes a record the collector found orphaned, if its body is still gone.
func (s *Store) DeleteAttachmentRecord(ctx context.Context, a Attachment) (bool, error) {
	var ok bool
	err := s.tx(ctx, nil, func(t *Tx) error {
		res, err := t.exec(ctx, `DELETE FROM attachments WHERE id = ? AND NOT EXISTS (SELECT 1 FROM builds b WHERE b.tenant_id = ? AND b.body_hash = ?)
AND NOT EXISTS (SELECT 1 FROM blocks k WHERE k.tenant_id = ? AND k.body_hash = ?)`, a.ID, a.TenantID, a.BodyHash, a.TenantID, a.BodyHash)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		ok = n > 0
		return err
	})
	return ok, err
}

// AttachmentRecordsExist answers which of ids have a record (removed ones included: a tombstone keeps
// no object, but an object is never collected while its record exists).
func (s *Store) AttachmentRecordsExist(ctx context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	q := "SELECT id FROM attachments WHERE id IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ") + ")"
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	err := s.eachRow(ctx, q, args, func(sc func(...any) error) error {
		var id string
		if err := sc(&id); err != nil {
			return err
		}
		out[id] = true
		return nil
	})
	return out, err
}
