// Package upspec checks an upstream's fields (spec 0009): the checks upstream add makes, in a package
// the configuration's bootstrap section (spec 0024) uses too. It decides nothing about what exists.
package upspec

import (
	"fmt"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Prefix normalises a repository prefix: https (http to loopback in development), no user
// info, query, fragment or dot segments, and a port egress may reach. A bundle-mode upstream never
// contacts its prefix, which is only the source's identity: its port is not checked.
func Prefix(raw, mode string, portAllowed func(uint16) bool) (string, error) {
	bad := func(why string) (string, error) {
		return "", fmt.Errorf("%w: prefix %q: %s", store.ErrInvalid, raw, why)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return bad("not an absolute URL")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return bad("no user info, query or fragment")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || mode == store.ModeBundle) { // http (loopback, dev) is egress's rule, checked by a fetch
		return bad("not https")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return bad("dot segments")
		}
	}
	if u.RawPath != "" {
		return bad("escaped path characters")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || mode != store.ModeBundle && (portAllowed == nil || !portAllowed(uint16(n))) {
			return bad("egress does not reach that port")
		}
	}
	u.Path = strings.TrimRight(path.Clean("/"+u.Path), "/")
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if u.Port() == map[string]string{"https": "443", "http": "80"}[u.Scheme] {
		u.Host = u.Hostname() // the default port is dropped: one upstream, one form
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	return u.String(), nil
}

func Fingerprint(fp string) error {
	h, ok := strings.CutPrefix(fp, "sha256:")
	if !ok || len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
		return fmt.Errorf("%w: key fingerprint %q (sha256:<64 lowercase hex>)", store.ErrInvalid, fp)
	}
	return nil
}

// Entry validates an allowlist entry for an upstream of kind (spec 0009): a canonical name,
// reserved names only from their own repository unless allow_reserved, valid versions.
func Entry(kind, mode string, e *store.UpstreamEntry) error {
	if e.Eager && mode != store.ModeMirror { // spec 0032: fetched at every run unasked
		return fmt.Errorf("%w: only a mirror's entry is eager", store.ErrInvalid)
	}
	if e.Name == "*" { // any name: never a reserved name, no versions
		// a pull-through upstream's, or a mirror's of a kista channel, which takes the names the
		// remote lists (spec 0039: the caller checks the remote is a kista)
		if mode != store.ModePullThrough && !(mode == store.ModeMirror && kind == store.UpstreamRepo) || len(e.Versions) > 0 || e.AllowReserved {
			return fmt.Errorf("%w: \"*\" is a pull-through upstream's entry, or a repository mirror's of a kista channel, without versions or allow_reserved",
				store.ErrInvalid)
		}
		if kind == store.UpstreamCore { // every core name is reserved: "*" would take none
			return fmt.Errorf("%w: \"*\" takes no core name; list them", store.ErrInvalid)
		}
		return nil
	}
	// an upstream's names are of its home channel's type, a DuckDB extension's (spec 0034: every upstream is one's)
	if c := reserved.Canonical(pkgtype.DuckDBExtension, e.Name); c != "" {
		return fmt.Errorf("%w: %q is an alias; DuckDB installs it as %q", store.ErrInvalid, e.Name, c)
	}
	if err := release.ValidName(e.Name); err != nil {
		return err
	}
	switch k := reserved.Kind(pkgtype.DuckDBExtension, e.Name); {
	case k == reserved.Core && kind != store.UpstreamCore && !e.AllowReserved,
		k == reserved.Community && kind != store.UpstreamCommunity && !e.AllowReserved:
		return fmt.Errorf("%w: %q is a DuckDB %s name; a %s upstream provides it only with allow_reserved", store.ErrInvalid, e.Name, k, kind)
	}
	for _, v := range e.Versions {
		if err := release.ValidExtVersion(v); err != nil {
			return err
		}
	}
	e.Versions = Dedup(e.Versions)
	return nil
}

// Dedup sorts and drops repeats.
func Dedup(xs []string) []string {
	out := slices.Clone(xs)
	slices.Sort(out)
	return slices.Compact(out)
}

// Covers reports whether a prefix is under one of prefixes (a credential's, spec 0009): the same
// scheme, host and port, and a path at or below it on a segment boundary.
func Covers(prefixes []*url.URL, prefix string) bool {
	u, err := url.Parse(prefix)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		slices.Contains(strings.Split(u.Path, "/"), "..") {
		return false
	}
	for _, p := range prefixes {
		if !strings.EqualFold(u.Scheme, p.Scheme) || !strings.EqualFold(u.Hostname(), p.Hostname()) || port(u) != port(p) {
			continue
		}
		base := strings.TrimSuffix(p.EscapedPath(), "/")
		path := u.EscapedPath()
		if base == "" || path == base || strings.HasPrefix(path, base+"/") {
			return true
		}
	}
	return false
}

func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	return "443"
}
