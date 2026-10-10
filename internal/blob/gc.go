package blob

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// The garbage collector (spec 0016): per storage domain, on one replica at a time (a lease), it
// deletes Builds no release references, body rows no Build references, and streams no row
// references, a grace after each became unreferenced; a stream in two steps (marked, then deleted),
// never while a commit claims it.

// Collector collects storage domains.
type Collector struct {
	Service *Service
	Holder  string        // this replica, for the leases
	Grace   time.Duration // spec 0016's gc.grace
	Log     *slog.Logger
	// Event records a pass (the server event storage.gc); nil: none.
	Event func(ctx context.Context, r Result)
}

// Result is what a pass found and, unless a dry run, deleted. A dry run deletes nothing, so its
// counts are of this pass alone: the streams of the bodies it would delete are not counted as marked.
type Result struct {
	Domain                                        string
	DryRun                                        bool
	Builds, Bodies, Marked, Streams, Tmp, Uploads int
	Bytes                                         int64 // of the streams, tmp objects deleted
	Claims                                        int   // expired claims removed
	Resolved                                      int   // deleting tombstones another collector left, finished
	Skipped                                       bool  // another replica holds the domain's lease
	Err                                           error // what stopped the pass
}

// Uploader aborts a store's incomplete multipart uploads older than a time (S3); stores without
// them do not implement it.
type Uploader interface {
	AbortIncompleteUploads(ctx context.Context, olderThan time.Time) (int, error)
}

const (
	gcBatch    = 500
	gcLeaseTTL = 10 * time.Minute
	// deleteStale is how long a deleting tombstone stays its collector's: another may take it again
	// after. It is well over a lease and an object's delete, so a collector whose delete is still
	// running (its stamp checked just before) is never overtaken.
	deleteStale   = 30 * time.Minute
	deleteTimeout = 30 * time.Second // one object's stat or delete
	resolveEvery  = 5 * time.Minute
)

// ErrLeaseLost is a collector whose lease expired or was taken mid-pass: it stops at once.
var ErrLeaseLost = errors.New("blob: the collector lost its lease")

func gcLease(domain string) string { return "kista/blob/gc/" + domain }

// Run collects every domain every interval (0: never) and resolves deleting tombstones every five
// minutes, each domain on its own, until ctx is done.
func (c *Collector) Run(ctx context.Context, interval time.Duration) {
	var wg sync.WaitGroup
	for _, d := range c.Service.Domains() {
		wg.Go(func() { c.run(ctx, d, interval) })
	}
	wg.Wait()
}

func (c *Collector) run(ctx context.Context, domain string, interval time.Duration) {
	var next time.Time
	if interval > 0 {
		next = time.Now().Add(time.Minute) // after the start's work
	}
	t := time.NewTicker(resolveEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var r Result
		var err error
		if interval > 0 && !time.Now().Before(next) {
			// a pass another replica holds the lease for (or its resolver) comes again next tick
			if r, err = c.Pass(ctx, domain, false); !r.Skipped {
				next = time.Now().Add(interval)
			}
		} else {
			_, err = c.Resolve(ctx, domain)
		}
		if err != nil && ctx.Err() == nil {
			c.Log.Error("blob: collecting a storage domain", "domain", domain, "error", err)
		}
	}
}

// Resolve finishes the domain's stale deleting tombstones (a collector that crashed mid-delete):
// commits waiting on them go on.
func (c *Collector) Resolve(ctx context.Context, domain string) (Result, error) {
	r := Result{Domain: domain}
	d, ok := c.Service.domains[domain]
	if !ok {
		return r, fmt.Errorf("blob: no storage domain %s", domain)
	}
	held, err := c.take(ctx, domain)
	if err != nil || !held {
		r.Skipped = err == nil && !held
		return r, err
	}
	defer c.release(ctx, domain)
	if err := c.ours(ctx, d); err != nil {
		return r, err
	}
	err = c.tombstones(ctx, d, time.Time{}, &r)
	if r.Resolved > 0 {
		c.Log.Info("blob: resolved deletes of a storage domain", "domain", domain, "resolved", r.Resolved, "bytes", r.Bytes)
	}
	return r, err
}

// ours checks the domain's store is this deployment's (spec 0005's marker): nothing else is
// collected.
func (c *Collector) ours(ctx context.Context, d Domain) error {
	deployment, err := c.Service.deployment(ctx)
	if err != nil {
		return err
	}
	got, err := readMarker(ctx, d)
	if err != nil {
		return err
	}
	if got != (marker{Domain: d.Name, StoreID: StoreID(d.Kind, d.Store), Deployment: deployment}) {
		return fmt.Errorf("%w: domain %s: the store is marked for another deployment, domain or store", ErrDomain, d.Name)
	}
	return nil
}

// Pass makes one collection of a domain: a dry run counts what it would delete and deletes nothing.
// A pass that ran is logged and recorded (the event storage.gc) whether it finished or not.
func (c *Collector) Pass(ctx context.Context, domain string, dryRun bool) (r Result, err error) {
	r = Result{Domain: domain, DryRun: dryRun}
	if c.Grace < 4*IntakeDeadline {
		return r, errors.New("blob: the collector's grace is under four intake deadlines")
	}
	d, ok := c.Service.domains[domain]
	if !ok {
		return r, fmt.Errorf("blob: no storage domain %s", domain)
	}
	held, err := c.take(ctx, domain)
	if err != nil || !held {
		r.Skipped = err == nil && !held
		return r, err
	}
	defer c.release(ctx, domain)
	defer func() {
		r.Err = err
		log := c.Log.Info
		if err != nil {
			log = c.Log.Error
		}
		log("blob: collected a storage domain", "domain", domain, "dry_run", dryRun, "builds", r.Builds, "bodies", r.Bodies,
			"marked", r.Marked, "streams", r.Streams, "tmp", r.Tmp, "uploads", r.Uploads, "bytes", r.Bytes, "claims", r.Claims,
			"resolved", r.Resolved, "error", err)
		if c.Event != nil {
			c.Event(context.WithoutCancel(ctx), r)
		}
	}()
	if err := c.ours(ctx, d); err != nil {
		return r, err
	}
	st := c.Service.st
	cutoff := st.Now().Add(-c.Grace)
	steps := []func() error{
		func() error { return c.builds(ctx, domain, cutoff, &r) },
		func() error { return c.bodies(ctx, domain, cutoff, &r) },
		func() error { return c.listing(ctx, d, &r) },
		func() error { return c.tombstones(ctx, d, cutoff, &r) },
		func() error { return c.leftovers(ctx, d, cutoff, &r) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return r, err
		}
		if err := c.renew(ctx, domain); err != nil {
			return r, err
		}
	}
	if !dryRun {
		if r.Claims, err = st.RemoveExpiredClaims(ctx, domain); err != nil {
			return r, err
		}
	}
	return r, nil
}

func (c *Collector) take(ctx context.Context, domain string) (bool, error) {
	return c.Service.st.AcquireLease(ctx, gcLease(domain), c.Holder, gcLeaseTTL)
}

// renew extends the lease the collector holds; one that expired meanwhile is lost (ErrLeaseLost),
// even if no other replica took it.
func (c *Collector) renew(ctx context.Context, domain string) error {
	held, err := c.Service.st.RenewLease(ctx, gcLease(domain), c.Holder, gcLeaseTTL)
	if err == nil && !held {
		err = ErrLeaseLost
	}
	return err
}

func (c *Collector) release(ctx context.Context, domain string) {
	_ = c.Service.st.ReleaseLease(context.WithoutCancel(ctx), gcLease(domain), c.Holder)
}

// builds deletes Builds no release references, unused for the grace.
func (c *Collector) builds(ctx context.Context, domain string, cutoff time.Time, r *Result) error {
	if !r.DryRun {
		if _, err := c.Service.st.FillUsedAt(ctx, domain); err != nil {
			return err
		}
	}
	after := ""
	for {
		ids, err := c.Service.st.DeadBuilds(ctx, domain, cutoff, after, gcBatch)
		if err != nil || len(ids) == 0 {
			return err
		}
		for _, id := range ids {
			if r.DryRun {
				r.Builds++
				continue
			}
			ok, err := c.Service.st.DeleteBuild(ctx, id, cutoff)
			if err != nil {
				return err
			}
			if ok {
				r.Builds++
			}
		}
		after = ids[len(ids)-1]
		if err := c.renew(ctx, domain); err != nil {
			return err
		}
	}
}

// bodies deletes body rows no Build references, committed before the cutoff, marking their streams.
func (c *Collector) bodies(ctx context.Context, domain string, cutoff time.Time, r *Result) error {
	after := ""
	for {
		ds, err := c.Service.st.DeadBodies(ctx, domain, cutoff, after, gcBatch)
		if err != nil || len(ds) == 0 {
			return err
		}
		for _, d := range ds {
			if r.DryRun {
				r.Bodies++
				continue
			}
			ok, err := c.Service.st.DeleteBody(ctx, domain, d, cutoff)
			if err != nil {
				return err
			}
			if ok {
				r.Bodies++
				c.Service.forget(domain, d.BodyHash)
			}
		}
		after = ds[len(ds)-1].BodyHash
		if err := c.renew(ctx, domain); err != nil {
			return err
		}
	}
}

// listing marks the streams of the store that no row references, no tombstone names and no claim
// holds.
func (c *Collector) listing(ctx context.Context, d Domain, r *Result) error {
	var batch []string
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		un, err := c.Service.st.UnreferencedStreams(ctx, d.Name, batch)
		batch = batch[:0]
		if err != nil {
			return err
		}
		for _, h := range un {
			if r.DryRun {
				r.Marked++
				continue
			}
			marked, err := c.Service.st.MarkStream(ctx, d.Name, h)
			if err != nil {
				return err
			}
			if marked {
				r.Marked++
			}
		}
		return c.renew(ctx, d.Name)
	}
	err := d.Store.List(ctx, "streams/", func(key string, size int64, modified time.Time) error {
		batch = append(batch, strings.TrimPrefix(key, "streams/"))
		if len(batch) >= 1000 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("blob: domain %s: listing: %w", d.Name, err)
	}
	return flush()
}

// tombstones deletes the streams of marked tombstones older than the cutoff (a zero cutoff: none),
// and finishes deleting ones another collector left. A delete the store refuses stops the pass: the
// tombstone goes back to marked (commits of the stream go on) and a later pass tries again; one
// whose outcome is unknown (a timeout) stays deleting, finished by a resolver once stale.
func (c *Collector) tombstones(ctx context.Context, d Domain, cutoff time.Time, r *Result) error {
	st := c.Service.st
	after := ""
	for {
		stale := st.Now().Add(-deleteStale)
		ts, err := st.DueTombstones(ctx, d.Name, cutoff, stale, after, gcBatch)
		if err != nil || len(ts) == 0 {
			return err
		}
		for _, t := range ts {
			key := StreamKey(t.StreamHash)
			size := c.size(ctx, d, key)
			if r.DryRun {
				r.Streams++
				r.Bytes += size
				continue
			}
			stamp, err := st.BeginDelete(ctx, d.Name, t.StreamHash, cutoff, stale)
			if err != nil {
				return err
			}
			if stamp.IsZero() {
				continue
			}
			// only the lease's holder deletes, and only a delete that is still its own: a collector
			// paused past its lease, or past deleteStale, stops here
			if err := c.renew(ctx, d.Name); err != nil {
				return err
			}
			mine, err := st.StillDeleting(ctx, d.Name, t.StreamHash, stamp)
			if err != nil {
				return err
			}
			if !mine {
				continue
			}
			dctx, cancel := context.WithTimeout(ctx, deleteTimeout)
			err = d.Store.Delete(dctx, key)
			cancel()
			if err != nil {
				if ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
					if aerr := st.AbortDelete(context.WithoutCancel(ctx), d.Name, t.StreamHash, stamp); aerr != nil {
						err = errors.Join(err, aerr)
					}
				}
				return fmt.Errorf("blob: domain %s: deleting stream %s: %w", d.Name, t.StreamHash, err)
			}
			if err := st.EndDelete(ctx, d.Name, t.StreamHash, stamp); err != nil {
				return err
			}
			r.Streams++
			r.Bytes += size
			if t.State == "deleting" {
				r.Resolved++
			}
		}
		after = ts[len(ts)-1].StreamHash
	}
}

// size is an object's size for the counts, 0 if it cannot be read.
func (c *Collector) size(ctx context.Context, d Domain, key string) int64 {
	sctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()
	n, _ := d.Store.Stat(sctx, key)
	return n
}

// leftovers deletes what failed uploads left: the fs store's tmp/ objects and S3's incomplete
// multipart uploads, older than the cutoff.
func (c *Collector) leftovers(ctx context.Context, d Domain, cutoff time.Time, r *Result) error {
	type obj struct {
		key  string
		size int64
	}
	var tmp []obj
	err := d.Store.List(ctx, "tmp/", func(key string, size int64, modified time.Time) error {
		if !modified.IsZero() && modified.Before(cutoff) {
			tmp = append(tmp, obj{key, size})
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("blob: domain %s: listing tmp/: %w", d.Name, err)
	}
	for _, o := range tmp {
		if !r.DryRun {
			if err := d.Store.Delete(ctx, o.key); err != nil {
				return fmt.Errorf("blob: domain %s: deleting %s: %w", d.Name, o.key, err)
			}
		}
		r.Tmp++
		r.Bytes += o.size
	}
	if u, ok := d.Store.(Uploader); ok && !r.DryRun {
		n, err := u.AbortIncompleteUploads(ctx, cutoff)
		r.Uploads = n
		if err != nil {
			return fmt.Errorf("blob: domain %s: incomplete uploads: %w", d.Name, err)
		}
	}
	return nil
}
