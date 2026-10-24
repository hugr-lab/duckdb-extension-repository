// Package reserved holds the extension names DuckDB owns (spec 0008, spec 0030): core and community
// names. Since spec 0030 they are the server's data (the store's reserved_names); this package holds
// the process's view of them, loaded from the store and reloaded when its version changes, with the
// lists kista compiled in until then (core.txt, extra.txt, community.txt) as the seed it starts from.
// It also holds DuckDB's extension aliases, which stay compiled in.
package reserved

import (
	"context"
	_ "embed"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
)

// Kinds of reserved names.
const (
	Core      = "core"
	Community = "community"
)

var (
	//go:embed core.txt
	coreList string
	//go:embed community.txt
	communityList string
	//go:embed extra.txt
	extraList string
)

// set is a view of the names at a version of the server's list (0: the seed), by (type, name):
// spec 0034's server-wide reservations are per package type.
type set struct {
	kinds   map[pkgtype.Key]string
	version int64
}

var current atomic.Pointer[set]

func init() { current.Store(&set{kinds: Keyed(Seed())}) }

// Keyed is DuckDB's names (Seed's form) as keys of the duckdb_extension type, whose names they are.
func Keyed(names map[string]string) map[pkgtype.Key]string {
	out := make(map[pkgtype.Key]string, len(names))
	for n, k := range names {
		out[pkgtype.Of(n)] = k
	}
	return out
}

// Seed is the lists kista compiled in: what migration 0030 loads into a server's list, and the view
// a process starts from.
func Seed() map[string]string {
	m := map[string]string{}
	for kind, list := range map[string]string{Community: communityList, Core: coreList + "\n" + extraList} {
		for _, line := range strings.Split(list, "\n") {
			if n := strings.TrimSpace(line); n != "" && !strings.HasPrefix(n, "#") {
				if m[n] != Core { // a name in both lists is core
					m[n] = kind
				}
			}
		}
	}
	return m
}

// Kind returns core or community for a reserved name of a type, and "" for any other, as the
// process's view holds it.
func Kind(packageType, name string) string {
	return current.Load().kinds[pkgtype.Key{Type: packageType, Name: name}]
}

// Version is the version of the server's list the view holds (0: the seed).
func Version() int64 { return current.Load().version }

// Source is where the server's list is read (the store).
type Source interface {
	ReservedNames(ctx context.Context) (map[pkgtype.Key]string, int64, error)
	ReservedVersion(ctx context.Context) (int64, error)
}

var loading sync.Mutex

// Ensure reloads the view from src when the server's list is at a newer version than the view: a
// serving request passes the version it read with its channel's row.
func Ensure(ctx context.Context, src Source, version int64) error {
	if version <= Version() {
		return nil
	}
	loading.Lock()
	defer loading.Unlock()
	if version <= Version() { // another request loaded it meanwhile
		return nil
	}
	kinds, v, err := src.ReservedNames(ctx)
	if err != nil {
		return err
	}
	if v > Version() {
		current.Store(&set{kinds: kinds, version: v})
	}
	return nil
}

// Sync reloads the view when the server's list changed: what a write path calls before it decides
// on a name (one small read).
func Sync(ctx context.Context, src Source) error {
	v, err := src.ReservedVersion(ctx)
	if err != nil {
		return err
	}
	return Ensure(ctx, src, v)
}

// SetForTest replaces the view with DuckDB's names given (tests); the returned function restores
// it.
func SetForTest(kinds map[string]string, version int64) func() {
	prev := current.Load()
	current.Store(&set{kinds: Keyed(kinds), version: version})
	return func() { current.Store(prev) }
}

//go:embed aliases.txt
var aliasList string

var aliases = loadAliases()

func loadAliases() map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(aliasList, "\n") {
		if f := strings.Fields(line); len(f) == 2 && !strings.HasPrefix(f[0], "#") {
			m[f[0]] = f[1]
		}
	}
	return m
}

// Canonical returns the name DuckDB installs an alias as (postgres: postgres_scanner), and "" for a
// name that is not an alias (spec 0009). Aliases are DuckDB's: a package of another type has none
// (spec 0034).
func Canonical(packageType, name string) string {
	if packageType != pkgtype.DuckDBExtension {
		return ""
	}
	return aliases[name]
}
