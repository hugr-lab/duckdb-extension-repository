package app

import (
	"context"
	"errors"
	"log/slog"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Collector is the storage garbage collector (spec 0016), recording each pass as storage.gc (failed
// when the pass stopped on an error).
func Collector(cfg config.Config, st *store.Store, bs *blob.Service, holder string, log *slog.Logger) *blob.Collector {
	bundles := cfg.Bundles.Settings()
	return &blob.Collector{Service: bs, Holder: holder, Grace: cfg.GCSettings().Grace, Log: log,
		UploadTTL: bundles.UploadTTL, UploadMaxAge: bundles.UploadMaxAge,
		Event: func(ctx context.Context, r blob.Result) {
			outcome := audit.OK
			data := map[string]any{"domain": r.Domain, "builds": r.Builds, "bodies": r.Bodies, "marked": r.Marked, "streams": r.Streams,
				"tmp": r.Tmp, "attachments": r.Attachments, "uploads": r.Uploads, "chunks": r.Chunks, "bytes": r.Bytes, "claims": r.Claims, "resolved": r.Resolved, "dry_run": r.DryRun}
			if r.Err != nil {
				outcome, data["error"] = audit.Failed, storageClass(r.Err)
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

// storageClass is a stopped pass's error class for its event (spec 0010: never the error's text,
// which may name a store's host or a database's; the text goes to the log and the command's stderr).
func storageClass(err error) string {
	switch {
	case errors.Is(err, blob.ErrLeaseLost):
		return "lease"
	case errors.Is(err, blob.ErrImplausible):
		return "implausible"
	case errors.Is(err, blob.ErrDomain):
		return "domain"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "interrupted"
	}
	return "error"
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
		data["error"] = storageClass(err)
	}
	e, eerr := st.NewEvent(ctx, "", actor, "storage.verify", outcome, "domain:"+r.Domain, data)
	if eerr == nil {
		eerr = st.InsertEvents(ctx, []store.Event{e})
	}
	return eerr
}
