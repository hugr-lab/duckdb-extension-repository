package buffer_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit/buffer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

func TestPruner(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	for i, age := range []time.Duration{40 * 24 * time.Hour, 20 * 24 * time.Hour, time.Hour} {
		st.Now = func() time.Time { return now.Add(-age).Add(time.Duration(i)) }
		if err := st.InTx(ctx, "", func(tx *store.Tx) error {
			return tx.Event(ctx, "", "os:1:t", "version.add", "version:v", map[string]any{"version": "v"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	st.Now = func() time.Time { return now }
	p := &buffer.Pruner{Store: st, Holder: "test", Retention: 30 * 24 * time.Hour, MaxRowsPerTenant: 1, Log: slog.New(slog.DiscardHandler)}
	p.Once(ctx)
	evs, err := st.ListEvents(ctx, "00000000-0000-0000-0000-000000000000", store.EventFilter{})
	if err != nil || len(evs) != 1 || evs[0].At.Before(now.Add(-2*time.Hour)) {
		t.Fatalf("after a pass: %+v %v", evs, err)
	}
}
