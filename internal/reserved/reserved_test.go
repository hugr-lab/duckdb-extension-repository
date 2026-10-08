package reserved

import "testing"

func TestKind(t *testing.T) {
	for name, want := range map[string]string{
		"httpfs": Core, "parquet": Core, "demo_capi": Core, "jemalloc": Core, "glue": Core,
		"h3": Community, "midi": Community, "duckdb_midi": Community,
		"tresor": "", "acl": "", "loadable_extension_demo": "",
	} {
		if got := Kind(name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}
