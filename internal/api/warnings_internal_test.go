package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0035 phase 2: withWarnings adds the warnings to an object answer for a caller who sees
// them, as its last member; it keeps an answer's own "warnings" member, and leaves a non-object, a
// caller who does not see them and an act without warnings as they are.
func TestWithWarnings(t *testing.T) {
	w := store.LicenseWarning{Name: "tresor", Source: "publisher:ci", Channel: "prod", Message: "m"}
	asked := 0 // the caller's rights are read only when there is a warning to answer
	with := func(show bool, ws []store.LicenseWarning) context.Context {
		ctx := store.WithLicenseWarnings(context.Background())
		store.AddLicenseWarnings(ctx, ws)
		return context.WithValue(ctx, answerWarnings{}, func() bool { asked++; return show })
	}
	for _, c := range []struct {
		name string
		show bool
		ws   []store.LicenseWarning
		in   string
		want string
	}{
		{"added", true, []store.LicenseWarning{w}, `{"a":1}`, `{"a":1,"warnings":[{"name":"tresor","source":"publisher:ci","channel":"prod","message":"m"}]}`},
		{"to an empty object", true, []store.LicenseWarning{w}, `{}`, `{"warnings":[{"name":"tresor","source":"publisher:ci","channel":"prod","message":"m"}]}`},
		{"its own member kept", true, []store.LicenseWarning{w}, `{"warnings":["x"]}`, `{"warnings":["x"]}`},
		{"a nested member is not its own", true, []store.LicenseWarning{w}, `{"b":{"warnings":1}}`,
			`{"b":{"warnings":1},"warnings":[{"name":"tresor","source":"publisher:ci","channel":"prod","message":"m"}]}`},
		{"not an object", true, []store.LicenseWarning{w}, `[1]`, `[1]`},
		{"not shown", false, []store.LicenseWarning{w}, `{"a":1}`, `{"a":1}`},
		{"none", true, nil, `{"a":1}`, `{"a":1}`},
	} {
		r := httptest.NewRequest("POST", "/", nil).WithContext(with(c.show, c.ws))
		before := asked
		if got := string(withWarnings(r, []byte(c.in))); got != c.want {
			t.Errorf("%s: %s", c.name, got)
		}
		if c.ws == nil && asked != before {
			t.Errorf("%s: the caller's rights read without a warning", c.name)
		}
	}
}
