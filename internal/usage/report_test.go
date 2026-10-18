package usage_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/usage"
)

func TestParse(t *testing.T) {
	r, err := usage.Parse([]byte(`{"version": "1.4.0", "platform": "linux_amd64", "duckdb_version": "v2.0.0", "license": "lic-1",
		"status": "valid", "future": {"ignored": true},
		"events": [{"name": "load", "count": 1}, {"name": "attach", "count": 12, "counters": {"sources": 30}},
		           {"name": "attach", "count": 1, "counters": {"sources": 2}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	tot := r.Totals()
	if tot[[2]string{"load", ""}] != 1 || tot[[2]string{"attach", ""}] != 13 || tot[[2]string{"attach", "sources"}] != 32 || len(tot) != 3 {
		t.Fatalf("totals: %v", tot)
	}
	if n := r.Names(); strings.Join(n, ",") != "attach,load" {
		t.Fatalf("names: %v", n)
	}
	if _, err := usage.Parse([]byte(`{"version": "1", "platform": "p", "duckdb_version": "v", "status": "missing", "events": []}`)); err != nil {
		t.Fatalf("no events, no licence: %v", err)
	}
	ev := func(events string) string {
		return `{"version": "1", "platform": "p", "duckdb_version": "v", "status": "valid", "events": ` + events + `}`
	}
	many := func(n int, f func(i int) string) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = f(i)
		}
		return strings.Join(parts, ",")
	}
	for _, c := range []struct{ name, body, rule string }{
		{"not JSON", `{`, usage.RuleJSON},
		{"an array", `[]`, usage.RuleJSON},
		{"trailing data", ev(`[]`) + `{}`, usage.RuleJSON},
		{"version missing", `{"platform": "p", "duckdb_version": "v", "status": "valid"}`, usage.RuleVersion},
		{"version not a string", `{"version": 1, "platform": "p", "duckdb_version": "v", "status": "valid"}`, usage.RuleJSON},
		{"status unknown", `{"version": "1", "platform": "p", "duckdb_version": "v", "status": "ok"}`, usage.RuleStatus},
		{"status missing", `{"version": "1", "platform": "p", "duckdb_version": "v"}`, usage.RuleStatus},
		{"licence id", `{"version": "1", "platform": "p", "duckdb_version": "v", "status": "valid", "license": "a b"}`, usage.RuleLicense},
		{"event name", ev(`[{"name": "Load", "count": 1}]`), usage.RuleName},
		{"event name too long", ev(`[{"name": "` + strings.Repeat("a", 65) + `", "count": 1}]`), usage.RuleName},
		{"counter name", ev(`[{"name": "a", "count": 1, "counters": {"1x": 1}}]`), usage.RuleName},
		{"count missing", ev(`[{"name": "a"}]`), usage.RuleValue},
		{"count negative", ev(`[{"name": "a", "count": -1}]`), usage.RuleValue},
		{"count a fraction", ev(`[{"name": "a", "count": 1.5}]`), usage.RuleValue},
		{"count too large", ev(`[{"name": "a", "count": 9007199254740992}]`), usage.RuleValue},
		{"count a string", ev(`[{"name": "a", "count": "1"}]`), usage.RuleValue},
		{"count null", ev(`[{"name": "a", "count": null}]`), usage.RuleValue},
		{"count with an exponent", ev(`[{"name": "a", "count": 1e3}]`), usage.RuleValue},
		{"counter too large", ev(`[{"name": "a", "count": 1, "counters": {"b": 9007199254740992}}]`), usage.RuleValue},
		{"counters not an object", ev(`[{"name": "a", "count": 1, "counters": [1]}]`), usage.RuleJSON},
		{"33 events", ev(`[` + many(33, func(int) string { return `{"name": "a", "count": 1}` }) + `]`), usage.RuleEvents},
		{"17 counters", ev(`[{"name": "a", "count": 1, "counters": {` + many(17, func(i int) string { return `"c` + string(rune('a'+i)) + `": 1` }) + `}}]`), usage.RuleEvents},
		{"over 64 KiB", ev(`[]`) + strings.Repeat(" ", usage.MaxSize), usage.RuleSize},
	} {
		_, err := usage.Parse([]byte(c.body))
		var ue *usage.Error
		if !errors.As(err, &ue) || ue.Rule != c.rule {
			t.Errorf("%s: %v, want rule %s", c.name, err, c.rule)
		}
	}
	// the limits themselves are fine
	if _, err := usage.Parse([]byte(ev(`[` + many(32, func(int) string { return `{"name": "a", "count": 9007199254740991}` }) + `]`))); err != nil {
		t.Fatalf("32 events at the largest count: %v", err)
	}
}
