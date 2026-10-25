package licenses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Fetcher is the egress client the relay reads through.
type Fetcher interface {
	Download(ctx context.Context, raw string, o egress.DownloadOptions, w io.Writer) (egress.Download, error)
}

// The relay's bounds (spec 0012): an answer over either is a failure, and changes nothing.
const (
	MaxRelayed     = 1000
	MaxRelayAnswer = 8 << 20
	maxRefusals    = 20
	relayTimeout   = 2 * time.Minute
	relayLeaseTTL  = 15 * time.Minute // above a run's worst case: a token, two downloads, the apply
	expiryLease    = "kista/licenses/expiry"
	expiryLeaseTTL = 5 * time.Minute
	relayParallel  = 2 // relay runs at once on a replica
)

// Result is a relay run's outcome, kept as the upstream's last sync and recorded as license.sync.
type Result struct {
	Counts  map[string]int `json:"counts"` // received, stored, replaced, unchanged, removed, refused
	Refused []Refusal      `json:"refused,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// Refusal is a licence the relay did not store, and why.
type Refusal struct {
	Name   string `json:"name,omitempty"`
	ID     string `json:"id,omitempty"`
	Rule   string `json:"rule"`
	Reason string `json:"reason"`
}

func (r *Result) refuse(name, id, rule, reason string) {
	r.Counts["refused"]++
	if len(r.Refused) < maxRefusals {
		r.Refused = append(r.Refused, Refusal{Name: name, ID: id, Rule: rule, Reason: reason})
	}
}

// Sync asks for an upstream's relay run now (POST …/upstreams/{u}/licenses/sync).
func (s *Service) Sync(ctx context.Context, a authz.Actor, tenant, name string) error {
	t, err := s.tenant(ctx, a, authz.VerbAdmin, tenant)
	if err != nil {
		return err
	}
	if !s.Enabled {
		return ErrOff
	}
	var u store.Upstream
	if err := s.Store.InTxLocks(ctx, nil, func(tx *store.Tx) error { u, err = tx.GetUpstream(ctx, t.ID, name); return err }); err != nil {
		return err
	}
	return s.Store.InTenantTx(ctx, t.ID, []string{store.LicenseSyncLock(u.ID)}, func(tx *store.Tx) error {
		u, err := tx.GetUpstreamByID(ctx, u.ID)
		if err != nil {
			return err
		}
		if !relays(u) {
			return ErrUpstream
		}
		if u.State != store.UpstreamActive {
			return ErrPaused
		}
		return tx.RequestLicenseSync(ctx, t.ID, u.ID)
	})
}

// LastSync is an upstream's last relay run (zero when it never ran).
func (s *Service) LastSync(ctx context.Context, a authz.Actor, tenant, name string) (time.Time, *Result, error) {
	t, err := s.tenant(ctx, a, authz.VerbAdmin, tenant)
	if err != nil {
		return time.Time{}, nil, err
	}
	var u store.Upstream
	if err := s.Store.InTxLocks(ctx, nil, func(tx *store.Tx) error { u, err = tx.GetUpstream(ctx, t.ID, name); return err }); err != nil {
		return time.Time{}, nil, err
	}
	ls, err := s.Store.GetLicenseSync(ctx, u.ID)
	if err != nil || ls.LastResult == "" {
		return ls.LastRunAt, nil, err
	}
	var res Result
	if err := json.Unmarshal([]byte(ls.LastResult), &res); err != nil {
		return ls.LastRunAt, nil, err
	}
	return ls.LastRunAt, &res, nil
}

// relays reports whether an upstream relays licences: a repository with a credential, online.
func relays(u store.Upstream) bool {
	return u.Kind == store.UpstreamRepo && u.Mode != store.ModeBundle && u.Credential != ""
}

// RunUpstream runs one upstream's relay (the caller holds its lease) and records the result.
func (s *Service) RunUpstream(ctx context.Context, upstreamID string) (Result, error) {
	started := s.Store.Now()
	res := Result{Counts: map[string]int{}}
	var u store.Upstream
	var t store.Tenant
	err := s.Store.InTxLocks(ctx, nil, func(tx *store.Tx) error {
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
	if t.State != store.TenantActive {
		return res, nil
	}
	if !s.Enabled {
		res.Error = ErrOff.Error()
	} else if !relays(u) || u.State != store.UpstreamActive {
		res.Error = ErrUpstream.Error()
	} else if tokens, found, err := s.fetch(ctx, t.Name, u); err != nil {
		res.Error = err.Error()
	} else if found {
		err = s.apply(ctx, t, u, tokens, &res)
		if err != nil {
			res.Error = err.Error()
		}
	}
	actor := authz.Actor{Kind: authz.ActorSystem, ID: "licenses"}.String()
	b, _ := json.Marshal(res)
	err = s.Store.InTenantTx(ctx, t.ID, []string{store.LicenseSyncLock(u.ID)}, func(tx *store.Tx) error {
		if err := tx.FinishLicenseSync(ctx, t.ID, u.ID, string(b), started); err != nil {
			return err
		}
		fields := map[string]any{"upstream": u.Name, "counts": res.Counts}
		if res.Error != "" {
			fields["error"] = res.Error
		}
		if len(res.Refused) > 0 {
			fields["refused"] = res.Refused
		}
		return tx.Event(ctx, t.ID, actor, "license.sync", "upstream:"+u.Name, fields)
	})
	return res, err
}

// fetch reads the upstream's licences for this kista's credential: found false for a 404 (the
// upstream serves none). A 401 forgets the token and tries once more.
func (s *Service) fetch(ctx context.Context, tenant string, u store.Upstream) ([]string, bool, error) {
	cred, err := s.Credentials.For(u.Credential, tenant, u.Prefix)
	if err != nil {
		return nil, false, errors.New("upstream.auth: the credential is not configured for this upstream")
	}
	var buf bytes.Buffer
	for attempt := 0; ; attempt++ {
		tok, err := cred.Token(ctx)
		if errors.Is(err, credential.ErrClient) {
			return nil, false, err
		} else if err != nil {
			return nil, false, errors.New("upstream.auth: no token")
		}
		buf.Reset()
		_, err = s.Fetch.Download(ctx, u.Prefix+"/-/licenses", egress.DownloadOptions{MaxBytes: MaxRelayAnswer, Timeout: relayTimeout,
			MinRate: 1 << 10, Header: http.Header{"Authorization": {"Bearer " + tok}}}, &buf)
		var ae *egress.AuthError
		if attempt == 0 && errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			cred.Invalidate(tok)
			continue
		}
		switch {
		case errors.Is(err, egress.ErrNotFound):
			return nil, false, nil
		case err != nil:
			return nil, false, err
		}
		break
	}
	var ans struct {
		Licenses []string `json:"licenses"`
	}
	if err := json.Unmarshal(buf.Bytes(), &ans); err != nil {
		return nil, false, fmt.Errorf("licenses: the upstream's answer is not a licence list: %w", err)
	}
	if ans.Licenses == nil {
		return nil, false, errors.New("licenses: the upstream's answer has no licences member")
	}
	if len(ans.Licenses) > MaxRelayed {
		return nil, false, fmt.Errorf("licenses: the upstream answered more than %d licences", MaxRelayed)
	}
	return ans.Licenses, true, nil
}

// apply stores an upstream's answer by spec 0012's rules, in one transaction: each licence checked
// and bound to a name the upstream supplies; the stored ones from this upstream that the answer
// leaves out removed.
func (s *Service) apply(ctx context.Context, t store.Tenant, u store.Upstream, tokens []string, res *Result) error {
	actor := authz.Actor{Kind: authz.ActorSystem, ID: "licenses"}.String()
	res.Counts["received"] = len(tokens)
	type item struct {
		tok   *license.Token
		token string
	}
	var ok []item
	for _, token := range tokens {
		tok, err := s.parse(token)
		if err != nil {
			rule := "not_in_force"
			var le *license.Error
			if errors.As(err, &le) {
				rule = le.Rule
			}
			name, id := "", ""
			if p, perr := license.Parse(token); perr == nil {
				name, id = p.Extension, p.ID
			}
			res.refuse(name, id, rule, err.Error())
			continue
		}
		ok = append(ok, item{tok, token})
	}
	lb, err := s.labels(ctx, t.ID)
	if err != nil {
		return err
	}
	src := store.UpstreamSource(u.ID)
	// the transaction may run again: its counts reach res only once it holds
	var counts map[string]int
	var refused []Refusal
	err = s.Store.InTenantTx(ctx, t.ID, []string{store.LicenseLock(t.ID)}, func(tx *store.Tx) error {
		// removing the upstream takes the licence lock too: one removed meanwhile is gone here;
		// one paused or pointed elsewhere meanwhile does not take this answer
		cur, err := tx.GetUpstreamByID(ctx, u.ID)
		if err != nil {
			return err
		}
		if !relays(cur) || cur.State != u.State || cur.Prefix != u.Prefix || cur.Credential != u.Credential {
			return ErrChanged
		}
		// spec 0034: a relay takes its upstream's type, its home channel's
		home, err := tx.ChannelByID(ctx, cur.ChannelID)
		if err != nil {
			return err
		}
		typ := home.PackageType
		counts, refused = map[string]int{}, nil
		keep := map[string]bool{}
		for _, it := range ok {
			name := it.tok.Extension
			supplied, err := tx.SuppliedBy(ctx, cur, typ, name)
			if err != nil {
				return err
			}
			if !supplied {
				refused = append(refused, Refusal{Name: name, ID: it.tok.ID, Rule: "not_supplied",
					Reason: "the upstream supplies no build of " + name + " here"})
				continue
			}
			// spec 0033 phase B: the key's owner for the name must be this upstream (another source's
			// copy of its key gets no licence, nor does this upstream's copy of another's), and the
			// licence verifies against the keys it brought (spec 0021). A key refusal keeps a licence
			// stored from this upstream: no live build declares its key now (an upgrade before the
			// builds are fetched again, a rotation under way), so it is not served, and it ends with
			// its grace unless a build brings the key back
			keep[name] = true
			if _, err := s.owned(ctx, tx, t.ID, typ, it.tok, src, lb.of, true); err != nil {
				if r := refusal(err, it.tok); r != nil {
					refused = append(refused, *r)
					continue
				}
				return err
			}
			// spec 0028: a licence of another holder than the tenant expects is refused; one stored
			// for the name from this upstream, of the holder expected, stays until its grace ends
			if r, err := holderRefusal(ctx, tx, t.ID, it.tok); err != nil {
				return err
			} else if r != nil {
				refused = append(refused, *r)
				continue
			}
			keep[name] = true
			l := record(it.tok, t.ID, typ, it.token, actor)
			l.Owner, l.Source, l.UpstreamID = src, store.LicenseUpstream, u.ID
			old, err := tx.GetLicense(ctx, t.ID, typ, name, src)
			adopted := false
			switch {
			case errors.Is(err, store.ErrNotFound):
				old.Version = 0
				if adopted, err = adoptUnknown(ctx, tx, &l, true); err != nil {
					return err
				}
			case err != nil:
				return err
			case old.Token == it.token:
				if err := rebind(ctx, tx, &old, u.ID, true); err != nil {
					return err
				}
				counts["unchanged"]++
				continue
			case replaces(old, l, s.Store.Now().Unix(), s.deadKey(ctx, tx, old)):
				l.Visibility = old.Visibility
			default:
				refused = append(refused, Refusal{Name: name, ID: it.tok.ID, Rule: "superseded",
					Reason: "the licence stored for " + name + " (" + old.Source + ") stays: it is newer or lasts longer"})
				continue
			}
			if err := tx.PutLicense(ctx, &l, old.Version); err != nil {
				return err
			}
			if old.Version == 0 && !adopted {
				counts["stored"]++
			} else {
				counts["replaced"]++
			}
		}
		stored, err := tx.UpstreamLicenses(ctx, t.ID, u.ID)
		if err != nil {
			return err
		}
		for _, l := range stored {
			if keep[l.Name] {
				continue
			}
			if err := tx.DeleteLicense(ctx, l, 0); err != nil {
				return err
			}
			counts["removed"]++
			if err := tx.Event(ctx, t.ID, actor, "license.remove", "license:"+l.Name,
				typed(map[string]any{"name": l.Name, "license": l.LicenseID, "reason": "revoked", "source": lb.of(l.Owner)}, l.PackageType)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for k, v := range counts {
		res.Counts[k] += v
	}
	for _, r := range refused {
		res.refuse(r.Name, r.ID, r.Rule, r.Reason)
	}
	return nil
}

// rebind makes a stored licence that the relaying upstream relays again unchanged, relayed by
// another upstream before (migration 0042 gave it its key's owner), the relaying upstream's (spec
// 0033 phase B), when write is set (a bundle's dry run writes nothing). An import stays an
// administrator's: no relay takes it over.
func rebind(ctx context.Context, tx *store.Tx, old *store.License, upstreamID string, write bool) error {
	if !write || old.Source != store.LicenseUpstream || old.UpstreamID == upstreamID {
		return nil
	}
	return tx.RebindLicense(ctx, old, upstreamID)
}

// adoptUnknown removes, for a licence l its upstream relays where the upstream holds none for the
// name, a licence of unknown source (migration 0042 found no owner for its key) that the same
// upstream relayed signed with the same key: l takes its place and its visibility (spec 0033 phase
// B: otherwise the name would keep a licence of two sources). It reports whether there was one;
// with write unset (a dry run) it removes nothing.
func adoptUnknown(ctx context.Context, tx *store.Tx, l *store.License, write bool) (bool, error) {
	old, err := tx.GetLicense(ctx, l.TenantID, l.PackageType, l.Name, store.LicenseUnknown)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if old.Source != store.LicenseUpstream || old.UpstreamID != l.UpstreamID || old.Key != l.Key {
		return false, nil
	}
	l.Visibility = old.Visibility
	if !write {
		return true, nil
	}
	return true, tx.DeleteLicense(ctx, old, 0)
}

// refusal is a licence's refusal by its rule (a key the tenant does not take for the source: no
// live build declares it, another source owns it, the server refuses it); nil for another error.
func refusal(err error, tok *license.Token) *Refusal {
	if !errors.Is(err, ErrRefused) {
		return nil
	}
	rule := license.RuleKeyUntrusted
	var le *license.Error
	if errors.As(err, &le) {
		rule = le.Rule
	}
	return &Refusal{Name: tok.Extension, ID: tok.ID, Rule: rule, Reason: err.Error()}
}

// replaces reports whether a relayed licence l replaces the stored one (spec 0012's rules, with spec
// 0021's condition): once the stored one's grace has ended or its key counts nowhere (dead);
// otherwise when l is a later licence from the same upstream or lasts longer, and, from another
// source, is signed with the same key.
func replaces(old, l store.License, now int64, dead bool) bool {
	if old.GraceUntil <= now || dead {
		return true
	}
	same := old.Source == store.LicenseUpstream && old.UpstreamID == l.UpstreamID
	if !same && old.Key != l.Key {
		return false
	}
	return same && l.IssuedAt > old.IssuedAt || l.ExpiresAt > old.ExpiresAt
}

// deadKey reports whether a stored licence's key counts nowhere: refused by the server, or not a live
// key of its name from its source (spec 0033 phase B). Such a licence is served nowhere, so it holds
// the name for nobody.
func (s *Service) deadKey(ctx context.Context, tx *store.Tx, l store.License) bool {
	if s.Refuse[l.Key] {
		return true
	}
	keys, err := tx.LiveLicenseKeys(ctx, l.TenantID, l.PackageType, l.Name, l.Owner)
	if err != nil {
		return false // as alive: the stored licence stays
	}
	for _, k := range keys {
		if k.Kid == l.Key {
			return false
		}
	}
	return true
}

// ExpireOnce removes, across tenants, the licences whose grace has ended (license.remove, reason
// expired). A licence that cannot be removed now (its tenant being deleted, a change in between) is
// skipped, and the rest go on.
func (s *Service) ExpireOnce(ctx context.Context) error {
	ls, err := s.Store.ExpiredLicenses(ctx, s.Store.Now().Unix())
	if err != nil {
		return err
	}
	actor := authz.Actor{Kind: authz.ActorSystem, ID: "licenses"}.String()
	for _, l := range ls {
		source := idLabel(l.Owner)
		if lb, err := s.labels(ctx, l.TenantID); err == nil {
			source = lb.of(l.Owner)
		}
		err := s.Store.InTenantTx(ctx, l.TenantID, []string{store.LicenseLock(l.TenantID)}, func(tx *store.Tx) error {
			if err := tx.DeleteLicense(ctx, l, 0); err != nil {
				return err
			}
			return tx.Event(ctx, l.TenantID, actor, "license.remove", "license:"+l.Name,
				typed(map[string]any{"name": l.Name, "license": l.LicenseID, "reason": "expired", "source": source}, l.PackageType))
		})
		if err != nil && !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrNotFound) {
			if ctx.Err() != nil {
				return err
			}
			s.Log.Error("licenses: removing an expired licence", "tenant", l.TenantID, "name", l.Name, "error", err)
		}
	}
	return nil
}

// Runner runs the relays that are due, each under its upstream's lease, and the expiry pass.
type Runner struct {
	Service *Service
	Holder  string
	Poll    time.Duration // default 30s
	wg      sync.WaitGroup
	mu      sync.Mutex
	running map[string]bool
	sem     chan struct{}
}

// Run polls until ctx ends.
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

// Once runs what is due and waits for it.
func (r *Runner) Once(ctx context.Context) {
	r.poll(ctx)
	r.wg.Wait()
}

func (r *Runner) poll(ctx context.Context) {
	s := r.Service
	if !s.Enabled {
		return
	}
	r.mu.Lock()
	if r.running == nil {
		r.running, r.sem = map[string]bool{}, make(chan struct{}, relayParallel)
	}
	r.mu.Unlock()
	if held, err := s.Store.AcquireLease(ctx, expiryLease, r.Holder, expiryLeaseTTL); err == nil && held {
		if err := s.ExpireOnce(ctx); err != nil {
			s.Log.Error("licenses: removing expired licences", "error", err)
		}
		_ = s.Store.ReleaseLease(context.WithoutCancel(ctx), expiryLease, r.Holder)
	}
	ids, err := s.Store.DueLicenseSyncs(ctx, s.Store.Now(), s.Interval)
	if err != nil {
		s.Log.Error("licenses: listing due relays", "error", err)
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
			return // the rest wait for the next poll
		}
		r.mu.Lock()
		r.running[id] = true
		r.mu.Unlock()
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer func() { r.mu.Lock(); delete(r.running, id); r.mu.Unlock(); <-r.sem }()
			r.one(ctx, id)
		}()
	}
}

func (r *Runner) one(ctx context.Context, id string) {
	s := r.Service
	lease := store.LicenseRelayLease(id)
	held, err := s.Store.AcquireLease(ctx, lease, r.Holder, relayLeaseTTL)
	if err != nil || !held {
		return
	}
	defer func() { _ = s.Store.ReleaseLease(context.WithoutCancel(ctx), lease, r.Holder) }()
	// another replica may have run it between the listing and the lease
	if due, err := s.Store.LicenseSyncDue(ctx, id, s.Store.Now(), s.Interval); err != nil || !due {
		return
	}
	res, err := s.RunUpstream(ctx, id)
	if err != nil {
		s.Log.Error("licenses: a relay run", "upstream", id, "error", err)
		return
	}
	if res.Error != "" {
		s.Log.Warn("licenses: a relay run failed", "upstream", id, "error", res.Error)
	}
}
