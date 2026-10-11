package attachment

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Every kind and media type parsing and summarising (spec 0011): valid, malformed, too deep.
func TestSummarize(t *testing.T) {
	statement := base64.StdEncoding.EncodeToString([]byte(`{"_type":"https://in-toto.io/Statement/v1","predicateType":"https://slsa.dev/provenance/v1",
"predicate":{"runDetails":{"builder":{"id":"https://github.com/actions/runner"}},"buildDefinition":{"externalParameters":{"workflow":
{"repository":"https://github.com/hugr-lab/tresor","ref":"refs/tags/v1.0"}}}}}`))
	dsse := `{"payloadType":"application/vnd.in-toto+json","payload":"` + statement + `","signatures":[]}`
	for _, c := range []struct {
		name, kind, mt, doc string
		want                map[string]any
	}{
		{"cyclonedx sbom", store.KindSBOM, CycloneDX, `{"bomFormat":"CycloneDX","specVersion":"1.6","components":[
{"type":"library","licenses":[{"license":{"id":"MIT"}}],"components":[{"type":"cryptographic-asset"}]},{"type":"library","licenses":[{"expression":"Apache-2.0"}]}]}`,
			map[string]any{"format": "cyclonedx 1.6", "components": 3.0, "licences": 2.0, "crypto_assets": 1.0}},
		{"spdx", store.KindSBOM, SPDX, `{"spdxVersion":"SPDX-2.3","packages":[{"licenseConcluded":"MIT"},{"licenseConcluded":"NOASSERTION"}]}`,
			map[string]any{"format": "spdx-2.3", "components": 2.0, "licences": 1.0}},
		{"cyclonedx vex", store.KindVulnerabilities, CycloneDX, `{"bomFormat":"CycloneDX","metadata":{"timestamp":"2026-10-01T10:00:00Z"},
"vulnerabilities":[{"ratings":[{"severity":"high"},{"severity":"critical"}]},{"ratings":[{"severity":"low"}]},{"analysis":{"state":"not_affected"}}]}`,
			map[string]any{"critical": 1.0, "low": 1.0, "findings": 2.0, "not_affected": 1.0, "scanned_at": "2026-10-01T10:00:00Z"}},
		{"openvex", store.KindVulnerabilities, OpenVEX, `{"@context":"https://openvex.dev/ns/v0.2.0","timestamp":"2026-10-01T10:00:00Z",
"statements":[{"status":"not_affected"},{"status":"affected"}]}`,
			map[string]any{"unknown": 1.0, "findings": 1.0, "not_affected": 1.0, "scanned_at": "2026-10-01T10:00:00Z"}},
		{"sarif", store.KindVulnerabilities, SARIF, `{"version":"2.1.0","runs":[{"invocations":[{"endTimeUtc":"2026-10-02T00:00:00Z"}],
"results":[{"level":"error"},{"level":"note"},{}]}]}`,
			map[string]any{"high": 1.0, "medium": 1.0, "low": 1.0, "findings": 3.0, "scanned_at": "2026-10-02T00:00:00Z"}},
		{"osv-scanner", store.KindVulnerabilities, OSVScanner, `{"results":[{"packages":[{"groups":[{"max_severity":"9.8"},{"max_severity":"5"}]},
{"vulnerabilities":[{},{}]}]}]}`, map[string]any{"critical": 1.0, "medium": 1.0, "unknown": 2.0, "findings": 4.0}},
		{"review", store.KindReview, Review, `{"schema":"kista.review.v1","verdict":"conditional","analysed_at":"2026-10-03T12:00:00+02:00",
"analyser":"Enterest","capabilities":["http"],"network":["*.example.com"],"files":[],"processes":[],"licences":["MIT"],
"findings":[{"severity":"high","title":"writes outside its directory","detail":"…"}],"text":"# Review"}`,
			map[string]any{"verdict": "conditional", "analysed_at": "2026-10-03T10:00:00Z", "analyser": "Enterest", "high": 1.0, "findings": 1.0}},
		{"dsse", store.KindProvenance, DSSE, dsse, map[string]any{"predicate_type": "https://slsa.dev/provenance/v1", "verified": false,
			"builder": "https://github.com/actions/runner", "repository": "https://github.com/hugr-lab/tresor", "ref": "refs/tags/v1.0"}},
		{"sigstore", store.KindProvenance, Sigstore, `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","dsseEnvelope":` + dsse + `}`,
			map[string]any{"predicate_type": "https://slsa.dev/provenance/v1", "verified": false, "builder": "https://github.com/actions/runner",
				"repository": "https://github.com/hugr-lab/tresor", "ref": "refs/tags/v1.0"}},
		{"notes", store.KindNotes, Markdown, "# Approved for prod\nby the platform team", map[string]any{"first_line": "Approved for prod"}},
		{"notes scrubbed", store.KindNotes, Markdown, "# a\u202eb\u009b<c> & d", map[string]any{"first_line": "ab<c> & d"}},
	} {
		s, err := Summarize(c.kind, c.mt, []byte(c.doc))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(s), &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: %v", c.name, got)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: %s = %v, want %v", c.name, k, got[k], v)
			}
		}
	}

	for name, c := range map[string]struct{ kind, mt, doc string }{
		"not json":          {store.KindSBOM, CycloneDX, "nope"},
		"too deep":          {store.KindSBOM, CycloneDX, strings.Repeat("[", 40) + strings.Repeat("]", 40)},
		"spdx as cyclonedx": {store.KindSBOM, CycloneDX, `{"spdxVersion":"SPDX-2.3"}`},
		"review unknown":    {store.KindReview, Review, `{"schema":"kista.review.v1","verdict":"approved","analysed_at":"2026-10-03T12:00:00Z","analyser":"x","extra":1}`},
		"review verdict":    {store.KindReview, Review, `{"schema":"kista.review.v1","verdict":"great","analysed_at":"2026-10-03T12:00:00Z","analyser":"x"}`},
		"sarif 2.0":         {store.KindVulnerabilities, SARIF, `{"version":"2.0.0","runs":[]}`},
		"bad payload":       {store.KindProvenance, DSSE, `{"payloadType":"application/vnd.in-toto+json","payload":"%%%"}`},
		"markdown bytes":    {store.KindNotes, Markdown, "\xff\xfe"},
	} {
		if _, err := Summarize(c.kind, c.mt, []byte(c.doc)); !errors.Is(err, ErrDocument) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestMediaTypes(t *testing.T) {
	for _, c := range []struct{ kind, declared, want string }{
		{store.KindSBOM, "application/vnd.cyclonedx+json; charset=utf-8", CycloneDX},
		{store.KindVulnerabilities, "application/json; profile=osv-scanner", OSVScanner},
		{store.KindNotes, "text/markdown; charset=UTF-8", Markdown},
	} {
		if got, err := CanonicalMediaType(c.kind, c.declared); err != nil || got != c.want {
			t.Errorf("%s %s: %q %v", c.kind, c.declared, got, err)
		}
	}
	for _, c := range [][2]string{{store.KindSBOM, "text/html"}, {store.KindReview, CycloneDX}, {"virus", CycloneDX}, {store.KindSBOM, "%%"}} {
		if _, err := CanonicalMediaType(c[0], c[1]); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%v: %v", c, err)
		}
	}
	for _, title := range []string{"", strings.Repeat("x", 201), "a\u202eb", "line\nbreak"} {
		if ValidTitle(title) == nil {
			t.Errorf("title %q accepted", title)
		}
	}
}
