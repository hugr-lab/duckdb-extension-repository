package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/duckdbsrc"
)

const duckdbUsage = `usage: kista duckdb core-extensions <tag> | <commit> -trust-commit | -dir <checkout> [-commit <sha>]
                                                       a DuckDB version's core extensions
       kista duckdb community-extensions [ref] | -dir <checkout>   duckdb/community-extensions' names (default: its default branch)`

// duckdbCmd prints the names DuckDB owns (spec 0030), read from its sources: no server
// configuration, no database. The core list is what version add -core-extensions takes.
func duckdbCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "core-extensions" && args[0] != "community-extensions") {
		fmt.Fprintln(stderr, duckdbUsage)
		return 2
	}
	core := args[0] == "core-extensions"
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	dir := fs.String("dir", "", "")
	commit := fs.String("commit", "", "")
	trust := fs.Bool("trust-commit", false, "")
	pos, err := flags(fs, args[1:])
	switch {
	case err != nil, len(pos) > 1, *dir != "" && len(pos) == 1, *commit != "" && *dir == "",
		core && *dir == "" && len(pos) == 0: // a DuckDB version is named
		fmt.Fprintln(stderr, duckdbUsage)
		return 2
	}
	ref := "HEAD" // the default branch
	if len(pos) == 1 {
		ref = pos[0]
	}
	if *dir == "" && duckdbsrc.IsCommit(ref) && !*trust {
		// GitHub serves a fork's commit under duckdb/duckdb's name: the caller vouches for it
		fmt.Fprintf(stderr, "kista duckdb %s: %s is a commit, and GitHub serves any fork's commits under DuckDB's name; "+
			"pass -trust-commit when you know it is DuckDB's (a release tag needs no such trust)\n", args[0], ref)
		return 1
	}
	names, from, err := duckdbNames(core, ref, *dir, *commit)
	if err != nil {
		fmt.Fprintf(stderr, "kista duckdb %s: %v\n", args[0], err)
		return 1
	}
	fmt.Fprintf(stdout, "# %s\n", from)
	for _, n := range names {
		fmt.Fprintln(stdout, n)
	}
	return 0
}

// duckdbNames reads the names from a checkout or from the ref's tarball, and says where they were
// read: "commit <sha> (<repository> <ref>)" when the commit is known.
func duckdbNames(core bool, ref, dir, commit string) ([]string, string, error) {
	repo, base, keep, read := "duckdb/community-extensions", duckdbsrc.CommunityTarball, duckdbsrc.CommunityFiles, duckdbsrc.CommunityNames
	if core {
		repo, base, keep, read = "duckdb/duckdb", duckdbsrc.CoreTarball, duckdbsrc.CoreFiles, duckdbsrc.CoreNames
	}
	var src fs.FS
	if dir != "" {
		if commit != "" && !commitArgRE.MatchString(commit) {
			return nil, "", fmt.Errorf("%q is not a commit", commit)
		}
		src, ref = os.DirFS(dir), dir
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		var err error
		if src, commit, err = duckdbsrc.Fetch(ctx, http.DefaultClient, base, ref, keep); err != nil {
			return nil, "", err
		}
	}
	names, err := read(src)
	if err != nil {
		return nil, "", err
	}
	from := fmt.Sprintf("%s %s", repo, ref)
	if commit != "" {
		from = fmt.Sprintf("commit %s (%s)", commit, from)
	}
	return names, from, nil
}

var (
	commitArgRE  = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	commitLineRE = regexp.MustCompile(`^#\s*commit\s+([0-9a-f]{7,64})\b`)
)

// maxCoreList bounds a core extensions file.
const maxCoreList = 1 << 20

// readCoreExtensions reads a list kista duckdb core-extensions printed (a file, or - for stdin):
// a name per line, and the commit its "# commit" line names.
func readCoreExtensions(path string, stdin io.Reader) ([]string, string, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, "", err
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, maxCoreList+1))
	if err != nil {
		return nil, "", err
	}
	if len(b) > maxCoreList {
		return nil, "", fmt.Errorf("%s: more than %d bytes", path, maxCoreList)
	}
	var names []string
	var commit string
	for _, l := range strings.Split(string(b), "\n") {
		switch l = strings.TrimSpace(l); {
		case l == "":
		case strings.HasPrefix(l, "#"):
			if strings.Contains(l, "duckdb/community-extensions") {
				return nil, "", errors.New(path + " is duckdb/community-extensions' list, not a DuckDB version's core extensions")
			}
			if m := commitLineRE.FindStringSubmatch(l); m != nil && commit == "" {
				commit = m[1]
			}
		default:
			names = append(names, l)
		}
	}
	if len(names) == 0 {
		return nil, "", errors.New("no core extensions in " + path)
	}
	return names, commit, nil
}
