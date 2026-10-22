package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// coreFile writes a core extensions list as kista duckdb core-extensions prints it.
func coreFile(t *testing.T, names ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "core.txt")
	body := "# commit d54cbbe0123456789abcdef0123456789abcdef (duckdb/duckdb v2.0.0)\n" + strings.Join(names, "\n") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadCoreExtensions(t *testing.T) {
	names, commit, err := readCoreExtensions(coreFile(t, "json", "parquet"), nil)
	if err != nil || !slices.Equal(names, []string{"json", "parquet"}) || commit != "d54cbbe0123456789abcdef0123456789abcdef" {
		t.Fatalf("%v %q %v", names, commit, err)
	}
	names, commit, err = readCoreExtensions("-", strings.NewReader("# duckdb/duckdb ./src\n\nicu\n"))
	if err != nil || !slices.Equal(names, []string{"icu"}) || commit != "" {
		t.Fatalf("%v %q %v", names, commit, err)
	}
	if _, _, err := readCoreExtensions("-", strings.NewReader("# nothing\n")); err == nil {
		t.Fatal("an empty list was read")
	}
	if _, _, err := readCoreExtensions("-", strings.NewReader("# commit 250f2d603d (duckdb/community-extensions main)\na5\n")); err == nil {
		t.Fatal("the community list was read as core extensions")
	}
	if _, _, err := readCoreExtensions("-", strings.NewReader(strings.Repeat("json\n", maxCoreList))); err == nil {
		t.Fatal("an oversized list was read")
	}
}

// TestDuckDBCmd reads names from checkouts laid out as DuckDB's (the parsing itself: internal/duckdbsrc).
func TestDuckDBCmd(t *testing.T) {
	write := func(root string, files map[string]string) {
		for p, body := range files {
			full := filepath.Join(root, p)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if body != "/" {
				if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	core := t.TempDir()
	write(core, map[string]string{
		"src/main/extension/extension_helper.cpp":       `static const DefaultExtension internal_extensions[] = {{"icu", "x", false}, {"json", "y", false}, {nullptr, nullptr, false}}; // before v2.0`,
		"src/include/duckdb/main/extension_entries.hpp": `{"read_parquet", "parquet", CatalogType::TABLE_FUNCTION_ENTRY}, static constexpr const char *AUTOLOADABLE_EXTENSIONS[] = {"httpfs"};`,
		"extension/tpch/x":                              "",
		".github/config/out_of_tree_extensions.cmake":   "duckdb_extension_load(spatial\n",
	})
	community := t.TempDir()
	write(community, map[string]string{"extensions/h3/description.yml": "extension:\n  name: h3\n", "extensions/Bad-Name/x": ""})
	run := func(want int, args ...string) string {
		t.Helper()
		var out, errw bytes.Buffer
		if code := duckdbCmd(args, &out, &errw); code != want {
			t.Fatalf("%v: exit %d, want %d\n%s", args, code, want, errw.String())
		}
		return out.String()
	}
	got := run(0, "core-extensions", "-dir", core, "-commit", "d54cbbe")
	if want := "# commit d54cbbe (duckdb/duckdb " + core + ")\nhttpfs\nicu\njson\nparquet\nspatial\ntpch\n"; got != want {
		t.Fatalf("core-extensions:\n%s\nwant\n%s", got, want)
	}
	if got := run(0, "community-extensions", "-dir", community); got != "# duckdb/community-extensions "+community+"\nh3\n" {
		t.Fatalf("community-extensions: %q", got)
	}
	run(2, "core-extensions")                                  // a DuckDB version is named
	run(2, "core-extensions", "v2.0.0", "-dir", core)          // one or the other
	run(2, "core-extensions", "v2.0.0", "-commit", "d54cbbe")  // a commit goes with a checkout
	run(1, "core-extensions", "-dir", community)               // not DuckDB's layout
	run(1, "core-extensions", "-dir", core, "-commit", "HEAD") // not a commit
	run(1, "core-extensions", "d54cbbe3df")                    // a commit, without -trust-commit: refused before any fetch
	run(2, "nothing")
}
