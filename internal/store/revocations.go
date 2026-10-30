package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"
)

// Revocation states (spec 0039), strongest first: what a kista's -/revocations route shows of a
// release, and what an upstream stores and applies of it.
const (
	RevokeWithdrawn = "withdrawn"
	RevokeBlocked   = "blocked"
	RevokeYanked    = "yanked"
)

// RevocationRank orders revocation states: withdrawn > blocked > yanked > anything else (0).
func RevocationRank(state string) int {
	switch state {
	case RevokeWithdrawn:
		return 3
	case RevokeBlocked:
		return 2
	case RevokeYanked:
		return 1
	}
	return 0
}

// What an upstream's yank record was made by (spec 0039): its block's sweep, or a yank.
const (
	YankCauseBlock = "block"
	YankCauseYank  = "yank"
)

// MaxRevocationReason is the most bytes of a revocation's reason kept.
const MaxRevocationReason = 400

// CleanReason makes a remote's reason storable (spec 0039): printable runes only (others become a
// space), cut at a UTF-8 boundary at most MaxRevocationReason bytes. It never refuses.
func CleanReason(s string) string {
	b := make([]byte, 0, min(len(s), MaxRevocationReason))
	for _, r := range s {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			r = ' '
		}
		if len(b)+utf8.RuneLen(r) > MaxRevocationReason {
			break
		}
		b = utf8.AppendRune(b, r)
	}
	return string(b)
}

// UpstreamRevocation is a remote's revocation of one of an upstream's builds, as the upstream last
// saw it (spec 0039), with what was applied and lifted of it.
type UpstreamRevocation struct {
	UpstreamID, BuildID, TenantID  string
	BodyHash, Name, PackageVersion string
	Platform, State, Reason        string
	SeenAt                         time.Time
	AppliedState, LiftedState      string // "": nothing applied, nothing lifted
	AppliedAt, LiftedAt            time.Time
}

// Due reports whether a stored revocation is to be applied: its state is stronger than what was
// applied of it, and than what an administrator lifted of it.
func (r UpstreamRevocation) Due() bool {
	rank := RevocationRank(r.State)
	return rank > RevocationRank(r.AppliedState) && rank > RevocationRank(r.LiftedState)
}

const revocationCols = `upstream_id, build_id, tenant_id, body_hash, name, package_version, platform, state, reason, seen_at,
applied_state, applied_at, lifted_state, lifted_at`

func scanRevocation(r interface{ Scan(...any) error }) (UpstreamRevocation, error) {
	var x UpstreamRevocation
	var applied, lifted sql.NullString
	err := r.Scan(&x.UpstreamID, &x.BuildID, &x.TenantID, &x.BodyHash, &x.Name, &x.PackageVersion, &x.Platform, &x.State, &x.Reason,
		scanTime{&x.SeenAt}, &applied, scanTime{&x.AppliedAt}, &lifted, scanTime{&x.LiftedAt})
	x.AppliedState, x.LiftedState = applied.String, lifted.String
	return x, err
}

// SetUpstreamRevocations switches applying the remote's revocations on or off (compare-and-set on
// the upstream's version; 0 for its current one).
func (t *Tx) SetUpstreamRevocations(ctx context.Context, u *Upstream, on bool, expected int64) error {
	if expected == 0 {
		expected = u.Version
	}
	return t.cas(ctx, "UPDATE upstreams SET revocations = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{on, u.ID, expected}, func() { u.Revocations, u.Version = on, expected+1 })
}

// SetUpstreamRemote records what a pass learned of the remote: whether it is a kista, and the
// revocations route's ETag. Neither is the administrators' change: the version stays.
func (t *Tx) SetUpstreamRemote(ctx context.Context, upstreamID string, kista bool, etag string) error {
	_, err := t.exec(ctx, "UPDATE upstreams SET remote_kista = ?, revocations_etag = ? WHERE id = ?", kista, nullable(etag), upstreamID)
	return err
}

// UpstreamBuilds maps an upstream's builds in a tenant by (name, extension version, platform, body
// hash) to their ids: what a remote's revocation rows match (spec 0039).
func (s *Store) UpstreamBuilds(ctx context.Context, tenantID, upstreamID string) (map[[4]string]string, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind("SELECT id, name, package_version, platform, body_hash FROM builds WHERE tenant_id = ? AND source = ?"),
		tenantID, UpstreamSource(upstreamID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[4]string]string{}
	for rows.Next() {
		var id, n, v, p, h string
		if err := rows.Scan(&id, &n, &v, &p, &h); err != nil {
			return nil, err
		}
		out[[4]string{n, v, p, h}] = id
	}
	return out, rows.Err()
}

const revocationsQuery = "SELECT " + revocationCols + " FROM upstream_revocations WHERE upstream_id = ? ORDER BY name, package_version, platform, build_id"

// UpstreamRevocations lists an upstream's stored revocations by name, version, platform and build.
func (s *Store) UpstreamRevocations(ctx context.Context, upstreamID string) ([]UpstreamRevocation, error) {
	return revocations(s.db.QueryContext(ctx, s.d.rebind(revocationsQuery), upstreamID))
}

// UpstreamRevocations is the transactional form.
func (t *Tx) UpstreamRevocations(ctx context.Context, upstreamID string) ([]UpstreamRevocation, error) {
	return revocations(t.query(ctx, revocationsQuery, upstreamID))
}

func revocations(rows *sql.Rows, err error) ([]UpstreamRevocation, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpstreamRevocation
	for rows.Next() {
		r, err := scanRevocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetUpstreamRevocation reads one stored revocation in a transaction.
func (t *Tx) GetUpstreamRevocation(ctx context.Context, upstreamID, buildID string) (UpstreamRevocation, error) {
	r, err := scanRevocation(t.queryRow(ctx, "SELECT "+revocationCols+" FROM upstream_revocations WHERE upstream_id = ? AND build_id = ?",
		upstreamID, buildID))
	return r, notFound(err, "revocation of build "+buildID)
}

// PutUpstreamRevocation stores the remote's state and reason of a build as last seen: a new row, or
// the stored one's state, reason and seen_at (what was applied and lifted stay).
func (t *Tx) PutUpstreamRevocation(ctx context.Context, r UpstreamRevocation) error {
	now := t.s.d.timeArg(t.Now())
	res, err := t.exec(ctx, "UPDATE upstream_revocations SET state = ?, reason = ?, seen_at = ? WHERE upstream_id = ? AND build_id = ?",
		r.State, r.Reason, now, r.UpstreamID, r.BuildID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = t.exec(ctx, "INSERT INTO upstream_revocations ("+revocationCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL, NULL)",
		r.UpstreamID, r.BuildID, r.TenantID, r.BodyHash, r.Name, r.PackageVersion, r.Platform, r.State, r.Reason, now)
	return t.s.mapErr(err, "revocation")
}

// DeleteUpstreamRevocation forgets a stored revocation (the remote no longer lists it, or its build
// is gone); what it applied stays applied.
func (t *Tx) DeleteUpstreamRevocation(ctx context.Context, upstreamID, buildID string) error {
	_, err := t.exec(ctx, "DELETE FROM upstream_revocations WHERE upstream_id = ? AND build_id = ?", upstreamID, buildID)
	return err
}

// MarkRevocationApplied records the state a revocation was applied at.
func (t *Tx) MarkRevocationApplied(ctx context.Context, upstreamID, buildID, state string) error {
	_, err := t.exec(ctx, "UPDATE upstream_revocations SET applied_state = ?, applied_at = ? WHERE upstream_id = ? AND build_id = ?",
		state, t.s.d.timeArg(t.Now()), upstreamID, buildID)
	return err
}

// MarkRevocationLifted records an administrator's lift of a revocation at its applied state.
func (t *Tx) MarkRevocationLifted(ctx context.Context, upstreamID, buildID, state string) error {
	_, err := t.exec(ctx, "UPDATE upstream_revocations SET lifted_state = ?, lifted_at = ? WHERE upstream_id = ? AND build_id = ?",
		state, t.s.d.timeArg(t.Now()), upstreamID, buildID)
	return err
}

// UpstreamBlock is an upstream's block of a body (spec 0039): the body is refused from that
// upstream, and its builds' releases with the body are yanked.
type UpstreamBlock struct {
	UpstreamID, BodyHash, TenantID, State, Reason string
	CreatedAt                                     time.Time
}

// PutUpstreamBlock records an upstream's block of a body, or strengthens the stored one's state and
// reason.
func (t *Tx) PutUpstreamBlock(ctx context.Context, b UpstreamBlock) error {
	var cur string
	err := t.queryRow(ctx, "SELECT state FROM upstream_blocks WHERE upstream_id = ? AND body_hash = ?", b.UpstreamID, b.BodyHash).Scan(&cur)
	switch {
	case err == nil:
		if RevocationRank(b.State) <= RevocationRank(cur) {
			return nil
		}
		_, err = t.exec(ctx, "UPDATE upstream_blocks SET state = ?, reason = ? WHERE upstream_id = ? AND body_hash = ?",
			b.State, b.Reason, b.UpstreamID, b.BodyHash)
		return err
	case err != sql.ErrNoRows:
		return err
	}
	_, err = t.exec(ctx, "INSERT INTO upstream_blocks (upstream_id, body_hash, tenant_id, state, reason, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		b.UpstreamID, b.BodyHash, b.TenantID, b.State, b.Reason, t.s.d.timeArg(t.Now()))
	return t.s.mapErr(err, "upstream block")
}

// DeleteUpstreamBlock lifts an upstream's block of a body; it reports whether there was one.
func (t *Tx) DeleteUpstreamBlock(ctx context.Context, upstreamID, hash string) (bool, error) {
	res, err := t.exec(ctx, "DELETE FROM upstream_blocks WHERE upstream_id = ? AND body_hash = ?", upstreamID, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// UpstreamBlocked reports whether an upstream blocked a body (checked under the channel lock with a
// release's insert, as blocks are).
func (t *Tx) UpstreamBlocked(ctx context.Context, upstreamID, hash string) (bool, error) {
	var n int
	err := t.queryRow(ctx, "SELECT COUNT(*) FROM upstream_blocks WHERE upstream_id = ? AND body_hash = ?", upstreamID, hash).Scan(&n)
	return n > 0, err
}

// UpstreamBlocked is the read-only form, outside a transaction.
func (s *Store) UpstreamBlocked(ctx context.Context, upstreamID, hash string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT COUNT(*) FROM upstream_blocks WHERE upstream_id = ? AND body_hash = ?"),
		upstreamID, hash).Scan(&n)
	return n > 0, err
}

// UpstreamBlocks lists a tenant's upstream blocks: their state and reason by (upstream, body hash).
func (s *Store) UpstreamBlocks(ctx context.Context, tenantID string) (map[[2]string]UpstreamBlock, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind("SELECT upstream_id, body_hash, tenant_id, state, reason, created_at FROM upstream_blocks WHERE tenant_id = ?"),
		tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]string]UpstreamBlock{}
	for rows.Next() {
		var b UpstreamBlock
		if err := rows.Scan(&b.UpstreamID, &b.BodyHash, &b.TenantID, &b.State, &b.Reason, scanTime{&b.CreatedAt}); err != nil {
			return nil, err
		}
		out[[2]string{b.UpstreamID, b.BodyHash}] = b
	}
	return out, rows.Err()
}

// UpstreamYank is a release an upstream yanked (spec 0039), with its state before: what a lift
// restores.
type UpstreamYank struct {
	ReleaseID, TenantID, UpstreamID, BuildID, BodyHash string
	PriorState, Cause                                  string
	ChannelID                                          string // read with UpstreamYanksOf: the release's channel ("": gone)
}

// RecordUpstreamYank records a release an upstream's block or yank yanked; a release recorded
// already keeps its first record (its state before the first yank).
func (t *Tx) RecordUpstreamYank(ctx context.Context, y UpstreamYank) error {
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM upstream_yanks WHERE release_id = ?", y.ReleaseID).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := t.exec(ctx, "INSERT INTO upstream_yanks (release_id, tenant_id, upstream_id, build_id, body_hash, prior_state, cause) VALUES (?, ?, ?, ?, ?, ?, ?)",
		y.ReleaseID, y.TenantID, y.UpstreamID, y.BuildID, y.BodyHash, y.PriorState, y.Cause)
	return t.s.mapErr(err, "upstream yank")
}

// UpstreamYanksOf lists the releases an upstream yanked of a build, with their channels.
func (s *Store) UpstreamYanksOf(ctx context.Context, upstreamID, buildID string) ([]UpstreamYank, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind(`SELECT y.release_id, y.tenant_id, y.upstream_id, y.build_id, y.body_hash, y.prior_state, y.cause,
r.channel_id FROM upstream_yanks y LEFT JOIN releases r ON r.id = y.release_id WHERE y.upstream_id = ? AND y.build_id = ? ORDER BY y.release_id`),
		upstreamID, buildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpstreamYank
	for rows.Next() {
		var y UpstreamYank
		var ch sql.NullString
		if err := rows.Scan(&y.ReleaseID, &y.TenantID, &y.UpstreamID, &y.BuildID, &y.BodyHash, &y.PriorState, &y.Cause, &ch); err != nil {
			return nil, err
		}
		y.ChannelID = ch.String
		out = append(out, y)
	}
	return out, rows.Err()
}

// DeleteUpstreamYank forgets one record.
func (t *Tx) DeleteUpstreamYank(ctx context.Context, releaseID string) error {
	_, err := t.exec(ctx, "DELETE FROM upstream_yanks WHERE release_id = ?", releaseID)
	return err
}

// DeleteUpstreamYanksWithBody forgets the records of a tenant's releases of a body: an
// administrator's block takes over (spec 0039), so no later lift restores what it blocked.
func (t *Tx) DeleteUpstreamYanksWithBody(ctx context.Context, tenantID, hash string) error {
	_, err := t.exec(ctx, "DELETE FROM upstream_yanks WHERE tenant_id = ? AND body_hash = ?", tenantID, hash)
	return err
}

// UpstreamYankStates maps a tenant's upstream-yanked releases to their record (the -/revocations
// route shows the revocation's state for them).
func (s *Store) UpstreamYankStates(ctx context.Context, tenantID string) (map[string]UpstreamYank, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind("SELECT release_id, tenant_id, upstream_id, build_id, body_hash, prior_state, cause FROM upstream_yanks WHERE tenant_id = ?"),
		tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]UpstreamYank{}
	for rows.Next() {
		var y UpstreamYank
		if err := rows.Scan(&y.ReleaseID, &y.TenantID, &y.UpstreamID, &y.BuildID, &y.BodyHash, &y.PriorState, &y.Cause); err != nil {
			return nil, err
		}
		out[y.ReleaseID] = y
	}
	return out, rows.Err()
}

// RevocationChannels lists the channels holding a live release of an upstream's builds with a body:
// where an upstream block's sweep goes.
func (s *Store) RevocationChannels(ctx context.Context, tenantID, upstreamID, hash string) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind(`SELECT DISTINCT r.channel_id FROM releases r JOIN builds b ON b.id = r.build_id
WHERE b.tenant_id = ? AND b.source = ? AND b.body_hash = ? AND r.state <> 'yanked'`), tenantID, UpstreamSource(upstreamID), hash)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []Channel
	for _, id := range ids {
		sc, err := s.ServeChannelByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, sc.Channel)
	}
	return out, nil
}

// LiveUpstreamReleases lists a channel's live releases of an upstream's builds with a body, or
// (buildID set) of that build: what a sweep or a yank yanks, under the channel's lock.
func (t *Tx) LiveUpstreamReleases(ctx context.Context, channelID, upstreamID, hash, buildID string) ([]Release, error) {
	q := "SELECT " + releaseCols + " FROM releases r JOIN builds b ON b.id = r.build_id" +
		" WHERE r.channel_id = ? AND b.source = ? AND b.body_hash = ? AND r.state <> 'yanked'"
	args := []any{channelID, UpstreamSource(upstreamID), hash}
	if buildID != "" {
		q += " AND b.id = ?"
		args = append(args, buildID)
	}
	rows, err := t.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReleaseByID reads a release with its build's withdrawal under the caller's channel lock: a lift's
// restore reads it there.
func (t *Tx) ReleaseByID(ctx context.Context, id string) (Candidate, error) {
	rows, err := t.query(ctx, "SELECT "+candidateCols+" FROM releases r JOIN builds b ON b.id = r.build_id WHERE r.id = ?", id)
	if err != nil {
		return Candidate{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Candidate{}, err
		}
		return Candidate{}, fmt.Errorf("%w: release %s", ErrNotFound, id)
	}
	return scanCandidate(rows)
}
