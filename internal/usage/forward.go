package usage

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Poster posts a body to a URL: any 2xx is success; the status is returned either way.
type Poster interface {
	Post(ctx context.Context, raw, contentType string, header http.Header, body []byte) (int, error)
}

// Forwarder sends each upstream set to forward this tenant's usage of the versions it supplied
// (spec 0012 phase 1c): once a day, the last ForwardDays finished days, each sent whole. It runs on
// one replica at a time (a lease).
type Forwarder struct {
	Store       *store.Store
	Post        Poster
	Credentials credential.Registry
	Holder      string
	Log         *slog.Logger
	Every       time.Duration // how often it looks for upstreams due; default 1h
	PartReports int           // reports a request holds at most; default MaxDailyReports (tests use fewer)
}

// ForwardDays are the finished days each run sends: a late report still reaches the upstream, and
// a failed day is sent again the next day.
const ForwardDays = 3

// forwardGrace is how long after a day's end it is sent: the replicas have written its counts.
const forwardGrace = time.Hour

// ForwardResult is a run's outcome, kept as the upstream's last run and in its usage.forward event.
type ForwardResult struct {
	Sent    []string        `json:"sent"`    // days the upstream took
	Skipped []string        `json:"skipped"` // days not sent: the upstream takes no usage (404)
	Failed  []ForwardFailed `json:"failed"`
}

// ForwardFailed is a day not sent, with why.
type ForwardFailed struct {
	Day    string `json:"day"`
	Reason string `json:"reason"`
}

const forwardLease = "kista/usage/forward"

// Run looks for upstreams due every Every until ctx is done.
func (f *Forwarder) Run(ctx context.Context) {
	if f.Every == 0 {
		f.Every = time.Hour
	}
	t := time.NewTicker(f.Every)
	defer t.Stop()
	for {
		f.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once forwards to every upstream due, if it takes the lease.
func (f *Forwarder) Once(ctx context.Context) {
	held, err := f.Store.AcquireLease(ctx, forwardLease, f.Holder, 30*time.Minute)
	if err != nil || !held {
		if err != nil {
			f.Log.Error("usage: taking the forwarding lease", "error", err)
		}
		return
	}
	defer func() { _ = f.Store.ReleaseLease(context.WithoutCancel(ctx), forwardLease, f.Holder) }()
	since := f.Store.Now().Add(-23 * time.Hour) // daily, with an hour's slack for the ticker
	ids, err := f.Store.DueUsageForwards(ctx, since)
	if err != nil {
		f.Log.Error("usage: listing the upstreams due", "error", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if due, err := f.Store.UsageForwardDue(ctx, id, since); err != nil || !due {
			continue // changed meanwhile
		}
		if _, err := f.Forward(ctx, id); err != nil {
			f.Log.Error("usage: forwarding", "upstream", id, "error", err)
		}
		if held, err := f.Store.AcquireLease(ctx, forwardLease, f.Holder, 30*time.Minute); err != nil || !held {
			return // a long pass keeps its lease, or stops
		}
	}
}

// Forward sends one upstream its days now and records the run.
func (f *Forwarder) Forward(ctx context.Context, upstreamID string) (ForwardResult, error) {
	res := ForwardResult{Sent: []string{}, Skipped: []string{}, Failed: []ForwardFailed{}}
	var u store.Upstream
	var t store.Tenant
	err := f.Store.InTxLocks(ctx, nil, func(tx *store.Tx) error {
		var err error
		if u, err = tx.GetUpstreamByID(ctx, upstreamID); err != nil {
			return err
		}
		t, err = tx.GetTenantByID(ctx, u.TenantID)
		return err
	})
	if err != nil {
		return res, err
	}
	now := f.Store.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	var days []string // the last ForwardDays days that ended at least forwardGrace ago, oldest first
	for i := 1; len(days) < ForwardDays; i++ {
		start := today.AddDate(0, 0, -i)
		if !now.Before(start.AddDate(0, 0, 1).Add(forwardGrace)) {
			days = append([]string{store.DayOf(start)}, days...)
		}
	}
	versions, err := f.Store.UpstreamVersions(ctx, u)
	if err != nil {
		return res, err
	}
	cred, cerr := f.Credentials.For(u.Credential, t.Name, u.Prefix)
	for _, day := range days {
		if cerr != nil {
			res.Failed = append(res.Failed, ForwardFailed{day, "the credential is not configured for this upstream"})
			continue
		}
		parts, err := f.day(ctx, t.ID, day, versions)
		for i := range parts {
			parts[i].Sender = u.ID // this kista's upstreams sharing a credential stay apart at the receiver
		}
		if err != nil {
			res.Failed = append(res.Failed, ForwardFailed{day, "reading the day's usage: " + err.Error()})
			continue
		}
		switch reason, skip := f.send(ctx, u, cred, parts); {
		case skip:
			res.Skipped = append(res.Skipped, day)
		case reason != "":
			res.Failed = append(res.Failed, ForwardFailed{day, reason})
		default:
			res.Sent = append(res.Sent, day)
		}
	}
	b, _ := json.Marshal(res)
	actor := authz.Actor{Kind: authz.ActorSystem, ID: "usage"}.String()
	err = f.Store.InTenantTx(ctx, t.ID, nil, func(tx *store.Tx) error {
		if err := tx.FinishUsageForward(ctx, t.ID, u.ID, string(b)); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, actor, "usage.forward", "upstream:"+u.Name,
			map[string]any{"upstream": u.Name, "sent": res.Sent, "skipped": res.Skipped, "failed": res.Failed})
	})
	return res, err
}

// day builds a day's parts: the tenant's counts of the versions the upstream supplied, summed over
// its channels, local and received, every status and caller; users left out below MinUsers. An
// empty day is one empty part, so the upstream drops what it held for the day.
func (f *Forwarder) day(ctx context.Context, tenantID, day string, versions map[[2]string]map[string]bool) ([]Daily, error) {
	names := map[string]bool{}
	for k := range versions {
		names[k[0]] = true
	}
	rows, err := f.Store.UsageRows(ctx, tenantID, store.StatsFilter{From: day, To: day}, 10_000_000,
		func(_, name string) bool { return names[name] })
	if err != nil {
		return nil, err
	}
	type key = [2]string
	users := map[key]int64{}
	events := map[key]map[string]*DailyEvent{}
	for _, r := range rows {
		k := key{r.Name, r.PackageVersion}
		if !versions[k][r.ChannelID] {
			continue // a channel without the upstream's build of it: another source's version
		}
		if r.Event == "" {
			users[k] = sumMax(users[k], r.Users)
			continue
		}
		if events[k] == nil {
			events[k] = map[string]*DailyEvent{}
		}
		e := events[k][r.Event]
		if e == nil {
			e = &DailyEvent{Name: r.Event}
			events[k][r.Event] = e
		}
		if r.Counter == "" {
			e.Count = sumMax(e.Count, r.Value)
		} else {
			if e.Counters == nil {
				e.Counters = map[string]int64{}
			}
			e.Counters[r.Counter] = sumMax(e.Counters[r.Counter], r.Value)
		}
	}
	keys := make([]key, 0, len(events))
	for k := range events {
		keys = append(keys, k)
	}
	for k := range users {
		if events[k] == nil {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b key) int { return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1])) })
	var parts []Daily
	cur := Daily{Day: day, Reports: []DailyReport{}}
	size, n := 0, 0
	for _, k := range keys {
		r := DailyReport{Extension: k[0], Version: k[1], Events: []DailyEvent{}}
		if users[k] >= MinUsers {
			r.Users = users[k]
		}
		names := make([]string, 0, len(events[k]))
		for name := range events[k] {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			e := *events[k][name]
			if room := MaxReportKeys - r.Keys() - 1; len(e.Counters) > room { // within the receiver's cap: the first counters by name
				if room < 0 {
					break
				}
				cs := slices.Sorted(maps.Keys(e.Counters))
				kept := map[string]int64{}
				for _, c := range cs[:room] {
					kept[c] = e.Counters[c]
				}
				e.Counters = kept
			}
			r.Events = append(r.Events, e)
		}
		if len(r.Events) == 0 && r.Users == 0 {
			continue // nothing to say: a version used by fewer than MinUsers is not named either
		}
		b, _ := json.Marshal(r)
		// a request stays within the receiver's limits, with room for its envelope
		if len(cur.Reports) > 0 && (len(cur.Reports) >= cmp.Or(f.PartReports, MaxDailyReports) || size+len(b) > MaxDailySize-4096 || n+r.Keys() > MaxDailyKeys) {
			parts = append(parts, cur)
			cur, size, n = Daily{Day: day, Reports: []DailyReport{}}, 0, 0
		}
		cur.Reports = append(cur.Reports, r)
		size, n = size+len(b)+1, n+r.Keys()
	}
	parts = append(parts, cur)
	for i := range parts {
		parts[i].Part, parts[i].Parts = i+1, len(parts)
	}
	return parts, nil
}

func sumMax(a, b int64) int64 { return min(a+b, MaxValue) } // both within 0..MaxValue: no overflow

// send posts a day's parts in order; skip true for a 404 (the upstream takes no usage). A 401
// forgets the token and tries once more. A part that fails does not stop the next ones (the first
// failure is the reason); a pass that lost its lease stops.
func (f *Forwarder) send(ctx context.Context, u store.Upstream, cred *credential.Credential, parts []Daily) (reason string, skip bool) {
	first := ""
	for _, p := range parts {
		if f.Holder != "" {
			if held, err := f.Store.AcquireLease(ctx, forwardLease, f.Holder, 30*time.Minute); err != nil || !held {
				return "the forwarding lease was lost", false
			}
		}
		if r, skip := f.post(ctx, u, cred, p); skip {
			return "", true
		} else if r != "" && first == "" {
			first = r
		}
	}
	return first, false
}

// post sends one part.
func (f *Forwarder) post(ctx context.Context, u store.Upstream, cred *credential.Credential, p Daily) (reason string, skip bool) {
	body, _ := json.Marshal(p)
	for attempt := 0; ; attempt++ {
		tok, err := cred.Token(ctx)
		if err != nil {
			return "no token for the credential", false
		}
		status, err := f.Post.Post(ctx, u.Prefix+"/-/usage", "application/json", http.Header{"Authorization": {"Bearer " + tok}}, body)
		switch {
		case err == nil:
			return "", false
		case status == http.StatusUnauthorized && attempt == 0:
			cred.Invalidate(tok)
		case status == http.StatusNotFound:
			return "", true
		case status != 0:
			return fmt.Sprintf("the upstream answered %d", status), false
		case errors.Is(ctx.Err(), context.Canceled):
			return "stopped", false
		default:
			return "unreachable: " + err.Error(), false
		}
	}
}
