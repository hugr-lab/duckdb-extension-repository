package upstream

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Runner runs requested and due upstreams, one replica per upstream at a time (spec 0009), and up
// to upstreams.concurrency upstreams at once on this replica (their cells share its gates).
type Runner struct {
	Service *Service
	Holder  string        // this replica
	Poll    time.Duration // default 30s
	Log     *slog.Logger

	once    sync.Once
	sem     chan struct{}
	mu      sync.Mutex
	running map[string]bool
	wg      sync.WaitGroup
}

const leaseTTL = 2 * time.Minute

// Run polls until ctx is done, then waits for the runs it started.
func (r *Runner) Run(ctx context.Context) {
	if r.Poll == 0 {
		r.Poll = 30 * time.Second
	}
	t := time.NewTicker(r.Poll)
	defer t.Stop()
	defer r.wg.Wait()
	for {
		r.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once starts every upstream that is requested or due and waits for the runs.
func (r *Runner) Once(ctx context.Context) {
	r.poll(ctx)
	r.wg.Wait()
}

// poll starts runs for the requested and due upstreams (requested ones first) while this replica
// has room; an upstream already running here is not started again, and one that does not fit waits
// for the next poll.
func (r *Runner) poll(ctx context.Context) {
	r.once.Do(func() {
		n := r.Service.Config.Concurrency
		if n <= 0 {
			n = 4
		}
		r.sem, r.running = make(chan struct{}, n), map[string]bool{}
	})
	ids, err := r.Service.Store.DueUpstreams(ctx, time.Now(), r.Service.Config.Interval > 0)
	if err != nil {
		r.Log.Error("upstream: listing due upstreams", "error", err)
		return
	}
	for _, id := range ids {
		r.mu.Lock()
		busy := r.running[id]
		r.mu.Unlock()
		if busy {
			continue
		}
		select {
		case r.sem <- struct{}{}:
		default:
			return
		}
		r.mu.Lock()
		r.running[id] = true
		r.mu.Unlock()
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer func() {
				r.mu.Lock()
				delete(r.running, id)
				r.mu.Unlock()
				<-r.sem
			}()
			r.one(ctx, id)
		}()
	}
}

// one runs an upstream if this replica takes its lease.
func (r *Runner) one(ctx context.Context, id string) {
	st := r.Service.Store
	lease := "kista/upstream/" + id
	held, err := st.AcquireLease(ctx, lease, r.Holder, leaseTTL)
	if err != nil {
		r.Log.Error("upstream: taking a run's lease", "upstream", id, "error", err)
		return
	}
	if !held {
		return
	}
	defer func() { _ = st.ReleaseLease(context.WithoutCancel(ctx), lease, r.Holder) }()
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
}
