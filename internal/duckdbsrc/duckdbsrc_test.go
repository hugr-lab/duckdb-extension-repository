package duckdbsrc_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/duckdbsrc"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
)

const commit = "d54cbbe0123456789abcdef0123456789abcdef0"

// tarball is a source tarball as git archive (GitHub's codeload) writes one: a pax global header
// naming the commit, then everything under one top directory.
func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": commit}}))
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if strings.HasSuffix(n, "/") {
			must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: n, Mode: 0o755}))
			continue
		}
		must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: n, Mode: 0o644, Size: int64(len(files[n]))}))
		_, err := tw.Write([]byte(files[n]))
		must(err)
	}
	must(tw.Close())
	must(zw.Close())
	return buf.Bytes()
}

var duckdbFiles = map[string]string{
	"duckdb-2.0.0/": "",
	"duckdb-2.0.0/src/main/extension/extension_helper.cpp": `internal_extensions[] = {{"icu", "a"}, {"json", "b"}, {nullptr, nullptr}};`,
	"duckdb-2.0.0/src/main/extension/other.cpp":            "not read",
	"duckdb-2.0.0/src/include/duckdb/main/extension_entries.hpp": `{"read_parquet", "parquet", X},
static constexpr const char *AUTOLOADABLE_EXTENSIONS[] = {"httpfs", "legacy_parser"};`,
	"duckdb-2.0.0/extension/":                              "",
	"duckdb-2.0.0/extension/tpch/":                         "",
	"duckdb-2.0.0/extension/tpch/tpch.cpp":                 "not read",
	"duckdb-2.0.0/.github/config/in_tree_extensions.cmake": "duckdb_extension_load(autocomplete)",
	"duckdb-2.0.0/.github/config/extensions/spatial.cmake": "duckdb_extension_load( spatial\n",
	"duckdb-2.0.0/.github/config/README.md":                "duckdb_extension_load(not_a_cmake)",
	"duckdb-2.0.0/../escape.cmake":                         "duckdb_extension_load(escaped)",
}

func TestFetchCore(t *testing.T) {
	body := tarball(t, duckdbFiles)
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		w.Write(body)
	}))
	defer srv.Close()
	src, got, err := duckdbsrc.Fetch(context.Background(), srv.Client(), srv.URL+"/duckdb/duckdb/tar.gz/", "v2.0.0", duckdbsrc.CoreFiles)
	if err != nil || got != commit || asked != "/duckdb/duckdb/tar.gz/v2.0.0" {
		t.Fatalf("%q %q %v", got, asked, err)
	}
	if _, err := fs.Stat(src, "extension/tpch/tpch.cpp"); err == nil {
		t.Fatal("a file not read was kept")
	}
	names, err := duckdbsrc.CoreNames(src)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"autocomplete", "httpfs", "icu", "json", "legacy_parser", "parquet", "spatial", "tpch"}; !slices.Equal(names, want) {
		t.Fatalf("%v, want %v", names, want)
	}
	for _, ref := range []string{"", "../x", "a/b", "-x", "v1..2"} {
		if _, _, err := duckdbsrc.Fetch(context.Background(), srv.Client(), srv.URL+"/", ref, duckdbsrc.CoreFiles); err == nil {
			t.Fatalf("ref %q was fetched", ref)
		}
	}
	srv404 := httptest.NewServer(http.NotFoundHandler())
	defer srv404.Close()
	if _, _, err := duckdbsrc.Fetch(context.Background(), srv404.Client(), srv404.URL+"/", "v9.9.9", duckdbsrc.CoreFiles); err == nil {
		t.Fatal("a 404 was read")
	}
}

func TestCoreNamesRefusesShortList(t *testing.T) {
	files := map[string]string{}
	for k, v := range duckdbFiles {
		files[k] = strings.ReplaceAll(v, `{"json", "b"}, `, "")
	}
	src, _, err := duckdbsrc.Read(bytes.NewReader(tarball(t, files)), duckdbsrc.CoreFiles)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := duckdbsrc.CoreNames(src); err == nil || !strings.Contains(err.Error(), "json") {
		t.Fatalf("a list without json: %v", err)
	}
}

func TestReadBounds(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 20001; i++ { // directories kept count, though they hold nothing
		files[fmt.Sprintf("duckdb-x/extension/e%d/", i)] = ""
	}
	if _, _, err := duckdbsrc.Read(bytes.NewReader(tarball(t, files)), duckdbsrc.CoreFiles); err == nil {
		t.Fatal("twenty thousand directories were kept")
	}
}

func TestCommunityNames(t *testing.T) {
	src, got, err := duckdbsrc.Read(bytes.NewReader(tarball(t, map[string]string{
		"community-extensions-main/extensions/h3/description.yml":         "extension:\n  name: h3\n  version: 1\n",
		"community-extensions-main/extensions/dir_x/description.yml":      "extension:\n  description: x\n  name: 'Other_Name'\n",
		"community-extensions-main/extensions/dir_x/docs/description.yml": "extension:\n  name: deep\n",
		"community-extensions-main/extensions/hy/description.yml":         "extension:\n  name: my-ext\n",
		"community-extensions-main/extensions/crlf/description.yml":       "extension:\r\n  name: crlf_name\r\n",
		"community-extensions-main/README.md":                             "",
	})), duckdbsrc.CommunityFiles)
	if err != nil || got != commit {
		t.Fatal(got, err)
	}
	names, err := duckdbsrc.CommunityNames(src)
	if want := []string{"crlf", "crlf_name", "dir_x", "h3", "hy", "other_name"}; err != nil || !slices.Equal(names, want) {
		t.Fatalf("%v %v, want %v", names, err, want)
	}
}

// TestPinCoreNames reads the DuckDB checkout make e2e-build makes at the pin: what it names core is
// in the seed migration 0030 loads (the seed was generated from the pin).
func TestPinCoreNames(t *testing.T) {
	root := filepath.Join("..", "..", "e2e", ".build", "src")
	if _, err := os.Stat(filepath.Join(root, "src", "main", "extension", "extension_helper.cpp")); err != nil {
		t.Skip("no DuckDB checkout (make e2e-build)")
	}
	names, err := duckdbsrc.CoreNames(os.DirFS(root))
	if err != nil {
		t.Fatal(err)
	}
	seed := reserved.Seed()
	for _, n := range names {
		if seed[n] != reserved.Core {
			t.Errorf("%s is core at the pin, not in the seed", n)
		}
	}
	if !slices.Contains(names, "legacy_parser") {
		t.Errorf("legacy_parser is not core at the pin: %v", names)
	}
}
