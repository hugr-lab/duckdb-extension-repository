package app

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0036 phase 2: the statistics settings reach the daily pass, which applies the public tenants
// and the minimum to the distributors' weekly snapshots.
func TestNewDaily(t *testing.T) {
	ss := config.Config{Statistics: config.Statistics{DistributorMinTenants: 4, PublicTenants: []string{"free"}}}.StatisticsSettings()
	d := newDaily(ss, nil, "host/1", 48*time.Hour, slog.New(slog.DiscardHandler))
	if d.MinTenants != 4 || !slices.Equal(d.PublicTenants, []string{"free"}) || d.Retention != config.DefaultStatisticsRetention ||
		d.EventRetention != 48*time.Hour || d.Holder != "host/1" {
		t.Fatalf("the daily pass: %+v", d)
	}
	if d := newDaily(config.Config{}.StatisticsSettings(), nil, "h", time.Hour, nil); d.MinTenants != 3 || len(d.PublicTenants) != 0 {
		t.Fatalf("the defaults: %+v", d)
	}
}

// A public tenant's name that matches no tenant, or one being deleted, is said at start.
func TestLogPublicTenants(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := st.InTx(ctx, "", func(tx *store.Tx) error {
		for _, n := range []string{"free", "gone"} {
			tn := store.Tenant{Name: n}
			if err := tx.CreateTenant(ctx, &tn); err != nil {
				return err
			}
			ids = append(ids, tn.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ExecRaw(ctx, st, "UPDATE tenants SET deleting_at = ? WHERE id = ?", store.TimeArg(st, time.Now()), ids[1]); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	logPublicTenants(ctx, st, []string{"free", "gone", "ghost"}, slog.New(slog.NewTextHandler(&buf, nil)))
	out := buf.String()
	if strings.Contains(out, "tenant=free") || !strings.Contains(out, "tenant=gone") || !strings.Contains(out, "tenant=ghost") {
		t.Fatalf("the warnings: %s", out)
	}
	buf.Reset()
	logPublicTenants(ctx, st, nil, slog.New(slog.NewTextHandler(&buf, nil)))
	if buf.Len() != 0 {
		t.Fatalf("no list, no warning: %s", buf.String())
	}
}
