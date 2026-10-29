package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Spec 0033 phase B part 1: distributors' publishers and withdrawals.

// ErrWithdrawn is a withdrawn entry (spec 0033 phase B): it is served nowhere and never placed
// again, a release of it is never made active, current or public again, its body never arrives again
// in its source, and the record stays (never purged).
var ErrWithdrawn = errors.New("store: the entry was withdrawn")

// MaxWithdrawReason bounds a withdrawal's reason.
const MaxWithdrawReason = 400

// MaxDistributor bounds a distributor's name.
const MaxDistributor = 128

// validText says whether s is 1 to max bytes of printable text (a distributor, a reason).
func validText(s string, max int) bool {
	return s != "" && len(s) <= max && utf8.ValidString(s) && strings.IndexFunc(s, func(c rune) bool { return !unicode.IsPrint(c) }) < 0
}

// SetPublisherDistributor renames a distributor's publisher's distributor (spec 0033 phase B):
// distributor is the distributor's name (1 to 128 bytes of printable text). It returns the publisher
// as it was. A publisher without the mark is not found (spec 0037: an existing publisher is never
// marked; InsertPublisher marks one at its creation). A mark is never removed: a distributor's
// publisher is removed instead.
func (t *Tx) SetPublisherDistributor(ctx context.Context, tenantID, name, distributor string) (Publisher, error) {
	if !validText(distributor, MaxDistributor) {
		return Publisher{}, fmt.Errorf("%w: distributor: 1 to %d bytes of printable text", ErrInvalid, MaxDistributor)
	}
	p, err := t.GetPublisher(ctx, tenantID, name)
	if err == nil && p.Distributor == "" {
		err = fmt.Errorf("%w: publisher %s is not a distributor's", ErrNotFound, name)
	}
	if err != nil || p.Distributor == distributor {
		return p, err
	}
	if _, err := t.exec(ctx, "UPDATE publishers SET distributor = ? WHERE id = ?", distributor, p.ID); err != nil {
		return p, err
	}
	return p, t.BumpAuth(ctx, tenantID) // the tenant's cached publishers carry it
}

// TenantActive reports, inside a transaction, whether a tenant is active (not suspended, its deletion
// not started), reading its row with a shared lock held until commit (as WritableTenant).
func (t *Tx) TenantActive(ctx context.Context, id string) (bool, error) {
	var state string
	var at time.Time
	if err := t.queryRow(ctx, t.lockedTenantQuery("SELECT state, deleting_at FROM tenants", "id = ?", false), id).Scan(&state, scanTime{&at}); err != nil {
		return false, notFound(err, "tenant")
	}
	return state == TenantActive && at.IsZero(), nil
}

// PublisherUsed reports whether a publisher has left any trace as a source (spec 0036 phase 2: only an
// unused distributor's publisher has its distributor renamed): a build in the catalogue (live or
// withdrawn), a release, a purged slot, a withdrawn version, a channel's rule or follow naming it, a
// hold, or a download count or installers.
func (t *Tx) PublisherUsed(ctx context.Context, p Publisher) (bool, error) {
	if p.Hold > 0 {
		return true, nil
	}
	src := PublisherSource(p.ID)
	ofTenant := "channel_id IN (SELECT id FROM channels WHERE tenant_id = ?)"
	for _, q := range []string{
		"SELECT COUNT(*) FROM builds WHERE source = ? AND tenant_id = ?",
		"SELECT COUNT(*) FROM releases WHERE source = ? AND tenant_id = ?",
		"SELECT COUNT(*) FROM purged_slots WHERE source = ? AND " + ofTenant,
		"SELECT COUNT(*) FROM withdrawn_versions WHERE source = ? AND tenant_id = ?",
		"SELECT COUNT(*) FROM channel_sources WHERE source = ? AND " + ofTenant,
		"SELECT COUNT(*) FROM channel_follows WHERE source = ? AND " + ofTenant,
		// spec 0036 phase 2: its downloads count for its distributor
		"SELECT COUNT(*) FROM download_counts WHERE source = ? AND tenant_id = ?",
		"SELECT COUNT(*) FROM download_installers WHERE source = ? AND tenant_id = ?",
	} {
		var n int
		if err := t.queryRow(ctx, q, src, p.TenantID).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}

// SourceDistributor is the distributor of a source of a tenant (spec 0033 phase B): a distributor's
// publisher's, live or removed (its record kept); "" for any other source, and for another
// tenant's publisher.
func (t *Tx) SourceDistributor(ctx context.Context, tenantID, source string) (string, error) {
	id, ok := strings.CutPrefix(source, SourcePublisherPrefix)
	if !ok {
		return "", nil
	}
	var d sql.NullString
	err := t.queryRow(ctx, "SELECT distributor FROM publishers WHERE id = ? AND tenant_id = ?", id, tenantID).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		err = t.queryRow(ctx, "SELECT distributor FROM removed_distributor_publishers WHERE publisher_id = ? AND tenant_id = ?", id,
			tenantID).Scan(&d)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
	}
	return d.String, err
}

// SourceDistributor reads a source's distributor outside a transaction.
func (s *Store) SourceDistributor(ctx context.Context, tenantID, source string) (string, error) {
	var d string
	err := s.InTx(ctx, "", func(tx *Tx) error {
		var err error
		d, err = tx.SourceDistributor(ctx, tenantID, source)
		return err
	})
	return d, err
}

// SourcePublisherIDPrefix names a distributor's publisher by its id (publisher-id:<id>), where its
// name is ambiguous: two of the tenant's distributors' publishers, live or removed, had one name.
const SourcePublisherIDPrefix = "publisher-id:"

// ErrAmbiguousSource is a distributor's publisher named publisher:<name> when several of the tenant's
// distributors' publishers (live or removed) had that name: publisher-id:<id> names one.
var ErrAmbiguousSource = errors.New("store: several distributors' publishers had that name; name one as publisher-id:<id>")

// DistributorSource is one of a tenant's distributors' publishers as a source (spec 0033 phase B),
// live or removed.
type DistributorSource struct {
	Source, PublisherID, Name, Distributor string
	Removed                                bool
	// Label is how answers name it: publisher:<name>, or publisher-id:<id> when another of the
	// tenant's distributors' publishers had the name too
	Label string
}

// DistributorSources are a tenant's distributors' publishers as sources (spec 0033 phase B), the live
// ones and those removed: what a distribution admin reads and acts on in the tenant, by source.
func (s *Store) DistributorSources(ctx context.Context, tenantID string) (map[string]DistributorSource, error) {
	out := map[string]DistributorSource{}
	add := func(removed bool) func(sc func(...any) error) error {
		return func(sc func(...any) error) error {
			var d DistributorSource
			if err := sc(&d.PublisherID, &d.Name, &d.Distributor); err != nil {
				return err
			}
			d.Source, d.Removed = PublisherSource(d.PublisherID), removed
			out[d.Source] = d
			return nil
		}
	}
	if err := s.eachRow(ctx, "SELECT id, name, distributor FROM publishers WHERE tenant_id = ? AND distributor IS NOT NULL", []any{tenantID},
		add(false)); err != nil {
		return nil, err
	}
	if err := s.eachRow(ctx, "SELECT publisher_id, name, distributor FROM removed_distributor_publishers WHERE tenant_id = ?", []any{tenantID},
		add(true)); err != nil {
		return nil, err
	}
	names := map[string]int{}
	for _, d := range out {
		names[d.Name]++
	}
	for k, d := range out {
		d.Label = SourcePublisherPrefix + d.Name
		if names[d.Name] > 1 {
			d.Label = SourcePublisherIDPrefix + d.PublisherID
		}
		out[k] = d
	}
	return out, nil
}

// ResolveDistributorSource reads a distributor's publisher as a caller names it into its source (spec
// 0033 phase B): publisher-id:<id>, or publisher:<name> when one of the tenant's distributors'
// publishers, live or removed, had that name (ErrAmbiguousSource when several did); ErrNoSource for
// none.
func (s *Store) ResolveDistributorSource(ctx context.Context, tenantID, label string) (string, error) {
	srcs, err := s.DistributorSources(ctx, tenantID)
	if err != nil {
		return "", err
	}
	if id, ok := strings.CutPrefix(label, SourcePublisherIDPrefix); ok {
		if d, ok := srcs[PublisherSource(id)]; ok && id != "" {
			return d.Source, nil
		}
		return "", ErrNoSource
	}
	name, ok := strings.CutPrefix(label, SourcePublisherPrefix)
	if !ok {
		return "", ErrNoSource
	}
	found := ""
	for _, d := range srcs {
		if d.Name != name {
			continue
		}
		if found != "" {
			return "", ErrAmbiguousSource
		}
		found = d.Source
	}
	if found == "" {
		return "", ErrNoSource
	}
	return found, nil
}

// Withdrawn is a withdrawal's result: the entries it withdrew (those withdrawn already are left as
// they were), and the ids of the tenant's channels holding a release of them, whose release version
// it bumped.
type Withdrawn struct {
	Builds   []Build
	Channels []string
}

// VersionWithdrawn reports whether a version of a name (of a type) is withdrawn from a source in a
// tenant (spec 0033 phase B: withdrawn_versions).
func (t *Tx) VersionWithdrawn(ctx context.Context, tenantID, packageType, source, name, version string) (bool, error) {
	var n int
	err := t.queryRow(ctx, `SELECT COUNT(*) FROM withdrawn_versions WHERE tenant_id = ? AND package_type = ? AND source = ? AND name = ?
AND package_version = ?`, tenantID, packageType, source, name, version).Scan(&n)
	return n > 0, err
}

// VersionWithdrawn reads it outside a transaction.
func (s *Store) VersionWithdrawn(ctx context.Context, tenantID, packageType, source, name, version string) (bool, error) {
	var w bool
	err := s.InTx(ctx, "", func(tx *Tx) error {
		var err error
		w, err = tx.VersionWithdrawn(ctx, tenantID, packageType, source, name, version)
		return err
	})
	return w, err
}

// Withdraw withdraws a version of a name (of a type) from a source in a tenant (spec 0033 phase B),
// in the caller's transaction, by actor with a reason (1 to MaxWithdrawReason bytes of printable
// text). The version itself is withdrawn (withdrawn_versions), whether the source holds entries of it
// or not (purged, collected, or not arrived yet): no build of it arrives again in the source
// (FindOrInsertBuild). Its entries not withdrawn yet are marked withdrawn, and every channel of the
// tenant holding a release of any of them gets its release version bumped, so its snapshot, which
// reads the entry's state, serves none of them from then on. The entries' rows are updated before the
// releases are read: a placement reads its entry with a row lock (BuildWithdrawn) that this update
// waits for, so it either sees the entry withdrawn or committed its release before this read (and the
// caller holds the tenant's upstream lock, which every release into its signed channels takes, and
// its auth lock, which a publication's build transaction takes). already says the version was
// withdrawn already.
func (t *Tx) Withdraw(ctx context.Context, tenantID, packageType, source, name, version, reason, actor string) (w Withdrawn, already bool, err error) {
	if !validText(reason, MaxWithdrawReason) {
		return w, false, fmt.Errorf("%w: reason: 1 to %d bytes of printable text", ErrInvalid, MaxWithdrawReason)
	}
	// the service checks the builds' grammars; the store keeps to printable text without separators
	if !validText(version, 64) || !validText(name, 64) || strings.ContainsAny(version+name, "/\\ ") {
		return w, false, fmt.Errorf("%w: a name and a version", ErrInvalid)
	}
	if already, err = t.VersionWithdrawn(ctx, tenantID, packageType, source, name, version); err != nil {
		return w, false, err
	}
	now := t.Now()
	if !already {
		if _, err := t.exec(ctx, `INSERT INTO withdrawn_versions (tenant_id, package_type, source, name, package_version, reason, withdrawn_by,
withdrawn_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, tenantID, packageType, source, name, version, reason, actor, t.s.d.timeArg(now)); err != nil {
			return w, false, t.s.mapErr(err, "withdrawn version")
		}
	}
	rows, err := t.query(ctx, "SELECT "+buildCols+` FROM builds WHERE tenant_id = ? AND package_type = ? AND source = ? AND name = ?
AND package_version = ? AND withdrawn_at IS NULL ORDER BY platform, slot, id`, tenantID, packageType, source, name, version)
	if err != nil {
		return w, false, err
	}
	var open []Build
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			rows.Close()
			return w, false, err
		}
		open = append(open, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return w, false, err
	}
	for _, b := range open {
		// a withdrawal committed meanwhile (the update waited for it) keeps its own time and reason
		res, err := t.exec(ctx, "UPDATE builds SET withdrawn_at = ?, withdrawn_reason = ? WHERE id = ? AND withdrawn_at IS NULL",
			t.s.d.timeArg(now), reason, b.ID)
		if err != nil {
			return w, false, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return w, false, err
		} else if n == 1 {
			b.WithdrawnAt, b.WithdrawnReason = now, reason
			w.Builds = append(w.Builds, b)
		}
	}
	if len(w.Builds) == 0 {
		return w, already, nil
	}
	chs, err := t.strings(ctx, `SELECT DISTINCT r.channel_id FROM releases r JOIN builds b ON b.id = r.build_id WHERE b.tenant_id = ?
AND b.package_type = ? AND b.source = ? AND b.name = ? AND b.package_version = ? ORDER BY r.channel_id`, tenantID, packageType, source, name, version)
	if err != nil {
		return w, false, err
	}
	for _, c := range chs {
		if err := t.BumpReleaseVersion(ctx, c); err != nil {
			return w, false, err
		}
	}
	w.Channels = chs
	return w, false, nil
}

// BuildWithdrawn reads whether a build is withdrawn, with a shared row lock its withdrawal's update
// waits for (spec 0033 phase B): a release's change that would make it active again checks it.
func (t *Tx) BuildWithdrawn(ctx context.Context, id string) (bool, error) {
	var w time.Time
	err := t.queryRow(ctx, t.lockedRowQuery("SELECT withdrawn_at FROM builds", "id = ?", false), id).Scan(scanTime{&w})
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: %s", ErrBuildGone, id)
	}
	return !w.IsZero(), err
}

// WithdrawnVersions are a tenant's catalogue of a type's withdrawn versions (spec 0033 phase B): those
// of each (source, name) with a withdrawn build.
func (s *Store) WithdrawnVersions(ctx context.Context, tenantID, packageType string) (map[[2]string][]string, error) {
	return s.versionsWhere(ctx, tenantID, packageType, "withdrawn_at IS NOT NULL")
}
