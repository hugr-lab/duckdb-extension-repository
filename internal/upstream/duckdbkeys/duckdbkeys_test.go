package duckdbkeys

import (
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
)

func TestKeys(t *testing.T) {
	if len(Core()) != 20 || len(Community()) != 19 {
		t.Fatalf("keys: %d core, %d community", len(Core()), len(Community()))
	}
	seen := map[string]bool{}
	for _, k := range append(Core(), Community()...) {
		fp := extfile.Fingerprint(k)
		if fp == "" || seen[fp] {
			t.Fatalf("a key that fails the checks or repeats: %q", fp)
		}
		seen[fp] = true
	}
}
