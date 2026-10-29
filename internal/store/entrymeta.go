package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Spec 0036: a catalogue entry's metadata, a row of its own keyed as the entry (tenant, package
// type, source, name). A publication never touches it, and it is kept when every build of the entry
// goes: only the tenant's deletion removes it.

// EntryMeta is an entry's metadata.
type EntryMeta struct {
	TenantID, PackageType, Source, Name string
	Summary                             string // "" for none
	// IconSHA256 is the icon's hash, the object icons/<sha256> of the tenant's storage domain; ""
	// for none
	IconSHA256            string
	IconWidth, IconHeight int
	IconBytes             int64
	SetBy                 string
	SetAt                 time.Time
	// SignedAt and SignedBody are the mark a signed write must not be older than: the last accepted
	// signed write's signed-at and body hash, or after a token's write that changed something the
	// later of the server's clock and the mark, with SignedBody "" (zero for a row never signed)
	SignedAt   time.Time
	SignedBody string
}

const entryMetaCols = "tenant_id, package_type, source, name, summary, icon_sha256, icon_width, icon_height, icon_bytes, set_by, set_at, " +
	"signed_at, signed_body"

func scanEntryMeta(sc func(...any) error) (EntryMeta, error) {
	var m EntryMeta
	var summary, icon, signedBody sql.NullString
	var w, h, n sql.NullInt64
	err := sc(&m.TenantID, &m.PackageType, &m.Source, &m.Name, &summary, &icon, &w, &h, &n, &m.SetBy, scanTime{&m.SetAt},
		scanTime{&m.SignedAt}, &signedBody)
	m.Summary, m.IconSHA256, m.SignedBody = summary.String, icon.String, signedBody.String
	m.IconWidth, m.IconHeight, m.IconBytes = int(w.Int64), int(h.Int64), n.Int64
	return m, err
}

// EntryMeta reads an entry's metadata inside the transaction, its row locked for the write that
// follows; ok is false when the entry has none.
func (t *Tx) EntryMeta(ctx context.Context, tenantID, packageType, source, name string) (m EntryMeta, ok bool, err error) {
	m, err = scanEntryMeta(t.queryRow(ctx, t.lockedRowQuery("SELECT "+entryMetaCols+" FROM entry_meta",
		"tenant_id = ? AND package_type = ? AND source = ? AND name = ?", true), tenantID, packageType, source, name).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return EntryMeta{}, false, nil
	}
	return m, err == nil, err
}

// EntryMeta reads an entry's metadata; ok is false when the entry has none.
func (s *Store) EntryMeta(ctx context.Context, tenantID, packageType, source, name string) (m EntryMeta, ok bool, err error) {
	m, err = scanEntryMeta(s.db.QueryRowContext(ctx, s.d.rebind("SELECT "+entryMetaCols+" FROM entry_meta "+
		"WHERE tenant_id = ? AND package_type = ? AND source = ? AND name = ?"), tenantID, packageType, source, name).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return EntryMeta{}, false, nil
	}
	return m, err == nil, err
}

// EntryMetas are a tenant's entries' metadata of a package type, by (source, name).
func (s *Store) EntryMetas(ctx context.Context, tenantID, packageType string) (map[[2]string]EntryMeta, error) {
	out := map[[2]string]EntryMeta{}
	err := s.eachRow(ctx, "SELECT "+entryMetaCols+" FROM entry_meta WHERE tenant_id = ? AND package_type = ?", []any{tenantID, packageType},
		func(sc func(...any) error) error {
			m, err := scanEntryMeta(sc)
			if err != nil {
				return err
			}
			out[[2]string{m.Source, m.Name}] = m
			return nil
		})
	return out, err
}

// PutEntryMeta writes an entry's metadata, replacing what it had. The caller holds the tenant's
// auth lock (PublisherAuthLock), which orders an entry's writes, and the icon's lock with its claim
// committed (CommitIcon) when the icon is new.
func (t *Tx) PutEntryMeta(ctx context.Context, m EntryMeta) error {
	null := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	var w, h, n any
	if m.IconSHA256 != "" {
		w, h, n = m.IconWidth, m.IconHeight, m.IconBytes
	}
	d := t.s.d
	res, err := t.exec(ctx, "UPDATE entry_meta SET summary = ?, icon_sha256 = ?, icon_width = ?, icon_height = ?, icon_bytes = ?, set_by = ?, "+
		"set_at = ?, signed_at = ?, signed_body = ? WHERE tenant_id = ? AND package_type = ? AND source = ? AND name = ?",
		null(m.Summary), null(m.IconSHA256), w, h, n, m.SetBy, d.timeArg(m.SetAt), d.nullTimeArg(m.SignedAt), null(m.SignedBody),
		m.TenantID, m.PackageType, m.Source, m.Name)
	if err != nil {
		return err
	}
	if k, err := res.RowsAffected(); err != nil || k == 1 {
		return err
	}
	_, err = t.exec(ctx, "INSERT INTO entry_meta ("+entryMetaCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		m.TenantID, m.PackageType, m.Source, m.Name, null(m.Summary), null(m.IconSHA256), w, h, n, m.SetBy, d.timeArg(m.SetAt),
		d.nullTimeArg(m.SignedAt), null(m.SignedBody))
	return err
}

// EntryExists reports whether a source has a build of a name and type in a tenant's catalogue
// (held, available or withdrawn): what makes it an entry (spec 0036).
func (s *Store) EntryExists(ctx context.Context, tenantID, packageType, source, name string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.d.rebind("SELECT COUNT(*) FROM builds WHERE tenant_id = ? AND package_type = ? AND source = ? AND name = ?"),
		tenantID, packageType, source, name).Scan(&n)
	return n > 0, err
}
