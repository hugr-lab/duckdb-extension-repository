package attachment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Service attaches, lists, reads and removes a tenant's documents (spec 0011), and serves the
// statements its feed offers (phase 1b).
type Service struct {
	Store   *store.Store
	Blob    *blob.Service
	Authz   authz.Authorizer
	MaxSize int64 // a document's largest size; 0: 4 MiB
	// Keys signs statements with the tenant's attestation key, and PublicURL names the tenant in
	// them (issuer); without either, no document can be marked feedable.
	Keys      *keys.Service
	PublicURL string
}

// DefaultMaxSize is a document's largest size unless configured (spec 0011: up to 16 MiB).
const DefaultMaxSize = 4 << 20

// MaxConfigurableSize is the largest attachments.max_size.
const MaxConfigurableSize = 16 << 20

func (s *Service) maxSize() int64 {
	if s.MaxSize > 0 {
		return s.MaxSize
	}
	return DefaultMaxSize
}

func lock(t store.Tenant) string { return "kista/attachments/" + t.ID }

func subject(ext, id string) string { return "ext:" + ext + "/attachment:" + id }

// allowed asks the authorizer: a refusal is false, any other error an error.
func (s *Service) allowed(ctx context.Context, a authz.Actor, verb authz.Verb, r authz.Resource) (bool, error) {
	err := s.Authz.Allow(ctx, a, verb, r)
	if errors.Is(err, authz.ErrDenied) {
		return false, nil
	}
	return err == nil, err
}

// administers reports whether an actor administers ext's documents: a tenant administrator, or an
// administrator of the extension whose grant has no channel (an attachment is shown in every
// channel, so a right on one must not change what another shows).
func (s *Service) administers(ctx context.Context, a authz.Actor, tenant, ext string) (bool, error) {
	return s.allowed(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, Extension: ext})
}

// reads reports whether an actor reads every document of ext: its administrators, and the
// tenant's auditors.
func (s *Service) reads(ctx context.Context, a authz.Actor, tenant, ext string) (bool, error) {
	if ok, err := s.administers(ctx, a, tenant, ext); ok || err != nil {
		return ok, err
	}
	return s.allowed(ctx, a, authz.VerbAudit, authz.Resource{Tenant: tenant})
}

// AttachInput is a document to attach.
type AttachInput struct {
	BodyHash, Kind, MediaType, Title, Supersedes string
	Content                                      []byte
}

// Attach stores a document about a body of ext. Administrators (tenant-wide, or of the extension
// without a channel) attach any kind as the tenant; a holder of publish on the extension without a
// channel attaches an SBOM or provenance, as itself, for a body it published.
func (s *Service) Attach(ctx context.Context, a authz.Actor, tenant, ext string, in AttachInput) (store.Attachment, error) {
	origin := store.AttachedByTenant
	admin, err := s.administers(ctx, a, tenant, ext)
	if err != nil {
		return store.Attachment{}, err
	}
	if !admin {
		kind, err := s.Store.ReservedFor(ctx, tenant, ext) // spec 0030: DuckDB's, the tenant's, its upstreams'
		if err != nil {
			return store.Attachment{}, err
		}
		ok, err := s.allowed(ctx, a, authz.VerbPublish, authz.Resource{Tenant: tenant, Extension: ext, Reserved: kind != ""})
		if err != nil {
			return store.Attachment{}, err
		}
		if !ok {
			return store.Attachment{}, authz.ErrDenied
		}
		origin = store.AttachedByPublisher
		if in.Kind != store.KindSBOM && in.Kind != store.KindProvenance {
			return store.Attachment{}, fmt.Errorf("%w: a publisher attaches an SBOM or provenance", store.ErrInvalid)
		}
	}
	if err := release.ValidName(ext); err != nil {
		return store.Attachment{}, err
	}
	if !hexHash(in.BodyHash) {
		return store.Attachment{}, fmt.Errorf("%w: body_hash is 64 lowercase hex", store.ErrInvalid)
	}
	mt, err := CanonicalMediaType(in.Kind, in.MediaType)
	if err != nil {
		return store.Attachment{}, err
	}
	if err := ValidTitle(in.Title); err != nil {
		return store.Attachment{}, err
	}
	if int64(len(in.Content)) > s.maxSize() || len(in.Content) == 0 {
		return store.Attachment{}, fmt.Errorf("%w: a document is 1 byte to %d bytes", store.ErrInvalid, s.maxSize())
	}
	summary, err := Summarize(in.Kind, mt, in.Content)
	if err != nil {
		return store.Attachment{}, fmt.Errorf("%w: %w", store.ErrInvalid, err)
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return store.Attachment{}, err
	}
	if in.Supersedes != "" {
		// one of ext's documents, of the same origin, and a publisher's own: one answer for every
		// other case, so the check tells nothing of documents elsewhere
		prev, err := s.ofExtension(ctx, t, ext, in.Supersedes)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return store.Attachment{}, err
		}
		if err != nil || prev.Issuer != "" || prev.Origin != origin || origin == store.AttachedByPublisher && prev.AttachedBy != a.String() {
			return store.Attachment{}, fmt.Errorf("%w: supersedes names none of the documents this caller may supersede", store.ErrInvalid)
		}
	}
	rec := store.Attachment{ID: store.NewID(), TenantID: t.ID, BodyHash: in.BodyHash, Kind: in.Kind, MediaType: mt, Title: in.Title,
		Size: int64(len(in.Content)), ContentHash: blob.ContentHash(in.Content), Summary: summary, Origin: origin,
		AttachedBy: a.String(), Visibility: store.VisibleAdministrators, Supersedes: in.Supersedes}
	// the object first: a record never names a missing object; an object without a record is collected
	if err := s.Blob.PutAttachment(ctx, t.StorageDomain, rec.ID, in.Content); err != nil {
		return store.Attachment{}, err
	}
	err = s.Store.InTenantTx(ctx, t.ID, []string{lock(t)}, func(tx *store.Tx) error {
		held, err := tx.TouchBuild(ctx, t.ID, ext, in.BodyHash)
		if err != nil {
			return err
		}
		if !held {
			return fmt.Errorf("%w: body %s of %s", store.ErrNotFound, in.BodyHash, ext)
		}
		if origin == store.AttachedByPublisher {
			ok, err := tx.PublishedBy(ctx, t.ID, ext, in.BodyHash, a.String())
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: a publisher attaches to a body it published", store.ErrInvalid)
			}
		}
		if err := tx.InsertAttachment(ctx, &rec); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "attachment.add", subject(ext, rec.ID),
			map[string]any{"body_hash": rec.BodyHash, "kind": rec.Kind, "origin": rec.Origin, "size": rec.Size})
	})
	if err != nil {
		// refused: the object goes now, unless a commit reported as failed did happen (a retry is then
		// refused) and the record names it; on any other error the object stays, and the collector
		// takes it after the grace if no record names it
		if errors.Is(err, store.ErrInvalid) || errors.Is(err, store.ErrNotFound) {
			ctx := context.WithoutCancel(ctx)
			if _, gerr := s.Store.GetAttachment(ctx, t.ID, rec.ID); errors.Is(gerr, store.ErrNotFound) {
				_ = s.Blob.DeleteAttachment(ctx, t.StorageDomain, rec.ID)
			}
		}
		return store.Attachment{}, err
	}
	return rec, nil
}

func hexHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// List answers every document of ext's bodies (management): its administrators and the tenant's
// auditors.
func (s *Service) List(ctx context.Context, a authz.Actor, tenant, ext string) ([]store.Attachment, error) {
	t, err := s.reader(ctx, a, tenant, ext)
	if err != nil {
		return nil, err
	}
	return s.Store.ExtensionAttachments(ctx, t.ID, ext)
}

func (s *Service) reader(ctx context.Context, a authz.Actor, tenant, ext string) (store.Tenant, error) {
	ok, err := s.reads(ctx, a, tenant, ext)
	if err != nil {
		return store.Tenant{}, err
	}
	if !ok {
		return store.Tenant{}, authz.ErrDenied
	}
	return s.Store.GetTenant(ctx, tenant)
}

// Get answers one document of ext's bodies (management).
func (s *Service) Get(ctx context.Context, a authz.Actor, tenant, ext, id string) (store.Attachment, error) {
	t, err := s.reader(ctx, a, tenant, ext)
	if err != nil {
		return store.Attachment{}, err
	}
	return s.ofExtension(ctx, t, ext, id)
}

// ofExtension reads a document and checks it is one of ext's bodies' (a body under two names is two
// Builds: its documents are each name's).
func (s *Service) ofExtension(ctx context.Context, t store.Tenant, ext, id string) (store.Attachment, error) {
	all, err := s.Store.ExtensionAttachments(ctx, t.ID, ext)
	if err != nil {
		return store.Attachment{}, err
	}
	for _, x := range all {
		if x.ID == id {
			return x, nil
		}
	}
	return store.Attachment{}, fmt.Errorf("%w: attachment %s", store.ErrNotFound, id)
}

// Content hands a document and its content, checked against its record, to use (the content is
// held in memory until use returns).
func (s *Service) Content(ctx context.Context, a authz.Actor, tenant, ext, id string, use func(store.Attachment, []byte) error) error {
	t, err := s.reader(ctx, a, tenant, ext)
	if err != nil {
		return err
	}
	rec, err := s.ofExtension(ctx, t, ext, id)
	if err != nil {
		return err
	}
	return s.Blob.ReadAttachment(ctx, t.StorageDomain, rec.ID, rec.ContentHash, rec.Size, func(b []byte) error { return use(rec, b) })
}

// ReadContent hands a document's content to use, for the index, whose caller the API has already
// checked may read it.
func (s *Service) ReadContent(ctx context.Context, t store.Tenant, rec store.Attachment, use func([]byte) error) error {
	return s.Blob.ReadAttachment(ctx, t.StorageDomain, rec.ID, rec.ContentHash, rec.Size, use)
}

// SetFlags changes a document's visibility and, for the feed (1b), whether it is offered: a tenant
// administrator's (expected: its version, 0 for any).
func (s *Service) SetFlags(ctx context.Context, a authz.Actor, tenant, ext, id string, visibility *string, feedable *bool, expected int64) (store.Attachment, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
		return store.Attachment{}, err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return store.Attachment{}, err
	}
	rec, err := s.ofExtension(ctx, t, ext, id)
	if err != nil {
		return store.Attachment{}, err
	}
	if expected != 0 && expected != rec.Version {
		return store.Attachment{}, store.ErrConflict
	}
	if feedable != nil && *feedable && rec.Origin == store.AttachedByFeed {
		return store.Attachment{}, fmt.Errorf("%w: a feed's document is not offered onward", store.ErrInvalid)
	}
	if visibility != nil && *visibility == store.VisibleReaders && rec.Origin == store.AttachedByFeed && Audience(rec) != store.VisibleReaders {
		// the source signed who may see it: a document for administrators stays theirs
		return store.Attachment{}, fmt.Errorf("%w: its source offers it to administrators only", store.ErrInvalid)
	}
	var out store.Attachment
	err = s.Store.InTenantTx(ctx, t.ID, []string{lock(t)}, func(tx *store.Tx) error {
		cur := rec // a copy: a retried transaction starts again from what was read
		out = cur
		v, f := cur.Visibility, cur.Feedable
		if visibility != nil {
			v = *visibility
		}
		if feedable != nil {
			f = *feedable
		}
		if v == cur.Visibility && f == cur.Feedable {
			return nil
		}
		// spec 0011 phase 1b: marking a document feedable issues its statement, a change of what it
		// says while offered (its audience) a new one, un-marking its withdrawal. Each is signed; only
		// un-marking goes ahead unsigned, and only when the tenant cannot sign at all (no active
		// attestation key, no public URL): the feed then leaves the document out. A signer that
		// fails now fails the change, to be tried again.
		var seq int64
		if f || cur.Feedable {
			next := cur
			next.Visibility = v
			var err error
			if seq, err = s.issue(ctx, tx, t, next, !f); err != nil {
				if f || !errors.Is(err, store.ErrInvalid) {
					return err
				}
				seq = 0
			}
		}
		change := map[string]any{}
		if v != cur.Visibility {
			change["visibility"] = map[string]string{"old": cur.Visibility, "new": v}
		}
		if f != cur.Feedable {
			change["feedable"] = map[string]bool{"old": cur.Feedable, "new": f}
		}
		if err := tx.SetAttachmentFlags(ctx, &cur, v, f, expected); err != nil {
			return err
		}
		out = cur
		data := map[string]any{"body_hash": cur.BodyHash, "kind": cur.Kind, "origin": cur.Origin, "change": change}
		if seq != 0 {
			data["statement"] = seq
		}
		return tx.Event(ctx, t.ID, a.String(), "attachment.change", subject(ext, cur.ID), data)
	})
	if err != nil {
		return store.Attachment{}, err
	}
	return out, nil
}

// Remove removes a document: a tenant administrator's, or an administrator of the extension without
// a channel.
func (s *Service) Remove(ctx context.Context, a authz.Actor, tenant, ext, id string) error {
	ok, err := s.administers(ctx, a, tenant, ext)
	if err != nil {
		return err
	}
	if !ok {
		return authz.ErrDenied
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return err
	}
	rec, err := s.ofExtension(ctx, t, ext, id)
	if err != nil {
		return err
	}
	return s.remove(ctx, a, t, subject(ext, rec.ID), rec)
}

func (s *Service) remove(ctx context.Context, a authz.Actor, t store.Tenant, subj string, rec store.Attachment) error {
	err := s.Store.InTenantTx(ctx, t.ID, []string{lock(t)}, func(tx *store.Tx) error {
		// read again under the lock: a document marked feedable meanwhile is withdrawn
		rec, err := tx.GetAttachment(ctx, t.ID, rec.ID)
		if err != nil {
			return err
		}
		if !rec.RemovedAt.IsZero() {
			return fmt.Errorf("%w: attachment %s", store.ErrNotFound, rec.ID)
		}
		if _, err := tx.RemoveAttachment(ctx, rec); err != nil {
			return err
		}
		data := map[string]any{"body_hash": rec.BodyHash, "kind": rec.Kind, "origin": rec.Origin, "size": rec.Size}
		if rec.Feedable { // an offered document's removal is its withdrawal, when the tenant can sign
			seq, err := s.issue(ctx, tx, t, rec, true)
			if err != nil && !errors.Is(err, store.ErrInvalid) {
				return err
			}
			if err == nil {
				data["statement"] = seq
			}
		}
		return tx.Event(ctx, t.ID, a.String(), "attachment.remove", subj, data)
	})
	if err != nil {
		return err
	}
	// the object after the record: an object without a record is collected anyway
	return s.Blob.DeleteAttachment(ctx, t.StorageDomain, rec.ID)
}

// BlockedDocuments lists the documents of a blocked body (a tenant administrator's: its Builds may
// be gone, spec 0011).
func (s *Service) BlockedDocuments(ctx context.Context, a authz.Actor, tenant, hash string) (store.Tenant, []store.Attachment, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
		return store.Tenant{}, nil, err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return t, nil, err
	}
	if blocked, err := s.Store.Blocked(ctx, t.ID, hash); err != nil || !blocked {
		if err == nil {
			err = fmt.Errorf("%w: block %s", store.ErrNotFound, hash)
		}
		return t, nil, err
	}
	all, err := s.Store.BodyAttachments(ctx, t.ID, hash)
	return t, all, err
}

// RemoveBlockedDocument removes a document of a blocked body.
func (s *Service) RemoveBlockedDocument(ctx context.Context, a authz.Actor, tenant, hash, id string) error {
	t, all, err := s.BlockedDocuments(ctx, a, tenant, hash)
	if err != nil {
		return err
	}
	for _, rec := range all {
		if rec.ID == id {
			return s.remove(ctx, a, t, "block:"+hash+"/attachment:"+id, rec)
		}
	}
	return fmt.Errorf("%w: attachment %s", store.ErrNotFound, id)
}

// Audience is what a feed's document's signed statement says of who may see it ("" if unreadable).
func Audience(rec store.Attachment) string {
	var st struct {
		Audience string `json:"audience"`
	}
	if json.Unmarshal([]byte(rec.Statement), &st) != nil {
		return ""
	}
	return st.Audience
}

// Heads keeps the documents that head their chains: none supersedes them (spec 0011).
func Heads(all []store.Attachment) []store.Attachment {
	superseded := map[[2]string]bool{}
	for _, a := range all {
		if a.Supersedes != "" {
			superseded[[2]string{a.Issuer, a.Supersedes}] = true
		}
	}
	var out []store.Attachment
	for _, a := range all {
		if !superseded[[2]string{a.Issuer, a.IssuerID}] {
			out = append(out, a)
		}
	}
	return out
}

// Readable keeps what readers see: chain heads, readers-visible, not withdrawn, of bodies in their
// view (spec 0011: never an older version in a head's place).
func Readable(all []store.Attachment, bodies map[string]bool) []store.Attachment {
	var out []store.Attachment
	for _, a := range Heads(all) {
		if a.Visibility == store.VisibleReaders && a.WithdrawnAt.IsZero() && bodies[a.BodyHash] {
			out = append(out, a)
		}
	}
	return out
}

// Expired reports whether a document is past its valid_until.
func Expired(a store.Attachment, now time.Time) bool {
	return !a.ValidUntil.IsZero() && now.After(a.ValidUntil)
}

// KindsOf are the kinds of documents, in the catalogue's order.
func KindsOf(as []store.Attachment) []string {
	var out []string
	for _, k := range Kinds {
		if slices.ContainsFunc(as, func(a store.Attachment) bool { return a.Kind == k }) {
			out = append(out, k)
		}
	}
	return out
}
