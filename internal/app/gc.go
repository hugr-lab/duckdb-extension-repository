package app

import (
	"context"
	"log/slog"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Collector is the storage garbage collector (spec 0016), recording each pass as storage.gc (failed
// when the pass stopped on an error).
func Collector(cfg config.Config, st *store.Store, bs *blob.Service, holder string, log *slog.Logger) *blob.Collector {
	return &blob.Collector{Service: bs, Holder: holder, Grace: cfg.GCSettings().Grace, Log: log,
		Event: func(ctx context.Context, r blob.Result) {
			outcome := audit.OK
			data := map[string]any{"domain": r.Domain, "builds": r.Builds, "bodies": r.Bodies, "marked": r.Marked, "streams": r.Streams,
				"tmp": r.Tmp, "uploads": r.Uploads, "bytes": r.Bytes, "claims": r.Claims, "resolved": r.Resolved, "dry_run": r.DryRun}
			if r.Err != nil {
				outcome = audit.Failed
				msg := r.Err.Error()
				if len(msg) > 1000 {
					msg = msg[:1000]
				}
				data["error"] = msg
			}
			e, err := st.NewEvent(ctx, "", "system:gc", "storage.gc", outcome, "domain:"+r.Domain, data)
			if err == nil {
				err = st.InsertEvents(ctx, []store.Event{e})
			}
			if err != nil {
				log.Error("blob: recording a collection", "error", err)
			}
		}}
}
