package store

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
)

// Package groups (spec 0017's extension groups; spec 0034 names them): named, explicit sets of
// package names in a tenant that grants may name. Grants on them live apart from grants, so code that does not know groups never reads one
// as a tenant- or channel-wide grant.

// Limits on package groups: the counts are ceilings the configuration sets (SetCeilings).
var (
	MaxPackageGroups = 100
	MaxGroupMembers  = 1000  // per group
	MaxTenantMembers = 10000 // per tenant, every group
)

// MaxGroupDescription is a description's most bytes.
const MaxGroupDescription = 256

var groupName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidGroupName checks an extension group's name (a path segment).
func ValidGroupName(n string) error {
	if !groupName.MatchString(n) {
		return fmt.Errorf("%w: extension group name %q must match [a-z0-9][a-z0-9-]{0,62}", ErrInvalid, n)
	}
	return nil
}

// ValidGroupDescription checks a description: at most 256 bytes of printable text.
func ValidGroupDescription(d string) error {
	if len(d) > MaxGroupDescription || !utf8.ValidString(d) || strings.ContainsFunc(d, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}) {
		return fmt.Errorf("%w: a description is at most %d bytes of printable text", ErrInvalid, MaxGroupDescription)
	}
	return nil
}

// PackageGroup is a tenant's package group (an extension group in the API). Members and Grants are counts, filled by reads.
type PackageGroup struct {
	ID, TenantID, Name, Description string
	Version                         int64
	Members, Grants                 int
	CreatedAt                       time.Time
	CreatedBy                       string
}

// GroupMember is a package in a group: its type and name (spec 0034).
type GroupMember struct {
	PackageType, Name string
	AddedAt           time.Time
	AddedBy           string
}

// GroupSet is a group as authorization reads it: its name and members, by (type, name).
type GroupSet struct {
	Name    string
	Members map[pkgtype.Key]bool
}

// groupCols are a group's columns and counts: its members of the registered types (spec 0034: a
// member of a type this binary does not know is read as nothing, and not counted either).
func groupCols() string {
	return `g.id, g.tenant_id, g.name, g.description, g.version, g.created_at, g.created_by,
(SELECT COUNT(*) FROM package_group_members m WHERE m.group_id = g.id AND m.package_type IN (` + knownTypesSQL() + `)),
(SELECT COUNT(*) FROM package_group_grants x WHERE x.group_id = g.id)`
}

// knownTypesSQL lists the registered package types as SQL literals ('a', 'b'): values the binary
// registers, never a request's.
func knownTypesSQL() string {
	ts := pkgtype.Types()
	for i, t := range ts {
		ts[i] = "'" + strings.ReplaceAll(t, "'", "''") + "'"
	}
	return strings.Join(ts, ", ")
}

func scanGroup(r interface{ Scan(...any) error }) (PackageGroup, error) {
	var g PackageGroup
	err := r.Scan(&g.ID, &g.TenantID, &g.Name, &g.Description, &g.Version, scanTime{&g.CreatedAt}, &g.CreatedBy, &g.Members, &g.Grants)
	return g, err
}

// InsertPackageGroup adds a group (at most MaxPackageGroups per tenant).
func (t *Tx) InsertPackageGroup(ctx context.Context, g *PackageGroup) error {
	if err := ValidGroupName(g.Name); err != nil {
		return err
	}
	if err := ValidGroupDescription(g.Description); err != nil {
		return err
	}
	if len(g.CreatedBy) > 400 {
		return fmt.Errorf("%w: a value is too long", ErrInvalid)
	}
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM package_groups WHERE tenant_id = ?", g.TenantID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxPackageGroups {
		return fmt.Errorf("%w: a tenant holds at most %d extension groups", ErrInvalid, MaxPackageGroups)
	}
	g.ID, g.CreatedAt, g.Version, g.Members, g.Grants = NewID(), t.Now(), 1, 0, 0
	_, err := t.exec(ctx, `INSERT INTO package_groups (id, tenant_id, name, description, version, created_at, created_by)
VALUES (?, ?, ?, ?, ?, ?, ?)`, g.ID, g.TenantID, g.Name, g.Description, g.Version, t.s.d.timeArg(g.CreatedAt), g.CreatedBy)
	if err := t.s.mapErr(err, "extension group "+g.Name); err != nil {
		return err
	}
	return t.BumpAuth(ctx, g.TenantID)
}

// GetPackageGroup reads a tenant's group by name, with its counts.
func (t *Tx) GetPackageGroup(ctx context.Context, tenantID, name string) (PackageGroup, error) {
	g, err := scanGroup(t.queryRow(ctx, "SELECT "+groupCols()+" FROM package_groups g WHERE g.tenant_id = ? AND g.name = ?", tenantID, name))
	return g, notFound(err, "extension group "+name)
}

// GetPackageGroup reads a tenant's group by name, with its counts.
func (s *Store) GetPackageGroup(ctx context.Context, tenantID, name string) (PackageGroup, error) {
	g, err := scanGroup(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+groupCols()+" FROM package_groups g WHERE g.tenant_id = ? AND g.name = ?"), tenantID, name))
	return g, notFound(err, "extension group "+name)
}

// ListPackageGroups lists a tenant's groups by name; with a package's name, those holding that
// package (of packageType).
func (s *Store) ListPackageGroups(ctx context.Context, tenantID, packageType, extension string) ([]PackageGroup, error) {
	q, args := "SELECT "+groupCols()+" FROM package_groups g WHERE g.tenant_id = ?", []any{tenantID}
	if extension != "" {
		q += " AND EXISTS (SELECT 1 FROM package_group_members m WHERE m.group_id = g.id AND m.package_type = ? AND m.package = ?)"
		args = append(args, packageType, extension)
	}
	rows, err := s.db.QueryContext(ctx, s.d.rebind(q+" ORDER BY g.name"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PackageGroup
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetGroupDescription changes a group's description (compare-and-set on its version; expected 0
// takes the group's). Descriptions are not authorization: auth_version stays.
func (t *Tx) SetGroupDescription(ctx context.Context, g *PackageGroup, description string, expected int64) error {
	if err := ValidGroupDescription(description); err != nil {
		return err
	}
	if expected == 0 {
		expected = g.Version
	}
	if description == g.Description && expected == g.Version {
		return nil // no change: the version, and clients' ETags, stay
	}
	return t.cas(ctx, "UPDATE package_groups SET description = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{description, g.ID, expected}, func() { g.Description, g.Version = description, expected+1 })
}

// DeletePackageGroup removes a group and its members (compare-and-set on its version; expected 0
// takes the group's). The caller refuses a group that a grant names first: the foreign key would.
func (t *Tx) DeletePackageGroup(ctx context.Context, g PackageGroup, expected int64) error {
	if expected == 0 {
		expected = g.Version
	}
	if _, err := t.exec(ctx, "DELETE FROM package_group_members WHERE group_id = ?", g.ID); err != nil {
		return err
	}
	if err := t.cas(ctx, "DELETE FROM package_groups WHERE id = ? AND version = ?", []any{g.ID, expected}, func() {}); err != nil {
		return err
	}
	return t.BumpAuth(ctx, g.TenantID)
}

// GroupMembers lists a group's members by name (and type), every type's.
func (t *Tx) GroupMembers(ctx context.Context, groupID string) ([]GroupMember, error) {
	return groupMembers(ctx, t.query, groupID)
}

// GroupMembers lists a group's members by name (and type); those of a type this binary does not
// know are left out.
func (s *Store) GroupMembers(ctx context.Context, groupID string) ([]GroupMember, error) {
	ms, err := groupMembers(ctx, s.query, groupID)
	return slices.DeleteFunc(ms, func(m GroupMember) bool { return !pkgtype.Known(m.PackageType) }), err
}

func groupMembers(ctx context.Context, query queryFn, groupID string) ([]GroupMember, error) {
	rows, err := query(ctx, "SELECT package_type, package, added_at, added_by FROM package_group_members WHERE group_id = ? ORDER BY package, package_type", groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupMember
	for rows.Next() {
		var m GroupMember
		if err := rows.Scan(&m.PackageType, &m.Name, scanTime{&m.AddedAt}, &m.AddedBy); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ChangeGroupMembers adds and removes members (already valid names of registered types), skipping
// those already present or not members, and answers what changed. The limits are counted here: the
// caller holds the tenant's auth lock.
func (t *Tx) ChangeGroupMembers(ctx context.Context, g PackageGroup, add, remove []pkgtype.Key, actor string) (added, removed []pkgtype.Key, err error) {
	if len(actor) > 400 {
		return nil, nil, fmt.Errorf("%w: a value is too long", ErrInvalid)
	}
	ms, err := t.GroupMembers(ctx, g.ID)
	if err != nil {
		return nil, nil, err
	}
	have := map[pkgtype.Key]bool{}
	for _, m := range ms {
		have[pkgtype.Key{Type: m.PackageType, Name: m.Name}] = true
	}
	for _, n := range remove {
		if !have[n] {
			continue
		}
		if _, err := t.exec(ctx, "DELETE FROM package_group_members WHERE group_id = ? AND package_type = ? AND package = ?", g.ID, n.Type, n.Name); err != nil {
			return nil, nil, err
		}
		removed = append(removed, n)
		delete(have, n)
	}
	for _, n := range add {
		if have[n] {
			continue
		}
		if !pkgtype.Known(n.Type) {
			return nil, nil, fmt.Errorf("%w: package type %q", ErrInvalid, n.Type)
		}
		have[n] = true
		added = append(added, n)
	}
	if len(added) > 0 {
		if len(have) > MaxGroupMembers {
			return nil, nil, fmt.Errorf("%w: an extension group holds at most %d names", ErrInvalid, MaxGroupMembers)
		}
		var total int
		if err := t.queryRow(ctx, "SELECT COUNT(*) FROM package_group_members WHERE tenant_id = ?", g.TenantID).Scan(&total); err != nil {
			return nil, nil, err
		}
		if total+len(added) > MaxTenantMembers {
			return nil, nil, fmt.Errorf("%w: a tenant's extension groups hold at most %d names", ErrInvalid, MaxTenantMembers)
		}
		now := t.s.d.timeArg(t.Now())
		for _, n := range added {
			if _, err := t.exec(ctx, "INSERT INTO package_group_members (group_id, tenant_id, package_type, package, added_at, added_by) VALUES (?, ?, ?, ?, ?, ?)",
				g.ID, g.TenantID, n.Type, n.Name, now, actor); err != nil {
				return nil, nil, err
			}
		}
	}
	if len(added)+len(removed) > 0 {
		if err := t.BumpAuth(ctx, g.TenantID); err != nil {
			return nil, nil, err
		}
	}
	return added, removed, nil
}

// readGroups reads a tenant's groups and member sets, by group id.
func readGroups(ctx context.Context, query queryFn, tenantID string) (map[string]*GroupSet, error) {
	rows, err := query(ctx, "SELECT id, name FROM package_groups WHERE tenant_id = ?", tenantID)
	if err != nil {
		return nil, err
	}
	out := map[string]*GroupSet{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = &GroupSet{Name: name, Members: map[pkgtype.Key]bool{}}
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	rows, err = query(ctx, "SELECT group_id, package_type, package FROM package_group_members WHERE tenant_id = ?", tenantID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, typ, ext string
		if err := rows.Scan(&id, &typ, &ext); err != nil {
			rows.Close()
			return nil, err
		}
		// spec 0034: a member of a type this binary does not know is covered by no group grant
		if g := out[id]; g != nil && pkgtype.Known(typ) {
			g.Members[pkgtype.Key{Type: typ, Name: ext}] = true
		}
	}
	return out, closeRows(rows)
}
