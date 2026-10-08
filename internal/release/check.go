package release

import (
	"fmt"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/symbols"
)

// CheckFile runs spec 0008's checks on a parsed extension file for name: the metadata prefix, the
// binary's format against the footer's platform, and the entry point DuckDB looks up for name with
// the footer's ABI (and no other extension's). Failures are store.ErrInvalid with the reason.
func CheckFile(f *extfile.File, name string) error {
	if !f.HasPrefix {
		return fmt.Errorf("%w: the file has no metadata prefix before its footer (DuckDB's build appends one)", store.ErrInvalid)
	}
	m := f.Metadata
	entry := symbols.EntryPoint(name, m.ABI.String(), m.CAPIVersion)
	if err := symbols.Check(f.ReaderAt(), f.Size(), m.Platform, entry); err != nil {
		return fmt.Errorf("%w: %s", store.ErrInvalid, strings.TrimPrefix(err.Error(), "symbols: "))
	}
	return nil
}
