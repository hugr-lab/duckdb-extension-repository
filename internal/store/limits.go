package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
)

// Spec 0038: a tenant's limits on what its own administrators, publishers and upstreams make it grow
// by: its own catalogue bytes, and its channels, upstreams and own publishers.

// The limits' names, as the API, the configuration and the refusals name them.
const (
	LimitBytes      = "bytes"
	LimitChannels   = "channels"
	LimitUpstreams  = "upstreams"
	LimitPublishers = "publishers"
)

// LimitNames are the limits, in the order answers list them.
var LimitNames = []string{LimitBytes, LimitChannels, LimitUpstreams, LimitPublishers}

// A limit's value: LimitDefault (none stored: the server's default applies; a stored NULL),
// LimitUnlimited (no limit for bytes, the ceiling for a count), or 0 or more, the limit (0: no growth
// of that kind at all).
const (
	LimitDefault   int64 = -2
	LimitUnlimited int64 = -1
)

// MaxChannels is the most channels a tenant holds (spec 0038: a ceiling, not policy).
const MaxChannels = 1000

// LimitCeiling is a limit's ceiling: a count's most, LimitUnlimited for bytes.
func LimitCeiling(name string) int64 {
	switch name {
	case LimitChannels:
		return MaxChannels
	case LimitUpstreams:
		return MaxUpstreams
	case LimitPublishers:
		return MaxPublishers
	}
	return LimitUnlimited
}

// Limits are the four limits' values.
type Limits struct {
	Bytes, Channels, Upstreams, Publishers int64
}

// DefaultLimits are no values stored.
func DefaultLimits() Limits {
	return Limits{LimitDefault, LimitDefault, LimitDefault, LimitDefault}
}

// UnlimitedLimits are no limits at all (the configuration's defaults when it sets none).
func UnlimitedLimits() Limits {
	return Limits{LimitUnlimited, LimitUnlimited, LimitUnlimited, LimitUnlimited}
}

func (l *Limits) field(name string) *int64 {
	switch name {
	case LimitBytes:
		return &l.Bytes
	case LimitChannels:
		return &l.Channels
	case LimitUpstreams:
		return &l.Upstreams
	case LimitPublishers:
		return &l.Publishers
	}
	return nil
}

// Get is a limit's value by name.
func (l Limits) Get(name string) int64 { return *l.field(name) }

// Set sets a limit's value by name.
func (l *Limits) Set(name string, v int64) { *l.field(name) = v }

// ValidLimit checks a value a tenant may store for a limit: LimitDefault, LimitUnlimited, or 0 up to
// the ceiling.
func ValidLimit(name string, v int64) error {
	if !slices.Contains(LimitNames, name) {
		return fmt.Errorf("%w: unknown limit %q", ErrInvalid, name)
	}
	if v == LimitDefault || v == LimitUnlimited {
		return nil
	}
	if c := LimitCeiling(name); v < 0 || c >= 0 && v > c {
		return fmt.Errorf("%w: %s must be from 0 to %d, unlimited or default", ErrInvalid, name, c)
	}
	return nil
}

// ParseLimit reads a limit's value as the API, the configuration and the CLI write it: "unlimited",
// "default", a number, or for bytes also a size (50GiB: KiB, MiB, GiB or TiB). 0 means none. It
// checks the value against the limit (ValidLimit).
func ParseLimit(name, s string) (int64, error) {
	s = strings.TrimSpace(s)
	var v int64
	switch s {
	case "unlimited":
		v = LimitUnlimited
	case "default":
		v = LimitDefault
	default:
		mult := int64(1)
		if name == LimitBytes {
			for _, u := range []struct {
				suffix string
				m      int64
			}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}} {
				if n, ok := strings.CutSuffix(s, u.suffix); ok {
					s, mult = strings.TrimSpace(n), u.m
					break
				}
			}
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 || n > (1<<62)/mult {
			what := "a number"
			if name == LimitBytes {
				what = "a size (bytes, or a number with KiB, MiB, GiB or TiB)"
			}
			return 0, fmt.Errorf("%w: %s must be %s, unlimited or default", ErrInvalid, name, what)
		}
		v = n * mult
	}
	return v, ValidLimit(name, v)
}

// Effective is the value in force of each limit: a tenant's own value, else the server's default
// (LimitDefaults); an unlimited count is its ceiling, unlimited bytes stay LimitUnlimited.
func (s *Store) Effective(stored Limits) Limits {
	out := stored
	for _, n := range LimitNames {
		v := stored.Get(n)
		if v == LimitDefault {
			v = s.LimitDefaults.Get(n)
		}
		if v == LimitDefault || v == LimitUnlimited {
			v = LimitCeiling(n)
		}
		out.Set(n, v)
	}
	return out
}

// ErrTenantLimit is growth refused by a tenant limit (spec 0038); the error is a *LimitError.
var ErrTenantLimit = errors.New("store: a tenant limit")

// LimitError is a refusal by a tenant limit: which limit, and whether it is at its ceiling (no
// administrator can raise it further).
type LimitError struct {
	Limit     string
	AtCeiling bool
}

func (e *LimitError) Error() string {
	if e.AtCeiling {
		return fmt.Sprintf("the tenant holds the most %s kista allows", e.Limit)
	}
	return fmt.Sprintf("the tenant's %s limit is reached; a server administrator can raise it", e.Limit)
}

// Is makes a LimitError an ErrTenantLimit.
func (e *LimitError) Is(target error) bool { return target == ErrTenantLimit }

var limitRefusals = map[string]*atomic.Int64{LimitBytes: {}, LimitChannels: {}, LimitUpstreams: {}, LimitPublishers: {}}

// LimitRefused is the refusal by a limit whose value in force is limit, counted
// (LimitRefusals).
func LimitRefused(name string, limit int64) error {
	limitRefusals[name].Add(1)
	return &LimitError{Limit: name, AtCeiling: limit == LimitCeiling(name)}
}

// LimitRefusals are this process's refusals by each limit since it started (the metric
// kista.tenant.limit.refusals).
func LimitRefusals() map[string]int64 {
	out := map[string]int64{}
	for n, c := range limitRefusals {
		out[n] = c.Load()
	}
	return out
}

// TenantBytesLock orders a limited tenant's intakes of new bodies (spec 0038): a transaction that
// inserts a build of one of its own sources holds it as its last lock key.
func TenantBytesLock(tenantID string) string { return "kista/tenant-bytes/" + tenantID }

func limitArg(v int64) any {
	if v == LimitDefault {
		return nil
	}
	return v
}

func scanLimit(n sql.NullInt64) int64 {
	if !n.Valid {
		return LimitDefault
	}
	return n.Int64
}

// SetTenantLimits stores a tenant's own values of the limits named in set, the others unchanged
// (spec 0038). The tenant's version is not bumped: limits change no snapshot. The caller holds the
// tenant's row with an update lock (GetTenantForUpdate), which orders the change with every insert
// the limits check.
func (t *Tx) SetTenantLimits(ctx context.Context, tn *Tenant, set map[string]int64) error {
	q, args := "", []any{}
	for _, n := range LimitNames {
		v, ok := set[n]
		if !ok {
			continue
		}
		if err := ValidLimit(n, v); err != nil {
			return err
		}
		if q != "" {
			q += ", "
		}
		q += "limit_" + n + " = ?"
		args = append(args, limitArg(v))
	}
	if q == "" {
		return nil
	}
	if err := t.cas(ctx, "UPDATE tenants SET "+q+" WHERE id = ? AND deleting_at IS NULL", append(args, tn.ID), func() {}); err != nil {
		return err
	}
	for n, v := range set {
		tn.Limits.Set(n, v)
	}
	return nil
}

// tenantLimit is the value in force of a tenant's limit, its row read with a shared lock (an update
// lock the transaction holds already serves): a change of the limits waits for the transaction, or
// the transaction for the change.
func (t *Tx) tenantLimit(ctx context.Context, tenantID, name string) (int64, error) {
	tn, err := t.getTenant(ctx, "id = ?", tenantID, false)
	if err != nil {
		return 0, err
	}
	return t.s.Effective(tn.Limits).Get(name), nil
}

// underLimit refuses the next of a tenant's things counted by a limit when it holds n already (spec
// 0038): the limit first, so a checked caller meets the limit's 429 even at the ceiling; a call
// marked WithoutLimits meets only the ceiling, which the caller checks.
func (t *Tx) underLimit(ctx context.Context, tenantID, name string, n int) error {
	if LimitsLifted(ctx) {
		return nil
	}
	lim, err := t.tenantLimit(ctx, tenantID, name)
	if err != nil {
		return err
	}
	if lim >= 0 && int64(n) >= lim {
		return LimitRefused(name, lim)
	}
	return nil
}

// keptOwn is the condition of a build a tenant's bytes count (spec 0038): one the collector keeps
// (landed, withdrawn, or released), of a source that is not a distributor's publisher's, live or
// removed. b is the build, its tenant's own.
func (s *Store) keptOwn() string {
	id := "substr(b.source, 11)"
	if s.d.Name == "sqlserver" {
		id = "SUBSTRING(b.source, 11, 64)"
	}
	return `(b.landed = ? OR b.withdrawn_at IS NOT NULL OR EXISTS (SELECT 1 FROM releases r WHERE r.build_id = b.id))
AND NOT (b.source LIKE 'publisher:%' AND (
	EXISTS (SELECT 1 FROM publishers p WHERE p.id = ` + id + ` AND p.distributor IS NOT NULL)
	OR EXISTS (SELECT 1 FROM removed_distributor_publishers x WHERE x.tenant_id = b.tenant_id AND x.publisher_id = ` + id + `)))`
}

// tenantBytes is a tenant's own catalogue bytes (spec 0038): each distinct body of its kept own
// builds once, and the icons of its own sources' entries that still have a kept build.
func (s *Store) tenantBytes(ctx context.Context, q queryFn, tenantID string) (int64, error) {
	icons := "SUM(m.icon_bytes)"
	if s.d.Name == "sqlserver" {
		icons = "SUM(CAST(m.icon_bytes AS bigint))" // an int sum overflows at 2 GiB
	}
	keptOwn := s.keptOwn()
	query := `SELECT (SELECT COALESCE(SUM(d.len), 0) FROM (SELECT b.body_hash, MAX(COALESCE(b.body_len, 0)) AS len FROM builds b
	WHERE b.tenant_id = ? AND ` + keptOwn + ` GROUP BY b.body_hash) d),
(SELECT COALESCE(` + icons + `, 0) FROM entry_meta m WHERE m.tenant_id = ? AND m.icon_bytes IS NOT NULL
	AND EXISTS (SELECT 1 FROM builds b WHERE b.tenant_id = m.tenant_id AND b.package_type = m.package_type AND b.source = m.source
		AND b.name = m.name AND ` + keptOwn + `))`
	rows, err := q(ctx, query, tenantID, true, tenantID, true)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var bodies, iconBytes int64
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, sql.ErrNoRows
	}
	if err := rows.Scan(&bodies, &iconBytes); err != nil {
		return 0, err
	}
	return bodies + iconBytes, rows.Err()
}

// TenantBytes is a tenant's own catalogue bytes (spec 0038), read outside a transaction.
func (s *Store) TenantBytes(ctx context.Context, tenantID string) (int64, error) {
	return s.tenantBytes(ctx, s.query, tenantID)
}

// TenantUsage is what each of a tenant's limits counts: its own bytes, its channels, its upstreams
// and its own publishers.
func (s *Store) TenantUsage(ctx context.Context, tenantID string) (Limits, error) {
	var u Limits
	var err error
	if u.Bytes, err = s.TenantBytes(ctx, tenantID); err != nil {
		return u, err
	}
	for _, c := range []struct {
		n     *int64
		table string
		where string
	}{{&u.Channels, "channels", ""}, {&u.Upstreams, "upstreams", ""}, {&u.Publishers, "publishers", " AND distributor IS NULL"}} {
		if err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT COUNT(*) FROM "+c.table+" WHERE tenant_id = ?"+c.where), tenantID).Scan(c.n); err != nil {
			return u, err
		}
	}
	return u, nil
}

// BytesLocks are the lock keys a transaction that inserts a build of a tenant's own source adds last
// (spec 0038): TenantBytesLock while the tenant has a bytes limit, none otherwise.
func (s *Store) BytesLocks(ctx context.Context, tenantID string) ([]string, error) {
	tn, err := s.GetTenantAnyByID(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if s.Effective(tn.Limits).Bytes < 0 {
		return nil, nil
	}
	return []string{TenantBytesLock(tenantID)}, nil
}

type liftedKey struct{}

// WithoutLimits marks what ctx's calls add as a bundle import's or the bootstrap's (spec 0038):
// counted, but never refused by a tenant limit (only the ceilings bound it).
func WithoutLimits(ctx context.Context) context.Context {
	return context.WithValue(ctx, liftedKey{}, true)
}

// LimitsLifted reports whether ctx is marked WithoutLimits.
func LimitsLifted(ctx context.Context) bool { v, _ := ctx.Value(liftedKey{}).(bool); return v }

// ownSource reports whether a source of a tenant is its own for the limits: not a distributor's
// publisher's, live or removed.
func ownSource(ctx context.Context, q queryFn, tenantID, source string) (bool, error) {
	id, ok := strings.CutPrefix(source, SourcePublisherPrefix)
	if !ok {
		return true, nil
	}
	n, err := count(ctx, q, `SELECT (SELECT COUNT(*) FROM publishers WHERE id = ? AND distributor IS NOT NULL)
	+ (SELECT COUNT(*) FROM removed_distributor_publishers WHERE tenant_id = ? AND publisher_id = ?)`, id, tenantID, id)
	return n == 0, err
}

func count(ctx context.Context, q queryFn, query string, args ...any) (int64, error) {
	rows, err := q(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int64
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
	}
	return n, rows.Err()
}

// bytesRoom refuses growth bytes of a tenant's own source when its bytes limit has no room for them
// (spec 0038): a body the tenant counts already (bodyHash; "" for an icon) adds nothing. In a
// transaction (t), the tenant's row is read with a shared lock, which orders the check with a change
// of the limits, and the figure under the tenant's bytes lock, which orders its intakes; outside one
// it is an early, cheap refusal, and the transaction decides.
func (s *Store) bytesRoom(ctx context.Context, t *Tx, tenantID, source, bodyHash string, growth int64) error {
	if LimitsLifted(ctx) || growth <= 0 {
		return nil
	}
	q, tn, err := s.query, Tenant{}, error(nil)
	if t != nil {
		q = t.query
		tn, err = t.getTenant(ctx, "id = ?", tenantID, false)
	} else {
		tn, err = s.GetTenantAnyByID(ctx, tenantID)
	}
	if err != nil {
		return err
	}
	lim := s.Effective(tn.Limits).Bytes
	if lim < 0 {
		return nil
	}
	if own, err := ownSource(ctx, q, tenantID, source); err != nil || !own {
		return err
	}
	if bodyHash != "" {
		if n, err := count(ctx, q, "SELECT COUNT(*) FROM builds b WHERE b.tenant_id = ? AND b.body_hash = ? AND "+s.keptOwn(),
			tenantID, bodyHash, true); err != nil || n > 0 {
			return err
		}
	}
	if t != nil {
		if err := t.hold(ctx, TenantBytesLock(tenantID)); err != nil {
			return err
		}
	}
	used, err := s.tenantBytes(ctx, q, tenantID)
	if err != nil {
		return err
	}
	if used+growth > lim {
		return LimitRefused(LimitBytes, lim)
	}
	return nil
}

// BytesRoom refuses a new body of size bytes (bodyHash; "" for an icon's growth) of a tenant's own
// source when its bytes limit has no room for it (spec 0038), read outside any lock: an early,
// cheap refusal before the body is stored; the transaction that adds it decides (Tx.BytesRoom, a
// build's insert).
func (s *Store) BytesRoom(ctx context.Context, tenantID, source, bodyHash string, size int64) error {
	return s.bytesRoom(ctx, nil, tenantID, source, bodyHash, size)
}

// keepRoom refuses keeping a build of a tenant's own source (spec 0038: landing it, or releasing it
// when it has no release yet) when its tenant's bytes limit has no room for it. Only kept builds
// count, so this is the decisive check, under the tenant's bytes lock: a build is kept only here,
// whatever transaction inserted it, so two intakes in flight at once never both take the last
// room. A build kept already, or whose body the tenant counts, adds nothing; its entry's icon counts
// with the entry's first kept build.
func (t *Tx) keepRoom(ctx context.Context, buildID string) error {
	if LimitsLifted(ctx) {
		return nil
	}
	b, err := t.GetBuild(ctx, buildID)
	if errors.Is(err, ErrNotFound) {
		return nil // collected meanwhile: the write's own statement says so (ErrBuildGone)
	} else if err != nil {
		return err
	}
	lim, err := t.tenantLimit(ctx, b.TenantID, LimitBytes)
	if err != nil || lim < 0 {
		return err
	}
	if own, err := ownSource(ctx, t.query, b.TenantID, b.Source); err != nil || !own {
		return err
	}
	keptOwn := t.s.keptOwn()
	kept := func(where string, args ...any) (bool, error) {
		n, err := count(ctx, t.query, "SELECT COUNT(*) FROM builds b WHERE "+where+" AND "+keptOwn, append(args, true)...)
		return n > 0, err
	}
	if k, err := kept("b.id = ?", b.ID); err != nil || k {
		return err // kept already (a placement, a promotion): no lock taken
	}
	// the lock before the rest is read, so a body kept meanwhile is seen counted
	if err := t.hold(ctx, TenantBytesLock(b.TenantID)); err != nil {
		return err
	}
	var growth int64
	if k, err := kept("b.tenant_id = ? AND b.body_hash = ?", b.TenantID, b.BodyHash); err != nil {
		return err
	} else if !k {
		growth += max(b.BodyLen, 1)
	}
	if k, err := kept("b.tenant_id = ? AND b.package_type = ? AND b.source = ? AND b.name = ?", b.TenantID, b.PackageType, b.Source, b.Name); err != nil {
		return err
	} else if !k {
		icon, err := count(ctx, t.query, "SELECT COALESCE(MAX(icon_bytes), 0) FROM entry_meta WHERE tenant_id = ? AND package_type = ? AND source = ? AND name = ?",
			b.TenantID, b.PackageType, b.Source, b.Name)
		if err != nil {
			return err
		}
		growth += icon
	}
	if growth == 0 {
		return nil
	}
	used, err := t.s.tenantBytes(ctx, t.query, b.TenantID)
	if err != nil {
		return err
	}
	if used+growth > lim {
		return LimitRefused(LimitBytes, lim)
	}
	return nil
}

// BytesRoom is Store.BytesRoom in a transaction, under the tenant's bytes lock: an icon's growth.
func (t *Tx) BytesRoom(ctx context.Context, tenantID, source, bodyHash string, growth int64) error {
	return t.s.bytesRoom(ctx, t, tenantID, source, bodyHash, growth)
}

// hold takes a lock key the transaction did not take at its start (a limit set after the caller read
// it): a deadlock this may make is the engine's to detect, and the transaction is retried.
func (t *Tx) hold(ctx context.Context, key string) error {
	if slices.Contains(t.keys, key) {
		return nil
	}
	if err := t.s.d.lock(ctx, t.tx, key, t.s.LockTimeout); err != nil {
		return err
	}
	t.keys = append(t.keys, key)
	return nil
}
