package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Entries' icons (spec 0036): icons/<sha256> in the tenant's storage domain, content-addressed, so
// tenants sharing a domain share an object sent to both. A write follows spec 0016's protocol for
// streams in tables of its own: the caller checks the image, PutIcon claims the hash (waiting a
// while if the collector is deleting it) and uploads the object, always, and the row's transaction
// commits the claim (store.Tx.CommitIcon) under the icon's lock. The collector marks the icons no
// entry's metadata of the domain's tenants references, and deletes them a grace later.

// iconClaimWait bounds how long an icon's write waits on the collector deleting the same icon: an
// API request, it answers busy (ErrStreamBusy) rather than hang.
var iconClaimWait = 10 * time.Second

// IconKey is the key of an icon: its SHA-256.
func IconKey(sha256 string) string { return "icons/" + sha256 }

// PutIcon claims an icon in a domain and uploads it, and returns its hash and the claim, which the
// caller commits with its row or releases (ReleaseIcon).
func (s *Service) PutIcon(ctx context.Context, domain string, data []byte) (sha256, claimID string, err error) {
	d, err := s.domain(domain)
	if err != nil {
		return "", "", err
	}
	sha256, claimID = ContentHash(data), store.NewID()
	if err := s.claim(ctx, s.st.Icons(), domain, sha256, claimID, iconClaimWait); err != nil {
		return "", "", err
	}
	if err := d.Store.Put(ctx, IconKey(sha256), bytes.NewReader(data), int64(len(data))); err != nil {
		_ = s.ReleaseIcon(context.WithoutCancel(ctx), domain, sha256, claimID)
		return "", "", err
	}
	return sha256, claimID, nil
}

// ReleaseIcon releases an icon's claim (a write giving up).
func (s *Service) ReleaseIcon(ctx context.Context, domain, sha256, claimID string) error {
	return s.st.Icons().ReleaseClaim(ctx, domain, sha256, claimID)
}

// ReadIcon reads an icon and checks it against its row: its size and its hash. A missing object, or
// one that does not match, is ErrCorrupt (the store is not trusted).
func (s *Service) ReadIcon(ctx context.Context, domain, sha256 string, size int64) ([]byte, error) {
	d, err := s.domain(domain)
	if err != nil {
		return nil, err
	}
	r, err := d.Store.Get(ctx, IconKey(sha256), 0, -1) // to its end: a longer object does not match
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: icon %s is missing", ErrCorrupt, sha256)
		}
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(r, size+1))
	_ = r.Close()
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != size || ContentHash(b) != sha256 {
		return nil, fmt.Errorf("%w: icon %s does not match its row", ErrCorrupt, sha256)
	}
	return b, nil
}
