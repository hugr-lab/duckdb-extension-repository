// Package serve is the HTTP server DuckDB installs from (spec 0006): .well-known and the extension
// paths of every channel, health, listeners and shutdown.
package serve

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

type routeKind int

const (
	routeNone routeKind = iota
	routeHealthz
	routeReadyz
	routeWellKnown
	routeFlat
	routeVersioned
	routeLicense  // <prefix>/-/licenses/<name> (spec 0012)
	routeLicenses // <prefix>/-/licenses
	routeUsage    // <prefix>/-/usage/<name> (spec 0012 phase 1b)
	routeDaily    // <prefix>/-/usage (spec 0012 phase 1c)
	// <prefix>/-/revocations[/<cursor>] (spec 0039): paged by path, as no route takes a query
	routeRevocations
)

// String is a route kind's name for metrics.
func (k routeKind) String() string {
	return [...]string{"none", "healthz", "readyz", "well-known", "extension", "extension-versioned", "license", "licenses", "usage", "usage-daily",
		"revocations"}[k]
}

// route is a parsed request path.
type route struct {
	kind                                      routeKind
	tenant, channel                           string
	name, extVersion, duckdbVersion, platform string
	gz                                        bool
	cursor                                    string // a paged route's cursor ("" for its first page)
}

// cursorRe is a page cursor: base64url without padding.
var cursorRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`)

const (
	extSuffix   = ".duckdb_extension"
	gzSuffix    = ".duckdb_extension.gz"
	wellKnownFn = "duckdb-extension-repo.json"
)

// parseRoute matches the request target exactly as sent: no unescaping, no cleaning. Anything that
// does not match a grammar is routeNone.
func parseRoute(r *http.Request) route {
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.Fragment != "" || !strings.HasPrefix(r.RequestURI, "/") {
		return route{}
	}
	p := r.URL.EscapedPath()
	if strings.ContainsAny(p, "%\\") {
		return route{}
	}
	switch p {
	case "/healthz":
		return route{kind: routeHealthz}
	case "/readyz":
		return route{kind: routeReadyz}
	}
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return route{}
		}
	}
	if len(segs) < 4 || store.ValidName(segs[0]) != nil || store.ValidName(segs[1]) != nil {
		return route{}
	}
	rt := route{tenant: segs[0], channel: segs[1]}
	// spec 0012's routes under "-", which is no DuckDB version, extension name or .well-known
	if segs[2] == "-" {
		switch {
		case len(segs) == 4 && segs[3] == "licenses":
			rt.kind = routeLicenses
			return rt
		case len(segs) == 4 && segs[3] == "usage":
			rt.kind = routeDaily
			return rt
		case len(segs) == 4 && segs[3] == "revocations":
			rt.kind = routeRevocations
			return rt
		case len(segs) == 5 && segs[3] == "revocations" && cursorRe.MatchString(segs[4]):
			rt.kind, rt.cursor = routeRevocations, segs[4]
			return rt
		case len(segs) == 5 && segs[3] == "licenses" && release.ValidName(segs[4]) == nil:
			rt.kind, rt.name = routeLicense, segs[4]
			return rt
		case len(segs) == 5 && segs[3] == "usage" && release.ValidName(segs[4]) == nil:
			rt.kind, rt.name = routeUsage, segs[4]
			return rt
		}
		return route{}
	}
	switch len(segs) {
	case 4:
		if segs[2] == ".well-known" && segs[3] == wellKnownFn {
			rt.kind = routeWellKnown
			return rt
		}
	case 5:
		rt.kind, rt.duckdbVersion, rt.platform = routeFlat, segs[2], segs[3]
		if !rt.file(segs[4]) {
			return route{}
		}
	case 7:
		rt.kind, rt.name, rt.extVersion, rt.duckdbVersion, rt.platform = routeVersioned, segs[2], segs[3], segs[4], segs[5]
		name := rt.name
		if !rt.file(segs[6]) || rt.name != name || release.ValidExtVersion(rt.extVersion) != nil {
			return route{}
		}
	default:
		return route{}
	}
	if release.ValidName(rt.name) != nil || release.ValidPlatform(rt.platform) != nil ||
		store.ValidDuckDBVersion(rt.duckdbVersion) != nil {
		return route{}
	}
	return rt
}

// file parses "<name>.duckdb_extension[.gz]" into rt.name and rt.gz.
func (rt *route) file(f string) bool {
	switch {
	case strings.HasSuffix(f, gzSuffix):
		rt.name, rt.gz = strings.TrimSuffix(f, gzSuffix), true
	case strings.HasSuffix(f, extSuffix):
		rt.name = strings.TrimSuffix(f, extSuffix)
	default:
		return false
	}
	return rt.name != ""
}
