// Package release puts builds into channels (spec 0006): builds, releases, their signatures by the
// channel's keys, the serving key, and the re-signer.
package release

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

var (
	nameRe       = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	extVersionRe = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)
	platformRe   = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
)

// refusedNames are names a client can never request (DuckDB rewrites them to their alias target
// before building the URL; src/main/extension/extension_alias.cpp) and Windows device names.
var refusedNames = map[string]bool{
	"http": true, "https": true, "s3": true, "md": true, "mysql": true, "odbc": true, "postgres": true,
	"sqlite": true, "sqlite3": true, "uc_catalog": true,
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// ValidName checks an extension name.
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%w: extension name %q must match [a-z0-9_]{1,64}", store.ErrInvalid, name)
	}
	if refusedNames[name] {
		return fmt.Errorf("%w: DuckDB never requests %q under that name (an alias or a device name)", store.ErrInvalid, name)
	}
	return nil
}

// ValidExtVersion checks an extension version (a path segment, matched exactly).
func ValidExtVersion(v string) error {
	if !extVersionRe.MatchString(v) || v == "." || v == ".." {
		return fmt.Errorf("%w: extension version %q", store.ErrInvalid, v)
	}
	return nil
}

// ValidPlatform checks a platform; wasm platforms use another layout and are refused.
func ValidPlatform(p string) error {
	if !platformRe.MatchString(p) || strings.HasPrefix(p, "wasm") {
		return fmt.Errorf("%w: platform %q", store.ErrInvalid, p)
	}
	return nil
}

// ValidDuckDBVersion checks a DuckDB version path segment: a release tag or a 10-hex source id (the
// store's grammar for DuckDB versions).
func ValidDuckDBVersion(v string) error { return store.ValidDuckDBVersion(v) }

// BuildFrom makes a Build from a parsed file and a declared name, checking every grammar.
func BuildFrom(f *extfile.File, tenantID, name, actor string) (store.Build, error) {
	m := f.Metadata
	b := store.Build{TenantID: tenantID, Name: name, ExtVersion: m.ExtensionVersion, Platform: m.Platform,
		BodyHash: f.Hash.String(), Origin: store.OriginAdmin, CreatedBy: actor}
	if err := ValidName(name); err != nil {
		return b, err
	}
	if err := ValidExtVersion(b.ExtVersion); err != nil {
		return b, err
	}
	if err := ValidPlatform(b.Platform); err != nil {
		return b, err
	}
	switch m.ABI {
	case extfile.ABICPP, extfile.ABICStructUnstable:
		b.ABI = store.ABICPP
		if m.ABI == extfile.ABICStructUnstable {
			b.ABI = store.ABICStructUnstable
		}
		if err := ValidDuckDBVersion(m.DuckDBVersion); err != nil {
			return b, err
		}
		b.DuckDBVersion = m.DuckDBVersion
	case extfile.ABICStruct:
		c, err := store.ParseCAPI(m.CAPIVersion)
		if err != nil {
			return b, err
		}
		b.ABI, b.CAPI = store.ABICStruct, &c
	default:
		return b, fmt.Errorf("%w: unknown ABI", store.ErrInvalid)
	}
	return b, nil
}
