// Package pkgtype lists the package types this binary registers (spec 0034): a package is a name of
// one type, and every place that holds a name holds the pair (type, name), directly or through a
// channel. Only duckdb_extension is registered; a later spec adds adbc_driver. A value of a type
// this binary does not register fails closed: refused at every route, and never matched when read
// (a row of it matches no grant, serves nothing, places nothing).
package pkgtype

import (
	"fmt"
	"slices"
	"sync/atomic"
)

const (
	// DuckDBExtension is a DuckDB extension, the type every package had before spec 0034, and the
	// default wherever a type is not named.
	DuckDBExtension = "duckdb_extension"
	// Any is "any type": a grant without a name that covers every type (one encoding everywhere,
	// never NULL).
	Any = "*"
	// Default is the type a request means when it names none.
	Default = DuckDBExtension
)

// Key is a package: its type and name. Maps keyed by a name hold it wherever a second type could
// hold the same name.
type Key struct{ Type, Name string }

// Of is a package of the default type.
func Of(name string) Key { return Key{DuckDBExtension, name} }

var registered atomic.Pointer[[]string]

func init() { registered.Store(&[]string{DuckDBExtension}) }

// Known reports whether t is a registered type ("*" is not one).
func Known(t string) bool { return slices.Contains(*registered.Load(), t) }

// Types lists the registered types.
func Types() []string { return slices.Clone(*registered.Load()) }

// Parse reads a type a request names: "" is the default, a registered type is itself, anything else
// (and "*") is refused.
func Parse(s string) (string, error) {
	if s == "" {
		return Default, nil
	}
	if !Known(s) {
		return "", fmt.Errorf("package type %q is not known (%s)", s, DuckDBExtension)
	}
	return s, nil
}

// ParseOrAny reads a type where "any type" may be meant (a grant without a name): "*" is itself, the
// rest as Parse, with dflt for "".
func ParseOrAny(s, dflt string) (string, error) {
	switch s {
	case "":
		return dflt, nil
	case Any:
		return Any, nil
	}
	return Parse(s)
}

// RegisterForTest registers a second type for a test (spec 0034: names of two types must not
// collide); the returned function unregisters it. Never called outside tests.
func RegisterForTest(t string) func() {
	prev := registered.Load()
	next := append(slices.Clone(*prev), t)
	registered.Store(&next)
	return func() { registered.Store(prev) }
}
