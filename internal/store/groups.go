package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Extension groups (spec 0017): named, explicit sets of extension names in a tenant that grants may
// name. Grants on them live apart from grants, so code that does not know groups never reads one
// as a tenant- or channel-wide grant.

// Limits on extension groups.
const (
	MaxExtensionGroups  = 100
	MaxGroupMembers     = 1000  // per group
	MaxTenantMembers    = 10000 // per tenant, every group
	MaxGroupDescription = 256   // bytes
)

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

// ExtensionGroup is a tenant's extension group. Members and Grants are counts, filled by reads.
type ExtensionGroup struct {
	ID, TenantID, Name, Description string
	Version                         int64
	Members, Grants                 int
	CreatedAt                       time.Time
	CreatedBy                       string
}

// GroupMember is an extension name in a group.
type GroupMember struct {
	Name    string
	AddedAt time.Time
	AddedBy string
}

// GroupSet is a group as authorization reads it: its name and member names.
type GroupSet struct {
	Name    string
	Members map[string]bool
}

const groupCols = `g.id, g.tenant_id, g.name, g.description, g.version, g.created_at, g.created_by,
(SELECT COUNT(*) FROM extension_group_members m WHERE m.group_id = g.id),
(SELECT COUNT(*) FROM extension_group_grants x WHERE x.group_id = g.id)`

func scanGroup(r interface{ Scan(...any) error }) (ExtensionGroup, error) {
	var g ExtensionGroup
	err := r.Scan(&g.ID, &g.TenantID, &g.Name, &g.Description, &g.Version, scanTime{&g.CreatedAt}, &g.CreatedBy, &g.Members, &g.Grants)
	return g, err
}

// InsertExtensionGroup adds a group (at most MaxExtensionGroups per tenant).
func (t *Tx) InsertExtensionGroup(ctx context.Context, g *ExtensionGroup) error {
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
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM extension_groups WHERE tenant_id = ?", g.TenantID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxExtensionGroups {
		return fmt.Errorf("%w: a tenant holds at most %d extension groups", ErrInvalid, MaxExtensionGroups)
	}
	g.ID, g.CreatedAt, g.Version, g.Members, g.Grants = NewID(), t.Now(), 1, 0, 0
	_, err := t.exec(ctx, `INSERT INTO extension_groups (id, tenant_id, name, description, version, created_at, created_by)
VALUES (?, ?, ?, ?, ?, ?, ?)`, g.ID, g.TenantID, g.Name, g.Description, g.Version, t.s.d.timeArg(g.CreatedAt), g.CreatedBy)
	if err := t.s.mapErr(err, "extension group "+g.Name); err != nil {
		return err
	}
	return t.BumpAuth(ctx, g.TenantID)
}

// GetExtensionGroup reads a tenant's group by name, with its counts.
func (t *Tx) GetExtensionGroup(ctx context.Context, tenantID, name string) (ExtensionGroup, error) {
	g, err := scanGroup(t.queryRow(ctx, "SELECT "+groupCols+" FROM extension_groups g WHERE g.tenant_id = ? AND g.name = ?", tenantID, name))
	return g, notFound(err, "extension group "+name)
}

// GetExtensionGroup reads a tenant's group by name, with its counts.
func (s *Store) GetExtensionGroup(ctx context.Context, tenantID, name string) (ExtensionGroup, error) {
	g, err := scanGroup(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+groupCols+" FROM extension_groups g WHERE g.tenant_id = ? AND g.name = ?"), tenantID, name))
	return g, notFound(err, "extension group "+name)
}

// ListExtensionGroups lists a tenant's groups by name; with extension, those holding that name.
func (s *Store) ListExtensionGroups(ctx context.Context, tenantID, extension string) ([]ExtensionGroup, error) {
	q, args := "SELECT "+groupCols+" FROM extension_groups g WHERE g.tenant_id = ?", []any{tenantID}
	if extension != "" {
		q += " AND EXISTS (SELECT 1 FROM extension_group_members m WHERE m.group_id = g.id AND m.extension = ?)"
		args = append(args, extension)
	}
	rows, err := s.db.QueryContext(ctx, s.d.rebind(q+" ORDER BY g.name"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExtensionGroup
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
func (t *Tx) SetGroupDescription(ctx context.Context, g *ExtensionGroup, description string, expected int64) error {
	if err := ValidGroupDescription(description); err != nil {
		return err
	}
	if expected == 0 {
		expected = g.Version
	}
	if description == g.Description && expected == g.Version {
		return nil // no change: the version, and clients' ETags, stay
	}
	return t.cas(ctx, "UPDATE extension_groups SET description = ?, version = version + 1 WHERE id = ? AND version = ?",
		[]any{description, g.ID, expected}, func() { g.Description, g.Version = description, expected+1 })
}

// DeleteExtensionGroup removes a group and its members (compare-and-set on its version; expected 0
// takes the group's). The caller refuses a group that a grant names first: the foreign key would.
func (t *Tx) DeleteExtensionGroup(ctx context.Context, g ExtensionGroup, expected int64) error {
	if expected == 0 {
		expected = g.Version
	}
	if _, err := t.exec(ctx, "DELETE FROM extension_group_members WHERE group_id = ?", g.ID); err != nil {
		return err
	}
	if err := t.cas(ctx, "DELETE FROM extension_groups WHERE id = ? AND version = ?", []any{g.ID, expected}, func() {}); err != nil {
		return err
	}
	return t.BumpAuth(ctx, g.TenantID)
}

// GroupMembers lists a group's members by name.
func (t *Tx) GroupMembers(ctx context.Context, groupID string) ([]GroupMember, error) {
	return groupMembers(ctx, t.query, groupID)
}

// GroupMembers lists a group's members by name.
func (s *Store) GroupMembers(ctx context.Context, groupID string) ([]GroupMember, error) {
	return groupMembers(ctx, s.query, groupID)
}

func groupMembers(ctx context.Context, query queryFn, groupID string) ([]GroupMember, error) {
	rows, err := query(ctx, "SELECT extension, added_at, added_by FROM extension_group_members WHERE group_id = ? ORDER BY extension", groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupMember
	for rows.Next() {
		var m GroupMember
		if err := rows.Scan(&m.Name, scanTime{&m.AddedAt}, &m.AddedBy); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ChangeGroupMembers adds and removes member names (already valid extension names), skipping names
// already present or not members, and answers what changed. The limits are counted here: the
// caller holds the tenant's auth lock.
func (t *Tx) ChangeGroupMembers(ctx context.Context, g ExtensionGroup, add, remove []string, actor string) (added, removed []string, err error) {
	if len(actor) > 400 {
		return nil, nil, fmt.Errorf("%w: a value is too long", ErrInvalid)
	}
	ms, err := t.GroupMembers(ctx, g.ID)
	if err != nil {
		return nil, nil, err
	}
	have := map[string]bool{}
	for _, m := range ms {
		have[m.Name] = true
	}
	for _, n := range remove {
		if !have[n] {
			continue
		}
		if _, err := t.exec(ctx, "DELETE FROM extension_group_members WHERE group_id = ? AND extension = ?", g.ID, n); err != nil {
			return nil, nil, err
		}
		removed = append(removed, n)
		delete(have, n)
	}
	for _, n := range add {
		if have[n] {
			continue
		}
		have[n] = true
		added = append(added, n)
	}
	if len(added) > 0 {
		if len(have) > MaxGroupMembers {
			return nil, nil, fmt.Errorf("%w: an extension group holds at most %d names", ErrInvalid, MaxGroupMembers)
		}
		var total int
		if err := t.queryRow(ctx, "SELECT COUNT(*) FROM extension_group_members WHERE tenant_id = ?", g.TenantID).Scan(&total); err != nil {
			return nil, nil, err
		}
		if total+len(added) > MaxTenantMembers {
			return nil, nil, fmt.Errorf("%w: a tenant's extension groups hold at most %d names", ErrInvalid, MaxTenantMembers)
		}
		now := t.s.d.timeArg(t.Now())
		for _, n := range added {
			if _, err := t.exec(ctx, "INSERT INTO extension_group_members (group_id, tenant_id, extension, added_at, added_by) VALUES (?, ?, ?, ?, ?)",
				g.ID, g.TenantID, n, now, actor); err != nil {
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
	rows, err := query(ctx, "SELECT id, name FROM extension_groups WHERE tenant_id = ?", tenantID)
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
		out[id] = &GroupSet{Name: name, Members: map[string]bool{}}
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	rows, err = query(ctx, "SELECT group_id, extension FROM extension_group_members WHERE tenant_id = ?", tenantID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, ext string
		if err := rows.Scan(&id, &ext); err != nil {
			rows.Close()
			return nil, err
		}
		if g := out[id]; g != nil {
			g.Members[ext] = true
		}
	}
	return out, closeRows(rows)
}
