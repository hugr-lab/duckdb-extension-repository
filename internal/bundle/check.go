package bundle

import (
	"crypto/rsa"
	"fmt"
	"slices"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
)

// Ref is a cell with its source.
type Ref struct {
	Source *Source
	Cell   Cell
}

// CellsByFile maps each file to the cells it fills, across sources, in manifest order.
func (m *Manifest) CellsByFile() map[string][]Ref {
	out := map[string][]Ref{}
	for i := range m.Sources {
		for _, c := range m.Sources[i].Cells {
			if c.Outcome == Fetched {
				out[c.File] = append(out[c.File], Ref{&m.Sources[i], c})
			}
		}
	}
	return out
}

// CheckOptions says what CheckFile checks besides the manifest's own claims.
type CheckOptions struct {
	// Keys are the keys a source's files verify with (its pinned keys, or DuckDB's).
	Keys func(*Source) []*rsa.PublicKey
	// Versions are the extension versions a cell accepts; none: any.
	Versions func(Ref) []string
	// FileChecks runs spec 0008's checks on the binary (its metadata prefix and entry point).
	FileChecks bool
}

// CheckFile checks an inflated file against every cell it fills: the manifest's body hash, the
// source's signature (with each referencing source's keys), the footer against the cell (an exact
// build names the cell's DuckDB version), the version filter, and with FileChecks spec 0008's
// checks. It returns every problem found; none means the file may fill those cells. The inspect
// command, the import and the export's own check share it.
func CheckFile(ef *extfile.File, refs []Ref, o CheckOptions) []error {
	var errs []error
	verified := map[*Source]bool{}
	checked := map[string]bool{}
	md := ef.Metadata
	exact := md.ABI == extfile.ABICPP || md.ABI == extfile.ABICStructUnstable
	for _, r := range refs {
		c := r.Cell
		at := fmt.Sprintf("%s %s %s", c.DuckDBVersion, c.Platform, c.Name)
		if ef.Hash.String() != c.BodyHash {
			errs = append(errs, fmt.Errorf("%s: the body hash differs from the manifest's", at))
		}
		if _, done := verified[r.Source]; !done {
			var keys []*rsa.PublicKey
			if o.Keys != nil {
				keys = o.Keys(r.Source)
			}
			_, ok := extfile.Verify(ef.Hash, ef.Signature, keys)
			verified[r.Source] = ok
		}
		if !verified[r.Source] {
			errs = append(errs, fmt.Errorf("%s: the signature does not verify with the source's keys", at))
		}
		if md.Platform != c.Platform || exact && md.DuckDBVersion != c.DuckDBVersion {
			errs = append(errs, fmt.Errorf("%s: the footer names %s %s", at, md.Platform, md.DuckDBVersion))
		}
		if o.Versions != nil {
			if vs := o.Versions(r); len(vs) > 0 && !slices.Contains(vs, md.ExtensionVersion) {
				errs = append(errs, fmt.Errorf("%s: version %s is not among %v", at, md.ExtensionVersion, vs))
			}
		}
		if o.FileChecks && !checked[c.Name] {
			checked[c.Name] = true
			if err := release.CheckFile(ef, c.Name); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", at, err))
			}
		}
	}
	return errs
}
