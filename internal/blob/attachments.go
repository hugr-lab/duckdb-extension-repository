package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"
)

// Attachment contents (spec 0011): one object per attachment, attachments/<id>, no deduplication; a
// read is checked against the record's content hash before anything is returned, since the store
// is not trusted. Objects without a record are collected like tmp/ leftovers.

// ContentHash is an attachment's content hash: hex sha256.
func ContentHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// PutAttachment stores an attachment's content (before its record is inserted).
func (s *Service) PutAttachment(ctx context.Context, domain, id string, content []byte) error {
	d, err := s.domain(domain)
	if err != nil {
		return err
	}
	return d.Store.Put(ctx, AttachmentKey(id), bytes.NewReader(content), int64(len(content)))
}

// ReadAttachment reads an attachment's content, checks it against the record (its size and its
// hash; a mismatch, or a missing object, is ErrCorrupt) and hands it to use: the content is held in
// memory until use returns, one of at most MaxAttachmentReads at once.
func (s *Service) ReadAttachment(ctx context.Context, domain, id, contentHash string, size int64, use func([]byte) error) error {
	d, err := s.domain(domain)
	if err != nil {
		return err
	}
	select {
	case s.reads <- struct{}{}:
		defer func() { <-s.reads }()
	case <-ctx.Done():
		return ctx.Err()
	}
	r, err := d.Store.Get(ctx, AttachmentKey(id), 0, size)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: attachment %s is missing", ErrCorrupt, id)
		}
		return err
	}
	b, err := io.ReadAll(io.LimitReader(r, size+1))
	_ = r.Close()
	if err != nil {
		return err
	}
	if int64(len(b)) != size || ContentHash(b) != contentHash {
		return fmt.Errorf("%w: attachment %s does not match its record", ErrCorrupt, id)
	}
	return use(b)
}

// DeleteAttachment deletes an attachment's content (after its record is removed); a missing object
// is no error.
func (s *Service) DeleteAttachment(ctx context.Context, domain, id string) error {
	d, err := s.domain(domain)
	if err != nil {
		return err
	}
	if err := d.Store.Delete(ctx, AttachmentKey(id)); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// attachments is the collector's step for them: the records of bodies the tenant no longer holds
// (and has not blocked) go with their contents; objects without a record older than the cutoff go.
func (c *Collector) attachments(ctx context.Context, d Domain, cutoff time.Time, r *Result) error {
	st := c.Service.st
	for {
		gone, err := st.AttachmentsOfGoneBodies(ctx, d.Name, gcBatch)
		if err != nil || len(gone) == 0 {
			if err != nil {
				return err
			}
			break
		}
		removed := 0
		for _, a := range gone {
			if r.DryRun {
				r.Attachments++
				continue
			}
			ok, err := st.DeleteAttachmentRecord(ctx, a)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			removed++
			r.Attachments++
			r.Bytes += a.Size
			if err := d.Store.Delete(ctx, AttachmentKey(a.ID)); err != nil && !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("blob: domain %s: deleting an attachment: %w", d.Name, err)
			}
		}
		if r.DryRun || removed == 0 {
			break // a dry run deletes nothing, so the same page would come back
		}
		if err := c.renew(ctx, d.Name); err != nil {
			return err
		}
	}
	// superseded versions older than a year go (spec 0011), a page a pass
	year := st.Now().AddDate(-1, 0, 0)
	superseded, err := st.SupersededBefore(ctx, d.Name, year, gcBatch)
	if err != nil {
		return err
	}
	for _, a := range superseded {
		if r.DryRun {
			r.Attachments++
			continue
		}
		ok, err := st.RemoveSuperseded(ctx, a, year)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		r.Attachments++
		r.Bytes += a.Size
		// a feed's document keeps a tombstone record; its object goes all the same
		if err := d.Store.Delete(ctx, AttachmentKey(a.ID)); err != nil && !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("blob: domain %s: deleting an attachment: %w", d.Name, err)
		}
	}
	type obj struct {
		id   string
		size int64
	}
	var old []obj
	err = d.Store.List(ctx, "attachments/", func(key string, size int64, modified time.Time) error {
		if !modified.IsZero() && modified.Before(cutoff) {
			old = append(old, obj{key[len("attachments/"):], size})
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("blob: domain %s: listing attachments/: %w", d.Name, err)
	}
	for i := 0; i < len(old); i += gcBatch {
		page := old[i:min(i+gcBatch, len(old))]
		ids := make([]string, len(page))
		for j, o := range page {
			ids[j] = o.id
		}
		have, err := st.AttachmentRecordsExist(ctx, ids)
		if err != nil {
			return err
		}
		for _, o := range page {
			if have[o.id] {
				continue
			}
			if !r.DryRun {
				if err := d.Store.Delete(ctx, AttachmentKey(o.id)); err != nil && !errors.Is(err, ErrNotFound) {
					return fmt.Errorf("blob: domain %s: deleting an attachment: %w", d.Name, err)
				}
			}
			r.Attachments++
			r.Bytes += o.size
		}
	}
	return nil
}
