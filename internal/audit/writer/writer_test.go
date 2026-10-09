package writer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/audit/writer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

var ctx = context.Background()

func setup(t *testing.T) (*store.Store, *writer.Writer, store.Tenant, store.Tenant) {
	t.Helper()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var a, b store.Tenant
	for i, tn := range []*store.Tenant{&a, &b} {
		tn.Name = []string{"acme", "beta"}[i]
		if err := st.InTx(ctx, "", func(tx *store.Tx) error { return tx.CreateTenant(ctx, tn) }); err != nil {
			t.Fatal(err)
		}
	}
	return st, &writer.Writer{Store: st, Log: slog.New(slog.DiscardHandler)}, a, b
}

func count(t *testing.T, st *store.Store, tenantID, kind string) []store.Event {
	t.Helper()
	evs, err := st.ListEvents(ctx, tenantID, store.EventFilter{Kind: kind, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func counts(t *testing.T, evs []store.Event) (sum int) {
	t.Helper()
	for _, e := range evs {
		var d struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal([]byte(e.Data), &d); err != nil {
			t.Fatal(err)
		}
		sum += d.Count
	}
	return sum
}

func TestQuota(t *testing.T) {
	st, w, a, b := setup(t)
	for range 1200 {
		e, err := st.NewEvent(ctx, a.ID, "anonymous", "auth.failure", audit.Refused, "server", nil)
		if err != nil {
			t.Fatal(err)
		}
		w.Add(e)
	}
	e, _ := st.NewEvent(ctx, b.ID, "anonymous", "auth.failure", audit.Refused, "server", nil)
	w.Add(e)
	w.Flush(ctx)
	if n := len(count(t, st, a.ID, "auth.failure")); n != 1000 {
		t.Fatalf("a tenant over its quota: %d", n)
	}
	if n := len(count(t, st, b.ID, "auth.failure")); n != 1 {
		t.Fatalf("another tenant is not dropped: %d", n)
	}
	if drops := count(t, st, a.ID, "audit.dropped"); len(drops) != 0 {
		t.Fatalf("drops are written once a minute, not with every flush: %+v", drops)
	}
	w.Close(ctx)
	drops := count(t, st, a.ID, "audit.dropped")
	if len(drops) != 1 {
		t.Fatalf("audit.dropped: %+v", drops)
	}
	var d struct {
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal([]byte(drops[0].Data), &d); err != nil || d.Counts["auth.failure"] != 200 {
		t.Fatalf("the drop counts: %s %v", drops[0].Data, err)
	}
	if n := len(count(t, st, b.ID, "audit.dropped")); n != 0 {
		t.Fatalf("no drops for b: %d", n)
	}
}

// When every queue together is full, the largest loses its oldest: flooded tenants never crowd out
// another one's events.
func TestTotalIsFair(t *testing.T) {
	st, w, a, b := setup(t)
	for i := range 10 {
		for range 1000 {
			e, _ := st.NewEvent(ctx, a.ID, "anonymous", "auth.failure", audit.Refused, "server", nil)
			e.TenantID = fmt.Sprintf("00000000-0000-0000-0000-00000000000%d", i)
			w.Add(e)
		}
	}
	e, _ := st.NewEvent(ctx, b.ID, "anonymous", "auth.failure", audit.Refused, "server", nil)
	w.Add(e)
	w.Close(ctx)
	if n := len(count(t, st, b.ID, "auth.failure")); n != 1 {
		t.Fatalf("a quiet tenant's event: %d", n)
	}
	if n := len(count(t, st, b.ID, "audit.dropped")); n != 0 {
		t.Fatalf("the quiet tenant dropped: %d", n)
	}
}

func TestRefusalRateLimit(t *testing.T) {
	st, w, a, _ := setup(t)
	rctx := audit.WithRequest(ctx, audit.Request{Client: "203.0.113.9"})
	for range 5 {
		w.Refusal(rctx, a.ID, "principal:x", "authz.refused", audit.Refused, "channel:prod", map[string]any{"route": "r", "status": 403})
	}
	w.Refusal(rctx, a.ID, "principal:y", "authz.refused", audit.Refused, "channel:prod", map[string]any{"route": "r", "status": 403})
	w.Refusal(rctx, "", "anonymous", "auth.failure", audit.Refused, "server", map[string]any{"reason": "invalid"})
	w.Flush(ctx)
	if evs := count(t, st, a.ID, "authz.refused"); len(evs) != 2 || counts(t, evs) != 2 {
		t.Fatalf("one a second per actor, the rest suppressed: %+v", evs)
	}
	if n := len(count(t, st, audit.ServerTenant, "auth.failure")); n != 1 {
		t.Fatalf("an unknown tenant's failure is the server's: %d", n)
	}
	// a quiet key's suppressed count is emitted, not lost
	w.Close(ctx)
	if evs := count(t, st, a.ID, "authz.refused"); len(evs) != 3 || counts(t, evs) != 6 {
		t.Fatalf("the trailing count: %+v", evs)
	}
	// a flood of identities, and of addresses inside one network, coalesces per client prefix
	for i := range 300 {
		c := audit.WithRequest(ctx, audit.Request{Client: fmt.Sprintf("2001:db8:7:%x::1", i)})
		w.Refusal(c, a.ID, "principal:"+store.NewID(), "auth.failure", audit.Refused, "server", map[string]any{"reason": "invalid"})
	}
	w.Close(ctx)
	evs := count(t, st, a.ID, "auth.failure")
	if len(evs) > 100 || counts(t, evs) != 300 {
		t.Fatalf("a flood: %d events counting %d", len(evs), counts(t, evs))
	}
}
