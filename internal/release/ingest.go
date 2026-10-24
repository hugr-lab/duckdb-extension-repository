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
	UpstreamID                    string   // the release's source (spec 0031)
	Versions                      []string // accepted extension versions; none: any
	Keys                          []*rsa.PublicKey
	Visibility                    string
	DryRun                        bool   // check only: nothing is committed or released
	Bundle                        string // the bundle's manifest id, for the event (spec 0014)
	Trigger                       string // what made the fetch, for the event (spec 0032): run, sync or ask
	// KeySource is the upstream's id when it is a trusted source of licence keys (a repository
	// upstream, spec 0021): a new release records the build's keys for it. "": none.
	KeySource string
	// Provenance makes the release's provenance from the fingerprint of the key that verified.
	Provenance func(key string) string
	// ReserveKind reserves the name (core or community) with ReserveSource when the release is
	// inserted: a build mirrored from DuckDB's own repositories brings its name (spec 0030). "": none.
	ReserveKind, ReserveSource string
	// AnyKind is the upstream's kind when "*" took the name (spec 0009 phase 2, spec 0030): the
	// release insert checks again, under its locks, that no reservation came since. "": listed.
	AnyKind string
}

// Ingested is an intake's result: the release (new or the slot's), whether it existed, and the
// key that verified. Landed is a build kept in the catalogue and placed nowhere: the channel's rule
// for the name is manual and holds no release of its version (spec 0033).
type Ingested struct {
	Release store.Release
	Existed bool
	Landed  bool
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
	if len(in.Versions) > 0 && !slices.Contains(in.Versions, b.PackageVersion) {
		return res, fmt.Errorf("%w: version %s is not on the allowlist", store.ErrInvalid, b.PackageVersion)
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
	it := item{b: b, visibility: in.Visibility, origin: store.OriginUpstream, originSignature: f.Signature, source: store.UpstreamSource(in.UpstreamID),
		reserveKind: in.ReserveKind, reserveSource: in.ReserveSource, anyKind: in.AnyKind}
	it.b.Source = it.source
	if in.Provenance != nil {
		it.provenance = in.Provenance(key)
	}
	if in.KeySource != "" && f.LicenseKeys.Err == nil && len(f.LicenseKeys.Keys) > 0 {
		it.keys, it.keyKind, it.keySources = licenseKeys(f), store.KeySourceUpstream, []string{in.KeySource}
	}
	// the slot and the name's source before the body is stored (both are checked again under the lock)
	var existing *store.Release
	selected := false     // the name has a source the release keeps (spec 0031)
	pinned := false       // a manual rule of this source that holds no release of the version (spec 0033)
	var completes *served // a manual rule's held version: completed as the placement pass would
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		var succeeds string
		if selected, succeeds, err = checkSource(ctx, tx, ch.ID, b.Name, it.source, true); err != nil {
			return err
		}
		rule, ok, err := tx.ChannelSourceOf(ctx, ch.ID, b.Name)
		if err != nil {
			return err
		}
		manual := ok && rule.Mode == store.RuleManual
		if ok && rule.Mode == store.RuleFollow { // a follow's rule: manual once the channel no longer follows
			following, err := tx.FollowingTx(ctx, ch.ID, rule.Source)
			if err != nil {
				return err
			}
			manual = !following
		}
		// a successor answers to the rule the removed upstream had (its releases become the successor's)
		if manual && ch.Kind == store.ChannelSigned && (rule.Source == it.source || rule.Source == succeeds && succeeds != "") {
			rels, err := tx.SourceReleases(ctx, ch.ID, rule.Source, b.Name)
			if err != nil {
				return err
			}
			capis, err := tx.ChannelCAPIs(ctx, ch.ID)
			if err != nil {
				return err
			}
			sv := newServed(rels, capis)
			if pinned = !sv.held[b.PackageVersion]; !pinned {
				completes = &sv
			}
		}
		cands, err := tx.SlotCandidates(ctx, ch.ID, it.source, b.Name, b.PackageVersion, b.Platform)
		if err != nil {
			return err
		}
		existing, err = checkUpstreamSlot(cands, b)
		return err
	}); err != nil {
		return res, err
	}
	// a name without a source goes through the insert, which sets it (spec 0031)
	if existing != nil && selected {
		// spec 0021: the body was fetched, so a trusted source records the keys it declares on the
		// slot's build, which may predate them (an earlier release)
		if len(it.keySources) > 0 && !in.DryRun {
			bb := it.b
			if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
				if err := tx.FindOrInsertBuild(ctx, &bb); err != nil {
					return err
				}
				return recordKeys(ctx, tx, bb, it)
			}); err != nil {
				return res, err
			}
		}
		res.Release, res.Existed = *existing, true
		return res, nil
	}
	if err := s.Store.SlotTaken(ctx, it.b); err != nil { // spec 0033: before the body is stored
		return res, slotErr(store.OriginUpstream, err)
	}
	if err := s.room(ctx, it.b, f.BodySize, false); err != nil { // spec 0033: the source's catalogue
		return res, err
	}
	if pinned {
		// the channel's administrator chose its versions by hand: the build waits in the catalogue
		res.Landed = true
		if in.DryRun {
			return res, nil
		}
		return res, slotErr(store.OriginUpstream, s.land(ctx, sp, &it, nil))
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
	if completes != nil {
		completes.complete(&it)
	}
	rs, existed, err := s.commitAndRelease(ctx, sp, &it, func(ctx context.Context, it item) ([]store.Release, bool, error) {
		return s.insert(ctx, a, ch.ID, []item{it}, func(tx *store.Tx, c store.Channel, made []store.Release) error {
			r := made[0]
			data := map[string]any{"upstream": in.Upstream, "release": r.ID, "name": r.Name, "version": r.PackageVersion, "platform": r.Platform,
				"slot": r.Slot, "body_hash": it.b.BodyHash, "url": in.URL, "key": key}
			if in.Bundle != "" {
				data["bundle"] = in.Bundle
			}
			if in.Trigger != "" {
				data["trigger"] = in.Trigger
			}
			return tx.Event(ctx, c.TenantID, a.String(), "upstream.release", releaseSubject(c, r), data)
		})
	})
	if err != nil {
		return res, err
	}
	res.Release, res.Existed = rs[0], existed
	return res, nil
}
