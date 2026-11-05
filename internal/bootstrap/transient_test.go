package bootstrap

import (
	"fmt"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// Spec 0044: a host off egress.hosts is the configuration's until a restart: never tried again,
// even inside an error that would be (an issuer's fetch, a source's).
func TestTransientHost(t *testing.T) {
	he := &egress.HostError{Host: "keys.example"}
	for name, c := range map[string]struct {
		err  error
		want bool
	}{
		"a host":                  {he, false},
		"ErrHostNotAllowed":       {egress.ErrHostNotAllowed, false},
		"in an issuer's fetch":    {fmt.Errorf("%w: discovery for x: %w", tenants.ErrIssuerFetch, he), false},
		"in a source's fetch":     {fmt.Errorf("%w: %w", upstream.ErrSourceFetch, he), false},
		"an issuer's fetch alone": {fmt.Errorf("%w: discovery for x: %w", tenants.ErrIssuerFetch, egress.ErrConnect), true},
	} {
		if got := transient(c.err); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}
