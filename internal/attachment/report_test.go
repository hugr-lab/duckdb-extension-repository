package attachment

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

var reportBody = strings.Repeat("a", 64)

// reportDoc is a valid report about reportBody with members replaced or added (a nil value removes
// one); raw appends text before the closing brace, for what a map cannot say (a duplicate).
func reportDoc(over map[string]any, raw string) []byte {
	m := map[string]any{
		"envelope": "kista.report.v1", "report": "rep_1:2026.A-b", "issue": 2, "issued_at": "2026-11-16T10:00:00+01:00", "depth": "static_v1",
		"subject":         map[string]any{"bodies": []any{map[string]any{"hash": reportBody, "read": true}, map[string]any{"hash": strings.Repeat("b", 64), "read": false}}},
		"attention":       []any{map[string]any{"kind": "network", "count": 3}, map[string]any{"kind": "native_code", "count": 0}},
		"kinds_evaluated": 12,
		"sections":        []any{map[string]any{"name": "imports", "status": "analysed"}, map[string]any{"name": "strings", "status": "skipped"}},
		"html":            "<h1>Report</h1><script>alert(1)</script>",
	}
	for k, v := range over {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	b, _ := json.Marshal(m)
	if raw != "" {
		b = append(b[:len(b)-1], []byte(","+raw+"}")...)
	}
	return b
}

func summaryOf(t *testing.T, doc []byte) (map[string]any, error) {
	t.Helper()
	s, err := Summarize(store.KindReport, Report, reportBody, doc)
	if err != nil {
		return nil, err
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(s), &got); err != nil {
		t.Fatal(err)
	}
	return got, nil
}

// The report kind's envelope (spec 0045): the summary, each strict member's type and bound, a
// summary list out of its rule dropped and flagged, a duplicate in another letter case, unknown
// members, a higher envelope version stored alike, a body not listed, the largest summary.
func TestReport(t *testing.T) {
	if mt, err := CanonicalMediaType(store.KindReport, Report); err != nil || mt != "application/vnd.kista.report+json" {
		t.Fatalf("the kind's media type: %q %v", mt, err)
	}
	if !KindRE.MatchString(store.KindReport) || len(Report) > 200 {
		t.Fatal("the kind or its media type is outside a statement's frozen bounds (spec 0046)")
	}
	// a kind added raises the reader level (spec 0046): a feed then takes again, by itself, the
	// reports an older binary left waiting as an unknown kind
	if ReaderLevel < 2 || !slices.Contains(Kinds, store.KindReport) || floors[store.KindReport] == nil {
		t.Fatalf("the reader level %d, the kinds %v", ReaderLevel, Kinds)
	}
	want := map[string]any{"envelope": "kista.report.v1", "report": "rep_1:2026.A-b", "issue": 2.0, "issued_at": "2026-11-16T09:00:00Z",
		"depth": "static_v1", "bodies": 2.0, "read": true, "kinds_evaluated": 12.0,
		"attention": []any{map[string]any{"kind": "network", "count": 3.0}, map[string]any{"kind": "native_code", "count": 0.0}},
		"sections":  []any{map[string]any{"name": "imports", "status": "analysed"}, map[string]any{"name": "strings", "status": "skipped"}}}
	got, err := summaryOf(t, reportDoc(nil, ""))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("the summary:\n got %v\nwant %v (%v)", got, want, err)
	}

	// the optional members absent; attention absent is an empty list, never "incomplete"
	got, err = summaryOf(t, reportDoc(map[string]any{"attention": nil, "kinds_evaluated": nil, "sections": nil}, ""))
	if err != nil || got["incomplete"] != nil || got["kinds_evaluated"] != nil || got["sections"] != nil || !reflect.DeepEqual(got["attention"], []any{}) {
		t.Fatalf("without the optional members: %v %v", got, err)
	}

	// unknown members, at every level, are carried unread; the html is never looked into
	same := func(name string, doc []byte) {
		t.Helper()
		if got, err := summaryOf(t, doc); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %v\nwant %v (%v)", name, got, want, err)
		}
	}
	same("unknown members", reportDoc(map[string]any{"verdict": "approved", "findings": []any{1, "x", nil}, "Later": map[string]any{"html": 1}}, ""))
	same("an unknown member in an entry", reportDoc(map[string]any{
		"attention": []any{map[string]any{"kind": "network", "count": 3, "detail": "x"}, map[string]any{"kind": "native_code", "count": 0}}}, ""))
	same("html of another shape of markup", reportDoc(map[string]any{"html": "<<<\u0000\u202e not html at all"}, ""))
	// a member that only looks like a read one (another case, alone) is not read
	if got, err := summaryOf(t, reportDoc(map[string]any{"kinds_evaluated": nil, "Kinds_Evaluated": 3}, "")); err != nil || got["kinds_evaluated"] != nil {
		t.Errorf("a member in another case was read: %v %v", got, err)
	}

	// a higher envelope version is stored and summarised alike, at an attach and from a feed: a
	// report never waits on a document version
	for _, v := range []string{"kista.report.v2", "kista.report.v2147483648"} {
		later := maps.Clone(want)
		later["envelope"] = v
		doc := reportDoc(map[string]any{"envelope": v, "future": map[string]any{"a": []any{1}}}, "")
		if got, err := summaryOf(t, doc); err != nil || !reflect.DeepEqual(got, later) {
			t.Errorf("%s attached: %v %v", v, got, err)
		}
		if s, err := SummarizeReceived(store.KindReport, Report, reportBody, doc); err != nil || !strings.Contains(s, v) {
			t.Errorf("%s received: %s %v", v, s, err)
		}
	}

	// the strict members: a document that breaks one is refused, never a version to wait on
	long := strings.Repeat("x", 129)
	manyBodies := make([]any, 1001)
	for i := range manyBodies {
		manyBodies[i] = map[string]any{"hash": fmt.Sprintf("%064x", i), "read": true}
	}
	bodies := func(entries ...any) map[string]any { return map[string]any{"bodies": entries} }
	entry := map[string]any{"hash": reportBody, "read": true}
	for name, doc := range map[string][]byte{
		"not an object":                  []byte(`[1]`),
		"not JSON":                       []byte(`{"envelope":`),
		"no envelope":                    reportDoc(map[string]any{"envelope": nil}, ""),
		"envelope a number":              reportDoc(map[string]any{"envelope": 1}, ""),
		"envelope of another format":     reportDoc(map[string]any{"envelope": "kista.review.v1"}, ""),
		"envelope v0":                    reportDoc(map[string]any{"envelope": "kista.report.v0"}, ""),
		"envelope v01":                   reportDoc(map[string]any{"envelope": "kista.report.v01"}, ""),
		"envelope over 2^31":             reportDoc(map[string]any{"envelope": "kista.report.v2147483649"}, ""),
		"envelope with a suffix":         reportDoc(map[string]any{"envelope": "kista.report.v1\n"}, ""),
		"no report id":                   reportDoc(map[string]any{"report": nil}, ""),
		"report id empty":                reportDoc(map[string]any{"report": ""}, ""),
		"report id too long":             reportDoc(map[string]any{"report": long}, ""),
		"report id with a space":         reportDoc(map[string]any{"report": "a b"}, ""),
		"report id with markup":          reportDoc(map[string]any{"report": "<b>"}, ""),
		"report id a number":             reportDoc(map[string]any{"report": 7}, ""),
		"no issue":                       reportDoc(map[string]any{"issue": nil}, ""),
		"issue a string":                 reportDoc(map[string]any{"issue": "2"}, ""),
		"issue negative":                 reportDoc(map[string]any{"issue": -1}, ""),
		"issue a fraction":               reportDoc(map[string]any{"issue": nil}, `"issue":2.0`),
		"issue an exponent":              reportDoc(map[string]any{"issue": nil}, `"issue":1e3`),
		"issue over 2^31":                reportDoc(map[string]any{"issue": 2147483649}, ""),
		"no issued_at":                   reportDoc(map[string]any{"issued_at": nil}, ""),
		"issued_at not RFC 3339":         reportDoc(map[string]any{"issued_at": "16 Nov 2026"}, ""),
		"issued_at a number":             reportDoc(map[string]any{"issued_at": 1700000000}, ""),
		"no depth":                       reportDoc(map[string]any{"depth": nil}, ""),
		"depth upper case":               reportDoc(map[string]any{"depth": "Static"}, ""),
		"depth too long":                 reportDoc(map[string]any{"depth": strings.Repeat("d", 33)}, ""),
		"depth empty":                    reportDoc(map[string]any{"depth": ""}, ""),
		"no subject":                     reportDoc(map[string]any{"subject": nil}, ""),
		"subject a list":                 reportDoc(map[string]any{"subject": []any{}}, ""),
		"no bodies":                      reportDoc(map[string]any{"subject": map[string]any{}}, ""),
		"bodies empty":                   reportDoc(map[string]any{"subject": bodies()}, ""),
		"bodies over 1000":               reportDoc(map[string]any{"subject": bodies(append(manyBodies, entry)...)}, ""),
		"a body's hash upper case":       reportDoc(map[string]any{"subject": bodies(entry, map[string]any{"hash": strings.Repeat("A", 64), "read": true})}, ""),
		"a body's hash short":            reportDoc(map[string]any{"subject": bodies(entry, map[string]any{"hash": "abc", "read": true})}, ""),
		"a body's read a string":         reportDoc(map[string]any{"subject": bodies(map[string]any{"hash": reportBody, "read": "true"})}, ""),
		"a body without read":            reportDoc(map[string]any{"subject": bodies(map[string]any{"hash": reportBody})}, ""),
		"a body entry a string":          reportDoc(map[string]any{"subject": bodies(reportBody)}, ""),
		"the body not listed":            reportDoc(map[string]any{"subject": bodies(map[string]any{"hash": strings.Repeat("c", 64), "read": true})}, ""),
		"no html":                        reportDoc(map[string]any{"html": nil}, ""),
		"html a list":                    reportDoc(map[string]any{"html": []any{"<p>"}}, ""),
		"html null":                      reportDoc(map[string]any{"html": nil}, `"html":null`),
		"html an object":                 reportDoc(map[string]any{"html": map[string]any{"body": "<p>"}}, ""),
		"envelope twice":                 reportDoc(nil, `"envelope":"kista.report.v1"`),
		"html twice in another case":     reportDoc(nil, `"HTML":"<p>the other page</p>"`),
		"report twice, mixed case":       reportDoc(nil, `"Report":"other"`),
		"kinds_evaluated twice (Kelvin)": reportDoc(map[string]any{"kinds_evaluated": nil}, `"\u212ainds_evaluated":1,"kinds_evaluated":2`),
		"subject twice":                  reportDoc(nil, `"Subject":{"bodies":[]}`),
		"attention twice":                reportDoc(nil, `"ATTENTION":[]`),
		"sections twice":                 reportDoc(nil, `"sections":[]`),
		"bodies twice in the subject":    reportDoc(map[string]any{"subject": nil}, `"subject":{"bodies":[{"hash":"`+reportBody+`","read":true}],"Bodies":[]}`),
		"hash twice in a body":           reportDoc(map[string]any{"subject": nil}, `"subject":{"bodies":[{"hash":"`+reportBody+`","read":true,"Hash":"x"}]}`),
		"read twice in a body":           reportDoc(map[string]any{"subject": nil}, `"subject":{"bodies":[{"hash":"`+reportBody+`","read":false,"read":true}]}`),
		"nested too deep":                reportDoc(map[string]any{"x": json.RawMessage(strings.Repeat("[", 40) + strings.Repeat("]", 40))}, ""),
	} {
		for _, received := range []bool{false, true} {
			_, err := summarize(store.KindReport, Report, reportBody, doc, received)
			if !errors.Is(err, ErrDocument) || errors.Is(err, ErrDocumentVersion) {
				t.Errorf("%s (received %v): %v", name, received, err)
			}
		}
	}
	// without a body to check against, no report passes
	if _, err := Summarize(store.KindReport, Report, "", reportDoc(nil, "")); !errors.Is(err, ErrDocument) {
		t.Errorf("a report summarised for no body: %v", err)
	}

	// read: this body's; a body listed twice was read only if every entry says so
	for name, c := range map[string]struct {
		bodies []any
		read   bool
	}{
		"not read":           {[]any{map[string]any{"hash": reportBody, "read": false}}, false},
		"twice, one unread":  {[]any{entry, map[string]any{"hash": reportBody, "read": false}}, false},
		"twice, both read":   {[]any{entry, entry}, true},
		"another's not read": {[]any{entry, map[string]any{"hash": strings.Repeat("b", 64), "read": false}}, true},
	} {
		got, err := summaryOf(t, reportDoc(map[string]any{"subject": bodies(c.bodies...)}, ""))
		if err != nil || got["read"] != c.read || got["bodies"] != float64(len(c.bodies)) {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}

	// a summary list that breaks its rule does not lose the report: the member is left out and the
	// summary flagged; the other members stay
	att := func(entries ...any) map[string]any { return map[string]any{"attention": entries} }
	many := make([]any, 33)
	for i := range many {
		many[i] = map[string]any{"kind": fmt.Sprintf("k%d", i), "count": i}
	}
	for name, c := range map[string]struct {
		over    map[string]any
		raw     string
		dropped string
	}{
		"attention not a list":            {map[string]any{"attention": "none"}, "", "attention"},
		"attention null":                  {map[string]any{"attention": nil}, `"attention":null`, "attention"},
		"attention over 32":               {att(many...), "", "attention"},
		"an attention kind upper case":    {att(map[string]any{"kind": "Network", "count": 1}), "", "attention"},
		"an attention kind with markup":   {att(map[string]any{"kind": "<img src=x>", "count": 1}), "", "attention"},
		"an attention kind too long":      {att(map[string]any{"kind": strings.Repeat("k", 33), "count": 1}), "", "attention"},
		"an attention count negative":     {att(map[string]any{"kind": "network", "count": -1}), "", "attention"},
		"an attention count over 2^31":    {att(map[string]any{"kind": "network", "count": 2147483649}), "", "attention"},
		"an attention count a string":     {att(map[string]any{"kind": "network", "count": "3"}), "", "attention"},
		"an attention entry without one":  {att(map[string]any{"kind": "network"}), "", "attention"},
		"an attention entry a string":     {att("network"), "", "attention"},
		"an attention kind twice":         {map[string]any{"attention": nil}, `"attention":[{"kind":"a","Kind":"b","count":1}]`, "attention"},
		"kinds_evaluated a list":          {map[string]any{"kinds_evaluated": []any{"network"}}, "", "kinds_evaluated"},
		"kinds_evaluated negative":        {map[string]any{"kinds_evaluated": -1}, "", "kinds_evaluated"},
		"kinds_evaluated a fraction":      {map[string]any{"kinds_evaluated": nil}, `"kinds_evaluated":1.5`, "kinds_evaluated"},
		"kinds_evaluated over 2^31":       {map[string]any{"kinds_evaluated": 2147483649}, "", "kinds_evaluated"},
		"sections over 16":                {map[string]any{"sections": make([]any, 17)}, "", "sections"},
		"a section's status with a space": {map[string]any{"sections": []any{map[string]any{"name": "imports", "status": "not analysed"}}}, "", "sections"},
		"a section without a status":      {map[string]any{"sections": []any{map[string]any{"name": "imports"}}}, "", "sections"},
		"sections an object":              {map[string]any{"sections": map[string]any{"imports": "analysed"}}, "", "sections"},
	} {
		got, err := summaryOf(t, reportDoc(c.over, c.raw))
		if err != nil || got["incomplete"] != true || got[c.dropped] != nil || got["report"] != "rep_1:2026.A-b" || got["depth"] != "static_v1" {
			t.Errorf("%s: %v %v", name, got, err)
			continue
		}
		for _, kept := range []string{"attention", "kinds_evaluated", "sections"} {
			if kept != c.dropped && got[kept] == nil {
				t.Errorf("%s: %s went with %s: %v", name, kept, c.dropped, got)
			}
		}
	}

	// the largest summary fits: every member at its bound, with no cut rule
	word := strings.Repeat("w", 32)
	var attention, sections []any
	for range 32 {
		attention = append(attention, map[string]any{"kind": word, "count": 2147483648})
	}
	for range 16 {
		sections = append(sections, map[string]any{"name": word, "status": word})
	}
	largest := reportDoc(map[string]any{"envelope": "kista.report.v2147483648", "report": strings.Repeat("R", 128), "issue": 2147483648,
		"issued_at": "2026-11-16T23:59:59.999999999+14:00", "depth": word, "attention": attention, "kinds_evaluated": 2147483648,
		"sections": sections, "subject": bodies(append(manyBodies[:999], map[string]any{"hash": reportBody, "read": false})...),
		"attention_incomplete_too": true}, "")
	s, err := Summarize(store.KindReport, Report, reportBody, largest)
	if err != nil || len(s) > 4000 || len(s) < 3000 {
		t.Fatalf("the largest summary: %d bytes, %v", len(s), err)
	}
	// and with the flag too
	s, err = Summarize(store.KindReport, Report, reportBody, reportDoc(map[string]any{"envelope": "kista.report.v2147483648",
		"report": strings.Repeat("R", 128), "issue": 2147483648, "depth": word, "attention": attention, "kinds_evaluated": "x", "sections": sections}, ""))
	if err != nil || len(s) > 4000 || !strings.Contains(s, `"incomplete":true`) {
		t.Fatalf("the largest flagged summary: %d bytes, %v", len(s), err)
	}
}

// The vulnerabilities rules (spec 0045): info and none are informational, outside findings; a
// CycloneDX document's components are counted, checked only when marked so exactly once.
func TestVulnerabilitiesSummary(t *testing.T) {
	sum := func(mt, doc string) map[string]any {
		t.Helper()
		s, err := Summarize(store.KindVulnerabilities, mt, "", []byte(doc))
		if err != nil {
			t.Fatalf("%s: %v", doc, err)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(s), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	prop := func(values ...string) string {
		var ps []string
		for _, v := range values {
			ps = append(ps, `{"name":"kista:vulnerabilities:check","value":"`+v+`"}`)
		}
		return `"properties":[{"name":"other","value":"checked"},` + strings.Join(ps, ",") + `]`
	}
	got := sum(CycloneDX, `{"bomFormat":"CycloneDX","specVersion":"1.6","components":[
{`+prop("checked")+`,"components":[{`+prop("not_checked")+`},{"name":"unmarked"}]},
{`+prop("checked", "checked")+`},{`+prop("Checked")+`},{`+prop("checked")+`},{"properties":[{"name":"Kista:Vulnerabilities:Check","value":"checked"}]}],
"vulnerabilities":[{"ratings":[{"severity":"info"}]},{"ratings":[{"severity":"none"},{"severity":"low"}]},{"ratings":[{"severity":"none"}]},{},
{"ratings":[{"severity":"high"}]}]}`)
	want := map[string]any{"findings": 3.0, "high": 1.0, "low": 1.0, "unknown": 1.0, "informational": 2.0, "components": 7.0, "checked": 2.0, "not_checked": 1.0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("a CycloneDX document:\n got %v\nwant %v", got, want)
	}
	// a document that checked no component: its components say so, whatever its findings
	got = sum(CycloneDX, `{"bomFormat":"CycloneDX","components":[{"name":"a"},{"name":"b"}],"vulnerabilities":[]}`)
	if got["findings"] != 0.0 || got["components"] != 2.0 || got["checked"] != 0.0 || got["not_checked"] != 0.0 {
		t.Fatalf("nothing checked: %v", got)
	}
	// SARIF: none is informational, note stays low
	got = sum(SARIF, `{"version":"2.1.0","runs":[{"results":[{"level":"none"},{"level":"note"},{"level":"error"}]}]}`)
	if got["findings"] != 2.0 || got["informational"] != 1.0 || got["low"] != 1.0 || got["high"] != 1.0 || got["components"] != nil {
		t.Fatalf("SARIF: %v", got)
	}
	// a review is unchanged: its info findings stay low, inside findings
	s, err := Summarize(store.KindReview, Review, "", []byte(`{"schema":"kista.review.v1","verdict":"approved","analysed_at":"2026-10-03T12:00:00Z",
"analyser":"x","findings":[{"severity":"info","title":"t"}]}`))
	if err != nil || !strings.Contains(s, `"low":1`) || !strings.Contains(s, `"findings":1`) || strings.Contains(s, "informational") {
		t.Fatalf("a review: %s %v", s, err)
	}
}
