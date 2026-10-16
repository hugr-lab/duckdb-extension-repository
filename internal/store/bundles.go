package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Bundle import states (spec 0014).
const (
	ImportRunning = "running"
	ImportDone    = "done"
	ImportFailed  = "failed"
)

// BundleImport is one bundle import into a tenant, dry runs included (spec 0014).
type BundleImport struct {
	ID, TenantID, ManifestID string
	ManifestCreatedAt        time.Time
	ManifestSHA256           string
	Exporter                 string // the manifest's, as data
	BundleKey                string // the fingerprint
	InputSHA256              string // "" until the import ends
	DryRun                   bool
	State                    string
	Counts                   string // JSON: per target and outcome
	StartedAt, FinishedAt    time.Time
	Actor                    string
	Interrupted              bool // not stored: running, its lease expired
}

// ImportLease is the lease an import holds while it runs: a running record whose lease is not held
// was interrupted.
func ImportLease(id string) string { return "kista/bundle-import/" + id }

const bundleImportCols = `id, tenant_id, manifest_id, manifest_created_at, manifest_sha256, exporter, bundle_key, input_sha256,
dry_run, state, counts, started_at, finished_at, actor`

func scanBundleImport(r interface{ Scan(...any) error }) (BundleImport, error) {
	var b BundleImport
	var input sql.NullString
	err := r.Scan(&b.ID, &b.TenantID, &b.ManifestID, scanTime{&b.ManifestCreatedAt}, &b.ManifestSHA256, &b.Exporter, &b.BundleKey,
		&input, &b.DryRun, &b.State, &b.Counts, scanTime{&b.StartedAt}, scanTime{&b.FinishedAt}, &b.Actor)
	b.InputSHA256 = input.String
	return b, err
}

// InsertBundleImport records an import as running; it sets the start, and the id unless given.
func (t *Tx) InsertBundleImport(ctx context.Context, b *BundleImport) error {
	if b.ID == "" {
		b.ID = NewID()
	}
	b.StartedAt, b.State = t.Now(), ImportRunning
	if b.Counts == "" {
		b.Counts = "{}"
	}
	if len(b.Exporter) > 1000 {
		b.Exporter = strings.ToValidUTF8(b.Exporter[:1000], "")
	}
	_, err := t.exec(ctx, "INSERT INTO bundle_imports ("+bundleImportCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		b.ID, b.TenantID, b.ManifestID, t.s.d.timeArg(b.ManifestCreatedAt), b.ManifestSHA256, b.Exporter, b.BundleKey, nil,
		b.DryRun, b.State, b.Counts, t.s.d.timeArg(b.StartedAt), nil, b.Actor)
	return err
}

// FinishBundleImport records an import's end.
func (t *Tx) FinishBundleImport(ctx context.Context, b *BundleImport) error {
	b.FinishedAt = t.Now()
	_, err := t.exec(ctx, "UPDATE bundle_imports SET state = ?, counts = ?, input_sha256 = ?, finished_at = ? WHERE id = ?",
		b.State, b.Counts, nullable(b.InputSHA256), t.s.d.timeArg(b.FinishedAt), b.ID)
	return err
}

// FailBundleImport marks a running import failed (interrupted: its lease expired).
func (t *Tx) FailBundleImport(ctx context.Context, id string) error {
	_, err := t.exec(ctx, "UPDATE bundle_imports SET state = ?, finished_at = ? WHERE id = ? AND state = ?",
		ImportFailed, t.s.d.timeArg(t.Now()), id, ImportRunning)
	return err
}

// ListBundleImports reads a tenant's imports, the newest first, at most limit.
func (s *Store) ListBundleImports(ctx context.Context, tenantID string, limit int) ([]BundleImport, error) {
	var out []BundleImport
	err := s.InTx(ctx, "", func(tx *Tx) error {
		rows, err := tx.query(ctx, "SELECT "+bundleImportCols+" FROM bundle_imports WHERE tenant_id = ? ORDER BY started_at DESC, id DESC",
			tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for len(out) < limit && rows.Next() {
			b, err := scanBundleImport(rows)
			if err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

// RunningBundleImports reads a tenant's imports still recorded as running.
func (t *Tx) RunningBundleImports(ctx context.Context, tenantID string) ([]string, error) {
	return t.strings(ctx, "SELECT id FROM bundle_imports WHERE tenant_id = ? AND state = ?", tenantID, ImportRunning)
}

// GetBundleImport reads one of a tenant's imports.
func (s *Store) GetBundleImport(ctx context.Context, tenantID, id string) (BundleImport, error) {
	var b BundleImport
	err := s.InTx(ctx, "", func(tx *Tx) error {
		var err error
		b, err = scanBundleImport(tx.queryRow(ctx, "SELECT "+bundleImportCols+" FROM bundle_imports WHERE tenant_id = ? AND id = ?",
			tenantID, id))
		return notFound(err, "bundle import "+id)
	})
	return b, err
}

// SetBundleMark records the last manifest imported into a bundle-mode upstream (spec 0014's
// freshness mark); an empty id clears it.
func (t *Tx) SetBundleMark(ctx context.Context, upstreamID, id string, createdAt time.Time, manifestSHA string) error {
	_, err := t.exec(ctx, "UPDATE upstreams SET bundle_id = ?, bundle_created_at = ?, bundle_manifest_sha256 = ? WHERE id = ?",
		nullable(id), t.s.d.nullTimeArg(createdAt), nullable(manifestSHA), upstreamID)
	return err
}
