package api_test

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/stats"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

type distributorRow struct {
	Week          string `json:"week"`
	Extension     string `json:"extension"`
	Downloads     int64  `json:"downloads"`
	Public        int64  `json:"public"`
	InstallerDays *int64 `json:"installer_days"`
	Withheld      bool   `json:"withheld"`
	MinTenants    int    `json:"min_tenants"`
}

// Spec 0036 phase 2: a distributor's downloads per extension per week over every tenant, for
// distribution admins and server administrators, read from the daily pass's snapshots: public
// tenants always counted, the others only with the minimum of them in a row, installer-days on
// their own and never in part; and the tenant route's source.
func TestDistributorStatistics(t *testing.T) {
	m := newMgmt(t)
	week := stats.LastCompleteWeek(m.st.Now())
	day := store.DayOf(week.AddDate(0, 0, 2))
	src := map[string]string{}
	ids := map[string][2]string{} // tenant -> (tenant id, prod's id)
	for _, tn := range []string{"acme", "other", "t3", "free"} {
		if tn == "t3" || tn == "free" {
			if _, err := m.env.ten.CreateTenant(ctx, admin, tn, "", "", ""); err != nil {
				t.Fatal(err)
			}
		}
		if tn != "acme" {
			if _, err := m.env.ten.CreateChannel(ctx, admin, tn, "prod", store.ChannelSigned, ""); err != nil {
				t.Fatal(err)
			}
		}
		p, err := m.env.adm.AddDistributorPublisher(ctx, admin, tn, "enterest-ci", "enterest")
		if err != nil {
			t.Fatal(err)
		}
		src[tn] = store.PublisherSource(p.ID)
		x, _ := m.st.GetTenant(ctx, tn)
		ch, _ := m.st.GetChannel(ctx, tn, "prod")
		ids[tn] = [2]string{x.ID, ch.ID}
	}
	counts := map[store.DownloadKey]int64{}
	installers := map[store.InstallerKey]int64{}
	add := func(tn, source, name string, n, inst int64) {
		counts[store.DownloadKey{TenantID: ids[tn][0], ChannelID: ids[tn][1], Source: source, Name: name, PackageVersion: "1.0",
			Platform: "linux_amd64", DuckDBVersion: "v2.0.0", Day: day}] += n
		if inst > 0 {
			installers[store.InstallerKey{TenantID: ids[tn][0], ChannelID: ids[tn][1], Source: source, Name: name, PackageVersion: "1.0",
				Platform: "linux_amd64"}] = inst
		}
	}
	add("acme", src["acme"], "geo", 5, 1)
	add("acme", store.SourceTenant, "geo", 1000, 3) // the tenant's own build of the name
	add("acme", "", "geo", 2000, 0)                 // counted before the source was
	add("other", src["other"], "geo", 6, 2)
	add("t3", src["t3"], "geo", 7, 0)
	add("t3", src["t3"], "h3", 4, 0)
	add("free", src["free"], "geo", 100, 10)
	add("free", src["free"], "h3", 7, 0)
	if _, err := m.st.AddDownloadCounts(ctx, counts); err != nil {
		t.Fatal(err)
	}
	if err := m.st.PutDaily(ctx, day, store.Daily{Installers: installers}); err != nil {
		t.Fatal(err)
	}
	const D = "/api/v1/distributor-stats/downloads"
	answer := func() []distributorRow {
		t.Helper()
		r := m.call(t, "GET", D+"?distributor=enterest", m.toks["server dist admin"], "")
		var got struct {
			Distributor string           `json:"distributor"`
			Rows        []distributorRow `json:"rows"`
		}
		if r.status != 200 || r.header.Get("Cache-Control") != "no-store" || json.Unmarshal(r.body, &got) != nil || got.Distributor != "enterest" {
			t.Fatalf("a distribution admin: %d %s", r.status, r.body)
		}
		for _, leak := range []string{ids["acme"][0], ids["acme"][1], "acme", "other", "t3", "free"} {
			if strings.Contains(string(r.body), leak) {
				t.Fatalf("an answer names a tenant or a channel (%s): %s", leak, r.body)
			}
		}
		return got.Rows
	}
	// before the daily pass, no week is computed: refused, never answered empty
	if r := m.call(t, "GET", D+"?distributor=enterest", m.toks["server dist admin"], ""); r.status != 400 ||
		r.json(t)["type"] != "urn:kista:problem:not-computed" || !strings.Contains(string(r.body), "no week is computed yet") {
		t.Fatalf("before the daily pass: %d %s", r.status, r.body)
	}
	(&stats.Daily{Store: m.st, Holder: "test", EventRetention: 30 * 24 * time.Hour, Log: slog.New(slog.DiscardHandler),
		PublicTenants: []string{"free", "ghost"}, MinTenants: 3}).Once(ctx)
	w := store.DayOf(week)
	want := []distributorRow{
		// geo: three non-public tenants downloaded (counted); two had installers, so the installer-days
		// are left out whole, the public tenant's too
		{Week: w, Extension: "geo", Downloads: 118, Public: 100, MinTenants: 3},
		// h3: one non-public tenant: withheld, the public part only
		{Week: w, Extension: "h3", Downloads: 7, Public: 7, Withheld: true, MinTenants: 3},
	}
	if rows := answer(); !reflect.DeepEqual(rows, want) {
		t.Fatalf("the answer: %+v", rows)
	}
	var span map[string]any
	_ = json.Unmarshal(m.call(t, "GET", D+"?distributor=enterest", m.toks["server dist admin"], "").body, &span)
	if span["from"] != store.DayOf(week.AddDate(0, 0, -77)) || span["to"] != w {
		t.Fatalf("the default, the 12 weeks to the last complete one, by their Mondays: %v %v", span["from"], span["to"])
	}
	for who, want := range map[string]int{"server admin": 200, "server dist admin": 200, "tenant admin": 404, "server user": 404,
		"server licence mgr": 404, "anonymous": 401, "stale server": 401} {
		if s := m.call(t, "GET", D+"?distributor=enterest&type=duckdb_extension", m.toks[who], "").status; s != want {
			t.Errorf("%s: %d, want %d", who, s, want)
		}
	}
	for q, want := range map[string]string{
		"":                                    "invalid", // no distributor
		"?distributor=enterest&distributor=x": "invalid",
		"?distributor=" + strings.Repeat("d", store.MaxDistributor+1): "invalid",
		"?distributor=enterest&type=nope":                             "invalid",
		"?distributor=enterest&from=2026-10-09&to=2026-10-01":         "invalid",
		"?distributor=enterest&from=2020-01-01":                       "invalid",      // beyond 53 weeks
		"?distributor=enterest&from=2999-01-01":                       "not-computed", // after the newest week computed
		"?distributor=nobody":                                         "",
	} {
		s := m.call(t, "GET", D+q, m.toks["server dist admin"], "")
		if typ, _ := s.json(t)["type"].(string); want == "" && s.status != 200 ||
			want != "" && (s.status != 400 || typ != "urn:kista:problem:"+want) {
			t.Errorf("%q: %d %s, want %q", q, s.status, s.body, want)
		}
	}
	// one week, by its days: the same rows, from and to its Monday
	if r := m.call(t, "GET", D+"?distributor=enterest&from="+day+"&to="+day, m.toks["server dist admin"], "").json(t); r["from"] != w ||
		r["to"] != w || len(r["rows"].([]any)) != 2 {
		t.Fatalf("one week: %v", r)
	}

	// the tenant's own statistics by source: labels, publisher-id:<id> for a removed distributor's
	// publisher (as the catalogue names it), unknown for counts before the source was
	if _, err := m.env.adm.RemovePublisher(ctx, admin, "acme", "enterest-ci"); err != nil {
		t.Fatal(err)
	}
	r := m.call(t, "GET", "/api/v1/tenants/acme/stats/downloads?group=source", m.toks["tenant admin"], "")
	by := map[string]float64{}
	for _, row := range r.json(t)["rows"].([]any) {
		x := row.(map[string]any)
		by[x["source"].(string)] = x["count"].(float64)
	}
	removed := store.SourcePublisherIDPrefix + strings.TrimPrefix(src["acme"], store.SourcePublisherPrefix)
	if r.status != 200 || len(by) != 3 || by[removed] != 5 || by["tenant"] != 1000 || by["unknown"] != 2000 {
		t.Fatalf("by source: %s", r.body)
	}
	if rows := answer(); !reflect.DeepEqual(rows, want) {
		t.Fatalf("a removed publisher's downloads keep counting: %+v", rows)
	}
	// to is the newest week computed, never a complete week not computed yet (answered empty)
	for _, q := range []string{"DELETE FROM distributor_weeks WHERE week = ?", "DELETE FROM distributor_weeks_done WHERE week = ?"} {
		if err := store.ExecRaw(ctx, m.st, q, w); err != nil {
			t.Fatal(err)
		}
	}
	if r := m.call(t, "GET", D+"?distributor=enterest&to="+day, m.toks["server dist admin"], "").json(t); r["to"] != store.DayOf(week.AddDate(0, 0, -7)) {
		t.Fatalf("to clamped to the newest week computed: %v", r)
	}
}
