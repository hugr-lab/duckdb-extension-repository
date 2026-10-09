// Package buffer keeps the event buffer to its retention (spec 0010).
package buffer

import (
	"context"
	"log/slog"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Pruner keeps the event buffer to its retention and its per-tenant cap (spec 0010): every hour,
// on one replica at a time (a lease), in batches.
type Pruner struct {
	Store     *store.Store
	Holder    string
	Retention time.Duration // events delivered to every sink older than this go
	// Overdue is how long past the retention an undelivered event is kept (7 days).
	Overdue          time.Duration
	MaxRowsPerTenant int
	Every            time.Duration // default 1h
	Log              *slog.Logger
}

const pruneBatch = 1000

// Run prunes until ctx is done.
func (p *Pruner) Run(ctx context.Context) {
	if p.Every == 0 {
		p.Every = time.Hour
	}
	t := time.NewTicker(p.Every)
	defer t.Stop()
	for {
		p.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once makes one pass if it takes the lease.
func (p *Pruner) Once(ctx context.Context) {
	const lease = "kista/events/retention"
	held, err := p.Store.AcquireLease(ctx, lease, p.Holder, 10*time.Minute)
	if err != nil || !held {
		if err != nil {
			p.Log.Error("audit: taking the retention lease", "error", err)
		}
		return
	}
	defer func() { _ = p.Store.ReleaseLease(context.WithoutCancel(ctx), lease, p.Holder) }()
	now := p.Store.Now()
	overdue := p.Overdue
	if overdue == 0 {
		overdue = 7 * 24 * time.Hour
	}
	total := 0
	for ctx.Err() == nil {
		n, err := p.Store.PruneEvents(ctx, now.Add(-p.Retention), now.Add(-p.Retention-overdue), pruneBatch)
		if err != nil {
			p.Log.Error("audit: pruning events", "error", err)
			return
		}
		total += n
		if n < pruneBatch {
			break
		}
		if _, err := p.Store.AcquireLease(ctx, lease, p.Holder, 10*time.Minute); err != nil { // a long pass keeps its lease
			return
		}
	}
	if p.MaxRowsPerTenant > 0 && ctx.Err() == nil {
		n, err := p.Store.PruneTenantOverflow(ctx, p.MaxRowsPerTenant, pruneBatch)
		if err != nil {
			p.Log.Error("audit: pruning a tenant's overflow", "error", err)
			return
		}
		total += n
	}
	if total > 0 {
		p.Log.Info("audit: pruned events", "count", total)
	}
}
