package bundle_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
		if newer && !strings.Contains(err.Error(), "upgrade the importing kista") {
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
	if err := bundle.Decode(newerPlan, &bundle.Plan{}); !errors.Is(err, bundle.ErrNewer) || !strings.Contains(err.Error(), "upgrade the exporting kista") {
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
		Kinds: []string{"report", "sbom"}, Sources: []int{}, Bodies: []bundle.PlanBody{}}}
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
