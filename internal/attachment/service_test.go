package attachment_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/fs"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

var ctx = context.Background()

// rights is an authorizer by actor: the verbs each holds, tenant-wide or on an extension (no channel).
type rights map[string][]string

func (r rights) Allow(_ context.Context, a authz.Actor, verb authz.Verb, res authz.Resource) error {
	if res.Channel != "" {
		return authz.ErrDenied // the test's grants have no channel
	}
	for _, v := range r[a.String()] {
		scope, held, _ := strings.Cut(v, "@") // admin@tenant, publish@tresor
		if held == "tenant" && authz.Verb(scope) == verb || held != "tenant" && held == res.Package && authz.Verb(scope) == verb {
			return nil
		}
	}
	return authz.ErrDenied
}

var (
	admin     = authz.Actor{Kind: authz.ActorPrincipal, ID: "acme/kc|tom", Tenant: "acme"}
	extAdmin  = authz.Actor{Kind: authz.ActorPrincipal, ID: "acme/kc|eve", Tenant: "acme"}
	auditor   = authz.Actor{Kind: authz.ActorPrincipal, ID: "acme/kc|ann", Tenant: "acme"}
	publisher = authz.Actor{Kind: authz.ActorPublisher, ID: "acme/ci", Tenant: "acme"}
	nobody    = authz.Actor{Kind: authz.ActorPrincipal, ID: "acme/kc|nob", Tenant: "acme"}
)

type env struct {
	st    *store.Store
	svc   *attachment.Service
	blobs *fs.Store
	body  string // tresor's body, released by publisher's publication
	other string // tresor's other body, released by an administrator
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	fsStore, err := fs.Open(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	bs, err := blob.NewService(ctx, st, []blob.Domain{{Name: "default", Kind: "fs", Store: fsStore}},
		blob.Options{SpoolDir: filepath.Join(t.TempDir(), "spool"), MaxBody: 1 << 20, MaxIngests: 2, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bs.Close() })
	e := &env{st: st, blobs: fsStore, body: strings.Repeat("a", 64), other: strings.Repeat("b", 64)}
	tn := store.Tenant{Name: "acme"}
	ch := store.Channel{Name: "prod", Kind: store.ChannelSigned}
	if err := st.InTx(ctx, "", func(tx *store.Tx) error {
		if err := tx.CreateTenant(ctx, &tn); err != nil {
			return err
		}
		ch.TenantID = tn.ID
		if err := tx.CreateChannel(ctx, &ch); err != nil {
			return err
		}
		for i, body := range []string{e.body, e.other} {
			b := store.Build{TenantID: tn.ID, Name: "tresor", PackageVersion: []string{"1.0", "1.1"}[i], Platform: "linux_amd64", ABI: store.ABICPP,
				DuckDBVersion: "v2.0.0", BodyHash: body, Origin: store.OriginAdmin, CreatedBy: "os:1:t"}
			if err := tx.FindOrInsertBuild(ctx, &b); err != nil {
				return err
			}
			r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: b.ID, Name: "tresor", PackageVersion: b.PackageVersion, Platform: b.Platform,
				Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public, Seq: int64(i + 1),
				Origin: []string{store.OriginPublication, store.OriginAdmin}[i]}
			if err := tx.InsertRelease(ctx, &r, []string{publisher.String(), admin.String()}[i]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.svc = &attachment.Service{Store: st, Blob: bs, Authz: rights{
		admin.String():     {"admin@tenant"},
		extAdmin.String():  {"admin@tresor"},
		auditor.String():   {"audit@tenant"},
		publisher.String(): {"publish@tresor"},
	}}
	return e
}

const sbom = `{"bomFormat":"CycloneDX","specVersion":"1.5","components":[{"type":"library"}]}`

func (e *env) in(body, kind, mt, doc string) attachment.AttachInput {
	return attachment.AttachInput{BodyHash: body, Kind: kind, MediaType: mt, Title: "SBOM", Content: []byte(doc)}
}

func TestAttachRules(t *testing.T) {
	e := newEnv(t)
	// an administrator attaches any kind, as the tenant; an extension administrator too
	a, err := e.svc.Attach(ctx, admin, "acme", "tresor", e.in(e.body, store.KindSBOM, attachment.CycloneDX, sbom))
	if err != nil || a.Origin != store.AttachedByTenant || a.Visibility != store.VisibleAdministrators || a.Summary == "" {
		t.Fatalf("admin: %+v %v", a, err)
	}
	if _, err := e.svc.Attach(ctx, extAdmin, "acme", "tresor", e.in(e.body, store.KindNotes, attachment.Markdown, "# ok")); err != nil {
		t.Fatalf("extension admin: %v", err)
	}
	// a publisher: an SBOM or provenance, for a body it published, as itself
	p, err := e.svc.Attach(ctx, publisher, "acme", "tresor", e.in(e.body, store.KindSBOM, attachment.CycloneDX, sbom))
	if err != nil || p.Origin != store.AttachedByPublisher || p.AttachedBy != publisher.String() {
		t.Fatalf("publisher: %+v %v", p, err)
	}
	// it supersedes its own
	if p2, err := e.svc.Attach(ctx, publisher, "acme", "tresor", attachment.AttachInput{BodyHash: e.body, Kind: store.KindSBOM,
		MediaType: attachment.CycloneDX, Title: "x", Content: []byte(sbom), Supersedes: p.ID}); err != nil || p2.Supersedes != p.ID {
		t.Fatalf("a publisher superseding its own: %+v %v", p2, err)
	}
	for name, c := range map[string]struct {
		a   authz.Actor
		in  attachment.AttachInput
		err error
	}{
		"a publisher's review":            {publisher, e.in(e.body, store.KindReview, attachment.Review, "{}"), store.ErrInvalid},
		"a publisher, a body not its own": {publisher, e.in(e.other, store.KindSBOM, attachment.CycloneDX, sbom), store.ErrInvalid},
		"a publisher superseding the tenant": {publisher, attachment.AttachInput{BodyHash: e.body, Kind: store.KindSBOM, MediaType: attachment.CycloneDX,
			Title: "x", Content: []byte(sbom), Supersedes: a.ID}, store.ErrInvalid},
		"an auditor":          {auditor, e.in(e.body, store.KindNotes, attachment.Markdown, "x"), authz.ErrDenied},
		"nobody":              {nobody, e.in(e.body, store.KindNotes, attachment.Markdown, "x"), authz.ErrDenied},
		"a body not held":     {admin, e.in(strings.Repeat("c", 64), store.KindNotes, attachment.Markdown, "x"), store.ErrNotFound},
		"another name's body": {admin, attachment.AttachInput{BodyHash: e.body, Kind: store.KindNotes, MediaType: attachment.Markdown, Title: "x", Content: []byte("x")}, nil},
		"not what it says":    {admin, e.in(e.body, store.KindSBOM, attachment.CycloneDX, "{}"), store.ErrInvalid},
		"a wrong media type":  {admin, e.in(e.body, store.KindSBOM, "text/html", sbom), store.ErrInvalid},
		"too large":           {admin, e.in(e.body, store.KindNotes, attachment.Markdown, strings.Repeat("x", attachment.DefaultMaxSize+1)), store.ErrInvalid},
		"a bad body hash":     {admin, e.in("ABC", store.KindNotes, attachment.Markdown, "x"), store.ErrInvalid},
	} {
		ext := "tresor"
		if name == "another name's body" {
			ext = "acl" // tresor's body is not acl's
			c.err = store.ErrNotFound
		}
		if _, err := e.svc.Attach(ctx, c.a, "acme", ext, c.in); !errors.Is(err, c.err) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// reading: administrators and auditors; content checked against its hash
	for _, who := range []authz.Actor{admin, extAdmin, auditor} {
		if all, err := e.svc.List(ctx, who, "acme", "tresor"); err != nil || len(all) != 4 {
			t.Errorf("%s lists: %d %v", who, len(all), err)
		}
	}
	if _, err := e.svc.List(ctx, publisher, "acme", "tresor"); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("a publisher lists: %v", err)
	}
	var b []byte
	if err := e.svc.Content(ctx, auditor, "acme", "tresor", a.ID, func(_ store.Attachment, c []byte) error { b = c; return nil }); err != nil || string(b) != sbom {
		t.Fatalf("content: %q %v", b, err)
	}
	if err := e.blobs.Put(ctx, blob.AttachmentKey(a.ID), strings.NewReader(strings.Replace(sbom, "1.5", "1.6", 1)), int64(len(sbom))); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Content(ctx, admin, "acme", "tresor", a.ID, func(store.Attachment, []byte) error { return nil }); !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("a changed object: %v", err)
	}

	// visibility is a tenant administrator's, with the version
	readers := store.VisibleReaders
	if _, err := e.svc.SetFlags(ctx, extAdmin, "acme", "tresor", a.ID, &readers, nil, 0); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("an extension admin's visibility: %v", err)
	}
	if _, err := e.svc.SetFlags(ctx, admin, "acme", "tresor", a.ID, &readers, nil, 9); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a stale version: %v", err)
	}
	x, err := e.svc.SetFlags(ctx, admin, "acme", "tresor", a.ID, &readers, nil, 1)
	if err != nil || x.Visibility != store.VisibleReaders || x.Version != 2 {
		t.Fatalf("readers: %+v %v", x, err)
	}
	// a change to what it is already: still a stale If-Match is refused
	if _, err := e.svc.SetFlags(ctx, admin, "acme", "tresor", a.ID, &readers, nil, 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a stale no-op: %v", err)
	}

	// readers see a chain's head only: a newer administrators-only version hides the older one
	all, _ := e.st.ExtensionAttachments(ctx, x.TenantID, "tresor")
	if got := attachment.Readable(all, map[string]bool{e.body: true}); len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("readable: %v", got)
	}
	newer, err := e.svc.Attach(ctx, admin, "acme", "tresor", attachment.AttachInput{BodyHash: e.body, Kind: store.KindSBOM,
		MediaType: attachment.CycloneDX, Title: "SBOM 2", Content: []byte(sbom), Supersedes: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	all, _ = e.st.ExtensionAttachments(ctx, x.TenantID, "tresor")
	if got := attachment.Readable(all, map[string]bool{e.body: true}); len(got) != 0 {
		t.Fatalf("an older version shown in a newer one's place: %v", got)
	}
	if got := attachment.Readable(all, map[string]bool{e.other: true}); len(got) != 0 {
		t.Fatal("a body outside the view")
	}

	// removal: administrators; the object goes too
	if err := e.svc.Remove(ctx, auditor, "acme", "tresor", newer.ID); !errors.Is(err, authz.ErrDenied) {
		t.Fatalf("an auditor removes: %v", err)
	}
	if err := e.svc.Remove(ctx, extAdmin, "acme", "tresor", newer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.blobs.Stat(ctx, blob.AttachmentKey(newer.ID)); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the object stayed: %v", err)
	}
	if _, err := e.svc.Get(ctx, admin, "acme", "tresor", newer.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a removed document: %v", err)
	}
	ev, _ := e.st.ListEvents(ctx, x.TenantID, store.EventFilter{Kind: "attachment.add", Limit: 10})
	if len(ev) != 5 || !strings.HasPrefix(ev[0].Subject, "ext:tresor/attachment:") {
		t.Fatalf("events: %d %+v", len(ev), ev)
	}
}
