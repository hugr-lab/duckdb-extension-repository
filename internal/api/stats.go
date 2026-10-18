package api

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Download statistics (spec 0010 phase 2a): readable with audit on the tenant (everything), or with
// admin, publish or promote on an extension by a token's principal (those names, in the channels
// the grants cover); server administrators read everything.

const (
	maxStatsDays = 366
	maxStatsRows = 100000
)

var (
	statsDims  = []string{"day", "channel", "extension", "version", "platform", "duckdb_version"}
	statsVerbs = []string{store.VerbAdmin, store.VerbPublish, store.VerbPromote}
)

// statsScope is what a caller may read: everything, or the (channel, extension) pairs its grants
// cover, its grants on extension groups included (spec 0017).
type statsScope struct {
	all           bool
	c             caller
	held          auth.Held
	plain, groups []store.Grant // the grants giving statistics
	cache         map[[2]string]bool
}

func (s *statsScope) allow(channel, ext string) bool {
	if s.all {
		return true
	}
	k := [2]string{channel, ext}
	if v, ok := s.cache[k]; ok {
		return v
	}
	res := authz.Resource{Tenant: s.c.tenant.Name, Channel: channel, Extension: ext, Reserved: reserved.Kind(ext) != ""}
	v := slices.ContainsFunc(statsVerbs, func(verb string) bool { return authz.Covers(s.held, verb, res) })
	s.cache[k] = v
	return v
}

// statsGrant reports whether a grant gives statistics: admin, publish or promote (never on an
// issuer-wide grant, which grant validation refuses and authz.Covers ignores).
func statsGrant(g store.Grant) bool {
	return g.Kind != store.PrincipalIssuer && slices.ContainsFunc(statsVerbs, func(v string) bool { return slices.Contains(g.Verbs, v) })
}

// grants are the caller's plain grants that give it statistics, and its grants on groups.
func (s *statsScope) grants() (plain, groups []store.Grant) { return s.plain, s.groups }

// mayAsk reports whether a caller may ask about a channel and an extension ("" for any): some grant
// of its covers them. Anything else answers as missing, so that a query never tells what exists
// beyond the caller's scope.
func (s *statsScope) mayAsk(channel, ext string) bool {
	if s.all {
		return true
	}
	reservedExt := ext != "" && reserved.Kind(ext) != ""
	inChannel := func(g store.Grant) bool { return channel == "" || g.ChannelID == "" || g.ChannelName == channel }
	plain, groups := s.grants()
	return slices.ContainsFunc(plain, func(g store.Grant) bool {
		// a reserved name is reached by publish or promote only through a grant naming it (authz.Covers)
		if reservedExt && g.Extension == "" && !slices.Contains(g.Verbs, store.VerbAdmin) {
			return false
		}
		return inChannel(g) && (ext == "" || g.Extension == "" || g.Extension == ext)
	}) || slices.ContainsFunc(groups, func(g store.Grant) bool {
		// never through a group (spec 0017)
		if reservedExt && !slices.Contains(g.Verbs, store.VerbAdmin) {
			return false
		}
		return inChannel(g) && (ext == "" && len(g.Members) > 0 || g.Members[ext])
	})
}

// maxStatsNames is the most names a scope gives the store to read (a name IN (…) list stays under
// SQL Server's parameter limit); beyond it the store reads every name and keep filters.
const maxStatsNames = 1000

// names are the extensions a scope is limited to, when every grant of it names some (the store then
// reads those only); nil: any.
func (s *statsScope) names() []string {
	if s.all {
		return nil
	}
	set := map[string]bool{}
	plain, groups := s.grants()
	for _, g := range plain {
		if g.Extension == "" {
			return nil
		}
		set[g.Extension] = true
	}
	for _, g := range groups {
		for n := range g.Members {
			set[n] = true
		}
	}
	if len(set) > maxStatsNames {
		return nil
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// statsScopeOf gives a caller's statistics scope (tenantPrincipal admits server administrators and
// the tenant's principals); false when it may read none.
func statsScopeOf(c caller) (*statsScope, bool) {
	held := c.held()
	if c.admin || authz.Covers(held, store.VerbAudit, authz.Resource{Tenant: c.tenant.Name}) {
		return &statsScope{all: true}, true
	}
	s := &statsScope{c: c, held: held, cache: map[[2]string]bool{}}
	for _, g := range held.Grants {
		if statsGrant(g) {
			s.plain = append(s.plain, g)
		}
	}
	for _, g := range held.GroupGrants {
		if statsGrant(g) {
			s.groups = append(s.groups, g)
		}
	}
	return s, len(s.plain)+len(s.groups) > 0
}

// statsRequest is a statistics query: its scope, filter and channel names.
type statsRequest struct {
	scope *statsScope
	f     store.StatsFilter
	names map[string]string // channel id → name
}

// statsRequestOf reads a query's channel and extension (a channel unknown, or beyond the caller's
// scope, is 404), and from and to when ranged (days; default the 30 days to `to`, or to today).
func (h *Handler) statsRequestOf(w http.ResponseWriter, r *http.Request, c caller, ranged bool) (statsRequest, bool) {
	scope, ok := statsScopeOf(c)
	if !ok {
		h.refused(w, r, c)
		return statsRequest{}, false
	}
	q := r.URL.Query()
	channel, ext := q.Get("channel"), q.Get("extension")
	if !scope.mayAsk(channel, ext) {
		h.refused(w, r, c)
		return statsRequest{}, false
	}
	sr := statsRequest{scope: scope, f: store.StatsFilter{Name: ext, Names: scope.names()}, names: map[string]string{}}
	if ranged {
		to := store.DayOf(h.o.Store.Now())
		if v := q.Get("to"); v != "" {
			if _, err := time.Parse(time.DateOnly, v); err != nil {
				problem(w, http.StatusBadRequest, typeInvalid, "to is a day, YYYY-MM-DD")
				return sr, false
			}
			to = v
		}
		end, _ := time.Parse(time.DateOnly, to)
		from := store.DayOf(end.AddDate(0, 0, -29))
		if v := q.Get("from"); v != "" {
			if _, err := time.Parse(time.DateOnly, v); err != nil {
				problem(w, http.StatusBadRequest, typeInvalid, "from is a day, YYYY-MM-DD")
				return sr, false
			}
			from = v
		}
		start, _ := time.Parse(time.DateOnly, from)
		if end.Before(start) || end.Sub(start) >= maxStatsDays*24*time.Hour {
			problem(w, http.StatusBadRequest, typeInvalid, "from..to is at most 366 days, from first")
			return sr, false
		}
		sr.f.From, sr.f.To = from, to
	}
	chs, err := h.o.Store.ListChannels(r.Context(), c.tenant.Name)
	if err != nil {
		h.fail(w, err)
		return sr, false
	}
	for _, ch := range chs {
		sr.names[ch.ID] = ch.Name
		if ch.Name == channel {
			sr.f.ChannelID = ch.ID
		}
	}
	if channel != "" && sr.f.ChannelID == "" {
		notFound(w)
		return sr, false
	}
	return sr, true
}

// keep is the store's row filter for the caller's scope.
func (sr statsRequest) keep(channelID, name string) bool {
	return sr.scope.allow(sr.names[channelID], name)
}

type statsKey struct{ day, channel, extension, version, platform, duckdb string }

type statsRowJSON struct {
	Day           string `json:"day,omitempty"`
	Channel       string `json:"channel,omitempty"`
	Extension     string `json:"extension,omitempty"`
	Version       string `json:"version,omitempty"`
	Platform      string `json:"platform,omitempty"`
	DuckDBVersion string `json:"duckdb_version,omitempty"`
	Count         int64  `json:"count"`
	Authenticated int64  `json:"authenticated"`
	Installers    *int64 `json:"installers,omitempty"` // not by DuckDB version: installs are counted per release
}

// statsDownloads answers GET …/stats/downloads?from=&to=&channel=&extension=&group=d[,d].
func (h *Handler) statsDownloads(w http.ResponseWriter, r *http.Request, c caller, p params) {
	sr, ok := h.statsRequestOf(w, r, c, true)
	if !ok {
		return
	}
	group := []string{"day"}
	if g := r.URL.Query().Get("group"); g != "" {
		group = strings.Split(g, ",")
		for _, d := range group {
			if !slices.Contains(statsDims, d) {
				problem(w, http.StatusBadRequest, typeInvalid, "group takes "+strings.Join(statsDims, ", "))
				return
			}
		}
	}
	has := func(d string) bool { return slices.Contains(group, d) }
	if has("version") && !has("extension") {
		group = append(group, "extension") // a version is an extension's
	}
	rows, err := h.o.Store.DownloadRows(r.Context(), c.tenant.ID, sr.f, maxStatsRows, sr.keep)
	if errors.Is(err, store.ErrInvalid) {
		problem(w, http.StatusBadRequest, typeInvalid, "more than 100,000 rows: narrow the range")
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	byDuckDB := has("duckdb_version")
	pick := func(d, v string) string {
		if has(d) {
			return v
		}
		return ""
	}
	out := map[statsKey]*statsRowJSON{}
	for _, row := range rows {
		if row.Installers > 0 && byDuckDB {
			continue // installers have no DuckDB version
		}
		k := statsKey{day: pick("day", row.Day), channel: pick("channel", sr.names[row.ChannelID]), extension: pick("extension", row.Name),
			version: pick("version", row.ExtVersion), platform: pick("platform", row.Platform), duckdb: pick("duckdb_version", row.DuckDBVersion)}
		o := out[k]
		if o == nil {
			o = &statsRowJSON{Day: k.day, Channel: k.channel, Extension: k.extension, Version: k.version, Platform: k.platform,
				DuckDBVersion: k.duckdb}
			if !byDuckDB {
				o.Installers = new(int64)
			}
			out[k] = o
		}
		o.Count += row.Count
		o.Authenticated += row.Authenticated
		if o.Installers != nil {
			*o.Installers += row.Installers
		}
	}
	list := make([]statsRowJSON, 0, len(out))
	for _, o := range out {
		list = append(list, *o)
	}
	slices.SortFunc(list, func(a, b statsRowJSON) int {
		return cmp.Or(cmp.Compare(a.Day, b.Day), cmp.Compare(a.Channel, b.Channel), cmp.Compare(a.Extension, b.Extension),
			cmp.Compare(a.Version, b.Version), cmp.Compare(a.Platform, b.Platform), cmp.Compare(a.DuckDBVersion, b.DuckDBVersion))
	})
	reply(w, r, http.StatusOK, map[string]any{"from": sr.f.From, "to": sr.f.To, "group": group, "rows": list}, "", noStore)
}

type releaseStatsJSON struct {
	Channel  string `json:"channel"`
	Name     string `json:"extension"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	Last7    int64  `json:"last_7_days"`
	Last30   int64  `json:"last_30_days"`
	LastDay  string `json:"last_download_day"`
}

// statsReleases answers GET …/stats/releases?channel=&extension=: per release downloaded within the
// statistics' retention, its downloads in the last 7 and 30 days and its last download day.
func (h *Handler) statsReleases(w http.ResponseWriter, r *http.Request, c caller, p params) {
	sr, ok := h.statsRequestOf(w, r, c, false)
	if !ok {
		return
	}
	rows, err := h.o.Store.ReleaseStats(r.Context(), c.tenant.ID, store.DayOf(h.o.Store.Now()), sr.f)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := []releaseStatsJSON{}
	for _, row := range rows {
		if !sr.keep(row.ChannelID, row.Name) {
			continue
		}
		out = append(out, releaseStatsJSON{Channel: sr.names[row.ChannelID], Name: row.Name, Version: row.ExtVersion,
			Platform: row.Platform, Last7: row.Last7, Last30: row.Last30, LastDay: row.LastDay})
	}
	slices.SortFunc(out, func(a, b releaseStatsJSON) int {
		return cmp.Or(cmp.Compare(a.Channel, b.Channel), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Version, b.Version),
			cmp.Compare(a.Platform, b.Platform))
	})
	reply(w, r, http.StatusOK, map[string]any{"releases": out}, "", noStore)
}

// refused answers a caller that may read none of what it asked: 404, logged and recorded as refused
// (spec 0010), as decide's refusals.
func (h *Handler) refused(w http.ResponseWriter, r *http.Request, c caller) {
	h.o.Log.Info("api: refused", "actor", c.logName(), "method", r.Method, "path", r.URL.EscapedPath(), "status", http.StatusNotFound,
		"client", client(r))
	note(w, func(sw *statusWriter) { sw.refused, sw.actor = true, c })
	notFound(w)
}

// usageDims are what usage statistics group by: rows are always per event and counter.
var usageDims = []string{"day", "channel", "extension", "version", "status"}

type usageKey struct{ day, channel, extension, version, status, event, counter string }

type usageRowJSON struct {
	Day           string `json:"day,omitempty"`
	Channel       string `json:"channel,omitempty"`
	Extension     string `json:"extension,omitempty"`
	Version       string `json:"version,omitempty"`
	Status        string `json:"status,omitempty"`
	Event         string `json:"event"`
	Counter       string `json:"counter,omitempty"`
	Value         int64  `json:"value"`
	Authenticated int64  `json:"authenticated"`
}

type usageUsersJSON struct {
	Day       string `json:"day,omitempty"`
	Channel   string `json:"channel,omitempty"`
	Extension string `json:"extension,omitempty"`
	Version   string `json:"version,omitempty"`
	Status    string `json:"status,omitempty"` // grouped by status: "received" for a downstream's, else none
	Users     int64  `json:"users"`
}

// statsUsage answers GET …/stats/usage?from=&to=&channel=&extension=&group=d[,d] (spec 0012 phase
// 1b): the usage reports' counts per event and counter, and the days' users (summed over the kept
// rows: a principal on two days counts twice), with download statistics' readers and scopes.
func (h *Handler) statsUsage(w http.ResponseWriter, r *http.Request, c caller, p params) {
	sr, ok := h.statsRequestOf(w, r, c, true)
	if !ok {
		return
	}
	group := []string{"day"}
	if g := r.URL.Query().Get("group"); g != "" {
		group = strings.Split(g, ",")
		for _, d := range group {
			if !slices.Contains(usageDims, d) {
				problem(w, http.StatusBadRequest, typeInvalid, "group takes "+strings.Join(usageDims, ", "))
				return
			}
		}
	}
	has := func(d string) bool { return slices.Contains(group, d) }
	if has("version") && !has("extension") {
		group = append(group, "extension") // a version is an extension's
	}
	rows, err := h.o.Store.UsageRows(r.Context(), c.tenant.ID, sr.f, maxStatsRows, sr.keep)
	if errors.Is(err, store.ErrInvalid) {
		problem(w, http.StatusBadRequest, typeInvalid, "more than 100,000 rows: narrow the range")
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	pick := func(d, v string) string {
		if has(d) {
			return v
		}
		return ""
	}
	counts, users := map[usageKey]*usageRowJSON{}, map[usageKey]*usageUsersJSON{}
	for _, row := range rows {
		k := usageKey{day: pick("day", row.Day), channel: pick("channel", sr.names[row.ChannelID]), extension: pick("extension", row.Name),
			version: pick("version", row.ExtVersion)}
		if row.Event == "" { // a day's users: this kista's (any status), or received
			k.status = pick("status", row.Status)
			o := users[k]
			if o == nil {
				o = &usageUsersJSON{Day: k.day, Channel: k.channel, Extension: k.extension, Version: k.version, Status: k.status}
				users[k] = o
			}
			o.Users = addCount(o.Users, row.Users)
			continue
		}
		k.status, k.event, k.counter = pick("status", row.Status), row.Event, row.Counter
		o := counts[k]
		if o == nil {
			o = &usageRowJSON{Day: k.day, Channel: k.channel, Extension: k.extension, Version: k.version, Status: k.status,
				Event: k.event, Counter: k.counter}
			counts[k] = o
		}
		o.Value = addCount(o.Value, row.Value)
		o.Authenticated = addCount(o.Authenticated, row.Authenticated)
	}
	rowList := make([]usageRowJSON, 0, len(counts))
	for _, o := range counts {
		rowList = append(rowList, *o)
	}
	slices.SortFunc(rowList, func(a, b usageRowJSON) int {
		return cmp.Or(cmp.Compare(a.Day, b.Day), cmp.Compare(a.Channel, b.Channel), cmp.Compare(a.Extension, b.Extension),
			cmp.Compare(a.Version, b.Version), cmp.Compare(a.Status, b.Status), cmp.Compare(a.Event, b.Event), cmp.Compare(a.Counter, b.Counter))
	})
	userList := make([]usageUsersJSON, 0, len(users))
	for _, o := range users {
		userList = append(userList, *o)
	}
	slices.SortFunc(userList, func(a, b usageUsersJSON) int {
		return cmp.Or(cmp.Compare(a.Day, b.Day), cmp.Compare(a.Channel, b.Channel), cmp.Compare(a.Extension, b.Extension),
			cmp.Compare(a.Version, b.Version), cmp.Compare(a.Status, b.Status))
	})
	reply(w, r, http.StatusOK, map[string]any{"from": sr.f.From, "to": sr.f.To, "group": group, "rows": rowList, "users": userList}, "", noStore)
}

// addCount adds stored counts (each within 0..store.MaxCount), stopping at store.MaxCount: clients'
// claims summed over many rows never overflow.
func addCount(a, b int64) int64 { return min(a+b, store.MaxCount) }
