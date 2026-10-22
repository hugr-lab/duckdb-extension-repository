// Package duckdbsrc reads the extension names DuckDB owns from its sources (specs 0008, 0030): a
// DuckDB version's core extensions from duckdb/duckdb, and the community extensions from
// duckdb/community-extensions. It parses text only, from a checkout or a source tarball streamed
// into memory, and never executes what it read.
package duckdbsrc

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing/fstest"
)

// The tarballs are GitHub's (codeload): a ref is a tag, a branch or a commit.
const (
	CoreTarball      = "https://codeload.github.com/duckdb/duckdb/tar.gz/"
	CommunityTarball = "https://codeload.github.com/duckdb/community-extensions/tar.gz/"
)

var (
	nameRE      = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	refRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	commitRE    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	commitRefRE = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	defaultRE   = regexp.MustCompile(`\{"([a-z0-9_]+)",\s*"`)                     // internal_extensions[]
	entryRE     = regexp.MustCompile(`\{"[^"\n]*",\s*"([a-z0-9_]+)"`)             // {"x", "<extension>", ...}
	quotedRE    = regexp.MustCompile(`"([a-z0-9_]+)"`)                            // AUTOLOADABLE_EXTENSIONS
	loadRE      = regexp.MustCompile(`duckdb_extension_load\(\s*([A-Za-z0-9_]+)`) // .github/config
	ymlNameRE   = regexp.MustCompile(`(?m)^extension:[ \t]*\r?\n(?:[ \t]+.*\n)*?  name:[ \t]*["']?([A-Za-z0-9_]+)["']?[ \t]*\r?$`)
	autoloadRE  = regexp.MustCompile(`(?s)AUTOLOADABLE_EXTENSIONS\[\]\s*=\s*\{(.*?)\}`)
)

// Paths of duckdb/duckdb that CoreNames reads.
const (
	helperPath  = "src/main/extension/extension_helper.cpp"
	entriesPath = "src/include/duckdb/main/extension_entries.hpp"
)

// maxNames bounds a list read: DuckDB has a few dozen core extensions, and some hundreds of
// community ones.
const maxNames = 2000

// alwaysCore are core extensions every DuckDB version has: a list without them was misparsed (a
// new layout of DuckDB's sources), and is refused rather than registered short.
var alwaysCore = []string{"icu", "json", "parquet"}

// CoreNames reads a DuckDB source tree's core extension names: internal_extensions[] (the
// extensions built in by default), extension_entries.hpp (the extensions DuckDB knows functions,
// settings and types of, and AUTOLOADABLE_EXTENSIONS), the in-tree extension/ directories and the
// extensions .github/config loads (out of tree, still DuckDB's).
func CoreNames(fsys fs.FS) ([]string, error) {
	set := map[string]bool{}
	add := func(n string) {
		if n = strings.ToLower(n); nameRE.MatchString(n) {
			set[n] = true
		}
	}
	helper, err := fs.ReadFile(fsys, helperPath)
	if err != nil {
		return nil, err
	}
	h := string(helper)
	i := strings.Index(h, "internal_extensions[]")
	j := strings.Index(h[max(i, 0):], "{nullptr, nullptr") // {nullptr, nullptr} or, before v2.0, {nullptr, nullptr, false}
	if i < 0 || j < 0 {
		return nil, fmt.Errorf("internal_extensions[] not found in %s", helperPath)
	}
	for _, m := range defaultRE.FindAllStringSubmatch(h[i:i+j], -1) {
		add(m[1])
	}
	b, err := fs.ReadFile(fsys, entriesPath)
	if err != nil {
		return nil, err
	}
	entries := string(b)
	ms := entryRE.FindAllStringSubmatch(entries, -1)
	if len(ms) == 0 {
		return nil, fmt.Errorf("no extension entries found in %s", entriesPath)
	}
	for _, m := range ms {
		add(m[1])
	}
	auto := autoloadRE.FindStringSubmatch(entries)
	if auto == nil {
		return nil, fmt.Errorf("AUTOLOADABLE_EXTENSIONS not found in %s", entriesPath)
	}
	ms = quotedRE.FindAllStringSubmatch(auto[1], -1)
	if len(ms) == 0 {
		return nil, fmt.Errorf("AUTOLOADABLE_EXTENSIONS is empty in %s", entriesPath)
	}
	for _, m := range ms {
		add(m[1])
	}
	dirs, err := fs.ReadDir(fsys, "extension")
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		if d.IsDir() {
			add(d.Name())
		}
	}
	loads := 0
	err = fs.WalkDir(fsys, ".github/config", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".cmake") {
			return err
		}
		s, err := fs.ReadFile(fsys, p)
		for _, m := range loadRE.FindAllStringSubmatch(string(s), -1) {
			add(m[1])
			loads++
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if loads == 0 {
		return nil, errors.New("no duckdb_extension_load in .github/config")
	}
	if len(set) > maxNames {
		return nil, fmt.Errorf("%d core extensions read: more than a DuckDB version has", len(set))
	}
	var missing []string
	for _, n := range alwaysCore {
		if !set[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the core extensions read lack %s, which every DuckDB version has: the sources' layout is not the one this kista reads",
			strings.Join(missing, ", "))
	}
	return sorted(set), nil
}

// CommunityNames reads duckdb/community-extensions' extension names: each extensions/<dir>, and the
// name its description.yml gives.
func CommunityNames(fsys fs.FS) ([]string, error) {
	set := map[string]bool{}
	dirs, err := fs.ReadDir(fsys, "extensions")
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if n := strings.ToLower(d.Name()); nameRE.MatchString(n) {
			set[n] = true
		}
		y, err := fs.ReadFile(fsys, path.Join("extensions", d.Name(), "description.yml"))
		if err != nil {
			continue
		}
		if m := ymlNameRE.FindStringSubmatch(string(y)); m != nil {
			if n := strings.ToLower(m[1]); nameRE.MatchString(n) {
				set[n] = true
			}
		}
	}
	if len(set) == 0 {
		return nil, errors.New("no community extensions found")
	}
	if len(set) > maxNames {
		return nil, fmt.Errorf("%d community extensions read: more than this kista takes", len(set))
	}
	return sorted(set), nil
}

// CoreFiles says which of duckdb/duckdb's paths (a tarball's top directory stripped) CoreNames reads:
// Fetch keeps those alone.
func CoreFiles(p string, dir bool) bool {
	switch {
	case dir:
		return strings.Count(p, "/") == 1 && strings.HasPrefix(p, "extension/")
	case p == helperPath || p == entriesPath:
		return true
	default:
		return strings.HasPrefix(p, ".github/config/") && strings.HasSuffix(p, ".cmake")
	}
}

// CommunityFiles says which of duckdb/community-extensions' paths CommunityNames reads.
func CommunityFiles(p string, dir bool) bool {
	parts := strings.Split(p, "/")
	if dir {
		return len(parts) == 2 && parts[0] == "extensions"
	}
	return len(parts) == 3 && parts[0] == "extensions" && parts[2] == "description.yml"
}

// Bounds of a fetch: the files kept (DuckDB's are a few MB together; each entry counts at least
// minEntry), the entries kept, and the tarball read, compressed and inflated.
const (
	maxKept     = 64 << 20
	maxFile     = 16 << 20
	minEntry    = 512
	maxEntries  = 20000
	maxTarball  = 2 << 30
	maxInflated = 8 << 30
)

// IsCommit reports whether a ref names a commit (a hex prefix of 7 to 40 characters). GitHub serves a
// commit of any fork under the repository's name, so a commit is DuckDB's only when the caller
// knows it is.
func IsCommit(ref string) bool { return commitRefRE.MatchString(ref) }

// ValidRef checks a ref is a plain tag, branch or commit name.
func ValidRef(ref string) error {
	if !refRE.MatchString(ref) || strings.Contains(ref, "..") {
		return fmt.Errorf("%q is not a tag, branch or commit", ref)
	}
	return nil
}

// Fetch streams the gzip tarball at base+ref and keeps in memory the files and directories keep
// takes (by their path below the tarball's top directory). It returns them and the commit the
// tarball was made from (the pax comment git archive writes; "" when there is none).
func Fetch(ctx context.Context, client *http.Client, base, ref string, keep func(p string, dir bool) bool) (fs.FS, string, error) {
	if err := ValidRef(ref); err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+ref, nil)
	if err != nil {
		return nil, "", err
	}
	c := *client // only https on the tarball's host: the redirects codeload may answer stay there
	host := req.URL.Host
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 || r.URL.Host != host || r.URL.Scheme != req.URL.Scheme {
			return fmt.Errorf("a redirect to %s", r.URL.Redacted())
		}
		return nil
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%s%s: %s", base, ref, resp.Status)
	}
	return Read(io.LimitReader(resp.Body, maxTarball), keep)
}

// Read is Fetch's reading of a gzip tarball.
func Read(r io.Reader, keep func(p string, dir bool) bool) (fs.FS, string, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, "", err
	}
	tr := tar.NewReader(&capped{r: zr, left: maxInflated})
	out := fstest.MapFS{}
	var commit string
	kept, entries := 0, 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			if c := h.PAXRecords["comment"]; commitRE.MatchString(c) {
				commit = c
			}
			continue
		}
		_, p, ok := strings.Cut(strings.TrimSuffix(h.Name, "/"), "/")
		if !ok || p == "" || !fs.ValidPath(p) {
			continue
		}
		dir := h.Typeflag == tar.TypeDir
		if !dir && h.Typeflag != tar.TypeReg || !keep(p, dir) {
			continue
		}
		entries++
		kept += max(int(h.Size), minEntry)
		if entries > maxEntries || h.Size > maxFile || kept > maxKept {
			return nil, "", fmt.Errorf("%s: more than this kista reads", p)
		}
		switch {
		case dir:
			out[p] = &fstest.MapFile{Mode: fs.ModeDir | 0o755}
		default:
			b, err := io.ReadAll(io.LimitReader(tr, h.Size))
			if err != nil {
				return nil, "", err
			}
			out[p] = &fstest.MapFile{Data: b, Mode: 0o644}
		}
	}
	return out, commit, nil
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// capped fails a read past its bound, where a LimitReader would end the tarball quietly.
type capped struct {
	r    io.Reader
	left int64
}

func (c *capped) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errors.New("the tarball inflates past what this kista reads")
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}
