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

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
)

// Build ABIs, release states and visibilities (spec 0006).
const (
	ABICPP             = "cpp"
	ABICStruct         = "c_struct"
	ABICStructUnstable = "c_struct_unstable"

	ReleaseActive     = "active"
	ReleaseDeprecated = "deprecated"
	ReleaseYanked     = "yanked"
	// ReleasePurged is a slot candidate whose release was purged (spec 0016 phase 2): its slot stays
	// taken. It is never a release's stored state.
	ReleasePurged = "purged"

	Public  = "public"
	Private = "private"
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

// Build is a body as a named extension in a tenant, from a source (spec 0033: upstream:<id>,
// publisher:<id> or tenant, as a release's).
type Build struct {
	ID, TenantID, Source, Name, PackageVersion, Platform, ABI string
	// PackageType is the build's type (spec 0034): its identity and its slot are within the type.
	PackageType      string
	DuckDBVersion    string // cpp, c_struct_unstable
	CAPI             *CAPI  // c_struct
	BodyHash, Origin string
	// BodyLen is the body's length (spec 0038: a tenant's bytes); 0 for a row an older replica
	// inserted, until the collector fills it
	BodyLen         int64
	OriginSignature []byte
	Checked         bool // passed spec 0008's file checks; set once, never cleared
	CreatedAt       time.Time
	CreatedBy       string
	// Visibility and Provenance are as its arrival made its release (spec 0033): the widest a
	// placement serves it with, and the facts a placement's release carries (a credential).
	Visibility, Provenance string
	// Legacy inserts it as a legacy duplicate, outside its source's slot (spec 0033): only a successor's
	// copy of a removed upstream's build, whose slot the successor holds with another body. Never read.
	Legacy bool
	// Inserted is set by FindOrInsertBuild when it inserted the build (not found it).
	Inserted bool
	// AvailableAt is when a held entry becomes available (spec 0033): its arrival plus its source's
	// hold, which FindOrInsertBuild reads when it inserts the build (raised with the hold while it
	// is held); zero for an entry available on arrival.
	AvailableAt time.Time
	// Copied inserts the build as a copy of another source's build (spec 0031: a successor's copy of
	// a removed upstream's released build): with the original's CreatedAt (its arrival, which a
	// channel's hold counts from), AvailableAt and RevokedAt, not its new source's hold. Never read.
	Copied bool
	// PublisherKeyID is the publisher's key a publication was signed with (spec 0023): revoking it
	// purges the entries it published that are still held. "": none.
	PublisherKeyID string
	// Landed is a build kept in the catalogue placed nowhere (spec 0033), as read.
	Landed bool
	// RevokedAt is when the publisher key that published a held entry was revoked with purge_held, or
	// its publisher removed, while a channel administrator had placed it early (spec 0033 phase A2):
	// the purge kept it, and it is never placed automatically nor promoted again, only placed early by
	// a channel administrator. Zero: not revoked.
	RevokedAt time.Time
	// WithdrawnAt is when a distribution admin or a server administrator withdrew it, with the reason
	// (spec 0033 phase B): served nowhere, never placed again, its body never arriving again in its
	// source. Zero: not withdrawn.
	WithdrawnAt     time.Time
	WithdrawnReason string
}

// Withdrawn reports whether the entry was withdrawn (spec 0033 phase B: WithdrawnAt).
func (b Build) Withdrawn() bool { return !b.WithdrawnAt.IsZero() }

// Revoked reports whether the entry was revoked (spec 0033 phase A2: RevokedAt).
func (b Build) Revoked() bool { return !b.RevokedAt.IsZero() }

// Held reports whether the entry is held at now (spec 0033).
func (b Build) Held(now time.Time) bool { return b.AvailableAt.After(now) }

// AvailableIn is when an automatic placement into a channel with a hold may take the entry: its
// own availability, or its arrival plus the channel's hold if that is later (spec 0033).
func (b Build) AvailableIn(channelHold time.Duration) time.Time {
	at := b.AvailableAt
	if channelHold > 0 && b.CreatedAt.Add(channelHold).After(at) {
		at = b.CreatedAt.Add(channelHold)
	}
	return at
}

// Release is a Build in a channel.
type Release struct {
	ID, TenantID, ChannelID, BuildID     string
	Name, PackageVersion, Platform, Slot string
	State, Visibility                    string
	Seq                                  int64 // 0: not current-eligible (served on the versioned path only)
	CreatedAt, StateChangedAt            time.Time
	CreatedBy, StateChangedBy            string
	Version                              int64
	Origin                               string // admin | publication | promotion (spec 0008) | upstream (spec 0009)
	// Source is whose release it is (spec 0031): upstream:<id>, publisher:<id> or tenant, its
	// build's (spec 0033); a promotion keeps the source of the release it promotes. Set at insert,
	// never changed: a slot is a source's.
	Source     string
	Provenance string // JSON, at most 4,000 bytes; "" when none
	// PublisherStatement and PublisherSignature are a publisher's signed request as it came (spec
	// 0023): the statement, and the SSHSIG blob in standard base64. "" when none.
	PublisherStatement, PublisherSignature string
	// OriginSignature is an upstream release's original signature (spec 0009), written at insert;
	// list reads leave it empty.
	OriginSignature []byte
}

// Origins of builds and releases.
const (
	OriginAdmin       = "admin"
	OriginPublication = "publication"
	OriginPromotion   = "promotion"
	// OriginPlacement is a release placed from the catalogue (spec 0033): by a rule, or by hand.
	OriginPlacement = "placement"
)

// Slot is a build's slot in a channel: the exact DuckDB version, or capi:<major>.
func (b Build) Slot() string {
	if b.ABI == ABICStruct && b.CAPI != nil {
		return "capi:" + strconv.Itoa(b.CAPI.Major)
	}
	return b.DuckDBVersion
}

const buildCols = `id, tenant_id, source, name, package_version, platform, abi, duckdb_version, c_api_major, c_api_minor,
c_api_patch, body_hash, origin, origin_signature, created_at, created_by, checked, visibility, provenance, package_type,
available_at, publisher_key_id, landed, revoked_at, withdrawn_at, withdrawn_reason, body_len`

func scanBuild(r interface{ Scan(...any) error }) (Build, error) {
	var b Build
	var dv sql.NullString
	var ma, mi, pa sql.NullInt64
	var prov, key, why sql.NullString
	var n sql.NullInt64
	err := r.Scan(&b.ID, &b.TenantID, &b.Source, &b.Name, &b.PackageVersion, &b.Platform, &b.ABI, &dv, &ma, &mi, &pa,
		&b.BodyHash, &b.Origin, &b.OriginSignature, scanTime{&b.CreatedAt}, &b.CreatedBy, &b.Checked, &b.Visibility, &prov, &b.PackageType,
		scanTime{&b.AvailableAt}, &key, &b.Landed, scanTime{&b.RevokedAt}, scanTime{&b.WithdrawnAt}, &why, &n)
	b.DuckDBVersion, b.Provenance, b.PublisherKeyID, b.WithdrawnReason = dv.String, prov.String, key.String, why.String
	b.BodyLen = n.Int64
	if ma.Valid {
		b.CAPI = &CAPI{int(ma.Int64), int(mi.Int64), int(pa.Int64)}
	}
	return b, err
}

const releaseCols = `r.id, r.tenant_id, r.channel_id, r.build_id, r.name, r.package_version, r.platform, r.slot, r.state,
r.visibility, r.seq, r.created_at, r.created_by, r.state_changed_at, r.state_changed_by, r.version, r.origin, r.provenance,
r.publisher_statement, r.publisher_signature, r.source`

func scanRelease(r interface{ Scan(...any) error }, extra ...any) (Release, error) {
	var x Release
	var seq sql.NullInt64
	var prov, stmt, sig sql.NullString
	dst := []any{&x.ID, &x.TenantID, &x.ChannelID, &x.BuildID, &x.Name, &x.PackageVersion, &x.Platform, &x.Slot,
		&x.State, &x.Visibility, &seq, scanTime{&x.CreatedAt}, &x.CreatedBy, scanTime{&x.StateChangedAt},
		&x.StateChangedBy, &x.Version, &x.Origin, &prov, &stmt, &sig, &x.Source}
	err := r.Scan(append(dst, extra...)...)
	x.Seq, x.Provenance, x.PublisherStatement, x.PublisherSignature = seq.Int64, prov.String, stmt.String, sig.String
	return x, err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- builds ---

// FindOrInsertBuild inserts b, or, if the tenant already has a build of that source, name and body,
// loads it into b (the footer fields then must agree, which they do for one body). A checked b marks
// an existing unchecked build checked.
func (t *Tx) FindOrInsertBuild(ctx context.Context, b *Build) error {
	if err := t.WritableTenant(ctx, b.TenantID, false); err != nil {
		return err // spec 0019: no Build for a tenant whose deletion started
	}
	if b.Source == "" {
		b.Source = SourceTenant
	}
	if b.Visibility == "" {
		b.Visibility = Private
	}
	if len(b.Provenance) > 4000 {
		return fmt.Errorf("%w: provenance is too long", ErrInvalid)
	}
	if !ValidSource(b.Source) {
		return fmt.Errorf("%w: source %q", ErrInvalid, b.Source)
	}
	if b.PackageType == "" {
		b.PackageType = pkgtype.Default
	}
	if !pkgtype.Known(b.PackageType) {
		return fmt.Errorf("%w: package type %q", ErrInvalid, b.PackageType)
	}
	// spec 0033 phase B: no build of a version withdrawn from its source arrives again (a withdrawal
	// takes the tenant's auth lock, which a publication's build transaction takes: one sees the other)
	if strings.HasPrefix(b.Source, SourcePublisherPrefix) {
		if w, err := t.VersionWithdrawn(ctx, b.TenantID, b.PackageType, b.Source, b.Name, b.PackageVersion); err != nil {
			return err
		} else if w {
			return fmt.Errorf("%w: %s %s from this source", ErrWithdrawn, b.Name, b.PackageVersion)
		}
	}
	cur, err := scanBuild(t.queryRow(ctx, "SELECT "+buildCols+" FROM builds WHERE tenant_id = ? AND package_type = ? AND source = ? AND name = ? AND body_hash = ?",
		b.TenantID, b.PackageType, b.Source, b.Name, b.BodyHash))
	if err == nil && cur.Withdrawn() {
		// spec 0033 phase B: a withdrawn entry's body never arrives again in its source
		return fmt.Errorf("%w: %s %s %s from this source", ErrWithdrawn, cur.Name, cur.PackageVersion, cur.Platform)
	}
	if err == nil {
		// its last use (spec 0016): a Build an intake found is not collected under it; an update that
		// finds no row (collected since the select) inserts it again below
		res, err := t.exec(ctx, "UPDATE builds SET used_at = ?, checked = ? WHERE id = ?", t.s.d.timeArg(t.Now()), b.Checked || cur.Checked, cur.ID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 1 {
			if err != nil {
				return err
			}
			cur.Checked = b.Checked || cur.Checked
			*b = cur
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	b.ID = NewID()
	if !b.Copied || b.CreatedAt.IsZero() {
		b.CreatedAt = t.Now()
	}
	var ma, mi, pa any
	if b.CAPI != nil {
		ma, mi, pa = b.CAPI.Major, b.CAPI.Minor, b.CAPI.Patch
	}
	var sig []byte // a typed nil: SQL Server refuses an untyped NULL for varbinary
	if len(b.OriginSignature) > 0 {
		sig = b.OriginSignature
	}
	if !b.Legacy {
		if err := t.SlotTaken(ctx, *b); err != nil {
			return err
		}
	}
	// spec 0033: an entry of a source with a hold is held from its arrival; the source is read with a
	// lock (fail closed: a removed upstream or publisher brings nothing, and a hold raised meanwhile
	// counts), and a copy keeps the original's arrival, hold and revoked marker
	hold, err := t.SourceHold(ctx, b.Source)
	if err != nil {
		return err
	}
	if !b.Copied {
		b.AvailableAt, b.RevokedAt = time.Time{}, time.Time{}
		if hold > 0 {
			b.AvailableAt = b.CreatedAt.Add(hold)
		}
	}
	// spec 0038: a new body of the tenant's own source within its bytes limit
	if err := t.BytesRoom(ctx, b.TenantID, b.Source, b.BodyHash, max(b.BodyLen, 1)); err != nil {
		return err
	}
	b.Landed = false
	b.WithdrawnAt, b.WithdrawnReason = time.Time{}, ""
	var bodyLen any // NULL when unknown: the collector fills it (FillBodyLen)
	if b.BodyLen > 0 {
		bodyLen = b.BodyLen
	}
	_, err = t.exec(ctx, "INSERT INTO builds ("+buildCols+", slot, legacy_duplicate, used_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		b.ID, b.TenantID, b.Source, b.Name, b.PackageVersion, b.Platform, b.ABI, nullable(b.DuckDBVersion), ma, mi, pa,
		b.BodyHash, b.Origin, sig, t.s.d.timeArg(b.CreatedAt), b.CreatedBy, b.Checked, b.Visibility, nullable(b.Provenance),
		b.PackageType, t.s.d.nullTimeArg(b.AvailableAt), nullable(b.PublisherKeyID), false, t.s.d.nullTimeArg(b.RevokedAt), t.s.d.nullTimeArg(time.Time{}),
		nullable(""), bodyLen, b.Slot(), b.Legacy,
		t.s.d.timeArg(t.Now()))
	b.Inserted = err == nil
	return t.s.mapErr(err, "build "+b.Name)
}

// GetBuild reads a build by id.
func (t *Tx) GetBuild(ctx context.Context, id string) (Build, error) {
	b, err := scanBuild(t.queryRow(ctx, "SELECT "+buildCols+" FROM builds WHERE id = ?", id))
	return b, notFound(err, "build")
}

// --- releases ---

// SlotCandidates reads a source's releases in a channel of one (name, extension version, platform),
// in every slot and state, with what their builds say, and its purged slots (state ReleasePurged:
// the source's, and those purged before slots had a source, spec 0033).
func (t *Tx) SlotCandidates(ctx context.Context, channelID, source, name, extVersion, platform string) ([]Candidate, error) {
	rows, err := t.query(ctx, "SELECT "+candidateCols+" FROM releases r JOIN builds b ON b.id = r.build_id"+
		" WHERE r.channel_id = ? AND r.source = ? AND r.name = ? AND r.package_version = ? AND r.platform = ?", channelID, source, name, extVersion, platform)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	purged, err := t.query(ctx, "SELECT release_id, slot, abi, body_hash, seq FROM purged_slots"+
		" WHERE channel_id = ? AND source IN (?, '*') AND name = ? AND package_version = ? AND platform = ?", channelID, source, name, extVersion, platform)
	if err != nil {
		return nil, err
	}
	defer purged.Close()
	for purged.Next() {
		c := Candidate{Release: Release{ChannelID: channelID, Source: source, Name: name, PackageVersion: extVersion, Platform: platform, State: ReleasePurged}}
		var seq sql.NullInt64
		if err := purged.Scan(&c.ID, &c.Slot, &c.ABI, &c.BodyHash, &seq); err != nil {
			return nil, err
		}
		c.Seq = seq.Int64
		out = append(out, c)
	}
	return out, purged.Err()
}

// PurgeRelease deletes a release and its signatures, keeping its slot taken in purged_slots (spec
// 0016 phase 2); call it under the channel's lock, with the release as read there.
func (t *Tx) PurgeRelease(ctx context.Context, c Candidate) error {
	if _, err := t.exec(ctx, "DELETE FROM release_signatures WHERE release_id = ?", c.ID); err != nil {
		return err
	}
	var seq any
	if c.Seq > 0 {
		seq = c.Seq
	}
	if _, err := t.exec(ctx, "INSERT INTO purged_slots (channel_id, source, name, package_version, platform, slot, abi, body_hash, seq, release_id, purged_at) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", c.ChannelID, c.Source, c.Name, c.PackageVersion, c.Platform, c.Slot, c.ABI, c.BodyHash, seq, c.ID,
		t.s.d.timeArg(t.Now())); err != nil {
		return t.s.mapErr(err, "purged slot")
	}
	if err := t.DeleteUpstreamYank(ctx, c.ID); err != nil { // spec 0039: nothing left to restore
		return err
	}
	res, err := t.exec(ctx, "DELETE FROM releases WHERE id = ? AND version = ?", c.ID, c.Version)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err == nil {
			err = ErrConflict
		}
		return err
	}
	// the Build's last use: the collector removes it a grace after the purge
	_, err = t.exec(ctx, "UPDATE builds SET used_at = ? WHERE id = ?", t.s.d.timeArg(t.Now()), c.BuildID)
	return err
}

// UnyankedReleases reports which of ids are releases of the channel that are not yanked.
func (t *Tx) UnyankedReleases(ctx context.Context, channelID string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	args := []any{channelID}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := t.query(ctx, "SELECT id FROM releases WHERE channel_id = ? AND state <> 'yanked' AND id IN ("+
		strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ")+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// GetCandidate reads a release by id within a channel, with what its build says.
func (t *Tx) GetCandidate(ctx context.Context, channelID, id string) (Candidate, error) {
	c, err := scanCandidate(t.queryRow(ctx, "SELECT "+candidateCols+" FROM releases r JOIN builds b ON b.id = r.build_id"+
		" WHERE r.id = ? AND r.channel_id = ?", id, channelID))
	return c, notFound(err, "release "+id)
}

// GetRelease reads a release by id within a channel.
func (t *Tx) GetRelease(ctx context.Context, channelID, id string) (Release, error) {
	r, err := scanRelease(t.queryRow(ctx, "SELECT "+releaseCols+" FROM releases r WHERE r.id = ? AND r.channel_id = ?", id, channelID))
	return r, notFound(err, "release "+id)
}

// NextSeq is the channel's next release sequence number; call it under the channel lock.
func (t *Tx) NextSeq(ctx context.Context, channelID string) (int64, error) {
	var max sql.NullInt64
	// a purged release's number is never given again (spec 0016 phase 2)
	if err := t.queryRow(ctx, "SELECT MAX(seq) FROM (SELECT seq FROM releases WHERE channel_id = ? UNION ALL "+
		"SELECT seq FROM purged_slots WHERE channel_id = ?) x", channelID, channelID).Scan(&max); err != nil {
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
	if r.Origin == "" {
		r.Origin = OriginAdmin
	}
	if r.Source == "" {
		r.Source = SourceTenant
	}
	if !ValidSource(r.Source) {
		return fmt.Errorf("%w: source %q", ErrInvalid, r.Source)
	}
	if len(r.Provenance) > 4000 {
		return fmt.Errorf("%w: provenance is too long", ErrInvalid)
	}
	if len(r.PublisherStatement) > 4096 || len(r.PublisherSignature) > 1400 {
		return fmt.Errorf("%w: a signed request's statement or signature is too long", ErrInvalid)
	}
	if err := t.keepRoom(ctx, r.BuildID); err != nil { // spec 0038: kept within the tenant's bytes limit
		return err
	}
	_, err := t.exec(ctx, `INSERT INTO releases (id, tenant_id, channel_id, build_id, name, package_version, platform, slot, state,
visibility, seq, created_at, created_by, state_changed_at, state_changed_by, version, origin, provenance, origin_signature,
publisher_statement, publisher_signature, source) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.TenantID, r.ChannelID, r.BuildID, r.Name, r.PackageVersion, r.Platform, r.Slot, r.State, r.Visibility, seq,
		t.s.d.timeArg(now), actor, t.s.d.timeArg(now), actor, r.Version, r.Origin, nullable(r.Provenance), nullBytes(r.OriginSignature),
		nullable(r.PublisherStatement), nullable(r.PublisherSignature), r.Source)
	if t.s.d.foreignKey(err) { // the channel is never deleted: the Build was collected (spec 0016)
		return fmt.Errorf("%w: %s", ErrBuildGone, r.BuildID)
	}
	if err != nil {
		return t.s.mapErr(err, "release")
	}
	if r.Visibility == Public { // spec 0035 phase 2: a public release of an entry holding a licence warns
		t.warnRelease(r.ID, "release.add", actor)
	}
	// a landed build is the release's now (spec 0033): the release keeps it from the collector
	_, err = t.exec(ctx, "UPDATE builds SET landed = ? WHERE id = ? AND landed = ?", false, r.BuildID, true)
	return err
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

// SetReleaseVisibility changes a release's visibility (compare-and-set). Making it private makes its
// build private too (spec 0033): no placement serves it publicly again. Making a private one public
// warns when its entry holds a licence (spec 0035 phase 2).
func (t *Tx) SetReleaseVisibility(ctx context.Context, r *Release, visibility, actor string) error {
	if visibility == Private {
		if err := t.NarrowBuild(ctx, r.BuildID); err != nil {
			return err
		}
	}
	now, changed := t.Now(), r.Visibility != visibility
	err := t.cas(ctx, "UPDATE releases SET visibility = ?, state_changed_at = ?, state_changed_by = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{visibility, t.s.d.timeArg(now), actor, r.ID, r.Version},
		func() { r.Visibility, r.StateChangedAt, r.StateChangedBy = visibility, now, actor; r.Version++ })
	if err == nil && changed && visibility == Public {
		t.warnRelease(r.ID, "release.public", actor)
	}
	return err
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
	if err == nil {
		err = t.WritableTenant(ctx, c.TenantID, false) // spec 0019: the tenant held open until commit
	}
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

// CountUnsigned counts a channel's non-yanked releases without a signature by key.
func (s *Store) CountUnsigned(ctx context.Context, channelID, keyID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.d.rebind(`SELECT COUNT(*) FROM releases r WHERE r.channel_id = ? AND r.state <> 'yanked'
AND NOT EXISTS (SELECT 1 FROM release_signatures s WHERE s.release_id = r.id AND s.key_id = ?)`), channelID, keyID).Scan(&n)
	return n, err
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
	// ReservedVersion is the reserved names' version (spec 0030), read with the channel's row: a
	// change of it means the names changed.
	ReservedVersion int64
}

// GetServeChannel reads a tenant and channel by name.
func (s *Store) GetServeChannel(ctx context.Context, tenant, channel string) (ServeChannel, error) {
	var sc ServeChannel
	var serving sql.NullString
	var hold int64
	t, c := &sc.Tenant, &sc.Channel
	err := s.db.QueryRowContext(ctx, s.d.rebind(`SELECT t.id, t.name, t.state, t.storage_domain, t.auth_version, c.id, c.tenant_id,
c.name, c.kind, c.version, c.serving_key_id, c.release_version, c.package_type, c.hold_seconds, (SELECT version FROM reserved_version WHERE id = 1)
FROM channels c JOIN tenants t ON t.id = c.tenant_id
WHERE t.name = ? AND c.name = ? AND t.deleting_at IS NULL`), tenant, channel).Scan(&t.ID, &t.Name, &t.State, &t.StorageDomain, &t.AuthVersion, &c.ID, &c.TenantID,
		&c.Name, &c.Kind, &c.Version, &serving, &c.ReleaseVersion, &c.PackageType, &hold, &sc.ReservedVersion)
	c.ServingKeyID, c.Hold = serving.String, time.Duration(hold)*time.Second
	if err == nil && !pkgtype.Known(c.PackageType) {
		err = sql.ErrNoRows // spec 0034: a channel of a type this binary does not know serves nothing
	}
	if err == nil {
		// spec 0030: the process's view of the reserved names follows the server's list, so what is
		// decided on this channel (shadows, pull-through) sees a name reserved since
		err = reserved.Ensure(ctx, s, sc.ReservedVersion)
	}
	return sc, s.closedOr(ctx, tenant, notFound(err, "channel "+tenant+"/"+channel))
}

// Candidate is a release with what resolution needs from its build.
type Candidate struct {
	Release
	ABI, DuckDBVersion, BodyHash string
	CAPI                         *CAPI
	BuildOrigin                  string // the Build's origin (spec 0009: a mirrored build promoted is no replacement)
	// Withdrawn says its build was withdrawn (spec 0033 phase B): the release is served nowhere, and
	// shown as withdrawn, with the distributor's reason.
	Withdrawn       bool
	WithdrawnReason string
}

const candidateCols = releaseCols + ", b.abi, b.duckdb_version, b.c_api_major, b.c_api_minor, b.c_api_patch, b.body_hash, b.origin, b.withdrawn_at, b.withdrawn_reason"

func scanCandidate(r interface{ Scan(...any) error }) (Candidate, error) {
	var c Candidate
	var dv sql.NullString
	var ma, mi, pa sql.NullInt64
	var withdrawn time.Time
	var reason sql.NullString
	rel, err := scanRelease(r, &c.ABI, &dv, &ma, &mi, &pa, &c.BodyHash, &c.BuildOrigin, scanTime{&withdrawn}, &reason)
	c.Release, c.DuckDBVersion, c.Withdrawn, c.WithdrawnReason = rel, dv.String, !withdrawn.IsZero(), reason.String
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

// ChannelReleases lists every release of a channel (every state and visibility) with its build: the
// snapshot the DuckDB routes and the index resolve on (spec 0007). A release of a withdrawn entry is
// left out (spec 0033 phase B: served nowhere; the withdrawal bumps the channel).
func (s *Store) ChannelReleases(ctx context.Context, channelID string) ([]Candidate, error) {
	return s.candidates(ctx, "SELECT "+candidateCols+" FROM releases r JOIN builds b ON b.id = r.build_id"+
		" WHERE r.channel_id = ? AND b.withdrawn_at IS NULL ORDER BY r.created_at, r.id", channelID)
}

// RevokedReleases lists a channel's releases of every source that are revoked (spec 0039): of a
// withdrawn build, or yanked. A snapshot keeps them apart from ChannelReleases, for the routes that
// show them (-/revocations, the index's withdrawn rows); nothing serves from them.
func (s *Store) RevokedReleases(ctx context.Context, channelID string) ([]Candidate, error) {
	return s.candidates(ctx, "SELECT "+candidateCols+" FROM releases r JOIN builds b ON b.id = r.build_id"+
		" WHERE r.channel_id = ? AND (b.withdrawn_at IS NOT NULL OR r.state = 'yanked') ORDER BY r.created_at, r.id", channelID)
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
WHERE c.kind = 'signed' AND c.tenant_id IN (SELECT id FROM tenants WHERE deleting_at IS NULL) AND (c.serving_key_id <> k.id OR EXISTS (SELECT 1 FROM releases r WHERE r.channel_id = c.id
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
	return channelCAPIs(s.db.QueryContext(ctx, s.d.rebind(channelCAPIsQuery), channelID))
}

// ChannelCAPIs is the transactional form (under the channel lock).
func (t *Tx) ChannelCAPIs(ctx context.Context, channelID string) (map[string][]CAPI, error) {
	return channelCAPIs(t.query(ctx, channelCAPIsQuery, channelID))
}

const channelCAPIsQuery = `SELECT v.name, v.c_api_version, a.major, a.max_minor, a.max_patch
FROM channel_duckdb_versions cv JOIN duckdb_versions v ON v.id = cv.duckdb_version_id
LEFT JOIN duckdb_version_c_apis a ON a.duckdb_version_id = v.id WHERE cv.channel_id = ?`

func channelCAPIs(rows *sql.Rows, err error) (map[string][]CAPI, error) {
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
	err := s.tx(ctx, nil, func(t *Tx) error {
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

// RenewLease extends a lease holder still holds (it has not expired): unlike AcquireLease it never
// takes one back, so a holder that was paused past the lease learns that it lost it.
func (s *Store) RenewLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	held := false
	err := s.tx(ctx, nil, func(t *Tx) error {
		now := t.Now()
		res, err := t.exec(ctx, "UPDATE leases SET expires_at = ?, version = version + 1 WHERE name = ? AND holder = ? AND expires_at > ?",
			t.s.d.timeArg(now.Add(ttl)), name, holder, t.s.d.timeArg(now))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		held = n == 1
		return err
	})
	return held, err
}

// leaseSkew is the clock skew between replicas a released lease allows for.
const leaseSkew = 5 * time.Second

// ReleaseLease gives up a lease if holder holds it: it expires (a little in the past, so a replica
// whose clock is behind may take it at once), and the row keeps by whom and about when it was last
// held (spec 0007's key view shows it).
func (s *Store) ReleaseLease(ctx context.Context, name, holder string) error {
	return s.tx(ctx, nil, func(t *Tx) error {
		_, err := t.exec(ctx, "UPDATE leases SET expires_at = ?, version = version + 1 WHERE name = ? AND holder = ?",
			t.s.d.timeArg(t.Now().Add(-leaseSkew)), name, holder)
		return err
	})
}

// DeleteLease removes a lease its holder is done with: leases named per item (a pull-through cell,
// spec 0009) do not accumulate.
func (s *Store) DeleteLease(ctx context.Context, name, holder string) error {
	return s.tx(ctx, nil, func(t *Tx) error {
		_, err := t.exec(ctx, "DELETE FROM leases WHERE name = ? AND holder = ?", name, holder)
		return err
	})
}

// LeaseState reads a lease: its last holder and until when it is (or was) held; found is false for
// a lease never taken.
func (s *Store) LeaseState(ctx context.Context, name string) (holder string, until time.Time, found bool, err error) {
	err = s.db.QueryRowContext(ctx, s.d.rebind("SELECT holder, expires_at FROM leases WHERE name = ?"), name).
		Scan(&holder, scanTime{&until})
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	return holder, until, err == nil, err
}

// --- blocks (spec 0008) ---

// Block bans a body hash in a tenant.
type Block struct {
	TenantID, BodyHash, Reason string
	CreatedAt                  time.Time
	CreatedBy                  string
}

const blockCols = "tenant_id, body_hash, reason, created_at, created_by"

func scanBlock(r interface{ Scan(...any) error }) (Block, error) {
	var b Block
	err := r.Scan(&b.TenantID, &b.BodyHash, &b.Reason, scanTime{&b.CreatedAt}, &b.CreatedBy)
	return b, err
}

// MaxBlocks is the most blocks a tenant may hold (every index snapshot carries them).
const MaxBlocks = 10000

// InsertBlock bans a body hash in a tenant (ErrExists if it is banned already).
func (t *Tx) InsertBlock(ctx context.Context, b *Block) error {
	if len(b.Reason) > 400 || len(b.CreatedBy) > 400 {
		return fmt.Errorf("%w: a value is too long", ErrInvalid)
	}
	if ok, err := t.Blocked(ctx, b.TenantID, b.BodyHash); err != nil || ok {
		if err == nil {
			err = fmt.Errorf("%w: block", ErrExists)
		}
		return err
	}
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM blocks WHERE tenant_id = ?", b.TenantID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxBlocks {
		return fmt.Errorf("%w: a tenant holds at most %d blocks", ErrInvalid, MaxBlocks)
	}
	b.CreatedAt = t.Now()
	_, err := t.exec(ctx, "INSERT INTO blocks ("+blockCols+") VALUES (?, ?, ?, ?, ?)", b.TenantID, b.BodyHash, b.Reason,
		t.s.d.timeArg(b.CreatedAt), b.CreatedBy)
	return t.s.mapErr(err, "block")
}

// DeleteBlock lifts a ban.
func (t *Tx) DeleteBlock(ctx context.Context, tenantID, hash string) error {
	res, err := t.exec(ctx, "DELETE FROM blocks WHERE tenant_id = ? AND body_hash = ?", tenantID, hash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: block %s", ErrNotFound, hash)
	}
	return nil
}

// Blocked reports whether a body hash is banned in a tenant.
func (t *Tx) Blocked(ctx context.Context, tenantID, hash string) (bool, error) {
	var n int
	err := t.queryRow(ctx, "SELECT COUNT(*) FROM blocks WHERE tenant_id = ? AND body_hash = ?", tenantID, hash).Scan(&n)
	return n > 0, err
}

// Blocked is the read-only form, outside a transaction.
func (s *Store) Blocked(ctx context.Context, tenantID, hash string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT COUNT(*) FROM blocks WHERE tenant_id = ? AND body_hash = ?"), tenantID, hash).Scan(&n)
	return n > 0, err
}

// GetBlock reads one block.
func (s *Store) GetBlock(ctx context.Context, tenantID, hash string) (Block, error) {
	b, err := scanBlock(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+blockCols+" FROM blocks WHERE tenant_id = ? AND body_hash = ?"),
		tenantID, hash))
	return b, notFound(err, "block "+hash)
}

// ListBlocks lists a tenant's blocks by body hash.
func (s *Store) ListBlocks(ctx context.Context, tenantID string) ([]Block, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind("SELECT "+blockCols+" FROM blocks WHERE tenant_id = ? ORDER BY body_hash"), tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Block
	for rows.Next() {
		b, err := scanBlock(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ProvidedNames lists the names of a type a tenant's upstreams provide (spec 0009): an upstream's
// names are of its home channel's type (spec 0034).
func (s *Store) ProvidedNames(ctx context.Context, tenantID, packageType string) (map[string]bool, error) {
	out := map[string]bool{}
	err := s.InTx(ctx, "", func(tx *Tx) error {
		names, err := tx.strings(ctx, `SELECT DISTINCT e.name FROM upstream_entries e JOIN upstreams u ON u.id = e.upstream_id
JOIN channels c ON c.id = u.channel_id WHERE u.tenant_id = ? AND c.package_type = ?`, tenantID, packageType)
		if err != nil {
			return err
		}
		for _, n := range names {
			if n != "*" { // "*" reserves no name
				out[n] = true
			}
		}
		// spec 0039: a name a "*" mirror discovered, once that upstream holds a release of it (as
		// UpstreamProvides counts it)
		rows, err := tx.query(ctx, `SELECT d.upstream_id, d.name FROM upstream_names d JOIN upstreams u ON u.id = d.upstream_id
JOIN channels c ON c.id = u.channel_id WHERE u.tenant_id = ? AND c.package_type = ?`, tenantID, packageType)
		if err != nil {
			return err
		}
		defer rows.Close()
		found := map[[2]string]bool{}
		for rows.Next() {
			var id, n string
			if err := rows.Scan(&id, &n); err != nil {
				return err
			}
			found[[2]string{UpstreamSource(id), n}] = true
		}
		if err := rows.Err(); err != nil || len(found) == 0 {
			return err
		}
		rows.Close()
		held, err := tx.query(ctx, "SELECT DISTINCT source, name FROM releases WHERE tenant_id = ? AND source LIKE ?", tenantID,
			SourceUpstreamPrefix+"%")
		if err != nil {
			return err
		}
		defer held.Close()
		for held.Next() {
			var src, n string
			if err := held.Scan(&src, &n); err != nil {
				return err
			}
			if found[[2]string{src, n}] {
				out[n] = true
			}
		}
		return held.Err()
	})
	return out, err
}

// Replacement reports whether a release replaces an upstream's build (spec 0009): neither it nor
// its Build came from an upstream.
func (c Candidate) Replacement() bool {
	return c.Origin != OriginUpstream && c.BuildOrigin != OriginUpstream
}

// BlockedHashes lists a tenant's banned body hashes (the index marks yanked rows with them).
func (s *Store) BlockedHashes(ctx context.Context, tenantID string) (map[string]bool, error) {
	bs, err := s.ListBlocks(ctx, tenantID)
	out := map[string]bool{}
	for _, b := range bs {
		out[b.BodyHash] = true
	}
	return out, err
}

// LiveReleasesWithBody lists a channel's releases that are not yanked and hold a body.
func (t *Tx) LiveReleasesWithBody(ctx context.Context, channelID, hash string) ([]Release, error) {
	rows, err := t.query(ctx, "SELECT "+releaseCols+" FROM releases r JOIN builds b ON b.id = r.build_id"+
		" WHERE r.channel_id = ? AND b.body_hash = ? AND r.state <> 'yanked'", channelID, hash)
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

// nullBytes is NULL for empty bytes, as a typed nil: SQL Server refuses an untyped NULL for varbinary.
func nullBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}
