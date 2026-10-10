package release

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"slices"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// ErrSignature is an upstream file whose signature verifies with none of the upstream's keys.
var ErrSignature = errors.New("release: the signature does not verify with the upstream's keys")

// Ingest is one cell of an upstream's matrix (spec 0009): what was fetched and what it must be.
type Ingest struct {
	Name, DuckDBVersion, Platform string
	Upstream, URL                 string   // for the event (spec 0010)
	Versions                      []string // accepted extension versions; none: any
	Keys                          []*rsa.PublicKey
	Visibility                    string
	DryRun                        bool // check only: nothing is committed or released
	// Provenance makes the release's provenance from the fingerprint of the key that verified.
	Provenance func(key string) string
}

// Ingested is an intake's result: the release (new or the slot's), whether it existed, and the
// key that verified.
type Ingested struct {
	Release store.Release
	Existed bool
	Key     string
	Hash    string
}

// Ingest takes a spooled upstream file into a signed channel (spec 0009): the signature with the
// upstream's keys, the footer against the cell, the version filter, spec 0008's file checks, the
// tenant's block, then commit, Build (origin upstream) and release under the channel lock, where
// a replacement of the name or another body in the slot refuses it and the same body in the slot
// is the existing release whatever its state. The caller has authorized the run.
func (s *Service) Ingest(ctx context.Context, a authz.Actor, sc store.ServeChannel, sp *blob.Spool, in Ingest) (Ingested, error) {
	ch := sc.Channel
	if ch.Kind == store.ChannelPassthrough {
		in.Visibility = store.Public // a passthrough channel is public by nature (spec 0001)
	}
	f := sp.File()
	key, ok := extfile.Verify(f.Hash, f.Signature, in.Keys)
	if !ok {
		return Ingested{}, ErrSignature
	}
	res := Ingested{Key: key, Hash: f.Hash.String()}
	b, err := BuildFrom(f, sc.Tenant.ID, in.Name, a.String())
	if err != nil {
		return res, err
	}
	b.Origin, b.OriginSignature = store.OriginUpstream, f.Signature
	if b.Platform != in.Platform {
		return res, fmt.Errorf("%w: the footer's platform is %q, the path's %q", store.ErrInvalid, b.Platform, in.Platform)
	}
	if b.ABI != store.ABICStruct && b.DuckDBVersion != in.DuckDBVersion {
		return res, fmt.Errorf("%w: the footer's DuckDB version is %q, the path's %q", store.ErrInvalid, b.DuckDBVersion, in.DuckDBVersion)
	}
	if len(in.Versions) > 0 && !slices.Contains(in.Versions, b.ExtVersion) {
		return res, fmt.Errorf("%w: version %s is not on the allowlist", store.ErrInvalid, b.ExtVersion)
	}
	if err := CheckFile(f, in.Name); err != nil {
		return res, err
	}
	b.Checked = true
	if blocked, err := s.Store.Blocked(ctx, sc.Tenant.ID, b.BodyHash); err != nil || blocked {
		if err == nil {
			err = ErrBlocked
		}
		return res, err
	}
	it := item{b: b, visibility: in.Visibility, origin: store.OriginUpstream, originSignature: f.Signature}
	if in.Provenance != nil {
		it.provenance = in.Provenance(key)
	}
	// the slot and the shadow before the body is stored (both are checked again under the lock)
	var existing *store.Release
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		shadowed, err := tx.LiveReplacement(ctx, ch.ID, b.Name)
		if err != nil {
			return err
		}
		if shadowed {
			return ErrShadowed
		}
		cands, err := tx.SlotCandidates(ctx, ch.ID, b.Name, b.ExtVersion, b.Platform)
		if err != nil {
			return err
		}
		existing, err = checkUpstreamSlot(cands, b)
		return err
	}); err != nil {
		return res, err
	}
	if existing != nil {
		res.Release, res.Existed = *existing, true
		return res, nil
	}
	capis, err := s.Store.ChannelCAPIs(ctx, ch.ID)
	if err != nil {
		return res, err
	}
	if err := servable(capis, b); err != nil {
		return res, err
	}
	if ch.Kind == store.ChannelSigned {
		if _, err := activeKey(ctx, s.Store, ch.ID); err != nil {
			return res, err
		}
	}
	if in.DryRun {
		return res, nil
	}
	rs, existed, err := s.commitAndRelease(ctx, sp, &it, func(ctx context.Context, it item) ([]store.Release, bool, error) {
		return s.insert(ctx, a, ch.ID, []item{it}, func(tx *store.Tx, c store.Channel, made []store.Release) error {
			r := made[0]
			return tx.Event(ctx, c.TenantID, a.String(), "upstream.release", releaseSubject(c, r),
				map[string]any{"upstream": in.Upstream, "release": r.ID, "name": r.Name, "version": r.ExtVersion, "platform": r.Platform,
					"slot": r.Slot, "body_hash": it.b.BodyHash, "url": in.URL, "key": key})
		})
	})
	if err != nil {
		return res, err
	}
	res.Release, res.Existed = rs[0], existed
	return res, nil
}
