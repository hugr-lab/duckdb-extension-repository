package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Build ABIs, release states and visibilities (spec 0006).
const (
	ABICPP             = "cpp"
	ABICStruct         = "c_struct"
	ABICStructUnstable = "c_struct_unstable"

	ReleaseActive     = "active"
	ReleaseDeprecated = "deprecated"
	ReleaseYanked     = "yanked"

	Public  = "public"
	Private = "private"

	OriginAdmin = "admin"
)

// CAPI is a C API version: for a Build the version it was built for, for a DuckDB version the
// maximum it accepts for that major.
type CAPI struct{ Major, Minor, Patch int }

func (c CAPI) String() string { return fmt.Sprintf("v%d.%d.%d", c.Major, c.Minor, c.Patch) }

// Accepts reports whether a DuckDB whose maximum for c.Major is c accepts a build for b: the same
// major and (minor, patch) lexicographically at most the maximum (src/main/extension.cpp:118-139).
func (c CAPI) Accepts(b CAPI) bool {
	if b.Major != c.Major {
		return false
	}
	return b.Minor < c.Minor || b.Minor == c.Minor && b.Patch <= c.Patch
}

var capiRe = regexp.MustCompile(`^v(\d{1,9})\.(\d{1,9})\.(\d{1,9})$`)

// ParseCAPI parses "vM.m.p" as DuckDB's ParseSemver does (numeric parts only).
func ParseCAPI(s string) (CAPI, error) {
	m := capiRe.FindStringSubmatch(s)
	if m == nil {
		return CAPI{}, fmt.Errorf("%w: C API version %q is not v<major>.<minor>.<patch>", ErrInvalid, s)
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])
	return CAPI{a, b, c}, nil
}

// Build is a body as a named extension in a tenant.
type Build struct {
	ID, TenantID, Name, ExtVersion, Platform, ABI string
	DuckDBVersion                                 string // cpp, c_struct_unstable
	CAPI                                          *CAPI  // c_struct
	BodyHash, Origin                              string
	OriginSignature                               []byte
	CreatedAt                                     time.Time
	CreatedBy                                     string
}

// Release is a Build in a channel.
type Release struct {
	ID, TenantID, ChannelID, BuildID string
	Name, ExtVersion, Platform, Slot string
	State, Visibility                string
	Seq                              int64 // 0: not current-eligible (served on the versioned path only)
	CreatedAt, StateChangedAt        time.Time
	CreatedBy, StateChangedBy        string
	Version                          int64
}

// Slot is a build's slot in a channel: the exact DuckDB version, or capi:<major>.
func (b Build) Slot() string {
	if b.ABI == ABICStruct && b.CAPI != nil {
		return "capi:" + strconv.Itoa(b.CAPI.Major)
	}
	return b.DuckDBVersion
}

const buildCols = `id, tenant_id, name, ext_version, platform, abi, duckdb_version, c_api_major, c_api_minor,
c_api_patch, body_hash, origin, origin_signature, created_at, created_by`

func scanBuild(r interface{ Scan(...any) error }) (Build, error) {
	var b Build
	var dv sql.NullString
	var ma, mi, pa sql.NullInt64
	err := r.Scan(&b.ID, &b.TenantID, &b.Name, &b.ExtVersion, &b.Platform, &b.ABI, &dv, &ma, &mi, &pa,
		&b.BodyHash, &b.Origin, &b.OriginSignature, scanTime{&b.CreatedAt}, &b.CreatedBy)
	b.DuckDBVersion = dv.String
	if ma.Valid {
		b.CAPI = &CAPI{int(ma.Int64), int(mi.Int64), int(pa.Int64)}
	}
	return b, err
}

const releaseCols = `r.id, r.tenant_id, r.channel_id, r.build_id, r.name, r.ext_version, r.platform, r.slot, r.state,
r.visibility, r.seq, r.created_at, r.created_by, r.state_changed_at, r.state_changed_by, r.version`

func scanRelease(r interface{ Scan(...any) error }, extra ...any) (Release, error) {
	var x Release
	var seq sql.NullInt64
	dst := []any{&x.ID, &x.TenantID, &x.ChannelID, &x.BuildID, &x.Name, &x.ExtVersion, &x.Platform, &x.Slot,
		&x.State, &x.Visibility, &seq, scanTime{&x.CreatedAt}, &x.CreatedBy, scanTime{&x.StateChangedAt},
		&x.StateChangedBy, &x.Version}
	err := r.Scan(append(dst, extra...)...)
	x.Seq = seq.Int64
	return x, err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- builds ---

// FindOrInsertBuild inserts b, or, if the tenant already has a build of that name and body, loads
// it into b (the footer fields then must agree, which they do for one body).
func (t *Tx) FindOrInsertBuild(ctx context.Context, b *Build) error {
	cur, err := scanBuild(t.queryRow(ctx, "SELECT "+buildCols+" FROM builds WHERE tenant_id = ? AND name = ? AND body_hash = ?",
		b.TenantID, b.Name, b.BodyHash))
	if err == nil {
		*b = cur
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	b.ID, b.CreatedAt = NewID(), t.Now()
	var ma, mi, pa any
	if b.CAPI != nil {
		ma, mi, pa = b.CAPI.Major, b.CAPI.Minor, b.CAPI.Patch
	}
	var sig []byte // a typed nil: SQL Server refuses an untyped NULL for varbinary
	if len(b.OriginSignature) > 0 {
		sig = b.OriginSignature
	}
	_, err = t.exec(ctx, "INSERT INTO builds ("+buildCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		b.ID, b.TenantID, b.Name, b.ExtVersion, b.Platform, b.ABI, nullable(b.DuckDBVersion), ma, mi, pa,
		b.BodyHash, b.Origin, sig, t.s.d.timeArg(b.CreatedAt), b.CreatedBy)
	return t.s.mapErr(err, "build "+b.Name)
}

// GetBuild reads a build by id.
func (t *Tx) GetBuild(ctx context.Context, id string) (Build, error) {
	b, err := scanBuild(t.queryRow(ctx, "SELECT "+buildCols+" FROM builds WHERE id = ?", id))
	return b, notFound(err, "build")
}

// --- releases ---

// SlotCandidates reads a channel's releases of one (name, extension version, platform), in every
// slot and state, with what their builds say.
func (t *Tx) SlotCandidates(ctx context.Context, channelID, name, extVersion, platform string) ([]Candidate, error) {
	rows, err := t.query(ctx, "SELECT "+candidateCols+" FROM releases r JOIN builds b ON b.id = r.build_id"+
		" WHERE r.channel_id = ? AND r.name = ? AND r.ext_version = ? AND r.platform = ?", channelID, name, extVersion, platform)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetRelease reads a release by id within a channel.
func (t *Tx) GetRelease(ctx context.Context, channelID, id string) (Release, error) {
	r, err := scanRelease(t.queryRow(ctx, "SELECT "+releaseCols+" FROM releases r WHERE r.id = ? AND r.channel_id = ?", id, channelID))
	return r, notFound(err, "release "+id)
}

// NextSeq is the channel's next release sequence number; call it under the channel lock.
func (t *Tx) NextSeq(ctx context.Context, channelID string) (int64, error) {
	var max sql.NullInt64
	if err := t.queryRow(ctx, "SELECT MAX(seq) FROM releases WHERE channel_id = ?", channelID).Scan(&max); err != nil {
		return 0, err
	}
	return max.Int64 + 1, nil
}

// InsertRelease inserts a release (ID, times and version are set here).
func (t *Tx) InsertRelease(ctx context.Context, r *Release, actor string) error {
	now := t.Now()
	r.ID, r.CreatedAt, r.StateChangedAt, r.CreatedBy, r.StateChangedBy, r.Version = NewID(), now, now, actor, actor, 1
	var seq any
	if r.Seq > 0 {
		seq = r.Seq
	}
	_, err := t.exec(ctx, `INSERT INTO releases (id, tenant_id, channel_id, build_id, name, ext_version, platform, slot, state,
visibility, seq, created_at, created_by, state_changed_at, state_changed_by, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.TenantID, r.ChannelID, r.BuildID, r.Name, r.ExtVersion, r.Platform, r.Slot, r.State, r.Visibility, seq,
		t.s.d.timeArg(now), actor, t.s.d.timeArg(now), actor, r.Version)
	return t.s.mapErr(err, "release")
}

// SetReleaseState changes a release's state (compare-and-set).
func (t *Tx) SetReleaseState(ctx context.Context, r *Release, state, actor string) error {
	now := t.Now()
	return t.cas(ctx, "UPDATE releases SET state = ?, state_changed_at = ?, state_changed_by = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{state, t.s.d.timeArg(now), actor, r.ID, r.Version},
		func() { r.State, r.StateChangedAt, r.StateChangedBy = state, now, actor; r.Version++ })
}

// SetReleaseSeq gives a release a sequence number (compare-and-set).
func (t *Tx) SetReleaseSeq(ctx context.Context, r *Release, seq int64, actor string) error {
	now := t.Now()
	return t.cas(ctx, "UPDATE releases SET seq = ?, state_changed_at = ?, state_changed_by = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{seq, t.s.d.timeArg(now), actor, r.ID, r.Version},
		func() { r.Seq, r.StateChangedAt, r.StateChangedBy = seq, now, actor; r.Version++ })
}

// SetReleaseVisibility changes a release's visibility (compare-and-set).
func (t *Tx) SetReleaseVisibility(ctx context.Context, r *Release, visibility, actor string) error {
	now := t.Now()
	return t.cas(ctx, "UPDATE releases SET visibility = ?, state_changed_at = ?, state_changed_by = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{visibility, t.s.d.timeArg(now), actor, r.ID, r.Version},
		func() { r.Visibility, r.StateChangedAt, r.StateChangedBy = visibility, now, actor; r.Version++ })
}

// BumpReleaseVersion increments a channel's release_version: caches of resolutions see the change.
func (t *Tx) BumpReleaseVersion(ctx context.Context, channelID string) error {
	_, err := t.exec(ctx, "UPDATE channels SET release_version = release_version + 1 WHERE id = ?", channelID)
	return err
}

// SetServingKey moves a channel's serving key from old (empty: none) to key, a key of the channel
// (checked here too: SQLite has no composite foreign key on the column).
func (t *Tx) SetServingKey(ctx context.Context, channelID, old, key string) error {
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM channel_keys WHERE id = ? AND channel_id = ?", key, channelID).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: the key is not a key of the channel", ErrInvalid)
	}
	if old == "" {
		return t.cas(ctx, "UPDATE channels SET serving_key_id = ? WHERE id = ? AND serving_key_id IS NULL",
			[]any{key, channelID}, func() {})
	}
	return t.cas(ctx, "UPDATE channels SET serving_key_id = ? WHERE id = ? AND serving_key_id = ?",
		[]any{key, channelID, old}, func() {})
}

// ChannelByID reads a channel inside the transaction.
func (t *Tx) ChannelByID(ctx context.Context, id string) (Channel, error) {
	c, err := scanChannel(t.queryRow(ctx, "SELECT "+channelCols+" FROM channels c WHERE c.id = ?", id))
	return c, notFound(err, "channel")
}

// --- signatures ---

// InsertSignature records a release's signature by a key; one that exists is left as it is.
func (t *Tx) InsertSignature(ctx context.Context, releaseID, channelID, keyID string, sig []byte) error {
	if len(sig) != 256 {
		return fmt.Errorf("%w: a signature is 256 bytes", ErrInvalid)
	}
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM release_signatures WHERE release_id = ? AND key_id = ?", releaseID, keyID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := t.exec(ctx, "INSERT INTO release_signatures (release_id, channel_id, key_id, signature, created_at) VALUES (?, ?, ?, ?, ?)",
		releaseID, channelID, keyID, sig, t.s.d.timeArg(t.Now()))
	return t.s.mapErr(err, "signature")
}

// SignatureKeys lists the keys a release has signatures by.
func (t *Tx) SignatureKeys(ctx context.Context, releaseID string) (map[string]bool, error) {
	rows, err := t.query(ctx, "SELECT key_id FROM release_signatures WHERE release_id = ?", releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

const unsignedQuery = "SELECT " + releaseCols + ", b.body_hash FROM releases r JOIN builds b ON b.id = r.build_id" +
	" WHERE r.channel_id = ? AND r.state <> 'yanked' AND NOT EXISTS (SELECT 1 FROM release_signatures s" +
	" WHERE s.release_id = r.id AND s.key_id = ?) ORDER BY r.created_at, r.id"

// UnsignedReleases lists up to limit (0: all) non-yanked releases of a channel without a signature
// by key, with their body hashes.
func (t *Tx) UnsignedReleases(ctx context.Context, channelID, keyID string, limit int) ([]Release, []string, error) {
	rows, err := t.query(ctx, unsignedQuery, channelID, keyID)
	return collectWithHash(rows, err, limit)
}

// UnsignedReleases is the read-only form, outside a transaction.
func (s *Store) UnsignedReleases(ctx context.Context, channelID, keyID string, limit int) ([]Release, []string, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind(unsignedQuery), channelID, keyID)
	return collectWithHash(rows, err, limit)
}

func collectWithHash(rows *sql.Rows, err error, limit int) ([]Release, []string, error) {
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []Release
	var hashes []string
	for rows.Next() && (limit <= 0 || len(out) < limit) {
		var h string
		r, err := scanRelease(rows, &h)
		if err != nil {
			return nil, nil, err
		}
		out, hashes = append(out, r), append(hashes, h)
	}
	return out, hashes, rows.Err()
}

// Signature reads a release's signature by a key.
func (s *Store) Signature(ctx context.Context, releaseID, keyID string) ([]byte, error) {
	var sig []byte
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT signature FROM release_signatures WHERE release_id = ? AND key_id = ?"),
		releaseID, keyID).Scan(&sig)
	return sig, notFound(err, "signature")
}

// --- reads for serving and listing ---

// ServeChannel is what serving needs about a tenant and channel, in one query.
type ServeChannel struct {
	Tenant  Tenant
	Channel Channel
}

// GetServeChannel reads a tenant and channel by name.
func (s *Store) GetServeChannel(ctx context.Context, tenant, channel string) (ServeChannel, error) {
	var sc ServeChannel
	var serving sql.NullString
	t, c := &sc.Tenant, &sc.Channel
	err := s.db.QueryRowContext(ctx, s.d.rebind(`SELECT t.id, t.name, t.state, t.storage_domain, c.id, c.tenant_id,
c.name, c.kind, c.version, c.serving_key_id, c.release_version FROM channels c JOIN tenants t ON t.id = c.tenant_id
WHERE t.name = ? AND c.name = ?`), tenant, channel).Scan(&t.ID, &t.Name, &t.State, &t.StorageDomain, &c.ID, &c.TenantID,
		&c.Name, &c.Kind, &c.Version, &serving, &c.ReleaseVersion)
	c.ServingKeyID = serving.String
	return sc, notFound(err, "channel "+tenant+"/"+channel)
}

// Candidate is a release with what resolution needs from its build.
type Candidate struct {
	Release
	ABI, DuckDBVersion, BodyHash string
	CAPI                         *CAPI
}

const candidateCols = releaseCols + ", b.abi, b.duckdb_version, b.c_api_major, b.c_api_minor, b.c_api_patch, b.body_hash"

func scanCandidate(r interface{ Scan(...any) error }) (Candidate, error) {
	var c Candidate
	var dv sql.NullString
	var ma, mi, pa sql.NullInt64
	rel, err := scanRelease(r, &c.ABI, &dv, &ma, &mi, &pa, &c.BodyHash)
	c.Release, c.DuckDBVersion = rel, dv.String
	if ma.Valid {
		c.CAPI = &CAPI{int(ma.Int64), int(mi.Int64), int(pa.Int64)}
	}
	return c, err
}

func (s *Store) candidates(ctx context.Context, q string, args ...any) ([]Candidate, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func visibilityFilter(publicOnly bool) string {
	if publicOnly {
		return " AND r.visibility = 'public'"
	}
	return ""
}

// FlatCandidates lists the current-eligible releases of (channel, name, platform), highest seq
// first. With publicOnly, private releases are not read at all.
func (s *Store) FlatCandidates(ctx context.Context, channelID, name, platform string, publicOnly bool) ([]Candidate, error) {
	return s.candidates(ctx, "SELECT "+candidateCols+" FROM releases r JOIN builds b ON b.id = r.build_id"+
		" WHERE r.channel_id = ? AND r.name = ? AND r.platform = ? AND r.state = 'active' AND r.seq IS NOT NULL"+
		visibilityFilter(publicOnly)+" ORDER BY r.seq DESC", channelID, name, platform)
}

// VersionedCandidates lists the active or deprecated releases of (channel, name, extension
// version, platform).
func (s *Store) VersionedCandidates(ctx context.Context, channelID, name, extVersion, platform string, publicOnly bool) ([]Candidate, error) {
	return s.candidates(ctx, "SELECT "+candidateCols+" FROM releases r JOIN builds b ON b.id = r.build_id"+
		" WHERE r.channel_id = ? AND r.name = ? AND r.ext_version = ? AND r.platform = ? AND r.state IN ('active', 'deprecated')"+
		visibilityFilter(publicOnly), channelID, name, extVersion, platform)
}

// ListReleases lists a channel's releases (optionally of one name), newest first.
func (s *Store) ListReleases(ctx context.Context, channelID, name string) ([]Candidate, error) {
	q := "SELECT " + candidateCols + " FROM releases r JOIN builds b ON b.id = r.build_id WHERE r.channel_id = ?"
	args := []any{channelID}
	if name != "" {
		q += " AND r.name = ?"
		args = append(args, name)
	}
	return s.candidates(ctx, q+" ORDER BY r.created_at DESC, r.id", args...)
}

// ChannelServes reports whether a channel serves a DuckDB version.
func (t *Tx) ChannelServes(ctx context.Context, channelID, version string) (bool, error) {
	var n int
	err := t.queryRow(ctx, `SELECT COUNT(*) FROM channel_duckdb_versions cv JOIN duckdb_versions v ON v.id = cv.duckdb_version_id
WHERE cv.channel_id = ? AND v.name = ?`, channelID, version).Scan(&n)
	return n > 0, err
}

// ChannelsToResign lists signed channels whose active key is not their serving key, or that have a
// non-yanked release without a signature by the active key.
func (s *Store) ChannelsToResign(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id FROM channels c JOIN channel_keys k ON k.channel_id = c.id AND k.state = 'active'
WHERE c.kind = 'signed' AND (c.serving_key_id <> k.id OR EXISTS (SELECT 1 FROM releases r WHERE r.channel_id = c.id
AND r.state <> 'yanked' AND NOT EXISTS (SELECT 1 FROM release_signatures x WHERE x.release_id = r.id AND x.key_id = k.id)))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// --- DuckDB versions' C APIs ---

// AddCAPI records a DuckDB version's maximum C API for a major. Rows are facts about the engine:
// an existing major is refused (ErrExists), never changed.
func (t *Tx) AddCAPI(ctx context.Context, versionID string, c CAPI) error {
	_, err := t.exec(ctx, "INSERT INTO duckdb_version_c_apis (duckdb_version_id, major, max_minor, max_patch) VALUES (?, ?, ?, ?)",
		versionID, c.Major, c.Minor, c.Patch)
	return t.s.mapErr(err, "C API major "+strconv.Itoa(c.Major))
}

// BumpChannelsServing bumps channels.version of every channel serving a DuckDB version.
func (t *Tx) BumpChannelsServing(ctx context.Context, versionID string) error {
	_, err := t.exec(ctx, "UPDATE channels SET version = version + 1 WHERE id IN (SELECT channel_id FROM channel_duckdb_versions WHERE duckdb_version_id = ?)", versionID)
	return err
}

// ChannelCAPIs returns, for each DuckDB version a channel serves, the C API maxima. A version added
// by an older binary has only the legacy c_api_version column, which is used when it parses.
func (s *Store) ChannelCAPIs(ctx context.Context, channelID string) (map[string][]CAPI, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind(`SELECT v.name, v.c_api_version, a.major, a.max_minor, a.max_patch
FROM channel_duckdb_versions cv JOIN duckdb_versions v ON v.id = cv.duckdb_version_id
LEFT JOIN duckdb_version_c_apis a ON a.duckdb_version_id = v.id WHERE cv.channel_id = ?`), channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]CAPI{}
	legacy := map[string]string{}
	for rows.Next() {
		var name string
		var leg sql.NullString
		var ma, mi, pa sql.NullInt64
		if err := rows.Scan(&name, &leg, &ma, &mi, &pa); err != nil {
			return nil, err
		}
		if _, ok := out[name]; !ok {
			out[name] = nil
		}
		if ma.Valid {
			out[name] = append(out[name], CAPI{int(ma.Int64), int(mi.Int64), int(pa.Int64)})
		} else if leg.Valid {
			legacy[name] = leg.String
		}
	}
	for name, l := range legacy {
		if len(out[name]) == 0 {
			if c, err := ParseCAPI(strings.TrimSpace(l)); err == nil {
				out[name] = []CAPI{c}
			}
		}
	}
	return out, rows.Err()
}

// VersionCAPIs returns one DuckDB version's C API rows (without the legacy column).
func (t *Tx) VersionCAPIs(ctx context.Context, versionID string) ([]CAPI, error) {
	rows, err := t.query(ctx, "SELECT major, max_minor, max_patch FROM duckdb_version_c_apis WHERE duckdb_version_id = ? ORDER BY major", versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CAPI
	for rows.Next() {
		var c CAPI
		if err := rows.Scan(&c.Major, &c.Minor, &c.Patch); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- leases ---

// AcquireLease takes or renews the lease name for holder until now+ttl; it reports whether holder
// holds it. A lease held by another holder and not expired is not taken.
func (s *Store) AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	held := false
	err := s.tx(ctx, "", func(t *Tx) error {
		now := t.Now()
		until := t.s.d.timeArg(now.Add(ttl))
		var cur string
		var exp time.Time
		var version int64
		err := t.queryRow(ctx, "SELECT holder, expires_at, version FROM leases WHERE name = ?", name).Scan(&cur, scanTime{&exp}, &version)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err := t.exec(ctx, "INSERT INTO leases (name, holder, expires_at, version) VALUES (?, ?, ?, 1)", name, holder, until)
			if t.s.d.unique(err) {
				return errLeaseTaken // another holder won the insert; the transaction is rolled back
			}
			held = err == nil
			return err
		case err != nil:
			return err
		case cur != holder && exp.After(now):
			return nil
		}
		err = t.cas(ctx, "UPDATE leases SET holder = ?, expires_at = ?, version = version + 1 WHERE name = ? AND version = ?",
			[]any{holder, until, name, version}, func() { held = true })
		if errors.Is(err, ErrConflict) {
			return nil
		}
		return err
	})
	if errors.Is(err, errLeaseTaken) {
		return false, nil
	}
	return held, err
}

var errLeaseTaken = errors.New("store: lease taken")

// ReleaseLease gives up a lease if holder holds it.
func (s *Store) ReleaseLease(ctx context.Context, name, holder string) error {
	return s.tx(ctx, "", func(t *Tx) error {
		_, err := t.exec(ctx, "DELETE FROM leases WHERE name = ? AND holder = ?", name, holder)
		return err
	})
}
