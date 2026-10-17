package attachment

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// The feed client (spec 0011 phase 1b): a tenant's subscriptions to other repositories' feeds. A
// run sends the bodies the feed offers with the last seq received for each, verifies every
// statement against a pinned key, fetches each new document from the fixed path under the feed's
// URL, and stores it if its body is one it sent and still holds.

// Fetcher is egress as the feed client uses it.
type Fetcher interface {
	Get(ctx context.Context, raw string) ([]byte, http.Header, error)
	PostJSON(ctx context.Context, raw string, header http.Header, body []byte, max int64) ([]byte, error)
	Download(ctx context.Context, raw string, o egress.DownloadOptions, w io.Writer) (egress.Download, error)
	PortAllowed(port uint16) bool
}

// Feeds manages a tenant's feeds and runs them.
type Feeds struct {
	Store       *store.Store
	Authz       authz.Authorizer
	Attachments *Service // documents' limits and storage
	Fetch       Fetcher
	Credentials credential.Registry
	// Interval is the time between scheduled runs of a feed (0: only requested runs).
	Interval time.Duration
	Log      *slog.Logger
}

// FeedSpec is what an administrator gives to add a feed.
type FeedSpec struct {
	Name, URL, Credential string
	Keys, Kinds           []string
	Readers               bool
	Offer                 string
	Channels              []string // channel names, for the channels offer
	// Mode is store.FeedOnline (the default) or store.FeedBundle: statements arrive in bundles
	// (spec 0014), never from the URL
	Mode string
	// Export keeps what a bundle needs to answer a site's plan (online feeds only)
	Export bool
}

// ErrBundle is a run asked of a bundle-mode feed: it never runs (spec 0014).
var ErrBundle = errors.New("attachment: a bundle-mode feed is never run: its statements arrive in bundles")

var feedPath = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)*/api/v1/tenants/[a-z0-9][a-z0-9-]{0,62}$`)

// checkFeedURL normalises a feed URL: https://<host>[:port][/<path>]/api/v1/tenants/<t>, no user
// info, query or fragment, at a port egress may reach (unless egress is false: a bundle-mode feed's
// URL is only its issuer's identity).
func (f *Feeds) checkFeedURL(raw string, egress bool) (string, error) {
	bad := func(why string) (string, error) {
		return "", fmt.Errorf("%w: feed URL %q: %s", store.ErrInvalid, raw, why)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return bad("not an absolute URL")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || u.RawPath != "" {
		return bad("no user info, query, fragment or escapes")
	}
	if u.Scheme != "https" {
		return bad("not https")
	}
	if !feedPath.MatchString(u.Path) || slices.ContainsFunc(strings.Split(u.Path, "/"), func(seg string) bool { return seg == "." || seg == ".." }) {
		return bad("not …/api/v1/tenants/<tenant>")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || egress && !f.Fetch.PortAllowed(uint16(n)) {
			return bad("egress does not reach that port")
		}
		if n == 443 { // the default port is dropped: one feed, one form (its statements' issuer)
			u.Host = u.Hostname()
			if strings.Contains(u.Host, ":") {
				u.Host = "[" + u.Host + "]"
			}
		}
	}
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

func checkFingerprint(fp string) error {
	h, ok := strings.CutPrefix(fp, "sha256:")
	if !ok || len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
		return fmt.Errorf("%w: key fingerprint %q (sha256:<64 lowercase hex>)", store.ErrInvalid, fp)
	}
	return nil
}

func checkKinds(kinds []string) error {
	for _, k := range kinds {
		if !slices.Contains(Kinds, k) {
			return fmt.Errorf("%w: kind %q", store.ErrInvalid, k)
		}
	}
	return nil
}

func dedup(xs []string) []string {
	out := slices.Clone(xs)
	slices.Sort(out)
	return slices.Compact(out)
}

func (f *Feeds) admin(ctx context.Context, a authz.Actor, tenant string) (store.Tenant, error) {
	if err := f.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
		return store.Tenant{}, err
	}
	return f.Store.GetTenant(ctx, tenant)
}

func (f *Feeds) checkCredential(name, tenant, feedURL string) error {
	if name == "" {
		return fmt.Errorf("%w: a feed needs a credential (its principal holds feed at the source)", store.ErrInvalid)
	}
	if _, err := f.Credentials.For(name, tenant, feedURL); err != nil {
		return fmt.Errorf("%w: %w", store.ErrInvalid, err)
	}
	return nil
}

// Add adds a feed (a tenant administrator's).
func (f *Feeds) Add(ctx context.Context, a authz.Actor, tenant string, sp FeedSpec) (store.Feed, error) {
	t, err := f.admin(ctx, a, tenant)
	if err != nil {
		return store.Feed{}, err
	}
	if sp.Mode == "" {
		sp.Mode = store.FeedOnline
	}
	bundle := sp.Mode == store.FeedBundle
	switch {
	case sp.Mode != store.FeedOnline && !bundle:
		return store.Feed{}, fmt.Errorf("%w: mode %q (online or bundle)", store.ErrInvalid, sp.Mode)
	case bundle && sp.Credential != "":
		return store.Feed{}, fmt.Errorf("%w: a bundle-mode feed takes no credential: kista never contacts its source", store.ErrInvalid)
	case bundle && sp.Export:
		return store.Feed{}, fmt.Errorf("%w: export is for online feeds", store.ErrInvalid)
	}
	u, err := f.checkFeedURL(sp.URL, !bundle)
	if err != nil {
		return store.Feed{}, err
	}
	if !bundle {
		if err := f.checkCredential(sp.Credential, tenant, u); err != nil {
			return store.Feed{}, err
		}
	}
	keys := dedup(sp.Keys)
	for _, k := range keys {
		if err := checkFingerprint(k); err != nil {
			return store.Feed{}, err
		}
	}
	kinds := dedup(sp.Kinds)
	if err := checkKinds(kinds); err != nil {
		return store.Feed{}, err
	}
	if sp.Offer == "" {
		sp.Offer = store.OfferUpstreams
	}
	if sp.Offer != store.OfferUpstreams && sp.Offer != store.OfferChannels || (sp.Offer == store.OfferChannels) != (len(sp.Channels) > 0) {
		return store.Feed{}, fmt.Errorf("%w: offer is upstreams, or channels with the channels named", store.ErrInvalid)
	}
	fd := store.Feed{TenantID: t.ID, Name: sp.Name, URL: u, Credential: sp.Credential, Keys: keys, Kinds: kinds, Readers: sp.Readers,
		Offer: sp.Offer, Mode: sp.Mode, Export: sp.Export, CreatedBy: a.String()}
	var names []string
	for _, c := range dedup(sp.Channels) {
		ch, err := f.Store.GetChannel(ctx, tenant, c)
		if err != nil {
			return store.Feed{}, err
		}
		fd.Channels, names = append(fd.Channels, ch.ID), append(names, ch.Name)
	}
	err = f.Store.InTenantTx(ctx, t.ID, []string{store.FeedLock(t.ID)}, func(tx *store.Tx) error {
		if err := tx.InsertFeed(ctx, &fd); err != nil {
			return err
		}
		if !bundle {
			if err := tx.RequestFeedRun(ctx, &fd); err != nil {
				return err
			}
		}
		return tx.Event(ctx, t.ID, a.String(), "feed.add", "feed:"+fd.Name, map[string]any{"name": fd.Name, "url": fd.URL,
			"credential": fd.Credential, "kinds": fd.Kinds, "readers": fd.Readers, "offer": fd.Offer, "channels": names, "keys": fd.Keys,
			"mode": fd.Mode, "export": fd.Export})
	})
	return fd, err
}

// List lists a tenant's feeds.
func (f *Feeds) List(ctx context.Context, a authz.Actor, tenant string) ([]store.Feed, error) {
	t, err := f.admin(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	var out []store.Feed
	err = f.Store.InTx(ctx, "", func(tx *store.Tx) error {
		out, err = tx.ListFeeds(ctx, t.ID)
		return err
	})
	return out, err
}

// Get reads a feed.
func (f *Feeds) Get(ctx context.Context, a authz.Actor, tenant, name string) (store.Feed, error) {
	t, err := f.admin(ctx, a, tenant)
	if err != nil {
		return store.Feed{}, err
	}
	var out store.Feed
	err = f.Store.InTx(ctx, "", func(tx *store.Tx) error {
		out, err = tx.GetFeed(ctx, t.ID, name)
		return err
	})
	return out, err
}

// FeedChange is a change to a feed: each set field is changed.
type FeedChange struct {
	State      *string   `json:"state,omitempty"`
	Credential *string   `json:"credential,omitempty"`
	Kinds      *[]string `json:"kinds,omitempty"`
	Readers    *bool     `json:"readers,omitempty"`
	Keys       *[]string `json:"keys,omitempty"`
	Export     *bool     `json:"export,omitempty"`
}

// Change changes a feed (expected: its version, 0 for any). Resuming an online feed asks for a run.
// Setting export resets its cursors, so its next run reads the whole history again and keeps what
// it refused before; clearing it drops what it kept (spec 0014).
func (f *Feeds) Change(ctx context.Context, a authz.Actor, tenant, name string, ch FeedChange, expected int64) (store.Feed, error) {
	t, err := f.admin(ctx, a, tenant)
	if err != nil {
		return store.Feed{}, err
	}
	var out store.Feed
	err = f.Store.InTenantTx(ctx, t.ID, []string{store.FeedLock(t.ID)}, func(tx *store.Tx) error {
		fd, err := tx.GetFeed(ctx, t.ID, name)
		if err != nil {
			return err
		}
		if expected != 0 && expected != fd.Version {
			return store.ErrConflict
		}
		change := map[string]any{}
		if ch.State != nil && *ch.State != fd.State {
			if *ch.State != store.FeedActive && *ch.State != store.FeedPaused {
				return fmt.Errorf("%w: state %q", store.ErrInvalid, *ch.State)
			}
			change["state"], fd.State = *ch.State, *ch.State
		}
		bundle := fd.Mode == store.FeedBundle
		if ch.Credential != nil && *ch.Credential != fd.Credential {
			if bundle {
				return fmt.Errorf("%w: a bundle-mode feed takes no credential: kista never contacts its source", store.ErrInvalid)
			}
			if err := f.checkCredential(*ch.Credential, tenant, fd.URL); err != nil {
				return err
			}
			change["credential"], fd.Credential = *ch.Credential, *ch.Credential
		}
		if ch.Kinds != nil {
			k := dedup(*ch.Kinds)
			if err := checkKinds(k); err != nil {
				return err
			}
			if !slices.Equal(k, fd.Kinds) {
				change["kinds"], fd.Kinds = k, k
			}
		}
		if ch.Readers != nil && *ch.Readers != fd.Readers {
			change["readers"], fd.Readers = *ch.Readers, *ch.Readers
		}
		if ch.Keys != nil {
			k := dedup(*ch.Keys)
			for _, fp := range k {
				if err := checkFingerprint(fp); err != nil {
					return err
				}
			}
			if !slices.Equal(k, fd.Keys) {
				change["keys"], fd.Keys = k, k
			}
		}
		if ch.Export != nil && *ch.Export != fd.Export {
			if bundle {
				return fmt.Errorf("%w: export is for online feeds", store.ErrInvalid)
			}
			change["export"], fd.Export = *ch.Export, *ch.Export
		}
		out = fd
		if len(change) == 0 {
			return nil
		}
		if err := tx.SetFeed(ctx, &fd, expected); err != nil {
			return err
		}
		switch change["export"] {
		case true:
			if err := tx.ResetFeedCursors(ctx, fd.ID); err != nil {
				return err
			}
			change["cursors"] = "reset"
		case false:
			if err := tx.DropFeedExport(ctx, fd); err != nil {
				return err
			}
			change["kept"] = "dropped"
		}
		if !bundle && fd.State == store.FeedActive && (change["state"] == store.FeedActive || change["export"] == true) {
			if err := tx.RequestFeedRun(ctx, &fd); err != nil {
				return err
			}
		}
		out = fd
		return tx.Event(ctx, t.ID, a.String(), "feed.change", "feed:"+fd.Name, map[string]any{"name": fd.Name, "change": change})
	})
	return out, err
}

// Remove removes a feed (expected: its version, 0 for any); its documents stay unless documents is
// set (their objects are then collected as objects without a record).
func (f *Feeds) Remove(ctx context.Context, a authz.Actor, tenant, name string, documents bool, expected int64) error {
	t, err := f.admin(ctx, a, tenant)
	if err != nil {
		return err
	}
	return f.Store.InTenantTx(ctx, t.ID, []string{store.FeedLock(t.ID), lock(t)}, func(tx *store.Tx) error {
		fd, err := tx.GetFeed(ctx, t.ID, name)
		if err != nil {
			return err
		}
		var n int64
		if documents {
			// another feed of the same source keeps them: its cursors are past them
			all, err := tx.ListFeeds(ctx, t.ID)
			if err != nil {
				return err
			}
			i := slices.IndexFunc(all, func(o store.Feed) bool { return o.ID != fd.ID && o.URL == fd.URL })
			if i >= 0 {
				err = tx.ReattributeFeedDocuments(ctx, fd, all[i])
			} else {
				n, err = tx.RemoveFeedDocuments(ctx, fd)
			}
			if err != nil {
				return err
			}
		}
		if fd.Export {
			if err := tx.DropFeedExport(ctx, fd); err != nil {
				return err
			}
		}
		if err := tx.DeleteFeed(ctx, fd, expected); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "feed.remove", "feed:"+fd.Name, map[string]any{"name": fd.Name, "documents": n, "export": fd.Export})
	})
}

// Sync asks for a run of a feed.
func (f *Feeds) Sync(ctx context.Context, a authz.Actor, tenant, name string) (store.Feed, error) {
	t, err := f.admin(ctx, a, tenant)
	if err != nil {
		return store.Feed{}, err
	}
	var out store.Feed
	err = f.Store.InTenantTx(ctx, t.ID, nil, func(tx *store.Tx) error {
		fd, err := tx.GetFeed(ctx, t.ID, name)
		if err != nil {
			return err
		}
		if fd.Mode == store.FeedBundle {
			return ErrBundle
		}
		if fd.State != store.FeedActive {
			return fmt.Errorf("%w: the feed is paused", store.ErrInvalid)
		}
		out = fd
		return tx.RequestFeedRun(ctx, &out)
	})
	return out, err
}

// --- runs ---

// ErrNotDue is a feed neither requested nor due (or paused, or run by another replica).
var ErrNotDue = errors.New("attachment: the feed is not due")

// RunResult is a run's outcome, recorded as the feed's last run.
type RunResult struct {
	Started time.Time      `json:"started"`
	Ended   time.Time      `json:"ended"`
	Counts  map[string]int `json:"counts"`
	Refused []Refusal      `json:"refused,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// Refusal is a statement a run refused for good: its cursor moved past it.
type Refusal struct {
	IssuerID string `json:"issuer_id,omitempty"`
	BodyHash string `json:"body_hash,omitempty"`
	Reason   string `json:"reason"`
}

const (
	maxRefusals    = 20 // kept in a run's result
	maxPagesPerRun = 50 // answers per batch of bodies in a run
	retryAfter     = 5 * time.Minute
)

// errAuth marks a run the source refused (the feed's status: unauthorized).
var errAuth = errors.New("attachment: the source refuses the feed's credential")

// errHeldByQuota is an export feed's refusal or statement that could not be kept within the cap on
// what export feeds keep: its body's cursor holds, so no gap is silent (spec 0014).
var errHeldByQuota = errors.New("attachment: what export feeds keep is at its cap; statements wait")

// errOvertaken is a run whose feed's export setting changed meanwhile: the change reset or dropped
// what it would write and asked for another run, so this one stops.
var errOvertaken = errors.New("attachment: the feed's export setting changed during the run")

// transient is a failure that leaves a body's cursor, so the next run tries again.
type transient struct{ err error }

func (e transient) Error() string { return e.err.Error() }
func (e transient) Unwrap() error { return e.err }

// run is one run's state.
type run struct {
	f      *Feeds
	fd     store.Feed
	t      store.Tenant
	res    *RunResult
	pinned map[string]*rsa.PublicKey
	renew  func(context.Context) error
	// verified is the highest seq of a statement that verified in this run: the source's unsigned
	// high-water never moves a cursor beyond it
	verified int64
	header   http.Header
	cred     *credential.Credential
	token    string
	// quota: an export feed held a body at the quota; the run ends failing
	quota bool
}

// RunFeed runs a feed if it is requested or due; renew keeps the run's lease.
func (f *Feeds) RunFeed(ctx context.Context, id string, renew func(context.Context) error) (RunResult, error) {
	res := RunResult{Started: time.Now().UTC(), Counts: map[string]int{}}
	var fd store.Feed
	var t store.Tenant
	runnable := false
	err := f.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		if fd, runnable, err = tx.StartFeedRun(ctx, id, f.Interval > 0); err != nil || !runnable {
			return err
		}
		t, err = tx.GetTenantByID(ctx, fd.TenantID)
		return err
	})
	if err != nil {
		return res, err
	}
	if !runnable {
		return res, ErrNotDue
	}
	runErr := f.run(ctx, &run{f: f, fd: fd, t: t, res: &res}, renew)
	status := store.FeedOK
	switch {
	case errors.Is(runErr, errAuth):
		status = store.FeedUnauthorized
	case runErr != nil:
		status = store.FeedFailing
	}
	if runErr != nil {
		res.Error = clip(runErr.Error())
	}
	res.Ended = time.Now().UTC()
	b, _ := json.Marshal(res)
	var next time.Time
	switch {
	case f.Interval <= 0:
	case runErr != nil && status == store.FeedFailing:
		next = res.Ended.Add(retryAfter)
	default:
		next = res.Ended.Add(f.Interval + time.Duration(rand.Int64N(int64(f.Interval/10)+1)))
	}
	fctx := context.WithoutCancel(ctx)
	err = f.Store.InTenantTx(fctx, t.ID, nil, func(tx *store.Tx) error {
		if err := tx.FinishFeedRun(fctx, id, string(b), status, res.Error, res.Started, next); err != nil {
			return err
		}
		reasons := map[string]int{}
		for _, r := range res.Refused {
			reasons[r.Reason]++
		}
		return tx.Event(fctx, fd.TenantID, "system:feed:"+t.Name+"/"+fd.Name, "feed.run", "feed:"+fd.Name,
			map[string]any{"feed": fd.Name, "counts": res.Counts, "refused": reasons, "error": runClass(runErr)})
	})
	return res, err
}

// runClass is an error's class for the run's event: never a message with a host or a body.
func runClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errAuth):
		return "unauthorized"
	case errors.Is(err, egress.ErrRefused):
		return "refused"
	case errors.Is(err, egress.ErrConnect), errors.Is(err, egress.ErrStatus), errors.Is(err, egress.ErrTooLarge):
		return "unreachable"
	case errors.Is(err, credential.ErrUnknown), errors.Is(err, credential.ErrNotAllowed):
		return "credential"
	case errors.Is(err, credential.ErrToken), errors.Is(err, credential.ErrClient):
		return "token"
	case errors.Is(err, ErrStatement):
		return "keys"
	case errors.Is(err, errHeldByQuota):
		return "limits"
	}
	return "failed"
}

func (r *run) refuse(st Statement, reason string) {
	r.res.Counts["refused"]++
	if len(r.res.Refused) < maxRefusals {
		r.res.Refused = append(r.res.Refused, Refusal{IssuerID: st.IssuerID, BodyHash: st.BodyHash, Reason: reason})
	}
}

func (f *Feeds) run(ctx context.Context, r *run, renew func(context.Context) error) error {
	cred, err := f.Credentials.For(r.fd.Credential, r.t.Name, r.fd.URL)
	if err != nil {
		return err
	}
	r.cred, r.renew = cred, renew
	if err := r.authorize(ctx); err != nil {
		return err
	}
	if r.pinned, err = r.keys(ctx); errors.Is(err, errOvertaken) {
		r.res.Counts["overtaken"]++
		return nil
	} else if err != nil {
		return err
	}
	bodies, err := f.Store.OfferedBodies(ctx, r.fd)
	if err != nil {
		return err
	}
	cursors, err := f.Store.FeedCursors(ctx, r.fd.ID)
	if err != nil {
		return err
	}
	r.res.Counts["offered"] = len(bodies)
	for start := 0; start < len(bodies); start += MaxFeedBodies {
		if err := renew(ctx); err != nil {
			return err
		}
		if err := r.batch(ctx, bodies[start:min(start+MaxFeedBodies, len(bodies))], cursors); errors.Is(err, errOvertaken) {
			r.res.Counts["overtaken"]++
			return nil
		} else if err != nil {
			return err
		}
	}
	if r.quota {
		return errHeldByQuota
	}
	return nil
}

// authorize takes a token for the feed's credential.
func (r *run) authorize(ctx context.Context) error {
	tok, err := r.cred.Token(ctx)
	if err != nil {
		return err
	}
	r.token, r.header = tok, http.Header{"Authorization": {"Bearer " + tok}}
	return nil
}

// keys reads the source's published attestation keys and keeps the pinned ones.
func (r *run) keys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	b, _, err := r.f.Fetch.Get(ctx, r.fd.URL+"/attestation-keys")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Keys []struct {
			PublicKey string `json:"public_key"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%w: the source's attestation keys do not parse", ErrStatement)
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		pub, err := extfile.ParsePublicKey(k.PublicKey)
		if err != nil {
			continue
		}
		if fp := extfile.Fingerprint(pub); slices.Contains(r.fd.Keys, fp) {
			out[fp] = pub
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: the source publishes none of the pinned keys", ErrStatement)
	}
	if r.fd.Export { // the file a bundle carries to a site (spec 0014), once it served this run
		if len(b) > store.MaxFeedKeysFile || !utf8.Valid(b) {
			return nil, fmt.Errorf("%w: the source's attestation keys file is over %d bytes, or not UTF-8", ErrStatement, store.MaxFeedKeysFile)
		}
		if err := r.write(ctx, false, func(tx *store.Tx) error { return tx.SetFeedKeysFile(ctx, r.fd.ID, b) }); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// post sends one feed request; a refused token is invalidated and taken again once.
func (r *run) post(ctx context.Context, body []byte) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		b, err := r.f.Fetch.PostJSON(ctx, r.fd.URL+"/feed", r.header, body, MaxFeedAnswer+64<<10)
		var se *egress.StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			// a kista source answers a principal without feed as it answers a missing route
			return nil, fmt.Errorf("%w: the source answers 404 (no feed grant for the credential, or no such tenant)", errAuth)
		}
		if !errors.Is(err, egress.ErrAuth) {
			return b, err
		}
		if attempt > 0 {
			return nil, fmt.Errorf("%w: %w", errAuth, err)
		}
		r.cred.Invalidate(r.token)
		if err := r.authorize(ctx); err != nil {
			return nil, err
		}
	}
}

// batch runs one request's bodies until the source has nothing more for them.
func (r *run) batch(ctx context.Context, bodies []string, cursors map[string]int64) error {
	sent := map[string]bool{}
	for _, b := range bodies {
		sent[b] = true
	}
	failed := map[string]bool{} // bodies with a transient failure in this run: their cursors stay
	for page := 0; page < maxPagesPerRun; page++ {
		req := struct {
			Bodies []FeedBody `json:"bodies"`
		}{}
		for _, b := range bodies {
			if !failed[b] {
				req.Bodies = append(req.Bodies, FeedBody{BodyHash: b, AfterSeq: cursors[b]})
			}
		}
		if len(req.Bodies) == 0 {
			return nil
		}
		if err := r.renew(ctx); err != nil {
			return err
		}
		body, _ := json.Marshal(req)
		raw, err := r.post(ctx, body)
		if err != nil {
			return err
		}
		var ans FeedAnswer
		if err := json.Unmarshal(raw, &ans); err != nil {
			return fmt.Errorf("%w: the feed's answer does not parse", egress.ErrStatus)
		}
		for _, w := range ans.Statements {
			if err := r.one(ctx, w, sent, cursors, failed); err != nil {
				return err
			}
		}
		for _, h := range ans.Bodies {
			if sent[h.BodyHash] && !failed[h.BodyHash] && h.Seq > cursors[h.BodyHash] && h.Seq <= r.verified {
				if err := r.write(ctx, false, func(tx *store.Tx) error { return tx.SetFeedCursor(ctx, r.fd.ID, h.BodyHash, h.Seq) }); err != nil {
					return err
				}
				cursors[h.BodyHash] = h.Seq
			}
		}
		if !ans.More {
			return nil
		}
	}
	return nil
}

// one handles a statement: verified, then stored, refused for good (its cursor moves past it), or
// failed for now (its body's cursor stays for the next run).
func (r *run) one(ctx context.Context, w WireStatement, sent map[string]bool, cursors map[string]int64, failed map[string]bool) error {
	stmt, err1 := base64.StdEncoding.DecodeString(w.Statement)
	sig, err2 := base64.StdEncoding.DecodeString(w.Signature)
	// a statement refused before it is trusted holds its body's cursor for this run (and every
	// cursor when its body cannot be told), so a later statement or the source's high-water never
	// moves past it: it is tried again at the next run, once its key is pinned
	hold := func(reason string) error {
		r.refuse(Statement{}, reason)
		var claimed struct {
			BodyHash string `json:"body_hash"`
		}
		if err1 == nil && json.Unmarshal(stmt, &claimed) == nil && sent[claimed.BodyHash] {
			failed[claimed.BodyHash] = true
			return nil
		}
		for b := range sent {
			failed[b] = true
		}
		return nil
	}
	if err1 != nil || err2 != nil {
		return hold("encoding")
	}
	st, err := Verify(stmt, sig, r.pinned)
	if err != nil {
		return hold("signature") // unverified: nothing of it is trusted, not even its body
	}
	if st.Issuer != r.fd.URL { // the source tenant's URL is its identity
		return hold("issuer")
	}
	r.verified = max(r.verified, st.Seq)
	if !sent[st.BodyHash] {
		r.refuse(st, "unrequested")
		return nil
	}
	if failed[st.BodyHash] || st.Seq <= cursors[st.BodyHash] {
		return nil // after a failure, the body's later statements wait for the next run; or a replay
	}
	reason, err := r.store(ctx, st, stmt, sig)
	if err == nil && reason != "" && r.fd.Export {
		err = r.keepRefusal(ctx, st, stmt, sig, reason)
	}
	var tr transient
	switch {
	case errors.Is(err, errHeldByQuota):
		failed[st.BodyHash], r.quota = true, true
		r.res.Counts["held"]++
		return nil
	case errors.As(err, &tr):
		failed[st.BodyHash] = true
		r.res.Counts["failed"]++
		r.f.Log.Warn("attachment: a feed's document failed", "feed", r.fd.Name, "issuer_id", st.IssuerID, "error", tr.err)
		return nil
	case err != nil:
		return err
	case reason != "":
		r.refuse(st, reason)
	}
	err = r.write(ctx, false, func(tx *store.Tx) error { return tx.SetFeedCursor(ctx, r.fd.ID, st.BodyHash, st.Seq) })
	if err == nil {
		cursors[st.BodyHash] = st.Seq
	}
	return err
}

// write runs one of a run's writes (a cursor, what an export feed keeps) under the tenant's feed
// lock, and only while the feed's export setting is the one the run started with: a change of
// export resets or drops what the run would write, under the same lock (spec 0014), and asks for
// another run.
func (r *run) write(ctx context.Context, attachments bool, fn func(*store.Tx) error) error {
	locks := []string{store.FeedLock(r.t.ID)}
	if attachments {
		locks = append(locks, lock(r.t))
	}
	return r.f.Store.InTenantTx(ctx, r.t.ID, locks, func(tx *store.Tx) error {
		ok, err := tx.FeedEpochIs(ctx, r.fd.ID, r.fd.ExportEpoch)
		if err != nil {
			return err
		}
		if !ok {
			return errOvertaken
		}
		return fn(tx)
	})
}

// keepRefusal keeps an export feed's refusal with the statement's bytes (spec 0014).
func (r *run) keepRefusal(ctx context.Context, st Statement, stmt, sig []byte, reason string) error {
	err := r.write(ctx, true, func(tx *store.Tx) error {
		return tx.KeepFeedRefusal(ctx, r.t.ID, store.FeedRefusal{FeedID: r.fd.ID, BodyHash: st.BodyHash, Seq: st.Seq, Reason: reason,
			Statement: string(stmt), Signature: base64.StdEncoding.EncodeToString(sig)})
	})
	if errors.Is(err, store.ErrQuota) {
		return errHeldByQuota
	}
	return err
}

// keep keeps an export feed's statement about a document the tenant does not hold: a withdrawal of
// one never stored, or any statement about one removed here (spec 0014). Other feeds keep nothing.
func (r *run) keep(ctx context.Context, st Statement, stmt, sig []byte) error {
	if !r.fd.Export {
		return nil
	}
	changed, withdrew := false, false
	sigText := base64.StdEncoding.EncodeToString(sig)
	err := r.write(ctx, true, func(tx *store.Tx) error {
		changed, withdrew = false, false
		// read again under the lock: another feed of the source may have stored it meanwhile
		a, err := tx.FeedAttachment(ctx, r.t.ID, st.Issuer, st.IssuerID)
		switch {
		case err == nil && a.RemovedAt.IsZero():
			if !st.Withdrawn || a.Seq >= st.Seq || a.BodyHash != st.BodyHash {
				return nil // held now: what it says is the online path's to judge at the next run
			}
			err := tx.WithdrawFeedAttachment(ctx, &a, st.Seq, string(stmt), sigText)
			withdrew = err == nil
			if errors.Is(err, store.ErrConflict) {
				return nil
			}
			return err
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return err
		}
		changed, err = tx.KeepFeedStatement(ctx, store.FeedKept{TenantID: r.t.ID, Issuer: st.Issuer, IssuerID: st.IssuerID, BodyHash: st.BodyHash,
			Seq: st.Seq, Statement: string(stmt), Signature: sigText})
		return err
	})
	if errors.Is(err, store.ErrQuota) {
		return errHeldByQuota
	}
	if changed {
		r.res.Counts["kept"]++
	}
	if withdrew {
		r.res.Counts["withdrawn"]++
	}
	return err
}

// store stores a verified statement: a withdrawal marks its document, a newer statement about a
// document already held updates it, a new one is fetched and inserted. It answers a refusal's
// reason, a transient error, or another error that stops the run.
func (r *run) store(ctx context.Context, st Statement, stmt, sig []byte) (string, error) {
	t := r.t
	var existing *store.Attachment
	err := r.f.Store.InTx(ctx, "", func(tx *store.Tx) error {
		a, err := tx.FeedAttachment(ctx, t.ID, st.Issuer, st.IssuerID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		existing = &a
		return err
	})
	if err != nil {
		return "", err
	}
	sigText := base64.StdEncoding.EncodeToString(sig)
	if st.Withdrawn {
		if existing == nil || !existing.RemovedAt.IsZero() && existing.Seq < st.Seq {
			return "", r.keep(ctx, st, stmt, sig) // nothing held of it: an export feed keeps it for a site
		}
		if !existing.RemovedAt.IsZero() || existing.Seq >= st.Seq {
			return "", nil // already newer
		}
		if existing.BodyHash != st.BodyHash {
			return "body", nil
		}
		err := r.f.Store.InTenantTx(ctx, t.ID, []string{lock(t)}, func(tx *store.Tx) error {
			return tx.WithdrawFeedAttachment(ctx, existing, st.Seq, string(stmt), sigText)
		})
		if errors.Is(err, store.ErrConflict) {
			return "", nil // moved meanwhile
		}
		if err == nil {
			r.res.Counts["withdrawn"]++
		}
		return "", err
	}
	if len(r.fd.Kinds) > 0 && !slices.Contains(r.fd.Kinds, st.Kind) {
		return "kind", nil
	}
	if mt, err := CanonicalMediaType(st.Kind, st.MediaType); err != nil || mt != st.MediaType {
		return "media type", nil
	}
	if ValidTitle(st.Title) != nil {
		return "title", nil
	}
	if st.Size > r.f.Attachments.maxSize() {
		return "size", nil
	}
	issued, _ := time.Parse(time.RFC3339, st.IssuedAt)
	var until time.Time
	if st.ValidUntil != "" {
		until, _ = time.Parse(time.RFC3339, st.ValidUntil)
	}
	visibility := store.VisibleAdministrators
	if st.Audience == store.VisibleReaders && r.fd.Readers {
		visibility = store.VisibleReaders
	}
	if existing != nil {
		switch {
		case !existing.RemovedAt.IsZero():
			if existing.Seq < st.Seq {
				return "", r.keep(ctx, st, stmt, sig) // the tombstone keeps it out here; a site may hold it
			}
			return "", nil
		case existing.Seq >= st.Seq:
			return "", nil
		case existing.BodyHash != st.BodyHash || existing.Kind != st.Kind || existing.ContentHash != st.ContentHash:
			return "changed", nil // a document's content never changes (its link may: a removal at the source relinks)
		}
		upd := *existing
		upd.Seq, upd.Statement, upd.Signature, upd.KeyFP, upd.IssuedAt, upd.ValidUntil = st.Seq, string(stmt), sigText, st.Key, issued, until
		upd.Visibility = store.VisibleReaders // kept as it is here...
		if st.Audience != store.VisibleReaders {
			upd.Visibility = store.VisibleAdministrators // ...unless narrowed at the source; widening is the administrators'
		}
		err := r.f.Store.InTenantTx(ctx, t.ID, []string{lock(t)}, func(tx *store.Tx) error { return tx.ReissueFeedAttachment(ctx, &upd) })
		if errors.Is(err, store.ErrConflict) {
			return "", nil
		}
		if err == nil {
			r.res.Counts["updated"]++
		}
		return "", err
	}
	// a new document: its content from the fixed path, checked against the statement
	if err := r.renew(ctx); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	_, err = r.f.Fetch.Download(ctx, r.fd.URL+"/feed/documents/"+url.PathEscape(st.IssuerID),
		egress.DownloadOptions{MaxBytes: st.Size, Timeout: 2 * time.Minute, MinRate: 16 << 10, Header: r.header}, &buf)
	switch {
	case errors.Is(err, egress.ErrNotFound):
		return "", transient{err} // no longer offered (the next answer leaves the offer out), or a glitch
	case errors.Is(err, egress.ErrAuth):
		return "", fmt.Errorf("%w: %w", errAuth, err)
	case errors.Is(err, egress.ErrTooLarge):
		return "content", nil
	case err != nil:
		return "", transient{err}
	}
	content := buf.Bytes()
	if int64(len(content)) != st.Size || blob.ContentHash(content) != st.ContentHash {
		return "content", nil
	}
	summary, err := Summarize(st.Kind, st.MediaType, content)
	if err != nil {
		return "document", nil
	}
	rec := store.Attachment{ID: store.NewID(), TenantID: t.ID, BodyHash: st.BodyHash, Kind: st.Kind, MediaType: st.MediaType,
		Title: st.Title, Size: st.Size, ContentHash: st.ContentHash, Summary: summary, Origin: store.AttachedByFeed, AttachedBy: r.fd.ID,
		Visibility: visibility, Issuer: st.Issuer, IssuerID: st.IssuerID, KeyFP: st.Key, SourceOrigin: st.Origin, IssuedAt: issued,
		ValidUntil: until, Supersedes: st.Supersedes, Statement: string(stmt), Signature: sigText, Seq: st.Seq}
	if err := r.f.Attachments.Blob.PutAttachment(ctx, t.StorageDomain, rec.ID, content); err != nil {
		return "", transient{err}
	}
	reason := ""
	err = r.f.Store.InTenantTx(ctx, t.ID, []string{lock(t)}, func(tx *store.Tx) error {
		reason = ""
		held, err := tx.TouchBody(ctx, t.ID, st.BodyHash)
		if err != nil {
			return err
		}
		if !held {
			reason = "not held"
			return nil
		}
		err = tx.InsertAttachment(ctx, &rec)
		switch {
		case errors.Is(err, store.ErrExists):
			reason = "duplicate" // another feed stored it first
			return nil
		case errors.Is(err, store.ErrInvalid):
			reason = "limits"
			return nil
		case err != nil:
			return err
		}
		return applyKept(ctx, tx, &rec, r.fd.Keys)
	})
	if err != nil || reason != "" {
		if errors.Is(err, errHeldByQuota) {
			reason = ""
		}
		dctx := context.WithoutCancel(ctx)
		if _, gerr := r.f.Store.GetAttachment(dctx, t.ID, rec.ID); errors.Is(gerr, store.ErrNotFound) {
			_ = r.f.Attachments.Blob.DeleteAttachment(dctx, t.StorageDomain, rec.ID)
		}
		if reason == "duplicate" {
			r.res.Counts["duplicate"]++
			return "", nil
		}
		return reason, err
	}
	r.res.Counts["received"]++
	if rec.Supersedes != "" {
		r.res.Counts["superseded"]++
	}
	return "", nil
}

// applyKept applies to a document just stored what an export feed kept about it while it was not
// held: a later withdrawal of the same body, signed by a key the storing feed pins, marks it; the
// kept row goes either way (spec 0014).
func applyKept(ctx context.Context, tx *store.Tx, rec *store.Attachment, pinned []string) error {
	k, ok, err := tx.GetFeedKept(ctx, rec.TenantID, rec.Issuer, rec.IssuerID)
	if err != nil || !ok {
		return err
	}
	var w struct {
		Withdrawn bool   `json:"withdrawn"`
		Key       string `json:"key"`
		BodyHash  string `json:"body_hash"`
	}
	if k.Seq > rec.Seq && json.Unmarshal([]byte(k.Statement), &w) == nil && w.Withdrawn && w.BodyHash == rec.BodyHash &&
		slices.Contains(pinned, w.Key) {
		if err := tx.WithdrawFeedAttachment(ctx, rec, k.Seq, k.Statement, k.Signature); err != nil {
			return err
		}
	}
	return tx.DeleteFeedKept(ctx, rec.TenantID, rec.Issuer, rec.IssuerID)
}
