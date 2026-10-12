package attachment

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Runner runs requested and due feeds, one replica per feed at a time (under a lease), and up to
// Concurrency feeds at once on this replica.
type Runner struct {
	Feeds       *Feeds
	Holder      string        // this replica
	Poll        time.Duration // default 30s
	Concurrency int           // default 2
	Log         *slog.Logger

	once    sync.Once
	sem     chan struct{}
	mu      sync.Mutex
	running map[string]bool
	wg      sync.WaitGroup
}

const feedLeaseTTL = 2 * time.Minute

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

// Once starts every feed that is requested or due and waits for the runs.
func (r *Runner) Once(ctx context.Context) {
	r.poll(ctx)
	r.wg.Wait()
}

func (r *Runner) poll(ctx context.Context) {
	r.once.Do(func() {
		n := r.Concurrency
		if n <= 0 {
			n = 2
		}
		r.sem, r.running = make(chan struct{}, n), map[string]bool{}
	})
	ids, err := r.Feeds.Store.DueFeeds(ctx, time.Now(), r.Feeds.Interval > 0)
	if err != nil {
		r.Log.Error("attachment: listing due feeds", "error", err)
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

// one runs a feed if this replica takes its lease.
func (r *Runner) one(ctx context.Context, id string) {
	st := r.Feeds.Store
	lease := "kista/feed/" + id
	held, err := st.AcquireLease(ctx, lease, r.Holder, feedLeaseTTL)
	if err != nil {
		r.Log.Error("attachment: taking a feed run's lease", "feed", id, "error", err)
		return
	}
	if !held {
		return
	}
	defer func() { _ = st.ReleaseLease(context.WithoutCancel(ctx), lease, r.Holder) }()
	renew := func(ctx context.Context) error {
		if ok, err := st.AcquireLease(ctx, lease, r.Holder, feedLeaseTTL); err != nil || !ok {
			return errors.New("attachment: the feed run's lease was lost")
		}
		return nil
	}
	res, err := r.Feeds.RunFeed(ctx, id, renew)
	switch {
	case errors.Is(err, ErrNotDue):
	case err != nil:
		r.Log.Error("attachment: a feed run failed", "feed", id, "error", err)
	case res.Error != "":
		r.Log.Warn("attachment: a feed run stopped", "feed", id, "reason", res.Error, "counts", res.Counts)
	default:
		r.Log.Info("attachment: a feed run ended", "feed", id, "counts", res.Counts)
	}
}
