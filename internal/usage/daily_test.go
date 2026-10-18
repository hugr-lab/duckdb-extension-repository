package usage_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/usage"
)

func TestParseDaily(t *testing.T) {
	today := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	ok := `{"day": "2026-10-17", "reports": [{"extension": "tresor", "version": "1.0", "users": 7,
		"events": [{"name": "attach", "count": 3, "counters": {"sources": 9}}]}], "part": 1, "parts": 2}`
	d, err := usage.ParseDaily([]byte(ok), today)
	if err != nil || d.Part != 1 || d.Parts != 2 || d.Reports[0].Keys() != 3 {
		t.Fatalf("%+v %v", d, err)
	}
	if d, err := usage.ParseDaily([]byte(`{"day": "2026-10-11", "reports": []}`), today); err != nil || d.Part != 1 || d.Parts != 1 {
		t.Fatalf("the oldest day, one part: %+v %v", d, err)
	}
	report := func(n int) string {
		return `{"extension": "x", "version": "1", "events": [` + strings.TrimSuffix(strings.Repeat(`{"name": "a", "count": 1},`, n), ",") + `]}`
	}
	for _, c := range []struct{ name, body, rule string }{
		{"today", `{"day": "2026-10-18", "reports": []}`, usage.RuleDay},
		{"8 days ago", `{"day": "2026-10-10", "reports": []}`, usage.RuleDay},
		{"not a day", `{"day": "yesterday", "reports": []}`, usage.RuleDay},
		{"part beyond parts", `{"day": "2026-10-17", "reports": [], "part": 3, "parts": 2}`, usage.RuleReports},
		{"no version", `{"day": "2026-10-17", "reports": [{"extension": "x", "events": []}]}`, usage.RuleReports},
		{"negative users", `{"day": "2026-10-17", "reports": [{"extension": "x", "version": "1", "users": -1}]}`, usage.RuleReports},
		{"a bad event name", `{"day": "2026-10-17", "reports": [{"extension": "x", "version": "1", "events": [{"name": "A", "count": 1}]}]}`, usage.RuleName},
		{"a count too large", `{"day": "2026-10-17", "reports": [{"extension": "x", "version": "1", "events": [{"name": "a", "count": 9007199254740992}]}]}`, usage.RuleValue},
		{"too many keys", `{"day": "2026-10-17", "reports": [` + strings.TrimSuffix(strings.Repeat(report(400)+",", 51), ",") + `]}`, usage.RuleReports},
		{"trailing data", `{"day": "2026-10-17", "reports": []} {}`, usage.RuleJSON},
	} {
		_, err := usage.ParseDaily([]byte(c.body), today)
		var ue *usage.Error
		if !errors.As(err, &ue) || ue.Rule != c.rule {
			t.Errorf("%s: %v, want %s", c.name, err, c.rule)
		}
	}
}
