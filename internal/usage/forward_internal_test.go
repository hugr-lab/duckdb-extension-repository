package usage

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// A day is split into parts within the receiver's limits, a pair never split; a version's report
// is cut to MaxReportKeys; a version with nothing to say is left out.
func TestForwardParts(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tn := store.Tenant{Name: "acme"}
	if err := st.InTx(ctx, "", func(tx *store.Tx) error { return tx.CreateTenant(ctx, &tn) }); err != nil {
		t.Fatal(err)
	}
	const day = "2026-10-09"
	counts := map[store.UsageKey]int64{}
	versions := map[[2]string]map[string]bool{}
	for i := range 5 {
		v := "1." + strconv.Itoa(i)
		versions[[2]string{"x", v}] = map[string]bool{"ch": true}
		counts[store.UsageKey{TenantID: tn.ID, ChannelID: "ch", Name: "x", ExtVersion: v, Event: "load", Status: "valid", Day: day}] = 1
	}
	// 9.9 counted in another channel only: not where this upstream's build is
	counts[store.UsageKey{TenantID: tn.ID, ChannelID: "other", Name: "x", ExtVersion: "9.9", Event: "load", Status: "valid", Day: day}] = 1
	versions[[2]string{"x", "9.9"}] = map[string]bool{"ch": true}
	// 1.4 has more counters than a report holds
	for c := range MaxReportKeys + 10 {
		counts[store.UsageKey{TenantID: tn.ID, ChannelID: "ch", Name: "x", ExtVersion: "1.4", Event: "load", Counter: "c" + strconv.Itoa(c),
			Status: "valid", Day: day}] = 1
	}
	// 1.5 has users below 5 only: left out
	versions[[2]string{"x", "1.5"}] = map[string]bool{"ch": true}
	if _, err := st.AddUsageCounts(ctx, counts); err != nil {
		t.Fatal(err)
	}
	if err := st.PutDaily(ctx, day, store.Daily{Users: map[store.UsageUserKey]int64{
		{TenantID: tn.ID, ChannelID: "ch", Name: "x", ExtVersion: "1.5", Day: day}: 3}}); err != nil {
		t.Fatal(err)
	}
	f := &Forwarder{Store: st, PartReports: 2}
	parts, err := f.day(ctx, tn.ID, day, versions)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 || parts[0].Part != 1 || parts[2].Part != 3 || parts[2].Parts != 3 {
		t.Fatalf("parts: %d %+v", len(parts), parts)
	}
	n := 0
	for _, p := range parts {
		for _, r := range p.Reports {
			n++
			if r.Version == "9.9" || r.Version == "1.5" {
				t.Fatalf("a report that should be left out: %+v", r)
			}
			if r.Keys() > MaxReportKeys {
				t.Fatalf("%s: %d keys", r.Version, r.Keys())
			}
			if r.Version == "1.4" && r.Keys() != MaxReportKeys {
				t.Fatalf("1.4 cut to the cap: %d", r.Keys())
			}
		}
	}
	if n != 5 {
		t.Fatalf("reports: %d", n)
	}
	// an empty day is one empty part
	if parts, err := f.day(ctx, tn.ID, "2026-10-08", versions); err != nil || len(parts) != 1 || len(parts[0].Reports) != 0 || parts[0].Parts != 1 {
		t.Fatalf("an empty day: %+v %v", parts, err)
	}
}
