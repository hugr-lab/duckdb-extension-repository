package upstream

import (
	"log/slog"
	"reflect"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// The API's copy of the service keeps every setting but the authorizer, and shares the runner's
// wake-up. Every exported field of the fixture is set, so a field added to Service and left out
// of WithAuthz fails here.
func TestWithAuthzKeepsEverything(t *testing.T) {
	s := &Service{Store: &store.Store{}, Releases: &release.Service{}, Blob: &blob.Service{}, Fetch: &egress.Client{}, Authz: authz.ServerAdmin{},
		Config: Config{Concurrency: 3}, MaxBody: 5, MaxIngests: 2, TempDir: "/x", Feeds: &attachment.Feeds{},
		Uploads: UploadSettings{ChunkSize: 1 << 20, MaxUploads: 7}, Log: slog.Default(), DuckDB: DuckDB{CoreURL: "https://core.example"},
		Credentials: credential.Registry{"c": nil}}
	sv := reflect.ValueOf(s).Elem()
	for i := 0; i < sv.NumField(); i++ {
		if f := sv.Type().Field(i); f.IsExported() && sv.Field(i).IsZero() {
			t.Fatalf("the fixture leaves %s unset: set it, so the check below means something", f.Name)
		}
	}
	c := s.WithAuthz(authz.ServerAdmin{})
	cv := reflect.ValueOf(c).Elem()
	for i := 0; i < sv.NumField(); i++ {
		f := sv.Type().Field(i)
		if !f.IsExported() || f.Name == "Authz" {
			continue
		}
		if !reflect.DeepEqual(sv.Field(i).Interface(), cv.Field(i).Interface()) {
			t.Errorf("WithAuthz drops %s", f.Name)
		}
	}
	if c.WakeUploads() != s.WakeUploads() {
		t.Error("the copy wakes another channel than the runner's")
	}
}
