package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
)

// Spec 0034: -type where kista admin chooses a package type (a channel, reservations, group
// members, the catalogue, licences, attachments, a placement); the default as before; a type kista
// does not know refused.
func TestAdminPackageTypes(t *testing.T) {
	t.Cleanup(reserved.SetForTest(reserved.Seed(), 0))
	unregister := pkgtype.RegisterForTest("test_package")
	defer func() { unregister() }()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "kista.yaml")
	os.WriteFile(cfg, []byte("profile: dev\nstore: { kind: sqlite, sqlite: { path: "+filepath.Join(dir, "k.db")+" } }\n"+
		"signers: { file_dir: "+dir+" }\n"), 0o600)
	run := func(want int, args ...string) string {
		t.Helper()
		var out, errw bytes.Buffer
		if code := admin(append([]string{"-config", cfg}, args...), nil, &out, &errw); code != want {
			t.Fatalf("%v: exit %d, want %d\n%s%s", args, code, want, out.String(), errw.String())
		}
		return out.String() + errw.String()
	}
	run(0, "migrate")
	run(0, "tenant", "create", "acme")
	run(0, "channel", "create", "acme/prod", "-kind", "signed")
	run(0, "channel", "create", "acme/drivers", "-kind", "signed", "-type", "test_package")
	run(1, "channel", "create", "acme/x", "-kind", "signed", "-type", "nope")
	run(1, "channel", "create", "acme/y", "-kind", "passthrough", "-type", "test_package")
	if got := run(0, "channel", "list", "acme"); !strings.Contains(got, "signed/test_package") ||
		strings.Count(got, "test_package") != 1 {
		t.Fatalf("channel list: %q", got)
	}
	// the server's and the tenant's reservations per type
	run(0, "reserved", "add", "httpfs", "-kind", "core", "-type", "test_package")
	if got := run(0, "reserved", "list", "-type", "test_package"); !strings.Contains(got, "httpfs") || strings.Contains(got, "legacy_parser") {
		t.Fatalf("the other type's reservations: %q", got)
	}
	run(1, "reserved", "list", "-type", "nope")
	run(0, "tenant-reserved", "add", "acme", "snowflake", "-holder", "columnar", "-type", "test_package")
	if got := run(0, "tenant-reserved", "list", "acme"); strings.Contains(got, "snowflake") {
		t.Fatalf("the default type's tenant reservations: %q", got)
	}
	if got := run(0, "tenant-reserved", "list", "acme", "-type", "test_package"); !strings.Contains(got, "columnar") {
		t.Fatalf("the other type's tenant reservations: %q", got)
	}
	// a group's members of each type
	run(0, "extension-group", "add", "acme", "warehouse")
	run(0, "extension-group", "members", "acme", "warehouse", "-add", "snowflake")
	run(0, "extension-group", "members", "acme", "warehouse", "-add", "snowflake", "-type", "test_package")
	run(1, "extension-group", "members", "acme", "warehouse", "-add", "snowflake", "-type", "nope")
	if got := run(0, "extension-group", "members", "acme", "warehouse"); !strings.Contains(got, "test_package:snowflake") ||
		strings.Count(got, "snowflake") != 2 {
		t.Fatalf("members: %q", got)
	}
	// the catalogue and the licences are listed per type
	run(0, "catalogue", "list", "acme", "-type", "test_package")
	run(1, "catalogue", "list", "acme", "-type", "nope")
	run(1, "license", "list", "acme", "-type", "nope")
	// a package's attachments are of its type
	run(0, "attachment", "list", "acme", "snowflake", "-type", "test_package")
	run(1, "attachment", "list", "acme", "snowflake", "-type", "nope")
	// a placement's builds are of the channel's type: another one is refused, and only place takes it
	if got := run(1, "release", "place", "acme/prod", "-name", "snowflake", "-source", "tenant", "-version", "1.0", "-type", "test_package"); !strings.Contains(got, "package type") {
		t.Fatalf("a placement across types: %q", got)
	}
	run(1, "release", "place", "acme/prod", "-name", "snowflake", "-source", "tenant", "-version", "1.0", "-type", "nope")
	run(2, "release", "list", "acme/prod", "-type", "test_package")
	// a downgrade: a channel of a type kista does not know is not listed
	unregister()
	unregister = func() {}
	if got := run(0, "channel", "list", "acme"); strings.Contains(got, "drivers") || !strings.Contains(got, "prod") {
		t.Fatalf("channel list with an unknown type's channel: %q", got)
	}
}
