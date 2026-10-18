package usage

import (
	"slices"
	"sync"
)

// Keys bounds what reports can make a replica write: the distinct (event, counter, status) keys of
// each (channel, extension, version) a day. Event names are the client's, so without it one
// anonymous caller inventing names could add rows without end. Versions are the channel's live
// ones, so the map is bounded by the releases served; it starts again each day.
type Keys struct {
	Max int // distinct keys per (channel, extension, version) a day; default 256

	mu   sync.Mutex
	days map[string]map[string]map[[3]string]bool // day → scope → keys; the newest two days
}

// Admit reports whether a report's keys fit within its scope's day (all of them, or none), and
// remembers them.
func (k *Keys) Admit(scope, day string, keys [][3]string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.Max == 0 {
		k.Max = 256
	}
	if k.days == nil {
		k.days = map[string]map[string]map[[3]string]bool{}
	}
	scopes := k.days[day]
	if scopes == nil {
		// a new day: keep the newest two, so a report around midnight is still bounded; an older
		// day (the server's clock went back) is refused
		scopes = map[string]map[[3]string]bool{}
		k.days[day] = scopes
		ds := make([]string, 0, len(k.days))
		for d := range k.days {
			ds = append(ds, d)
		}
		slices.Sort(ds)
		for _, d := range ds[:max(0, len(ds)-2)] {
			delete(k.days, d)
		}
		if k.days[day] == nil {
			return false
		}
	}
	s := scopes[scope]
	added := 0
	for _, key := range keys {
		if !s[key] {
			added++
		}
	}
	if len(s)+added > k.Max {
		return false
	}
	if s == nil {
		s = map[[3]string]bool{}
		scopes[scope] = s
	}
	for _, key := range keys {
		s[key] = true
	}
	return true
}
