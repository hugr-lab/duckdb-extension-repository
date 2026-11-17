package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Migration 0054 (spec 0046) rebuilds SQLite's attachments to drop its kind's CHECK: the rows, the
// issuer's uniqueness and the body index survive; a feed's cursors gain their waiting state at its
// defaults; and a kind this binary does not know, refused by the schema before, is stored after.
func TestMigration54KeepsAttachments(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ms, err := loadMigrations(s.d.Name)
	if err != nil {
		t.Fatal(err)
	}
	upTo := func(n int) {
		t.Helper()
		for {
			next, err := s.migrateOne(ctx, ms[:n])
			if err != nil {
				t.Fatal(err)
			}
			if !next {
				return
			}
		}
	}
	upTo(53)
	tn := Tenant{Name: "acme"}
	f := Feed{Name: "src", URL: "https://src.example/api/v1/tenants/x", Credential: "c", Offer: OfferUpstreams,
		Keys: []string{"sha256:" + strings.Repeat("9", 64)}, CreatedBy: "os:1:t"}
	body := strings.Repeat("a", 64)
	doc := Attachment{BodyHash: body, Kind: KindReview, MediaType: "application/vnd.kista.review.v1+json", Title: "R", Size: 7,
		ContentHash: strings.Repeat("0", 64), Summary: `{"verdict":"approved"}`, Origin: AttachedByFeed, AttachedBy: "feed-1",
		Visibility: VisibleReaders, Issuer: f.URL, IssuerID: "d1", KeyFP: f.Keys[0], SourceOrigin: "tenant", Statement: `{"seq":4}`,
		Signature: "c2ln", Seq: 4}
	report := doc
	report.Kind, report.IssuerID = "report", "d2"
	// the rows as schema 53 has them (no narrowed, no waiting state)
	insert := func(a *Attachment) error {
		a.ID = NewID()
		_, err := s.db.ExecContext(ctx, `INSERT INTO attachments (id, tenant_id, body_hash, kind, media_type, title, size, content_hash, summary,
origin, attached_by, visibility, feedable, version, created_at, issuer, issuer_id, key_fp, source_origin, issued_at, statement, signature, seq)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 1, '2026-11-01T00:00:00.000000Z', ?, ?, ?, ?, '2026-11-01T00:00:00.000000Z', ?, ?, ?)`,
			a.ID, a.TenantID, a.BodyHash, a.Kind, a.MediaType, a.Title, a.Size, a.ContentHash, a.Summary, a.Origin, a.AttachedBy, a.Visibility,
			a.Issuer, a.IssuerID, a.KeyFP, a.SourceOrigin, a.Statement, a.Signature, a.Seq)
		return err
	}
	if err := s.InTx(ctx, "", func(tx *Tx) error {
		if err := tx.CreateTenant(ctx, &tn); err != nil {
			return err
		}
		f.TenantID, doc.TenantID, report.TenantID = tn.ID, tn.ID, tn.ID
		// the feed as schema 53 has it (no via)
		f.ID = NewID()
		if _, err := tx.exec(ctx, `INSERT INTO feeds (id, tenant_id, name, url, credential, kinds, readers, offer, state, version, status,
created_at, created_by, mode, export) VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, 1, ?, '2026-11-01T00:00:00.000000Z', ?, ?, ?)`, f.ID, f.TenantID, f.Name, f.URL,
			f.Credential, false, f.Offer, FeedActive, FeedNew, f.CreatedBy, FeedOnline, false); err != nil {
			return err
		}
		_, err := tx.exec(ctx, "INSERT INTO feed_cursors (feed_id, body_hash, seq) VALUES (?, ?, 4)", f.ID, body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := insert(&doc); err != nil {
		t.Fatal(err)
	}
	if err := insert(&report); err == nil {
		t.Fatal("schema 53 stored an attachment of a sixth kind")
	}
	report.ID = ""

	upTo(54)
	if cur, minReader, err := s.schemaState(ctx, s.db); err != nil || cur != 54 || minReader != 53 {
		t.Fatalf("after 0054: schema %d, min_reader %d, %v", cur, minReader, err)
	}
	upTo(len(ms)) // the rest, for this binary's reads and writes below
	after, err := s.GetAttachment(ctx, tn.ID, doc.ID)
	if err != nil || after.Kind != doc.Kind || after.Title != "R" || after.Size != 7 || after.Summary != doc.Summary || after.Seq != 4 ||
		after.Statement != doc.Statement || after.Signature != doc.Signature || after.Issuer != doc.Issuer || after.IssuerID != "d1" ||
		after.KeyFP != doc.KeyFP || after.SourceOrigin != "tenant" || after.Visibility != VisibleReaders || after.Narrowed ||
		after.Version != 1 || after.CreatedAt.IsZero() || after.Origin != AttachedByFeed {
		t.Fatalf("the row after the rebuild: %+v %v", after, err)
	}
	if cur, err := s.FeedCursors(ctx, f.ID); err != nil || cur[body] != 4 {
		t.Fatalf("the cursor: %v %v", cur, err)
	}
	if w, err := s.FeedWaits(ctx, f.ID); err != nil || len(w) != 0 {
		t.Fatalf("a cursor of before waits: %v %v", w, err)
	}
	// the kind is the code's to validate now; the issuer's uniqueness and the index are kept
	if err := s.InTx(ctx, "", func(tx *Tx) error { return tx.InsertAttachment(ctx, &report) }); err != nil {
		t.Fatalf("an attachment of a sixth kind: %v", err)
	}
	again := doc
	again.ID = ""
	if err := s.InTx(ctx, "", func(tx *Tx) error { return tx.InsertAttachment(ctx, &again) }); err == nil {
		t.Fatal("an issuer's document twice")
	}
	var idx int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'ix_attachments_body' AND tbl_name = 'attachments'").
		Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("the body index: %d %v", idx, err)
	}
	for _, bad := range []string{"origin", "visibility"} { // the other checks stand
		if _, err := s.db.ExecContext(ctx, "UPDATE attachments SET "+bad+" = 'nope' WHERE id = ?", doc.ID); err == nil {
			t.Fatalf("the check on %s went with the kind's", bad)
		}
	}
}
