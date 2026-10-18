package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// Limits of a daily aggregate (spec 0012 phase 1c).
const (
	MaxDailySize    = 1 << 20 // bytes
	MaxDailyReports = 1000    // reports a request sends
	MaxDailyKeys    = 20000   // counts a request holds: events, counters and users
	MaxReportKeys   = 1024    // counts one version's report holds
	MaxParts        = 1000
	DailyWindow     = 7 // the finished days a receiver takes
	MinUsers        = 5 // users below are left out of a forwarded report
	RuleDay         = "day"
	RuleReports     = "reports"
)

// Daily is a kista's usage of one day, forwarded upstream: POST <prefix>/-/usage. A day is sent
// whole, in parts when it is large; the first part clears what the receiver had for the day from
// this sender, and each part replaces the (extension, version) pairs it carries.
type Daily struct {
	Day string `json:"day"`
	// Sender tells apart the forwarders behind one credential (a kista's upstreams sharing it): the
	// receiver keeps each (principal, sender) apart. Optional, [A-Za-z0-9_-]{1,64}.
	Sender  string        `json:"sender,omitempty"`
	Reports []DailyReport `json:"reports"`
	Part    int           `json:"part,omitempty"`  // 1-based; 0 is 1
	Parts   int           `json:"parts,omitempty"` // 0 is 1
}

// DailyReport is one extension version's counts of the day, summed over the sender's channels.
type DailyReport struct {
	Extension string       `json:"extension"`
	Version   string       `json:"version"`
	Users     int64        `json:"users,omitempty"` // left out below MinUsers
	Events    []DailyEvent `json:"events"`
}

// DailyEvent is an event's count with its counters.
type DailyEvent struct {
	Name     string           `json:"name"`
	Count    int64            `json:"count"`
	Counters map[string]int64 `json:"counters,omitempty"`
}

// Keys is how many counts a report holds: its events, their counters, and its users.
func (r DailyReport) Keys() int {
	n := 1
	for _, e := range r.Events {
		n += 1 + len(e.Counters)
	}
	return n
}

// ParseDaily reads a daily aggregate and checks its rules; today is the receiver's day: day must be
// one of the DailyWindow days before it.
func ParseDaily(b []byte, today time.Time) (Daily, error) {
	var d Daily
	if len(b) > MaxDailySize {
		return d, refuse(RuleSize, "an aggregate is at most 1 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&d); err != nil {
		return d, refuse(RuleJSON, "%v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return d, refuse(RuleJSON, "one JSON object, nothing after it")
	}
	day, err := time.Parse(time.DateOnly, d.Day)
	end := today.UTC().Truncate(24 * time.Hour)
	if err != nil || !day.Before(end) || day.Before(end.AddDate(0, 0, -DailyWindow)) {
		return d, refuse(RuleDay, "day is one of the %d finished days, YYYY-MM-DD", DailyWindow)
	}
	d.Part, d.Parts = max(d.Part, 1), max(d.Parts, 1)
	if d.Part > d.Parts || d.Parts > MaxParts {
		return d, refuse(RuleReports, "part is 1..parts, parts at most %d", MaxParts)
	}
	if d.Sender != "" && !jtiRe.MatchString(d.Sender) {
		return d, refuse(RuleReports, "sender is [A-Za-z0-9_-]{1,64}")
	}
	if len(d.Reports) > MaxDailyReports {
		return d, refuse(RuleReports, "at most %d reports", MaxDailyReports)
	}
	keys := 0
	for _, r := range d.Reports {
		if r.Extension == "" || r.Version == "" || r.Users < 0 || r.Users > MaxValue {
			return d, refuse(RuleReports, "a report names its extension and version; users is 0..2^53-1")
		}
		for _, e := range r.Events {
			if !nameRe.MatchString(e.Name) {
				return d, refuse(RuleName, "an event name is [a-z][a-z0-9_.]{0,63}")
			}
			if e.Count < 0 || e.Count > MaxValue {
				return d, refuse(RuleValue, "a count is an integer within 0..2^53-1")
			}
			for c, v := range e.Counters {
				if !nameRe.MatchString(c) {
					return d, refuse(RuleName, "a counter name is [a-z][a-z0-9_.]{0,63}")
				}
				if v < 0 || v > MaxValue {
					return d, refuse(RuleValue, "a count is an integer within 0..2^53-1")
				}
			}
		}
		if r.Keys() > MaxReportKeys {
			return d, refuse(RuleReports, "at most %d counts in a version's report", MaxReportKeys)
		}
		if keys += r.Keys(); keys > MaxDailyKeys {
			return d, refuse(RuleReports, "at most %d counts in a request", MaxDailyKeys)
		}
	}
	return d, nil
}
