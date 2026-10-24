package upstream_test

import (
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/usage"
)

// Spec 0012 phase 1c: an upstream set to forward gets the tenant's daily usage of the versions it
// supplied: whole days, no principal, users below 5 left out; 404 means it takes none.
func TestUsageForwarding(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en, tokenFile := privateEnv(t, e)
			en.repo.token = "T-sub"
			if err := os.WriteFile(tokenFile, []byte("T-sub\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
			sp := en.spec(store.UpstreamEntry{Name: "tresor"})
			sp.Credential = "sub"
			u, err := en.up.Add(ctx, admin, "acme", sp)
			if err != nil {
				t.Fatal(err)
			}
			if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
				t.Fatalf("a run: %+v", r)
			}
			if u.Usage != store.UsageForward {
				t.Fatalf("usage defaults to forward: %q", u.Usage)
			}
			tid := en.tenantID(t)
			today := en.st.Now().UTC().Truncate(24 * time.Hour)
			day := func(i int) string { return store.DayOf(today.AddDate(0, 0, -i)) }
			key := func(d, name, event, counter, status string, auth bool) store.UsageKey {
				return store.UsageKey{TenantID: tid, ChannelID: u.ChannelID, Name: name, PackageVersion: "1.0", Event: event, Counter: counter,
					Status: status, Day: d, Authenticated: auth}
			}
			if _, err := en.st.AddUsageCounts(ctx, map[store.UsageKey]int64{
				key(day(2), "tresor", "load", "", "valid", true): 3, key(day(2), "tresor", "load", "", "missing", false): 2,
				key(day(2), "tresor", "attach", "sources", "valid", true): 9, key(day(2), "other", "load", "", "valid", true): 100,
				key(day(3), "tresor", "load", "", "valid", true): 1,
				// the same version counted in a channel without the upstream's build: not its usage
				{TenantID: tid, ChannelID: "elsewhere", Name: "tresor", PackageVersion: "1.0", Event: "load", Status: "valid", Day: day(2)}: 1000}); err != nil {
				t.Fatal(err)
			}
			for d, n := range map[string]int64{day(2): 4, day(3): 2} {
				if err := en.st.PutDaily(ctx, d, store.Daily{Users: map[store.UsageUserKey]int64{
					{TenantID: tid, ChannelID: u.ChannelID, Name: "tresor", PackageVersion: "1.0", Day: d}: n}}); err != nil {
					t.Fatal(err)
				}
			}
			// a downstream kista's aggregate is forwarded too: users 4 + 3
			if err := en.st.PutReceived(ctx, tid, u.ChannelID, "downstream", day(2), true, []store.ReceivedRow{
				{Name: "tresor", PackageVersion: "1.0", Users: 3}, {Name: "tresor", PackageVersion: "1.0", Event: "load", Value: 10}}); err != nil {
				t.Fatal(err)
			}
			fw := &usage.Forwarder{Store: en.st, Post: en.up.Fetch.(usage.Poster), Credentials: en.up.Credentials, Holder: "test",
				Log: slog.New(slog.DiscardHandler)}
			due := func() bool {
				ids, err := en.st.DueUsageForwards(ctx, en.st.Now().Add(-23*time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				return slices.Contains(ids, u.ID)
			}
			if !due() {
				t.Fatal("a credentialed upstream that never forwarded is not due")
			}

			// the upstream takes no usage: every day skipped
			res, err := fw.Forward(ctx, u.ID)
			if err != nil || len(res.Skipped) == 0 || len(res.Sent) != 0 || len(res.Failed) != 0 {
				t.Fatalf("404: %+v %v", res, err)
			}
			if due() {
				t.Fatal("due right after a run")
			}

			en.repo.usage = true
			res, err = fw.Forward(ctx, u.ID)
			if err != nil || len(res.Sent) < 2 || len(res.Failed) != 0 || !slices.Contains(res.Sent, day(2)) {
				t.Fatalf("sent: %+v %v", res, err)
			}
			if a := en.repo.authorizations(); a[len(a)-1] != "Bearer T-sub" {
				t.Fatalf("the Authorization: %q", a[len(a)-1])
			}
			got := map[string]usage.Daily{}
			for _, b := range en.repo.posts {
				if strings.Contains(string(b), "principal") || strings.Contains(string(b), "prod") || strings.Contains(string(b), "downstream") {
					t.Fatalf("a principal, a channel or a sender sent: %s", b)
				}
				var d usage.Daily
				if err := json.Unmarshal(b, &d); err != nil {
					t.Fatal(err)
				}
				if _, err := usage.ParseDaily(b, en.st.Now()); err != nil {
					t.Fatalf("the receiver refuses what the sender sends: %v", err)
				}
				got[d.Day] = d
			}
			d2 := got[day(2)]
			if d2.Part != 1 || d2.Parts != 1 || len(d2.Reports) != 1 || d2.Sender != u.ID {
				t.Fatalf("day 2: %+v", d2)
			}
			r := d2.Reports[0]
			if r.Extension != "tresor" || r.Version != "1.0" || r.Users != 7 || len(r.Events) != 2 || r.Events[0].Name != "attach" ||
				r.Events[0].Counters["sources"] != 9 || r.Events[1].Count != 15 {
				t.Fatalf("day 2's report (all callers and statuses, local and received; another name left out): %+v", r)
			}
			if d3 := got[day(3)]; len(d3.Reports) != 1 || d3.Reports[0].Users != 0 || d3.Reports[0].Events[0].Count != 1 {
				t.Fatalf("day 3: users below 5 left out: %+v", d3)
			}
			if d1, ok := got[day(1)]; ok && len(d1.Reports) != 0 {
				t.Fatalf("an empty day is sent empty: %+v", d1)
			}
			at, last, err := en.st.GetUsageForward(ctx, u.ID)
			if err != nil || at.IsZero() || !strings.Contains(last, day(2)) {
				t.Fatalf("the last run: %v %s %v", at, last, err)
			}
			evs, err := en.st.ListEvents(ctx, tid, store.EventFilter{Kind: "usage.forward", Limit: 10})
			if err != nil || len(evs) != 2 || evs[0].Actor != "system:usage" || evs[0].Subject != "upstream:"+u.Name {
				t.Fatalf("usage.forward events: %+v %v", evs, err)
			}

			// off: never due
			if _, err := en.up.SetUsage(ctx, admin, "acme", u.Name, store.UsageOff, 0); err != nil {
				t.Fatal(err)
			}
			if fws, _ := en.st.UsageForwarders(ctx); len(fws) != 0 {
				t.Fatalf("forwarders while off: %+v", fws)
			}
			en.st.Now = func() time.Time { return time.Now().Add(48 * time.Hour) }
			if due() {
				t.Fatal("an upstream set to off is due")
			}
			if _, err := en.up.SetUsage(ctx, admin, "acme", u.Name, "sometimes", 0); err == nil {
				t.Fatal("usage is forward or off")
			}
			if _, err := en.up.SetUsage(ctx, admin, "acme", u.Name, store.UsageForward, 0); err != nil {
				t.Fatal(err)
			}
			if !due() {
				t.Fatal("forwarding again, a day later: not due")
			}
		})
	}
}
