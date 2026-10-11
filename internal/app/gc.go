package app

import (
	"context"
	"log/slog"
	"strings"

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

// VerifyEvent records a verification pass over a domain (spec 0018) as storage.verify: ok when every
// record was, failed otherwise or when the pass stopped on err.
func VerifyEvent(ctx context.Context, st *store.Store, actor string, r blob.VerifyResult, err error) error {
	outcome := audit.OK
	if !r.Clean() || err != nil {
		outcome = audit.Failed
	}
	data := map[string]any{"domain": r.Domain, "full": r.Full, "mark": r.Mark, "unmark": r.Unmark, "checked": r.Checked, "ok": r.OK,
		"skipped": r.Skipped, "marked_ok": r.MarkedOK, "missing": r.Missing, "short": r.Short, "long": r.Long, "corrupt": r.Corrupt,
		"record": r.Record, "unreadable": r.Unreadable, "marked": r.Marked, "unmarked": r.Unmarked, "bytes": r.Bytes}
	if err != nil {
		msg := err.Error()
		if len(msg) > 1000 {
			msg = strings.ToValidUTF8(msg[:1000], "")
		}
		data["error"] = msg
	}
	e, eerr := st.NewEvent(ctx, "", actor, "storage.verify", outcome, "domain:"+r.Domain, data)
	if eerr == nil {
		eerr = st.InsertEvents(ctx, []store.Event{e})
	}
	return eerr
}
