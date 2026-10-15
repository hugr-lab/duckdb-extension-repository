package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
)

// bundleFixture writes an unsigned bundle of one signed extension from a repository source, and
// its plan pinning the bundle key and the source's key.
func bundleFixture(t *testing.T, dir, bundleFP string, edit func(*bundle.Manifest)) (plan, unsigned string) {
	t.Helper()
	ext, pub := writeExtension(t)
	file, _ := os.ReadFile(ext)
	pubPEM, _ := os.ReadFile(pub)
	k, err := extfile.ParsePublicKey(string(pubPEM))
	if err != nil {
		t.Fatal(err)
	}
	ef, err := extfile.Open(bytes.NewReader(file), int64(len(file)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	zw.Write(file)
	zw.Close()
	wk, _ := json.Marshal(map[string]any{"signature_keys": []string{string(pubPEM)}})
	const prefix = "https://src.example.org/acme/prod"
	now := time.Now().UTC().Truncate(time.Second)
	p := bundle.Plan{Format: bundle.PlanFormat, CreatedAt: now, Tenant: "acme", BundleKeys: []string{bundleFP},
		Sources: []bundle.PlanSource{{Kind: "repository", Prefix: prefix, Keys: []string{extfile.Fingerprint(k)},
			Cells: []bundle.PlanCell{{DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "x", Versions: []string{}}}}}}
	planData, _ := json.Marshal(p)
	plan = filepath.Join(dir, "plan.json")
	os.WriteFile(plan, planData, 0o644)
	m := bundle.Manifest{Format: bundle.ManifestFormat, ID: "0192f0c4-1b2a-7c3d-8e4f-0123456789ab", CreatedAt: now,
		Exporter: bundle.Exporter{Version: "kista test", PublicURL: "https://gw.example.org"},
		Plan:     bundle.PlanRef{Tenant: "acme", CreatedAt: now, SHA256: sha(planData)},
		Sources: []bundle.Source{{Kind: "repository", Prefix: prefix,
			WellKnown: &bundle.FileRef{Path: "sources/1/well-known.json", SHA256: sha(wk), Size: int64(len(wk))},
			Cells: []bundle.Cell{{DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "x", Outcome: bundle.Fetched,
				File: "files/" + sha(file) + ".gz", SHA256: sha(file), Size: int64(z.Len()), InflatedSize: int64(len(file)),
				BodyHash: ef.Hash.String(), FetchedAt: now}}}}}
	if edit != nil {
		edit(&m)
	}
	md, _ := json.Marshal(m)
	var out bytes.Buffer
	w := bundle.NewWriter(&out)
	if err := bundle.WriteHead(w, md, nil); err != nil {
		t.Fatal(err)
	}
	w.Entry("sources/1/well-known.json", int64(len(wk)), bytes.NewReader(wk))
	w.Entry(m.Sources[0].Cells[0].File, int64(z.Len()), bytes.NewReader(z.Bytes()))
	w.Close()
	unsigned = filepath.Join(dir, "b.unsigned")
	os.WriteFile(unsigned, out.Bytes(), 0o644)
	return plan, unsigned
}

func TestBundleCLI(t *testing.T) {
	dir := t.TempDir()
	run := func(want int, args ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != want {
			t.Fatalf("kista %v: exit %d, want %d\n%s%s", args, code, want, out.String(), errb.String())
		}
		return out.String() + errb.String()
	}
	keyFile := filepath.Join(dir, "bundle-key.pem")
	fp := strings.TrimSpace(run(0, "bundle", "keygen", "-o", keyFile))
	if st, err := os.Stat(keyFile); err != nil || st.Mode().Perm() != 0o600 || !strings.HasPrefix(fp, "sha256:") {
		t.Fatalf("keygen: %v %v %q", st, err, fp)
	}
	run(1, "bundle", "keygen", "-o", keyFile) // never over an existing file

	plan, unsigned := bundleFixture(t, dir, fp, nil)
	// unsigned: inspect reports it, the import would refuse it
	if got := run(1, "bundle", "inspect", unsigned, "-plan", plan); !strings.Contains(got, "unsigned") {
		t.Errorf("an unsigned bundle: %s", got)
	}
	signed := filepath.Join(dir, "b.kbundle")
	if got := run(0, "bundle", "sign", "-key", keyFile, "-plan", plan, "-in", unsigned, "-o", signed); !strings.Contains(got, fp) {
		t.Errorf("sign: %s", got)
	}
	run(1, "bundle", "sign", "-key", keyFile, "-plan", plan, "-in", signed, "-o", signed+"2") // already signed
	got := run(0, "bundle", "inspect", signed, "-plan", plan)
	for _, want := range []string{"consistent with the plan's pins", "(pinned by the plan)", " pinned\n", "fetched 1", "sha256 "} {
		if !strings.Contains(got, want) {
			t.Errorf("inspect: no %q in\n%s", want, got)
		}
	}
	if got := run(0, "bundle", "inspect", signed); !strings.Contains(got, "not checked against a site's pins") {
		t.Errorf("inspect without a plan: %s", got)
	}
	// another key: sign refuses it; a plan that does not pin the bundle's key: inspect says so
	other := filepath.Join(dir, "other.pem")
	otherFP := strings.TrimSpace(run(0, "bundle", "keygen", "-o", other))
	run(1, "bundle", "sign", "-key", other, "-plan", plan, "-in", unsigned, "-o", filepath.Join(dir, "x"))
	plan2, _ := bundleFixture(t, t.TempDir(), otherFP, nil)
	if got := run(1, "bundle", "inspect", signed, "-plan", plan2); !strings.Contains(got, "not pinned by the plan") {
		t.Errorf("an unpinned bundle key: %s", got)
	}
	// a file filling a cell its footer does not name, and a body hash the manifest misstates
	for name, edit := range map[string]func(*bundle.Manifest){
		"another DuckDB version": func(m *bundle.Manifest) { m.Sources[0].Cells[0].DuckDBVersion = "v2.1.0" },
		"another body hash":      func(m *bundle.Manifest) { m.Sources[0].Cells[0].BodyHash = strings.Repeat("0", 64) },
	} {
		d := t.TempDir()
		p, u := bundleFixture(t, d, fp, edit)
		s := filepath.Join(d, "b.kbundle")
		run(0, "bundle", "sign", "-key", keyFile, "-plan", p, "-in", u, "-o", s)
		if got := run(1, "bundle", "inspect", s, "-plan", p); !strings.Contains(got, "problem:") {
			t.Errorf("%s: %s", name, got)
		}
	}
	run(2, "bundle")
	run(2, "bundle", "unknown")
}
