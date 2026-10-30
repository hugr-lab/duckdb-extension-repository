package bundle_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
)

// Spec 0039: a source's revocations come only in a v2 manifest, at most bundle.MaxRevocations, in
// canonical order, each of a name the source's cells name, with a version, platform, body hash and
// state of their grammars; Canonical orders them.
func TestManifestRevocations(t *testing.T) {
	rev := func(name, version, state string) bundle.Revocation {
		return bundle.Revocation{Name: name, Version: version, Platform: "linux_amd64", BodyHash: strings.Repeat("d", 64), State: state,
			Reason: "CVE"}
	}
	ok := newFixture(t)
	ok.m.Format = bundle.ManifestFormatV2
	ok.m.Sources[1].Revocations = []bundle.Revocation{rev("tresor", "1.0", "yanked"), rev("acl", "1.0", "withdrawn")}
	bundle.Canonical(ok.m.Sources)
	if err := ok.m.Validate(t0); err != nil || ok.m.Sources[1].Revocations[0].Name != "acl" {
		t.Fatalf("a v2 manifest: %v %+v", err, ok.m.Sources[1].Revocations)
	}
	many := make([]bundle.Revocation, bundle.MaxRevocations+1)
	for i := range many {
		many[i] = rev("acl", "1.0", "yanked")
	}
	for name, change := range map[string]func(m *bundle.Manifest){
		"in a v1 manifest":  func(m *bundle.Manifest) { m.Format = bundle.ManifestFormat },
		"a name not a cell": func(m *bundle.Manifest) { m.Sources[1].Revocations[0].Name = "httpfs" },
		"a bad body hash":   func(m *bundle.Manifest) { m.Sources[1].Revocations[0].BodyHash = "x" },
		"a bad state":       func(m *bundle.Manifest) { m.Sources[1].Revocations[0].State = "gone" },
		"a bad version":     func(m *bundle.Manifest) { m.Sources[1].Revocations[0].Version = "../1" },
		"out of order": func(m *bundle.Manifest) {
			r := m.Sources[1].Revocations
			r[0], r[1] = r[1], r[0]
		},
		"too many":          func(m *bundle.Manifest) { m.Sources[1].Revocations = many },
		"a reason too long": func(m *bundle.Manifest) { m.Sources[1].Revocations[0].Reason = strings.Repeat("r", bundle.MaxDetail+1) },
	} {
		g := newFixture(t)
		g.m.Format = bundle.ManifestFormatV2
		g.m.Sources[1].Revocations = []bundle.Revocation{rev("acl", "1.0", "withdrawn"), rev("tresor", "1.0", "yanked")}
		change(&g.m)
		if err := g.m.Validate(t0); !errors.Is(err, bundle.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
