package api_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/api"
	"github.com/hugr-lab/duckdb-extension-repository/internal/stats"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0036 phase 2: a distributor's downloads per extension per week over every tenant, for
// distribution admins and server administrators; public tenants always counted, the others only
// with the minimum of them in a row, installer-days on their own; and the tenant route's source.
func TestDistributorStatistics(t *testing.T) {
	m := newMgmt(t, func(o *api.Options) { o.PublicTenants = []string{"free", "ghost"} })
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
	r := m.call(t, "GET", D+"?distributor=enterest", m.toks["server dist admin"], "")
	if r.status != 200 || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("a distribution admin: %d %s", r.status, r.body)
	}
	var got struct {
		Distributor string                 `json:"distributor"`
		MinTenants  int                    `json:"min_tenants"`
		Rows        []stats.DistributorRow `json:"rows"`
	}
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatal(err)
	}
	ten := int64(10)
	zero := int64(0)
	w := store.DayOf(week)
	want := []stats.DistributorRow{
		// geo: three non-public tenants downloaded (counted), two had installers (left out); the public
		// tenant's installer-days answered
		{Week: w, Extension: "geo", Downloads: 118, Public: 100, InstallerDays: &ten, Withheld: true},
		// h3: one non-public tenant: left out
		{Week: w, Extension: "h3", Downloads: 7, Public: 7, InstallerDays: &zero, Withheld: true},
	}
	if got.Distributor != "enterest" || got.MinTenants != 3 || len(got.Rows) != 2 || !reflect.DeepEqual(got.Rows[0], want[0]) ||
		!reflect.DeepEqual(got.Rows[1], want[1]) {
		t.Fatalf("the answer: %s", r.body)
	}
	var span map[string]any
	_ = json.Unmarshal(r.body, &span)
	if span["from"] != store.DayOf(week.AddDate(0, 0, -77)) || span["to"] != store.DayOf(week.AddDate(0, 0, 6)) {
		t.Fatalf("the default: the 12 weeks to the last complete one: %v %v", span["from"], span["to"])
	}
	for _, leak := range []string{ids["acme"][0], ids["acme"][1], "acme", "other", "t3", "free"} {
		if strings.Contains(string(r.body), leak) {
			t.Fatalf("an answer names a tenant or a channel (%s): %s", leak, r.body)
		}
	}
	for who, want := range map[string]int{"server admin": 200, "server dist admin": 200, "tenant admin": 404, "server user": 404,
		"server licence mgr": 404, "anonymous": 401, "stale server": 401} {
		if s := m.call(t, "GET", D+"?distributor=enterest&type=duckdb_extension", m.toks[who], "").status; s != want {
			t.Errorf("%s: %d, want %d", who, s, want)
		}
	}
	for q, want := range map[string]int{
		"":                                    400, // no distributor
		"?distributor=enterest&distributor=x": 400,
		"?distributor=enterest&type=nope":     400,
		"?distributor=enterest&from=2026-10-09&to=2026-10-01": 400,
		"?distributor=enterest&from=2020-01-01":               400, // beyond 53 weeks
		"?distributor=nobody":                                 200,
	} {
		if s := m.call(t, "GET", D+q, m.toks["server dist admin"], ""); s.status != want {
			t.Errorf("%q: %d, want %d: %s", q, s.status, want, s.body)
		}
	}
	// one week, by its days: the same rows
	if r := m.call(t, "GET", D+"?distributor=enterest&from="+day+"&to="+day, m.toks["server dist admin"], "").json(t); r["from"] != w ||
		len(r["rows"].([]any)) != 2 {
		t.Fatalf("one week: %v", r)
	}

	// the tenant's own statistics by source: labels, unknown for counts before the source was
	r = m.call(t, "GET", "/api/v1/tenants/acme/stats/downloads?group=source", m.toks["tenant admin"], "")
	by := map[string]float64{}
	for _, row := range r.json(t)["rows"].([]any) {
		x := row.(map[string]any)
		by[x["source"].(string)] = x["count"].(float64)
	}
	if r.status != 200 || len(by) != 3 || by["publisher:enterest-ci"] != 5 || by["tenant"] != 1000 || by["unknown"] != 2000 {
		t.Fatalf("by source: %s", r.body)
	}
}
