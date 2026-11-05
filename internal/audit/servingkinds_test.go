package audit_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
)

// Spec 0041: every event the serving plane writes under a channel's subject (its actor the serving
// caller) is in audit.ServingKinds, which a channel's administrators never read. A new kind written
// by internal/serve fails here until it is listed (or written under another subject).
func TestServingKinds(t *testing.T) {
	files, err := filepath.Glob("../serve/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("internal/serve's files: %v %v", files, err)
	}
	call := regexp.MustCompile(`Events\.(?:Event|Refusal)\(([^\n]*)`)
	literal := regexp.MustCompile(`"([a-z_.]+)"`)
	subject := regexp.MustCompile(`"([a-z_]+):`)
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range call.FindAllStringSubmatch(string(b), -1) {
			var kind audit.Kind
			for _, l := range literal.FindAllStringSubmatch(m[1], -1) {
				if audit.Known(audit.Kind(l[1])) {
					kind = audit.Kind(l[1])
				}
			}
			sub := subject.FindStringSubmatch(m[1])
			if kind == "" || sub == nil {
				t.Errorf("%s: an event write the test cannot read: %s", f, m[0])
				continue
			}
			seen++
			if sub[1] == "channel" && !slices.Contains(audit.ServingKinds, kind) {
				t.Errorf("%s writes %s under a channel's subject: list it in audit.ServingKinds", f, kind)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no event write found in internal/serve: the pattern no longer matches them")
	}
	for _, k := range audit.ServingKinds {
		if !audit.Known(k) {
			t.Errorf("%s is not in the catalogue", k)
		}
	}
}
