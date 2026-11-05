package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Spec 0041: the administrators' decisions on the decision queue, kept so every administrator sees
// the same queue.

// Decision kinds: a skip of a waiting version (a channel's), a dismissal keyed to its situation (a
// follow's skip reason, a channel's; the channels taking a name, the tenant's).
const (
	DecisionSkip    = "skip"
	DecisionDismiss = "dismiss"
)

// Decision is one decision, a channel's (ChannelID set) or the tenant's: one per (scope, kind,
// source, name). Version is its row's, the ETag an Undo names.
type Decision struct {
	ID, TenantID, PackageType string
	ChannelID                 string // "" for the tenant's
	Channel                   string // the channel's name, as reads answer it
	Kind, Source, Name        string
	PackageVersion            string // a skip's
	Situation                 string // a dismissal's, its canonical JSON text
	Version                   int64
	DecidedBy                 string
	DecidedAt                 time.Time
}

const decisionCols = `d.id, d.tenant_id, d.package_type, d.channel_id, c.name, d.kind, d.source, d.name, d.package_version, d.situation,
d.version, d.decided_by, d.decided_at`

const decisionFrom = " FROM decisions d LEFT JOIN channels c ON c.id = d.channel_id"

func scanDecision(r interface{ Scan(...any) error }) (Decision, error) {
	var d Decision
	var ch, chName, v, sit sql.NullString
	err := r.Scan(&d.ID, &d.TenantID, &d.PackageType, &ch, &chName, &d.Kind, &d.Source, &d.Name, &v, &sit, &d.Version, &d.DecidedBy,
		scanTime{&d.DecidedAt})
	d.ChannelID, d.Channel, d.PackageVersion, d.Situation = ch.String, chName.String, v.String, sit.String
	return d, err
}

// afterDecisionRead runs between SetDecision's read and its write; for tests only.
var afterDecisionRead func(*Tx)

// DecisionLock is the lock a tenant's decisions are written under.
func DecisionLock(tenantID string) string { return "kista/decisions/" + tenantID }

// SetDecision writes the decision of (scope, kind, source, name), replacing the one there: its row
// version moves on. It answers the one it replaced (ok false for none). The caller holds
// DecisionLock.
func (t *Tx) SetDecision(ctx context.Context, d *Decision) (before Decision, ok bool, err error) {
	q := "SELECT " + decisionCols + decisionFrom + " WHERE d.tenant_id = ? AND d.package_type = ? AND d.kind = ? AND d.source = ? AND d.name = ?"
	args := []any{d.TenantID, d.PackageType, d.Kind, d.Source, d.Name}
	if d.ChannelID == "" {
		q += " AND d.channel_id IS NULL"
	} else {
		q, args = q+" AND d.channel_id = ?", append(args, d.ChannelID)
	}
	before, err = scanDecision(t.queryRow(ctx, q, args...))
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return Decision{}, false, err
	default:
		ok = true
	}
	if afterDecisionRead != nil {
		afterDecisionRead(t)
	}
	d.DecidedAt = t.Now()
	var ch, v, sit any
	if d.ChannelID != "" {
		ch = d.ChannelID
	}
	if d.PackageVersion != "" {
		v = d.PackageVersion
	}
	if d.Situation != "" {
		sit = d.Situation
	}
	if ok {
		// at the row version read: a decision deleted meanwhile (its source removed, its entry's last
		// build collected) is not answered as written
		d.ID, d.Version = before.ID, before.Version+1
		err = t.cas(ctx, `UPDATE decisions SET package_version = ?, situation = ?, version = ?, decided_by = ?, decided_at = ? WHERE id = ? AND version = ?`,
			[]any{v, sit, d.Version, d.DecidedBy, t.s.d.timeArg(d.DecidedAt), d.ID, before.Version}, func() {})
		return before, true, err
	}
	d.ID, d.Version = NewID(), 1
	_, err = t.exec(ctx, `INSERT INTO decisions (id, tenant_id, package_type, channel_id, kind, source, name, package_version, situation, version,
decided_by, decided_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, d.ID, d.TenantID, d.PackageType, ch, d.Kind, d.Source, d.Name, v, sit,
		d.Version, d.DecidedBy, t.s.d.timeArg(d.DecidedAt))
	return Decision{}, false, t.s.mapErr(err, "decision")
}

// EntryVersions are the versions of a (source, name) of a tenant's type in the catalogue, read in
// a decision's transaction (spec 0041: a decision is written only while its entry is there); none:
// the entry is gone.
func (t *Tx) EntryVersions(ctx context.Context, tenantID, packageType, source, name string) ([]string, error) {
	return t.strings(ctx, `SELECT DISTINCT package_version FROM builds WHERE tenant_id = ? AND package_type = ? AND source = ? AND name = ?`,
		tenantID, packageType, source, name)
}

// GetDecision reads a tenant's decision by id (ErrNotFound for one of another tenant).
func (t *Tx) GetDecision(ctx context.Context, tenantID, id string) (Decision, error) {
	d, err := scanDecision(t.queryRow(ctx, "SELECT "+decisionCols+decisionFrom+" WHERE d.id = ? AND d.tenant_id = ?", id, tenantID))
	return d, notFound(err, "decision")
}

// DeleteDecision deletes a tenant's decision at its row version (0: any); ErrConflict when it
// changed since.
func (t *Tx) DeleteDecision(ctx context.Context, tenantID, id string, version int64) (Decision, error) {
	d, err := t.GetDecision(ctx, tenantID, id)
	if err != nil {
		return Decision{}, err
	}
	if version != 0 && d.Version != version {
		return Decision{}, ErrConflict
	}
	return d, t.cas(ctx, "DELETE FROM decisions WHERE id = ? AND version = ?", []any{d.ID, d.Version}, func() {})
}

// DecisionFilter narrows a listing: a package type ("": every one), a channel's id ("": every
// scope), the tenant's only, and a page after an id.
type DecisionFilter struct {
	PackageType, ChannelID string
	TenantOnly             bool
	After                  string
	Limit                  int
}

// ListDecisions lists a tenant's decisions by id.
func (s *Store) ListDecisions(ctx context.Context, tenantID string, f DecisionFilter) ([]Decision, error) {
	q := "SELECT " + decisionCols + decisionFrom + " WHERE d.tenant_id = ? AND d.id > ?"
	args := []any{tenantID, f.After}
	if f.PackageType != "" {
		q, args = q+" AND d.package_type = ?", append(args, f.PackageType)
	}
	switch {
	case f.ChannelID != "":
		q, args = q+" AND d.channel_id = ?", append(args, f.ChannelID)
	case f.TenantOnly:
		q += " AND d.channel_id IS NULL"
	}
	q += " ORDER BY d.id"
	if f.Limit > 0 {
		q += s.limit(f.Limit)
	}
	var out []Decision
	err := s.eachRow(ctx, q, args, func(sc func(...any) error) error {
		d, err := scanDecision(rowScanner(sc))
		if err == nil {
			out = append(out, d)
		}
		return err
	})
	return out, err
}

// dropEntryDecisions deletes the decisions on a (source, name) of a tenant's type once the catalogue
// holds no build of it (spec 0041: they never outlive the entries they are about).
func (t *Tx) dropEntryDecisions(ctx context.Context, tenantID, packageType, source, name string) error {
	_, err := t.exec(ctx, `DELETE FROM decisions WHERE tenant_id = ? AND package_type = ? AND source = ? AND name = ?
AND NOT EXISTS (SELECT 1 FROM builds b WHERE b.tenant_id = decisions.tenant_id AND b.package_type = decisions.package_type
	AND b.source = decisions.source AND b.name = decisions.name)`, tenantID, packageType, source, name)
	return err
}
