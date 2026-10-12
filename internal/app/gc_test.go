package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
)

// A stopped pass's event carries a class, never the error's text (spec 0010).
func TestStorageClass(t *testing.T) {
	for err, want := range map[error]string{
		fmt.Errorf("pass: %w", blob.ErrLeaseLost):                         "lease",
		blob.ErrImplausible:                                               "implausible",
		fmt.Errorf("%w: the marker names another domain", blob.ErrDomain): "domain",
		context.Canceled:                                                  "interrupted",
		errors.New("dial tcp db.internal.example:5432: refused"):          "error",
	} {
		if got := storageClass(err); got != want {
			t.Errorf("%v: %s, want %s", err, got, want)
		}
	}
}
