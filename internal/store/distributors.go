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

// validText says whether s is 1 to max bytes of printable text (a holder, a reason).
func validText(s string, max int) bool {
	return s != "" && len(s) <= max && utf8.ValidString(s) && strings.IndexFunc(s, func(c rune) bool { return !unicode.IsPrint(c) }) < 0
}

// SetPublisherDistributor marks a tenant's publisher a distributor's (spec 0033 phase B): distributor
// is the distributor's name, as a tenant reservation names its holder (1 to 128 bytes of printable
// text). It returns the publisher as it was. A mark is never removed: a distributor's publisher is
// removed instead.
func (t *Tx) SetPublisherDistributor(ctx context.Context, tenantID, name, distributor string) (Publisher, error) {
	if !validText(distributor, MaxHolder) {
		return Publisher{}, fmt.Errorf("%w: distributor: 1 to %d bytes of printable text", ErrInvalid, MaxHolder)
	}
	p, err := t.GetPublisher(ctx, tenantID, name)
	if err != nil || p.Distributor == distributor {
		return p, err
	}
	if _, err := t.exec(ctx, "UPDATE publishers SET distributor = ? WHERE id = ?", distributor, p.ID); err != nil {
		return p, err
	}
	return p, t.BumpAuth(ctx, tenantID) // the tenant's cached publishers carry it
}

// SourceDistributor is the distributor of a source (spec 0033 phase B): a distributor's publisher's,
// live or removed (its record kept); "" for any other source.
func (t *Tx) SourceDistributor(ctx context.Context, source string) (string, error) {
	id, ok := strings.CutPrefix(source, SourcePublisherPrefix)
	if !ok {
		return "", nil
	}
	var d sql.NullString
	err := t.queryRow(ctx, "SELECT distributor FROM publishers WHERE id = ?", id).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		err = t.queryRow(ctx, "SELECT distributor FROM removed_distributor_publishers WHERE publisher_id = ?", id).Scan(&d)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
	}
	return d.String, err
}

// SourceDistributor reads a source's distributor outside a transaction.
func (s *Store) SourceDistributor(ctx context.Context, source string) (string, error) {
	var d string
	err := s.InTx(ctx, "", func(tx *Tx) error {
		var err error
		d, err = tx.SourceDistributor(ctx, source)
		return err
	})
	return d, err
}

// DistributorSources are a tenant's distributors' publishers as sources (spec 0033 phase B), the live
// ones and those removed, each labelled publisher:<name>: what a distribution admin reads and acts on
// in the tenant. A live publisher's name wins over a removed one's; of two removed ones of one name,
// the one removed last.
func (s *Store) DistributorSources(ctx context.Context, tenantID string) (map[string]string, error) {
	out := map[string]string{}
	taken := map[string]bool{}
	err := s.eachRow(ctx, "SELECT id, name FROM publishers WHERE tenant_id = ? AND distributor IS NOT NULL", []any{tenantID},
		func(sc func(...any) error) error {
			var id, name string
			if err := sc(&id, &name); err != nil {
				return err
			}
			out[PublisherSource(id)], taken[name] = SourcePublisherPrefix+name, true
			return nil
		})
	if err != nil {
		return nil, err
	}
	err = s.eachRow(ctx, "SELECT publisher_id, name FROM removed_distributor_publishers WHERE tenant_id = ? ORDER BY removed_at DESC, publisher_id",
		[]any{tenantID}, func(sc func(...any) error) error {
			var id, name string
			if err := sc(&id, &name); err != nil {
				return err
			}
			if !taken[name] {
				out[PublisherSource(id)], taken[name] = SourcePublisherPrefix+name, true
			}
			return nil
		})
	return out, err
}

// ResolveDistributorSource reads a distributor's publisher as a caller names it (publisher:<name>)
// into its source (spec 0033 phase B): the tenant's live publisher of that name when it is a
// distributor's, else the distributor's publisher of that name removed last; ErrNoSource for none.
func (s *Store) ResolveDistributorSource(ctx context.Context, tenantID, label string) (string, error) {
	srcs, err := s.DistributorSources(ctx, tenantID)
	if err != nil {
		return "", err
	}
	for src, l := range srcs {
		if l == label {
			return src, nil
		}
	}
	return "", ErrNoSource
}

// TenantReservedHolder is who holds a name of a type reserved in a tenant (spec 0030), inside the
// transaction; ok is false when it is not reserved there.
func (t *Tx) TenantReservedHolder(ctx context.Context, tenantID, packageType, name string) (holder string, ok bool, err error) {
	err = t.queryRow(ctx, "SELECT holder FROM tenant_reserved_names WHERE tenant_id = ? AND package_type = ? AND name = ?",
		tenantID, packageType, name).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return holder, err == nil, err
}

// TenantReservedHolder reads who holds a reserved name outside a transaction.
func (s *Store) TenantReservedHolder(ctx context.Context, tenantID, packageType, name string) (holder string, ok bool, err error) {
	err = s.InTx(ctx, "", func(tx *Tx) error {
		holder, ok, err = tx.TenantReservedHolder(ctx, tenantID, packageType, name)
		return err
	})
	return holder, ok, err
}

// Withdrawn is a withdrawal's result: the entries it withdrew (those withdrawn already are left as
// they were), and the ids of the tenant's channels holding a release of them, whose release version
// it bumped.
type Withdrawn struct {
	Builds   []Build
	Channels []string
}

// Withdraw withdraws a version of a name (of a type) from a source in a tenant (spec 0033 phase B), in
// the caller's transaction: its entries not withdrawn yet are marked withdrawn with the reason (1 to
// MaxWithdrawReason bytes of printable text), and every channel of the tenant holding a release of
// any of them gets its release version bumped, so its snapshot, which reads the entry's state, serves
// none of them from then on. The entries' rows are updated before the releases are read: a placement
// reads its entry with a row lock (BuildWithdrawn) that this update waits for, so it either sees the
// entry withdrawn or committed its release before this read (and the caller holds the tenant's
// upstream lock, which every release into its signed channels takes, besides). ErrNotFound when the
// source has no entry of the version; already says every entry was withdrawn already.
func (t *Tx) Withdraw(ctx context.Context, tenantID, packageType, source, name, version, reason string) (w Withdrawn, already bool, err error) {
	if !validText(reason, MaxWithdrawReason) {
		return w, false, fmt.Errorf("%w: reason: 1 to %d bytes of printable text", ErrInvalid, MaxWithdrawReason)
	}
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM builds WHERE tenant_id = ? AND package_type = ? AND source = ? AND name = ? AND package_version = ?",
		tenantID, packageType, source, name, version).Scan(&n); err != nil {
		return w, false, err
	}
	if n == 0 {
		return w, false, fmt.Errorf("%w: no entry of %s %s from the source", ErrNotFound, name, version)
	}
	rows, err := t.query(ctx, "SELECT "+buildCols+` FROM builds WHERE tenant_id = ? AND package_type = ? AND source = ? AND name = ?
AND package_version = ? AND withdrawn_at IS NULL ORDER BY platform, slot, id`, tenantID, packageType, source, name, version)
	if err != nil {
		return w, false, err
	}
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			rows.Close()
			return w, false, err
		}
		w.Builds = append(w.Builds, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return w, false, err
	}
	now := t.Now()
	marked := w.Builds[:0]
	for _, b := range w.Builds {
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
			marked = append(marked, b)
		}
	}
	w.Builds = marked
	if len(w.Builds) == 0 {
		return w, true, nil
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
