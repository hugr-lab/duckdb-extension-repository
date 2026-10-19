package bundle_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
)

const testToken = "eyJhbGciOiJSUzI1NiJ9.eyJleHQiOiJhY2wifQ.c2ln"

// Spec 0021: a repository source carries licences, by name, only when the plan asked, only of
// names whose files it carries or the site holds.
func TestManifestLicenses(t *testing.T) {
	with := func() *fixture {
		f := newFixture(t)
		f.m.Plan.Licenses, f.m.Plan.LicenseHolder = true, "acme"
		f.m.Sources[1].Licenses = []bundle.License{{Name: "acl", Token: testToken}, {Name: "tresor", Token: testToken}}
		return f
	}
	if err := with().m.Validate(t0); err != nil {
		t.Fatal(err)
	}
	// an unchanged cell counts as held
	f := with()
	f.m.Sources[1].Cells[0] = bundle.Cell{DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "acl", Outcome: bundle.Unchanged,
		BodyHash: strings.Repeat("a", 64), FetchedAt: t0}
	if err := f.m.Validate(t0); err != nil {
		t.Fatalf("a licence of an unchanged cell: %v", err)
	}
	for name, change := range map[string]func(m *bundle.Manifest){
		"not asked":            func(m *bundle.Manifest) { m.Plan.Licenses, m.Plan.LicenseHolder = false, "" },
		"a holder, no request": func(m *bundle.Manifest) { m.Plan.Licenses = false },
		"a long holder":        func(m *bundle.Manifest) { m.Plan.LicenseHolder = strings.Repeat("h", 129) },
		"a control character":  func(m *bundle.Manifest) { m.Plan.LicenseHolder = "a\x1bb" },
		"a core source's":      func(m *bundle.Manifest) { m.Sources[0].Licenses = []bundle.License{{Name: "httpfs", Token: testToken}} },
		"out of order":         func(m *bundle.Manifest) { ls := m.Sources[1].Licenses; ls[0], ls[1] = ls[1], ls[0] },
		"twice":                func(m *bundle.Manifest) { m.Sources[1].Licenses[1].Name = "acl" },
		"a name without files": func(m *bundle.Manifest) { m.Sources[1].Licenses[1].Name = "vault" },
		"a missing cell's": func(m *bundle.Manifest) {
			m.Sources[1].Cells[1] = bundle.Cell{DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "tresor", Outcome: bundle.Missing}
		},
		"a bad name":     func(m *bundle.Manifest) { m.Sources[1].Licenses[0].Name = "../acl" },
		"not a token":    func(m *bundle.Manifest) { m.Sources[1].Licenses[0].Token = "a.b" },
		"a spaced token": func(m *bundle.Manifest) { m.Sources[1].Licenses[0].Token = testToken + " " },
		"a long token":   func(m *bundle.Manifest) { m.Sources[1].Licenses[0].Token = "a.b." + strings.Repeat("c", 16<<10) },
		"over the bound": func(m *bundle.Manifest) { m.Sources[1].Licenses = make([]bundle.License, bundle.MaxLicenses+1) },
	} {
		g := with()
		change(&g.m)
		if err := g.m.Validate(t0); !errors.Is(err, bundle.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// an older reader, which knows no licences, refuses the manifest: decoding is strict
	type oldSource struct {
		Kind      string          `json:"kind"`
		Prefix    string          `json:"prefix,omitempty"`
		WellKnown *bundle.FileRef `json:"well_known,omitempty"`
		Cells     []bundle.Cell   `json:"cells"`
	}
	type oldPlanRef struct {
		Tenant    string    `json:"tenant"`
		CreatedAt time.Time `json:"created_at"`
		SHA256    string    `json:"sha256"`
	}
	type oldManifest struct {
		Format    string          `json:"format"`
		ID        string          `json:"id"`
		CreatedAt time.Time       `json:"created_at"`
		Exporter  bundle.Exporter `json:"exporter"`
		Plan      oldPlanRef      `json:"plan"`
		Sources   []oldSource     `json:"sources"`
		Feeds     []bundle.Feed   `json:"feeds,omitempty"`
	}
	// the plan's request and a source's licences, each on its own
	asked, carrying := with(), with()
	asked.m.Sources[1].Licenses = nil
	carrying.m.Plan.Licenses, carrying.m.Plan.LicenseHolder = false, ""
	for name, m := range map[string]bundle.Manifest{"the request": asked.m, "the licences": carrying.m} {
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := bundle.DecodeStrict(data, &oldManifest{}); !errors.Is(err, bundle.ErrInvalid) {
			t.Errorf("an older reader took a manifest with %s: %v", name, err)
		}
	}
	// a bundle without licences is the same document as before
	data, err := json.Marshal(newFixture(t).m)
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.DecodeStrict(data, &oldManifest{}); err != nil || strings.Contains(string(data), "licen") {
		t.Errorf("a bundle without licences: %v", err)
	}
}

func TestPlanLicenses(t *testing.T) {
	p := bundle.Plan{Format: bundle.PlanFormat, CreatedAt: t0, Tenant: "acme", BundleKeys: []string{"sha256:" + strings.Repeat("1", 64)},
		Sources: []bundle.PlanSource{}, Licenses: true, LicenseHolder: "acme-corp"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	// an older exporter, which knows no licence request, refuses the plan
	type oldPlan struct {
		Format     string              `json:"format"`
		CreatedAt  time.Time           `json:"created_at"`
		Tenant     string              `json:"tenant"`
		BundleKeys []string            `json:"bundle_keys"`
		Sources    []bundle.PlanSource `json:"sources"`
		Feeds      []bundle.PlanFeed   `json:"feeds,omitempty"`
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.DecodeStrict(data, &oldPlan{}); !errors.Is(err, bundle.ErrInvalid) {
		t.Errorf("an older exporter took a plan asking for licences: %v", err)
	}
	q := p
	q.Licenses, q.LicenseHolder = false, ""
	if data, _ = json.Marshal(q); bundle.DecodeStrict(data, &oldPlan{}) != nil {
		t.Error("a plan without licences is not the same document as before")
	}
	for name, change := range map[string]func(p *bundle.Plan){
		"a holder without licences": func(p *bundle.Plan) { p.Licenses = false },
		"a long holder":             func(p *bundle.Plan) { p.LicenseHolder = strings.Repeat("h", 129) },
		"a format character":        func(p *bundle.Plan) { p.LicenseHolder = "a\u200bb" },
	} {
		q := p
		change(&q)
		if err := q.Validate(); !errors.Is(err, bundle.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
