package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

// Spec 0042 from the CLI: the tenant's and a channel's rules (a flag left out keeps its field,
// default and none), the plan, a pin and its removal, the pass's dry run and -apply, a restore, the
// line act; release list shows retired.
func TestAdminRetention(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := signer.GenerateKeyFile(filepath.Join(keys, "a.pem")); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "kista.yaml")
	os.WriteFile(cfg, []byte("profile: dev\nstore: { kind: sqlite, sqlite: { path: "+filepath.Join(dir, "k.db")+" } }\n"+
		"signers: { file_dir: "+keys+" }\ngc: { interval: 24h }\n"), 0o600)
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
	run(0, "version", "add", "v2.0.0", "-kind", "release", "-c-api", "v1.5.6", "-core-extensions", coreFile(t, "json"))
	run(0, "channel", "create", "acme/prod", "-kind", "signed")
	run(0, "channel", "versions", "acme/prod", "-add", "v2.0.0")
	run(0, "key", "add", "acme/prod", "-signer", "file:a.pem", "-active")
	var ids []string
	for i, v := range []string{"1.0", "1.1", "1.2"} {
		file := filepath.Join(dir, "geo-"+v)
		block, err := extfile.EncodeMetadata(extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: v, ABI: extfile.ABICPP})
		if err != nil {
			t.Fatal(err)
		}
		body := append(append(bytes.Repeat([]byte{byte(i + 1)}, 3000), extfile.MetadataPrefix...), block[:]...)
		if err := os.WriteFile(file, append(body, make([]byte, extfile.SignatureSize)...), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errw bytes.Buffer
		if code := admin([]string{"-config", cfg, "release", "add", "acme/prod", file, "-name", "geo", "-unchecked"}, nil, &out, &errw); code != 0 {
			t.Fatalf("release add: %s", errw.String())
		}
		ids = append(ids, strings.TrimSpace(out.String()))
	}
	// the rules: the tenant's keep, the channel's grace; default, none
	if got := run(0, "tenant", "retention", "acme", "-keep", "1"); !strings.Contains(got, "keep 1, grace default") {
		t.Fatalf("tenant retention: %q", got)
	}
	if got := run(0, "channel", "retention", "acme/prod"); !strings.Contains(got, "in force: none (missing: grace)") {
		t.Fatalf("no grace in force: %q", got)
	}
	if got := run(0, "channel", "retention", "acme/prod", "-grace", "0"); !strings.Contains(got, "keep 1 (tenant), grace 0s (channel)") {
		t.Fatalf("channel retention: %q", got)
	}
	if got := run(0, "channel", "retention", "acme/prod", "-keep", "none"); !strings.Contains(got, "in force: none (the channel's)") ||
		!strings.Contains(got, "grace 0s") {
		t.Fatalf("none: %q", got)
	}
	run(0, "channel", "retention", "acme/prod", "-keep", "default")
	run(2, "channel", "retention", "acme/prod", "-keep", "many")
	run(1, "channel", "retention", "acme/prod", "-grace", "100000h") // above the ceiling
	// the plan, the dry run, then the pass
	if got := run(0, "channel", "retention-plan", "acme/prod"); strings.Count(got, "retire") != 2 {
		t.Fatalf("the plan: %q", got)
	}
	if got := run(0, "retention", "run", "acme"); !strings.Contains(got, "retire") || !strings.Contains(run(0, "release", "list", "acme/prod"), "active") ||
		strings.Contains(run(0, "release", "list", "acme/prod"), "retired") {
		t.Fatalf("a dry run: %q", got)
	}
	if got := run(0, "retention", "run", "acme/prod", "-apply"); !strings.Contains(got, "acme/prod: 2 retired, 0 purged, 0 failed") {
		t.Fatalf("the pass: %q", got)
	}
	if got := run(0, "release", "list", "acme/prod"); strings.Count(got, "retired") != 2 {
		t.Fatalf("release list: %q", got)
	}
	// a pin keeps retired 1.0 from the purge; grace 0: the next pass purges 1.1, then 1.0 is restored
	pin, _, _ := strings.Cut(run(0, "channel", "pin", "acme/prod", "geo", "1.0", "-reason", "LTS nodes"), "\n") // stdout first
	run(2, "channel", "pin", "acme/prod", "geo", "1.0")                                                         // -reason
	if got := run(0, "channel", "retention", "acme/prod"); !strings.Contains(got, "LTS nodes") {
		t.Fatalf("the pins: %q", got)
	}
	if got := run(0, "retention", "run", "-apply"); !strings.Contains(got, "acme/prod: 0 retired, 1 purged, 0 failed") {
		t.Fatalf("the purge: %q", got)
	}
	run(0, "release", "restore", "acme/prod", ids[0])
	run(1, "release", "restore", "acme/prod", ids[0]) // not retired
	run(0, "channel", "unpin", "acme/prod", pin)
	run(1, "channel", "unpin", "acme/prod", pin)
	// the line act
	if got := run(0, "channel", "retire-line", "acme/prod", "v2.0.0", "-name", "geo"); !strings.Contains(got, "2 retired") {
		t.Fatalf("retire-line: %q", got)
	}
	if got := run(0, "events", "list", "acme"); !strings.Contains(got, "retention.line") || !strings.Contains(got, "release.retire") ||
		!strings.Contains(got, "retention.pin") || !strings.Contains(got, "retention.run") || !strings.Contains(got, "release.restore") {
		t.Fatalf("events: %q", got)
	}
}
