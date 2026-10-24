package bundle_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
)

// v014Source and v014PlanSource are a source as kista v0.1.4 decodes it (its manifest and plan
// decoders are strict): without package_type.
type v014Source struct {
	Kind      string           `json:"kind"`
	Prefix    string           `json:"prefix,omitempty"`
	WellKnown *bundle.FileRef  `json:"well_known,omitempty"`
	Cells     []bundle.Cell    `json:"cells"`
	Licenses  []bundle.License `json:"licenses,omitempty"`
}

type v014Manifest struct {
	Format    string            `json:"format"`
	ID        string            `json:"id"`
	CreatedAt time.Time         `json:"created_at"`
	Exporter  bundle.Exporter   `json:"exporter"`
	Plan      bundle.PlanRef    `json:"plan"`
	Sources   []v014Source      `json:"sources"`
	Feeds     []json.RawMessage `json:"feeds,omitempty"`
}

type v014PlanSource struct {
	Kind   string            `json:"kind"`
	Prefix string            `json:"prefix,omitempty"`
	Keys   []string          `json:"keys,omitempty"`
	Cells  []bundle.PlanCell `json:"cells"`
}

// Spec 0034: a source's package_type is omitted for duckdb_extension, so an older kista reads every
// extension bundle and plan a newer one writes; written, it must be another type, one the reader
// knows (and every source kind here is DuckDB's).
func TestSourcePackageTypes(t *testing.T) {
	f := newFixture(t)
	data := f.manifest(t)
	if strings.Contains(string(data), "package_type") {
		t.Fatalf("an extension manifest names its type: %s", data)
	}
	var old v014Manifest
	if err := bundle.DecodeStrict(data, &old); err != nil || len(old.Sources) != 2 {
		t.Fatalf("v0.1.4's decoder: %v", err)
	}
	var back bundle.Manifest
	if err := bundle.DecodeStrict(data, &back); err != nil || back.Sources[0].PackageType != "" || back.Validate(t0) != nil {
		t.Fatalf("round trip: %v", err)
	}
	ps := []bundle.PlanSource{{Kind: "duckdb-core", Cells: []bundle.PlanCell{{DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "httpfs"}}}}
	pb, _ := json.Marshal(ps)
	var oldPS []v014PlanSource
	if err := bundle.DecodeStrict(pb, &oldPS); err != nil || strings.Contains(string(pb), "package_type") {
		t.Fatalf("a plan's source: %s %v", pb, err)
	}

	defer pkgtype.RegisterForTest("test_package")()
	for typ, want := range map[string]bool{"": true, pkgtype.DuckDBExtension: false, "nope": false, "test_package": false} {
		g := newFixture(t)
		g.m.Sources[0].PackageType = typ
		if err := g.m.Validate(t0); (err == nil) != want || err != nil && !errors.Is(err, bundle.ErrInvalid) {
			t.Errorf("a manifest's source of %q: %v", typ, err)
		}
		p := bundle.Plan{Format: bundle.PlanFormat, CreatedAt: t0, Tenant: "acme", BundleKeys: []string{"sha256:" + strings.Repeat("1", 64)},
			Sources: []bundle.PlanSource{{Kind: "duckdb-core", PackageType: typ,
				Cells: []bundle.PlanCell{{DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "httpfs"}}}}}
		if err := p.Validate(); (err == nil) != want || err != nil && !errors.Is(err, bundle.ErrInvalid) {
			t.Errorf("a plan's source of %q: %v", typ, err)
		}
	}
}
