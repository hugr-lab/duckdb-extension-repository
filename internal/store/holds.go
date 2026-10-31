package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Spec 0033 phase A2: holds on sources (a publisher, an eager mirror) and on channels.

// MaxHold is the longest hold a source or a channel may have.
var MaxHold = 90 * 24 * time.Hour

// ValidHold says whether d is a hold: whole seconds, 0 (none) up to MaxHold.
func ValidHold(d time.Duration) error {
	if d < 0 || d > MaxHold || d%time.Second != 0 {
		return fmt.Errorf("%w: a hold is a duration of whole seconds from 0 (none) to %s", ErrInvalid, MaxHold)
	}
	return nil
}

func seconds(d time.Duration) int64 { return int64(d / time.Second) }

// ErrSourceGone is an arrival from a source that is gone: a removed upstream or publisher, or the
// publisher key that signed a publication, removed while it was in flight (spec 0033 phase A2: fail
// closed, a revoked key's upload reaches nothing).
var ErrSourceGone = errors.New("store: the source is gone (its upstream, publisher or key was removed)")

// SourceHold is a source's hold: a publisher's, or an eager mirror's (a lazy or pull-through
// upstream fetches because someone is installing now, and takes none); 0 for the tenant's own source.
// A distributor's publisher's is the larger of its own and the tenant's minimum for distributors'
// publishers (spec 0037). It reads the source's row (and then the tenant's) with a shared lock held
// until commit, so a hold raised (RaiseHeld, after the row's update) is never missed by a build
// inserted in the same transaction; a removed upstream or publisher is ErrSourceGone (fail closed).
func (t *Tx) SourceHold(ctx context.Context, source string) (time.Duration, error) {
	var hold int64
	var err error
	switch {
	case strings.HasPrefix(source, SourcePublisherPrefix):
		var tenantID string
		var dist sql.NullString
		err = t.queryRow(ctx, t.lockedRowQuery("SELECT hold_seconds, tenant_id, distributor FROM publishers", "id = ?", false),
			strings.TrimPrefix(source, SourcePublisherPrefix)).Scan(&hold, &tenantID, &dist)
		if err == nil && dist.Valid {
			// spec 0037: a distributor's publisher's entries are held at least for the tenant's
			// minimum, read with a shared lock on the tenant's row, which raising it updates
			var floor int64
			err = t.queryRow(ctx, t.lockedTenantQuery("SELECT distributor_hold_seconds FROM tenants", "id = ?", false), tenantID).Scan(&floor)
			hold = max(hold, floor)
		}
	case strings.HasPrefix(source, SourceUpstreamPrefix):
		var mode, fetch string
		err = t.queryRow(ctx, t.lockedRowQuery("SELECT hold_seconds, mode, fetch_mode FROM upstreams", "id = ?", false),
			strings.TrimPrefix(source, SourceUpstreamPrefix)).Scan(&hold, &mode, &fetch)
		if mode != ModeMirror || fetch != FetchEager {
			hold = 0
		}
	default:
		return 0, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s", ErrSourceGone, source)
	}
	return time.Duration(hold) * time.Second, err
}

// PublicationLive checks that a publication's publisher and the key that signed it (keyID, "" for
// none) still exist, reading their rows with a shared lock held until commit, which removing the key
// or the publisher waits for (spec 0033 phase A2): the publication's build transaction (which also
// holds the tenant's auth lock, PublisherAuthLock) and its release's insert both check, so an upload
// in flight when its key is revoked, or its publisher removed, is refused (ErrSourceGone) instead of
// landing, or being released, after the removal.
func (t *Tx) PublicationLive(ctx context.Context, source, keyID string) error {
	id, ok := strings.CutPrefix(source, SourcePublisherPrefix)
	if !ok {
		return nil
	}
	var got string
	err := t.queryRow(ctx, t.lockedRowQuery("SELECT id FROM publishers", "id = ?", false), id).Scan(&got)
	if err == nil && keyID != "" {
		err = t.queryRow(ctx, t.lockedRowQuery("SELECT id FROM publisher_keys", "id = ? AND publisher_id = ?", false), keyID, id).Scan(&got)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrSourceGone, source)
	}
	return err
}

// PublisherAuthLock is the lock that orders a tenant's publishers, keys and grants (their changes,
// spec 0017), which a publication's build transaction takes too (PublicationLive).
func PublisherAuthLock(tenantID string) string { return "kista/tenant-auth/" + tenantID }

// SourceHold reads a source's hold outside a transaction.
func (s *Store) SourceHold(ctx context.Context, source string) (time.Duration, error) {
	var d time.Duration
	err := s.InTx(ctx, "", func(tx *Tx) error {
		var err error
		d, err = tx.SourceHold(ctx, source)
		return err
	})
	return d, err
}

// SetUpstreamHold sets an upstream's hold (expected: its version, 0 for the one read).
func (t *Tx) SetUpstreamHold(ctx context.Context, u *Upstream, d time.Duration, expected int64) error {
	if err := ValidHold(d); err != nil {
		return err
	}
	if expected == 0 {
		expected = u.Version
	}
	return t.cas(ctx, "UPDATE upstreams SET hold_seconds = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{seconds(d), u.ID, expected}, func() { u.Hold, u.Version = d, expected+1 })
}

// SetPublisherHold sets a tenant's publisher's hold, and returns the publisher as it was.
func (t *Tx) SetPublisherHold(ctx context.Context, tenantID, name string, d time.Duration) (Publisher, error) {
	if err := ValidHold(d); err != nil {
		return Publisher{}, err
	}
	p, err := t.GetPublisher(ctx, tenantID, name)
	if err != nil {
		return p, err
	}
	if _, err := t.exec(ctx, "UPDATE publishers SET hold_seconds = ? WHERE id = ?", seconds(d), p.ID); err != nil {
		return p, err
	}
	return p, t.BumpAuth(ctx, tenantID) // the tenant's cached publishers carry it
}

// DefaultDistributorHold is a tenant's minimum hold for distributors' publishers until its
// administrators set one (spec 0037: the column's default).
const DefaultDistributorHold = 24 * time.Hour

// SetDistributorHold sets a tenant's minimum hold for distributors' publishers (spec 0037) and returns
// the one it had. The caller holds the tenant's row with an update lock (InTenantTxForUpdate): an
// arrival reads it with a shared lock (SourceHold), so the two are ordered.
func (t *Tx) SetDistributorHold(ctx context.Context, tenantID string, d time.Duration) (time.Duration, error) {
	if err := ValidHold(d); err != nil {
		return 0, err
	}
	var before int64
	if err := t.queryRow(ctx, "SELECT distributor_hold_seconds FROM tenants WHERE id = ?", tenantID).Scan(&before); err != nil {
		return 0, notFound(err, "tenant")
	}
	if before != seconds(d) {
		if _, err := t.exec(ctx, "UPDATE tenants SET distributor_hold_seconds = ? WHERE id = ?", seconds(d), tenantID); err != nil {
			return 0, err
		}
	}
	return time.Duration(before) * time.Second, nil
}

// DistributorPublisherSources are the sources of a tenant's distributors' publishers, live and
// removed (spec 0037: the tenant's minimum hold applies to each), by id.
func (t *Tx) DistributorPublisherSources(ctx context.Context, tenantID string) ([]string, error) {
	ids, err := t.strings(ctx, `SELECT id FROM publishers WHERE tenant_id = ? AND distributor IS NOT NULL
UNION SELECT publisher_id FROM removed_distributor_publishers WHERE tenant_id = ? ORDER BY 1`, tenantID, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = PublisherSource(id)
	}
	return out, nil
}

// SetChannelHold sets a channel's hold and returns the one it had. The placement pass looks at the
// channel's rules again (a shorter hold ends sooner), and its snapshot is renewed (a channel with a
// hold asks no lazy or pull-through upstream). The caller holds the channel's lock.
func (t *Tx) SetChannelHold(ctx context.Context, channelID string, d time.Duration) (time.Duration, error) {
	if err := ValidHold(d); err != nil {
		return 0, err
	}
	var before int64
	if err := t.queryRow(ctx, "SELECT hold_seconds FROM channels WHERE id = ?", channelID).Scan(&before); err != nil {
		return 0, notFound(err, "channel")
	}
	if before == seconds(d) {
		return d, nil
	}
	if _, err := t.exec(ctx, "UPDATE channels SET hold_seconds = ? WHERE id = ?", seconds(d), channelID); err != nil {
		return 0, err
	}
	if err := t.RecheckRules(ctx, channelID); err != nil {
		return 0, err
	}
	return time.Duration(before) * time.Second, t.BumpReleaseVersion(ctx, channelID)
}

// RaiseHeld applies a raised hold to a tenant's entries of a source that are held now: each is held
// until its arrival plus the hold, if that is later (spec 0033: lowering a hold releases nothing
// already held). It returns how many entries it held longer.
func (t *Tx) RaiseHeld(ctx context.Context, tenantID, source string, d time.Duration) (int, error) {
	type held struct {
		id      string
		created time.Time
		until   time.Time
	}
	var hs []held
	now := t.Now()
	rows, err := t.query(ctx, "SELECT id, created_at, available_at FROM builds WHERE tenant_id = ? AND source = ? AND available_at > ?",
		tenantID, source, t.s.d.timeArg(now))
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.id, scanTime{&h.created}, scanTime{&h.until}); err != nil {
			rows.Close()
			return 0, err
		}
		hs = append(hs, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, h := range hs {
		if until := h.created.Add(d); until.After(h.until) {
			if _, err := t.exec(ctx, "UPDATE builds SET available_at = ? WHERE id = ?", t.s.d.timeArg(until), h.id); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// BuildAvailability reads a build's arrival, when it becomes available (zero: on arrival) and when it
// was revoked (zero: not), for the check a release insert makes under the channel lock; ErrBuildGone
// when the collector removed it since the intake found it (spec 0016: the intake starts again).
func (t *Tx) BuildAvailability(ctx context.Context, id string) (created, available, revoked time.Time, err error) {
	err = t.queryRow(ctx, "SELECT created_at, available_at, revoked_at FROM builds WHERE id = ?", id).
		Scan(scanTime{&created}, scanTime{&available}, scanTime{&revoked})
	if errors.Is(err, sql.ErrNoRows) {
		return created, available, revoked, fmt.Errorf("%w: %s", ErrBuildGone, id)
	}
	return created, available, revoked, err
}

// HeldAtArrival marks a mirror's entry that its run left to the placement pass, held at its arrival
// (spec 0033 phase A2): an entry held by its home channel's hold only (its source's hold set
// available_at already) gets available_at = its arrival, so it is not held by itself (another
// channel takes it as soon as its own hold allows) but the pass knows to place it in the mirror's
// home channel, as it does an entry its source held (store.DueRules, release's home rule).
func (t *Tx) HeldAtArrival(ctx context.Context, buildID string) error {
	_, err := t.exec(ctx, "UPDATE builds SET available_at = created_at WHERE id = ? AND available_at IS NULL", buildID)
	return err
}

// KeptEntry is a held entry a purge kept (PurgeHeld): released, or released once, so it is marked
// revoked instead; Channels is how many of the tenant's channels hold a release of it.
type KeptEntry struct {
	Build
	Channels int
}

// PurgeHeld deletes a tenant's entries of a publisher that are held now and were never released
// (spec 0033: revoking a key, or removing the publisher, before its builds reach a channel), those
// published with one of its keys when keyID is set, with their licence-key records. An entry placed
// early (a channel administrator's choice), or released once, stays, marked revoked: no rule, follow
// or promotion takes it again, only a channel administrator's early placement. It returns the entries
// it deleted and those it kept. The caller holds the tenant's upstream lock (TenantUpstreamLock),
// which every release into the tenant's signed channels takes: no placement releases an entry between
// the check and the deletion.
func (t *Tx) PurgeHeld(ctx context.Context, tenantID, source, keyID string) ([]Build, []KeptEntry, error) {
	now := t.Now()
	// a withdrawn entry is left as it is (spec 0033 phase B: its record stays, and it reaches no channel)
	q, args := "SELECT "+buildCols+" FROM builds WHERE tenant_id = ? AND source = ? AND available_at > ? AND withdrawn_at IS NULL",
		[]any{tenantID, source, t.s.d.timeArg(now)}
	if keyID != "" {
		q, args = q+" AND publisher_key_id = ?", append(args, keyID)
	}
	rows, err := t.query(ctx, q+" ORDER BY created_at, id", args...)
	if err != nil {
		return nil, nil, err
	}
	var held []Build
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		held = append(held, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var purged []Build
	var kept []KeptEntry
	for _, b := range held {
		err := t.entryPlaced(ctx, b)
		if errors.Is(err, ErrConflict) {
			k := KeptEntry{Build: b}
			if err := t.queryRow(ctx, "SELECT COUNT(DISTINCT channel_id) FROM releases WHERE build_id = ?", b.ID).Scan(&k.Channels); err != nil {
				return nil, nil, err
			}
			if !b.Revoked() {
				if _, err := t.exec(ctx, "UPDATE builds SET revoked_at = ? WHERE id = ?", t.s.d.timeArg(now), b.ID); err != nil {
					return nil, nil, err
				}
				k.RevokedAt = now
			}
			kept = append(kept, k)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		d, err := t.DeleteEntry(ctx, tenantID, b.ID)
		if err != nil {
			return nil, nil, err
		}
		purged = append(purged, d)
	}
	return purged, kept, nil
}
