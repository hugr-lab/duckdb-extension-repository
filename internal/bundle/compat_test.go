package bundle_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
)

// Spec 0046 in a bundle's formats: a higher format is refused whole, saying which side to upgrade;
// one unknown thing fails its own part (a cell's outcome, a revocation's state, a plan feed's kind);
// an extra member anywhere is ignored.
func TestForwardCompat(t *testing.T) {
	f := newFixture(t)
	edit := func(data []byte, fn func(m map[string]any)) []byte {
		t.Helper()
		var m map[string]any
		d := json.NewDecoder(bytes.NewReader(data))
		d.UseNumber()
		if err := d.Decode(&m); err != nil {
			t.Fatal(err)
		}
		fn(m)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// a newer exporter's extra members, at every depth: the manifest reads as before
	extra := edit(f.manifest(t), func(m map[string]any) {
		m["exporter_build"] = map[string]any{"commit": "abc", "flags": []any{1, "x"}}
		m["exporter"].(map[string]any)["region"] = "eu"
		src := m["sources"].([]any)[0].(map[string]any)
		src["mirror_of"] = "x"
		src["cells"].([]any)[0].(map[string]any)["attempts"] = 3
	})
	var m bundle.Manifest
	if err := bundle.Decode(extra, &m); err != nil {
		t.Fatalf("extra members: %v", err)
	}
	if err := m.Validate(t0); err != nil || len(m.Sources) != 2 {
		t.Fatalf("extra members: %v, %d sources", err, len(m.Sources))
	}

	// formats: the known ones, a higher one of each family, and one of no family
	for format, newer := range map[string]bool{"kista-bundle/v3": true, "kista-bundle/v10": true, "kista-bundle/v0": false,
		"kista-bundle-plan/v1": false, "other/v9": false} {
		data := edit(f.manifest(t), func(m map[string]any) { m["format"] = format })
		var m bundle.Manifest
		err := bundle.Decode(data, &m)
		if err == nil {
			err = m.Validate(t0)
		}
		if !errors.Is(err, bundle.ErrInvalid) || errors.Is(err, bundle.ErrNewer) != newer {
			t.Errorf("a manifest of format %s: %v", format, err)
		}
		// the reader is the side to upgrade, whichever it is: an importer, kista bundle sign or inspect
		if newer && !strings.Contains(err.Error(), "this kista is older than the bundle's writer; upgrade this kista") {
			t.Errorf("a newer manifest does not say which side to upgrade: %v", err)
		}
	}
	plan := bundle.Plan{Format: bundle.PlanFormat, CreatedAt: t0, Tenant: "acme", BundleKeys: []string{"sha256:" + strings.Repeat("1", 64)},
		Sources: []bundle.PlanSource{}}
	pdata, _ := json.Marshal(plan)
	newerPlan := edit(pdata, func(m map[string]any) {
		m["format"] = "kista-bundle-plan/v2"
		m["sources"] = "another shape altogether" // never read: the format is said first
	})
	if err := bundle.Decode(newerPlan, &bundle.Plan{}); !errors.Is(err, bundle.ErrNewer) ||
		!strings.Contains(err.Error(), "this kista is older than the plan's writer; upgrade this kista") {
		t.Errorf("a newer plan: %v", err)
	}
	withMember := edit(pdata, func(m map[string]any) { m["accepts_reports"] = true })
	var p bundle.Plan
	if err := bundle.Decode(withMember, &p); err != nil || p.Validate() != nil {
		t.Errorf("a plan with an extra member: %v", err)
	}

	// a cell with an outcome this binary does not know is that cell's, not the manifest's
	g := newFixture(t)
	g.m.Sources[0].Cells[1].Outcome = "quarantined"
	if err := g.m.Validate(t0); err != nil || bundle.KnownOutcome("quarantined") || !bundle.KnownOutcome(bundle.Fetched) {
		t.Errorf("an unknown outcome: %v", err)
	}
	// a revocation in a state this binary does not know is carried (the import takes it as a yank)
	g = newFixture(t)
	g.m.Format = bundle.ManifestFormatV2
	g.m.Sources[1].Revocations = []bundle.Revocation{{Name: "acl", Version: "1.0", Platform: "linux_amd64", BodyHash: strings.Repeat("d", 64),
		State: "recalled"}}
	if err := g.m.Validate(t0); err != nil {
		t.Errorf("an unknown revocation state: %v", err)
	}
	// a plan feed's kinds are checked by grammar: an older exporter reads a newer site's plan
	plan.Feeds = []bundle.PlanFeed{{URL: "https://src.example/api/v1/tenants/x", Keys: []string{"sha256:" + strings.Repeat("9", 64)},
		Kinds: []string{"audit", "sbom"}, Sources: []int{}, Bodies: []bundle.PlanBody{}}}
	if err := plan.Validate(); err != nil {
		t.Errorf("a plan naming a kind this binary does not know: %v", err)
	}
	// a plan's source of an unknown type stays in its place (the feeds name sources by index): the
	// exporter leaves it out
	plan.Feeds = nil
	plan.Sources = []bundle.PlanSource{{Kind: "adbc-registry", PackageType: "adbc_driver", Prefix: "whatever it is there",
		Cells: []bundle.PlanCell{{Name: "not a cell of ours"}}}}
	if err := plan.Validate(); err != nil || !bundle.UnknownType("adbc_driver") || bundle.UnknownType("") {
		t.Errorf("a plan's source of an unknown type: %v", err)
	}
	// nothing of such a source is read, whatever shape a later release gave its members
	shaped := edit(pdata, func(m map[string]any) {
		m["sources"] = []any{map[string]any{"kind": "adbc-registry", "package_type": "adbc_driver", "prefix": "x", "keys": "none",
			"cells": []any{map[string]any{"versions": 7}, 2}}}
	})
	var sp bundle.Plan
	if err := bundle.Decode(shaped, &sp); err != nil || sp.Validate() != nil || len(sp.Sources) != 1 || len(sp.Sources[0].Cells) != 2 ||
		sp.Sources[0].Kind != "adbc-registry" || len(sp.Sources[0].Keys) != 0 {
		t.Errorf("a plan's source of an unknown type, in another shape: %+v %v", sp.Sources, err)
	}
	// the same members in a source this binary reads are a broken plan
	known := edit(pdata, func(m map[string]any) {
		m["sources"] = []any{map[string]any{"kind": "duckdb-core", "keys": "none", "cells": []any{}}}
	})
	if err := bundle.Decode(known, &bundle.Plan{}); !errors.Is(err, bundle.ErrInvalid) {
		t.Errorf("a known source in another shape: %v", err)
	}
	plan.Sources[0].Kind = "a kind\x00with a control character"
	if err := plan.Validate(); !errors.Is(err, bundle.ErrInvalid) {
		t.Errorf("a left-out source's text is not bounded: %v", err)
	}
}

// Spec 0046: what a bundle carries for a part this binary left out is read through without
// counting toward MaxSkipped, so one unknown thing never stops the rest: the entries of a source of
// an unknown package type (nothing of it is read, its entries' names neither), and the file of a
// cell in an unknown outcome. Other unnamed entries are capped as before.
func TestLeftOutEntriesAreNotCapped(t *testing.T) {
	f := newFixture(t)
	_, src, files := split(t, f, nil)
	eoa := make([]byte, 1024)
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	stray := "files/" + strings.Repeat("e", 64) + ".gz"
	big := make([]byte, bundle.MaxSkipped+1)
	head := func(edit func(m *bundle.Manifest)) []byte {
		g := newFixture(t)
		g.files = f.files
		if edit != nil {
			edit(&g.m)
		}
		var hb bytes.Buffer
		if err := bundle.WriteHead(bundle.NewWriter(&hb), g.manifest(t), nil); err != nil {
			t.Fatal(err)
		}
		return hb.Bytes()
	}
	read := func(data []byte) (int, error) {
		t.Helper()
		b, err := bundle.Open(bytes.NewReader(data), t0, nil)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		_, got, err := readAll(t, b)
		if err == io.EOF {
			err = nil
		}
		return len(got), err
	}
	leftOut := func(m *bundle.Manifest) { // sorts first: adbc-registry < duckdb-core
		m.Sources = append([]bundle.Source{{Kind: "adbc-registry", PackageType: "adbc_driver", Prefix: "https://drivers.example.org/adbc"}}, m.Sources...)
	}
	unknown := func(m *bundle.Manifest) {
		c := m.Sources[1].Cells[0]
		c.Name, c.Outcome, c.File = "zzz", "quarantined", stray
		m.Sources[1].Cells = append(m.Sources[1].Cells, c)
	}

	// without a part left out, the stray file is over the cap
	if _, err := read(cat(head(nil), src, entry(stray, '0', big), files, eoa)); !errors.Is(err, bundle.ErrMalformed) || !strings.Contains(err.Error(), "read through") {
		t.Fatalf("a stray file over the cap: %v", err)
	}
	// a left-out source's entries, before the known source's and among the files: the two files come
	if n, err := read(cat(head(leftOut), entry("sources/9/index.json", '0', []byte(`{}`)), src, entry(stray, '0', big), files, eoa)); err != nil || n != 2 {
		t.Fatalf("a left-out source's entries: %d files, %v", n, err)
	}
	// the cap still holds for what is no source's: a name outside sources/ and files/
	if _, err := read(cat(head(leftOut), src, entry("junk", '0', big), files, eoa)); !errors.Is(err, bundle.ErrMalformed) {
		t.Fatalf("junk beside a left-out source: %v", err)
	}
	// nothing is remembered of a left-out source's entries (their names are not the manifest's), so
	// a name twice is read through like any other: the bundle's whole size is its reader's to bound
	half := entry(stray, '0', big[:len(big)/2+1])
	if n, err := read(cat(head(leftOut), src, half, half, half, files, eoa)); err != nil || n != 2 {
		t.Fatalf("copies of a left-out source's entry: %d files, %v", n, err)
	}
	// the order rules hold through them: a .well-known file after a left-out source's file is refused
	if _, err := read(cat(head(leftOut), entry(stray, '0', []byte("x")), src, files, eoa)); !errors.Is(err, bundle.ErrMalformed) ||
		!strings.Contains(err.Error(), "after the files") {
		t.Fatalf("sources after a left-out source's file: %v", err)
	}
	// the file of a cell in an outcome this binary does not know
	if n, err := read(cat(head(unknown), src, entry(stray, '0', big), files, eoa)); err != nil || n != 2 {
		t.Fatalf("an unknown outcome's file: %d files, %v", n, err)
	}
}

// Spec 0046: a cell in an outcome this binary does not know never fails the manifest for what its
// other members hold, nor a licence of its name for want of files.
func TestUnknownOutcomeCell(t *testing.T) {
	g := newFixture(t)
	g.m.Plan.Licenses = true
	src := &g.m.Sources[1]
	c := src.Cells[0]
	c.Name, c.Outcome, c.FetchedAt, c.Detail, c.ETag = "zzz", "staged", t0.Add(time.Hour), strings.Repeat("d", bundle.MaxDetail+1), "\x01"
	src.Cells = append(src.Cells, c)
	src.Licenses = []bundle.License{{Name: "zzz", Token: "a.b.c"}}
	if err := g.m.Validate(t0); err != nil {
		t.Fatalf("a cell in an unknown outcome: %v", err)
	}
	core := &g.m.Sources[0]
	core.Cells[1].Outcome, core.Cells[1].Authenticated = "staged", true
	if err := g.m.Validate(t0); err != nil {
		t.Fatalf("an unknown outcome's other members: %v", err)
	}
	// the same members on a cell this binary reads are refused as before
	core.Cells[1].Outcome = bundle.Rejected
	if err := g.m.Validate(t0); !errors.Is(err, bundle.ErrInvalid) {
		t.Fatalf("a rejected cell that is authenticated: %v", err)
	}
	src.Licenses[0].Name = "nope"
	core.Cells[1].Authenticated = false
	if err := g.m.Validate(t0); !errors.Is(err, bundle.ErrInvalid) {
		t.Fatalf("a licence of a name no cell names: %v", err)
	}
}
