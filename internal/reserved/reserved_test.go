package reserved

import (
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
)

func TestKind(t *testing.T) {
	for name, want := range map[string]string{
		"httpfs": Core, "parquet": Core, "demo_capi": Core, "jemalloc": Core, "glue": Core,
		"h3": Community, "midi": Community, "duckdb_midi": Community,
		"tresor": "", "acl": "", "loadable_extension_demo": "",
	} {
		if got := Kind(pkgtype.DuckDBExtension, name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

func TestAliases(t *testing.T) {
	if Canonical(pkgtype.DuckDBExtension, "postgres") != "postgres_scanner" || Canonical(pkgtype.DuckDBExtension, "s3") != "httpfs" || Canonical(pkgtype.DuckDBExtension, "httpfs") != "" {
		t.Fatal("aliases")
	}
}
