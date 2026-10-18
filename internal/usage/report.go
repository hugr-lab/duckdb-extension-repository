// Package usage holds spec 0012's usage reports: what an extension's SDK posts to
// <prefix>/-/usage/<name>, its rules, and the counts it adds.
package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"

	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Limits of a report.
const (
	MaxSize     = 64 << 10 // bytes
	MaxEvents   = 32
	MaxCounters = 16 // per event
	MaxValue    = store.MaxCount
)

// The rules a report can break: an Error's Rule, stable for the SDK's tests.
const (
	RuleJSON    = "json"    // not a JSON object of the report's members and types
	RuleStatus  = "status"  // not a licence status, or missing
	RuleLicense = "license" // not a licence id ([A-Za-z0-9_-]{1,64})
	RuleEvents  = "events"  // more than 32 events, or more than 16 counters in one
	RuleName    = "name"    // an event or counter name is not [a-z][a-z0-9_.]{0,63}
	RuleValue   = "value"   // a count or counter value is not an integer within 0..2^53-1
	RuleVersion = "version" // version, platform or duckdb_version missing or not a string
	RuleSize    = "size"    // over 64 KiB
	RuleType    = "content" // not application/json
)

// Error is a report refused: the rule it breaks.
type Error struct{ Rule, Msg string }

func (e *Error) Error() string { return "usage: " + e.Msg }

func refuse(rule, format string, args ...any) error {
	return &Error{Rule: rule, Msg: rule + ": " + fmt.Sprintf(format, args...)}
}

// Statuses are the licence statuses a report may give: a licence's, or missing for none.
var Statuses = []string{string(license.Invalid), string(license.NotCovered), string(license.NotYetValid), string(license.Valid),
	string(license.Grace), string(license.Expired), "missing"}

var (
	nameRe = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
	jtiRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Report is an SDK's usage report.
type Report struct {
	Version       string  `json:"version"`
	Platform      string  `json:"platform"`
	DuckDBVersion string  `json:"duckdb_version"`
	License       string  `json:"license,omitempty"`
	Status        string  `json:"status"`
	Events        []Event `json:"events"`
}

// Event is a report's count of an event, with its counters.
type Event struct {
	Name     string           `json:"name"`
	Count    json.RawMessage  `json:"count"`
	Counters map[string]int64 `json:"-"`
	Raw      json.RawMessage  `json:"counters,omitempty"`
}

// Parse reads a report and checks its rules. Unknown members are ignored (a later SDK may send
// more); an event named twice counts as one, summed.
func Parse(b []byte) (Report, error) {
	var r Report
	if len(b) > MaxSize {
		return r, refuse(RuleSize, "a report is at most %d bytes", MaxSize)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&r); err != nil {
		return r, refuse(RuleJSON, "%v", err)
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return r, refuse(RuleJSON, "one JSON object, nothing after it")
	}
	if r.Version == "" || r.Platform == "" || r.DuckDBVersion == "" {
		return r, refuse(RuleVersion, "version, platform and duckdb_version are required")
	}
	if !slices.Contains(Statuses, r.Status) {
		return r, refuse(RuleStatus, "status is one of %v", Statuses)
	}
	if r.License != "" && !jtiRe.MatchString(r.License) {
		return r, refuse(RuleLicense, "license is a licence id, [A-Za-z0-9_-]{1,64}")
	}
	if len(r.Events) > MaxEvents {
		return r, refuse(RuleEvents, "at most %d events", MaxEvents)
	}
	for i := range r.Events {
		e := &r.Events[i]
		if !nameRe.MatchString(e.Name) {
			return r, refuse(RuleName, "an event name is [a-z][a-z0-9_.]{0,63}")
		}
		if _, err := value(e.Count); err != nil {
			return r, err
		}
		if len(e.Raw) > 0 && string(e.Raw) != "null" {
			var cs map[string]json.RawMessage
			if err := json.Unmarshal(e.Raw, &cs); err != nil {
				return r, refuse(RuleJSON, "counters is an object of integers")
			}
			if len(cs) > MaxCounters {
				return r, refuse(RuleEvents, "at most %d counters in an event", MaxCounters)
			}
			e.Counters = make(map[string]int64, len(cs))
			for k, v := range cs {
				if !nameRe.MatchString(k) {
					return r, refuse(RuleName, "a counter name is [a-z][a-z0-9_.]{0,63}")
				}
				n, err := value(v)
				if err != nil {
					return r, err
				}
				e.Counters[k] = n
			}
		}
	}
	return r, nil
}

// value is a count: a JSON integer within 0..2^53-1 (not a string, not a fraction).
func value(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, refuse(RuleValue, "count is required")
	}
	v, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || v < 0 || v > MaxValue {
		return 0, refuse(RuleValue, "a count is an integer within 0..2^53-1")
	}
	return v, nil
}

// Totals are a report's counts per (event, counter), counter "" for the event's own count; an
// event named twice is summed (capped at 2^53-1).
func (r Report) Totals() map[[2]string]int64 {
	out := map[[2]string]int64{}
	add := func(k [2]string, n int64) { out[k] = min(out[k]+n, MaxValue) }
	for _, e := range r.Events {
		n, _ := value(e.Count)
		add([2]string{e.Name, ""}, n)
		for c, v := range e.Counters {
			add([2]string{e.Name, c}, v)
		}
	}
	return out
}

// Names are a report's event names, sorted, each once.
func (r Report) Names() []string {
	out := make([]string, 0, len(r.Events))
	for _, e := range r.Events {
		out = append(out, e.Name)
	}
	slices.Sort(out)
	return slices.Compact(out)
}
