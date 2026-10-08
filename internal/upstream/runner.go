package upstream

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Runner runs requested and due upstreams, one replica per upstream at a time (spec 0009).
type Runner struct {
	Service *Service
	Holder  string        // this replica
	Poll    time.Duration // default 30s
	Log     *slog.Logger
}

const leaseTTL = 2 * time.Minute

// Run loops until ctx is done.
func (r *Runner) Run(ctx context.Context) {
	if r.Poll == 0 {
		r.Poll = 30 * time.Second
	}
	t := time.NewTicker(r.Poll)
	defer t.Stop()
	for {
		r.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once runs every upstream that is requested or due and whose lease this replica takes.
func (r *Runner) Once(ctx context.Context) {
	st := r.Service.Store
	ids, err := st.DueUpstreams(ctx, time.Now())
	if err != nil {
		r.Log.Error("upstream: listing due upstreams", "error", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		lease := "kista/upstream/" + id
		held, err := st.AcquireLease(ctx, lease, r.Holder, leaseTTL)
		if err != nil {
			r.Log.Error("upstream: taking a run's lease", "upstream", id, "error", err)
			continue
		}
		if !held {
			continue
		}
		renew := func(ctx context.Context) error {
			if ok, err := st.AcquireLease(ctx, lease, r.Holder, leaseTTL); err != nil || !ok {
				return errors.New("upstream: the run's lease was lost")
			}
			return nil
		}
		res, err := r.Service.RunUpstream(ctx, id, renew)
		switch {
		case errors.Is(err, ErrNotDue):
		case err != nil:
			r.Log.Error("upstream: a run failed", "upstream", id, "error", err)
		case res.Error != "":
			r.Log.Warn("upstream: a run stopped", "upstream", id, "reason", res.Error, "counts", res.Counts)
		default:
			r.Log.Info("upstream: a run ended", "upstream", id, "dry_run", res.DryRun, "counts", res.Counts)
		}
		_ = st.ReleaseLease(context.WithoutCancel(ctx), lease, r.Holder)
	}
}
