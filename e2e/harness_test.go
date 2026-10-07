// Package e2e runs DuckDB, built at kista's pin by build-duckdb.sh, against signed repositories, and
// confirms the DuckDB behaviour spec 0001 relies on. Set KISTA_E2E_DUCKDB to the build output
// directory; without it the tests are skipped.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// build is the e2e DuckDB build: the runner and the unsigned extensions it built.
type build struct {
	dir        string
	runner     string
	extensions map[string]string // name -> path of the unsigned .duckdb_extension
	platform   string
	versionDir string // the extension folder DuckDB uses: a release tag or the source id
}

var (
	theBuild     *build
	theBuildErr  error
	theBuildOnce sync.Once
)

// needBuild returns the build or skips the test when KISTA_E2E_DUCKDB is unset.
func needBuild(t *testing.T) *build {
	t.Helper()
	dir := os.Getenv("KISTA_E2E_DUCKDB")
	if dir == "" {
		t.Skip("KISTA_E2E_DUCKDB is not set (see e2e/build-duckdb.sh)")
	}
	theBuildOnce.Do(func() { theBuild, theBuildErr = loadBuild(dir) })
	if theBuildErr != nil {
		t.Fatal(theBuildErr)
	}
	return theBuild
}

func loadBuild(dir string) (*build, error) {
	b := &build{dir: dir, runner: filepath.Join(dir, "runner"), extensions: map[string]string{}}
	for _, name := range []string{"loadable_extension_demo", "demo_capi", "httpfs"} {
		p := filepath.Join(dir, "extensions", name+".duckdb_extension")
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("e2e build: %w", err)
		}
		b.extensions[name] = p
	}
	res, err := b.run(context.Background(), nil, "PRAGMA platform", "SELECT source_id FROM pragma_version()",
		"SELECT version() FROM pragma_version()")
	if err != nil {
		return nil, err
	}
	for _, r := range res {
		if !r.OK {
			return nil, fmt.Errorf("e2e build: %s: %s", r.Stmt, r.Error)
		}
	}
	b.platform = res[0].Rows[0][0].(string)
	version := res[2].Rows[0][0].(string)
	// GetVersionDirectoryName: the version for a release, the source id for anything else
	if isRelease(version) {
		b.versionDir = version
	} else {
		b.versionDir = res[1].Rows[0][0].(string)
	}
	return b, nil
}

func isRelease(v string) bool {
	if !strings.HasPrefix(v, "v") || strings.Contains(v, "-") {
		return false
	}
	return strings.Count(v, ".") == 2
}

// result is one statement's outcome, as the runner prints it.
type result struct {
	Stmt    string   `json:"stmt"`
	OK      bool     `json:"ok"`
	Error   string   `json:"error"`
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// run starts one runner process (one database) with startup options and runs statements in it.
func (b *build) run(ctx context.Context, opts map[string]string, stmts ...string) ([]result, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	args := []string{}
	for k, v := range opts {
		args = append(args, k+"="+v)
	}
	cmd := exec.CommandContext(ctx, b.runner, args...)
	cmd.Stdin = strings.NewReader(strings.Join(stmts, "\x00"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// The harness is hermetic: the runner gets a minimal environment, so no proxy variable (curl reads
	// the lowercase ones too) or other setting from the caller's environment applies.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.TempDir()}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("runner: %v: %s", err, stderr.String())
	}
	var res []result
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var r result
		if err := dec.Decode(&r); err != nil {
			return nil, fmt.Errorf("runner output: %w", err)
		}
		res = append(res, r)
	}
	return res, nil
}

// session runs statements in one database and returns the results, failing the test on runner
// errors (not on statement errors).
type session struct {
	t    *testing.T
	b    *build
	opts map[string]string
}

// newSession returns a session with the harness defaults: repositories allowed, a private extension
// directory, no autoinstall or autoload (nothing reaches extensions.duckdb.org).
func newSession(t *testing.T, b *build, extra map[string]string) *session {
	opts := map[string]string{
		"allow_extension_repositories": "allowed",
		"extension_directory":          t.TempDir(),
		"autoinstall_known_extensions": "false",
		"autoload_known_extensions":    "false",
	}
	for k, v := range extra {
		opts[k] = v
	}
	return &session{t: t, b: b, opts: opts}
}

func (s *session) exec(stmts ...string) []result {
	s.t.Helper()
	res, err := s.b.run(context.Background(), s.opts, stmts...)
	if err != nil {
		s.t.Fatal(err)
	}
	if len(res) != len(stmts) {
		s.t.Fatalf("runner returned %d results for %d statements", len(res), len(stmts))
	}
	return res
}

// mustOK fails unless every result succeeded.
func mustOK(t *testing.T, res []result) {
	t.Helper()
	for _, r := range res {
		if !r.OK {
			t.Fatalf("%s\n  failed: %s", r.Stmt, r.Error)
		}
	}
}

// mustFail fails unless r failed with an error containing want (case-insensitive).
func mustFail(t *testing.T, r result, want string) {
	t.Helper()
	if r.OK {
		t.Fatalf("%s\n  succeeded, expected an error containing %q", r.Stmt, want)
	}
	if !strings.Contains(strings.ToLower(r.Error), strings.ToLower(want)) {
		t.Fatalf("%s\n  failed with %q, expected %q", r.Stmt, r.Error, want)
	}
}

func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
