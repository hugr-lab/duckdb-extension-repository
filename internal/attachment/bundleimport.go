package attachment

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Statements from a bundle (spec 0014 phase 1b): a bundle-mode feed takes a bundle's statements as
// a run takes its source's answer, with each document's content from the bundle. The bundle's
// reader (internal/upstream) drives it: the keys file, each batch, each document, then Finish.

// BundleStatement is a statement a bundle carries, decoded, with the exporting side's note when it
// carries no content for it.
type BundleStatement struct {
	Statement, Signature []byte
	Note                 string
}

// FeedImport is one target feed's part of a bundle import.
type FeedImport struct {
	f       *Feeds
	r       *run
	dryRun  bool
	started time.Time
	bundle  string // the manifest's id, for the run's record

	ranges   map[string][2]int64 // body: (after_seq, seq] the bundle covers
	offered  map[string]bool     // the bodies the feed offers now (in a dry run, with the bundle's)
	extra    map[string]bool
	cursors  map[string]int64
	verified map[string]int64 // body: the highest seq verified in the bundle
	last     map[string]int64 // body: the highest seq stored or refused for good
	held     map[string]bool  // body: a statement held it; nothing after it is applied
	gap      map[string]bool  // body: its range does not start where the feed is

	pending []BundleStatement // the last batch: applied once its documents were read
	docs    map[string]string // content hash: its spooled file
	dir     string
	keys    bool
	err     error // the target failed: nothing more is applied
}

// ImportFeed starts a bundle-mode feed's import: ranges are the bundle's per body; extra are the
// bodies the bundle brings, counted as held by a dry run. The caller holds the feed's lease
// (kista/feed/<id>) and renews it with renew.
func (f *Feeds) ImportFeed(ctx context.Context, t store.Tenant, fd store.Feed, manifestID string, ranges map[string][2]int64, extra []string,
	dryRun bool, renew func(context.Context) error) (*FeedImport, error) {
	if fd.Mode != store.FeedBundle {
		return nil, fmt.Errorf("%w: feed %s is not in bundle mode", store.ErrInvalid, fd.Name)
	}
	fi := &FeedImport{f: f, dryRun: dryRun, started: time.Now().UTC(), bundle: manifestID, ranges: ranges, offered: map[string]bool{},
		extra: map[string]bool{}, verified: map[string]int64{}, last: map[string]int64{}, held: map[string]bool{}, gap: map[string]bool{},
		docs: map[string]string{}}
	fi.r = &run{f: f, fd: fd, t: t, res: &RunResult{Started: fi.started, Counts: map[string]int{}}, renew: renew}
	fi.r.content = fi.content
	for _, b := range extra {
		fi.extra[b] = true
	}
	var err error
	if fi.cursors, err = f.Store.FeedCursors(ctx, fd.ID); err != nil {
		return nil, err
	}
	for body, rg := range ranges {
		if rg[0] > fi.cursors[body] {
			fi.gap[body] = true // a plan written before a reset, or a cursor the collector removed
			fi.r.res.Counts["gap"]++
		}
	}
	return fi, nil
}

// Gaps is the number of bodies whose range did not start where the feed is: write a new plan.
func (fi *FeedImport) Gaps() int { return fi.r.res.Counts["gap"] }

// Failed reports why the target failed, if it did.
func (fi *FeedImport) Failed() error { return fi.err }

// Fail fails the target (damaged entries, a lost lease): nothing more is applied, no cursor moves.
func (fi *FeedImport) Fail(err error) {
	if fi.err == nil {
		fi.err = err
	}
}

// Keys takes the bundle's keys file: the keys the feed pins among it.
func (fi *FeedImport) Keys(data []byte) {
	if fi.err != nil {
		return
	}
	all, err := ParseKeysFile(data)
	if err != nil {
		fi.Fail(err)
		return
	}
	fi.r.pinned = map[string]*rsa.PublicKey{}
	for fp, k := range all {
		if slices.Contains(fi.r.fd.Keys, fp) {
			fi.r.pinned[fp] = k
		}
	}
	if len(fi.r.pinned) == 0 {
		fi.Fail(fmt.Errorf("%w: the bundle's keys file lists none of the pinned keys", ErrStatement))
		return
	}
	fi.keys = true
}

// Batch takes a batch: the previous one is applied first, its documents all read by now.
func (fi *FeedImport) Batch(ctx context.Context, stmts []BundleStatement) error {
	if err := fi.flush(ctx); err != nil {
		return err
	}
	fi.pending = stmts
	return nil
}

// Document spools a document's content, its hash checked by the bundle's reader.
func (fi *FeedImport) Document(hash string, content []byte) error {
	if fi.err != nil {
		return nil
	}
	if fi.dir == "" {
		if fi.f.TempDir != "" {
			if err := os.MkdirAll(fi.f.TempDir, 0o700); err != nil {
				return err
			}
		}
		d, err := os.MkdirTemp(fi.f.TempDir, "kista-feed-import-*")
		if err != nil {
			return err
		}
		fi.dir = d
	}
	p := filepath.Join(fi.dir, hash)
	if err := os.WriteFile(p, content, 0o600); err != nil {
		return err
	}
	fi.docs[hash] = p
	return nil
}

// content gives a document's content from the bundle: none carried is a refusal for good.
func (fi *FeedImport) content(_ context.Context, st Statement) ([]byte, string, error) {
	p, ok := fi.docs[st.ContentHash]
	if !ok {
		return nil, "bundle: no content", nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, "", transient{err}
	}
	return b, "", nil
}

func (fi *FeedImport) flush(ctx context.Context) error {
	stmts := fi.pending
	fi.pending = nil
	if fi.err != nil || len(stmts) == 0 {
		return nil
	}
	if !fi.keys {
		fi.Fail(fmt.Errorf("%w: the bundle carries no keys file before its statements", ErrStatement))
		return nil
	}
	if len(fi.offered) == 0 { // read once the files are in: a body the bundle brings is held now
		bodies, err := fi.f.Store.OfferedBodies(ctx, fi.r.fd)
		if err != nil {
			return err
		}
		for _, b := range bodies {
			fi.offered[b] = true
		}
		fi.r.res.Counts["offered"] = len(bodies)
	}
	for _, s := range stmts {
		if err := fi.apply(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// apply takes one statement as a run's one and store do: verified under the feed's pins, its
// issuer the feed's URL, its body offered and within the bundle's range; then stored, or refused
// for good; a statement that does not verify holds its body.
func (fi *FeedImport) apply(ctx context.Context, s BundleStatement) error {
	r := fi.r
	hold := func(reason string) {
		r.refuse(Statement{}, reason)
		r.res.Counts["held"]++
		var claimed struct {
			BodyHash string `json:"body_hash"`
		}
		if json.Unmarshal(s.Statement, &claimed) == nil {
			if _, ok := fi.ranges[claimed.BodyHash]; ok {
				fi.held[claimed.BodyHash] = true
				return
			}
		}
		for b := range fi.ranges { // its body cannot be told: every body waits, as online
			fi.held[b] = true
		}
	}
	st, err := Verify(s.Statement, s.Signature, r.pinned)
	if err != nil {
		hold("signature")
		return nil
	}
	if st.Issuer != r.fd.URL {
		hold("issuer")
		return nil
	}
	rg, ok := fi.ranges[st.BodyHash]
	switch {
	case !ok || st.Seq <= rg[0] || st.Seq > rg[1]:
		r.refuse(st, "bundle: outside its range")
		return nil
	case fi.held[st.BodyHash] || fi.gap[st.BodyHash]:
		return nil
	case !fi.offered[st.BodyHash] && (!fi.dryRun || !fi.extra[st.BodyHash]):
		r.res.Counts["not offered"]++
		return nil
	case st.Seq <= fi.cursors[st.BodyHash]:
		return nil // a replay
	}
	fi.verified[st.BodyHash] = max(fi.verified[st.BodyHash], st.Seq)
	if fi.dryRun {
		fi.check(st, s)
		return nil
	}
	reason, err := r.store(ctx, st, s.Statement, s.Signature)
	var tr transient
	switch {
	case errors.As(err, &tr):
		fi.held[st.BodyHash] = true
		r.res.Counts["failed"]++
		r.f.Log.Warn("attachment: a bundle's document failed", "feed", r.fd.Name, "issuer_id", st.IssuerID, "error", tr.err)
		return nil
	case err != nil:
		return err
	case reason != "":
		if reason == "bundle: no content" && s.Note != "" {
			reason = "bundle: no content (" + s.Note + ")"
		}
		r.refuse(st, reason)
	}
	fi.last[st.BodyHash] = st.Seq
	return nil
}

// check is a dry run's judgement of a statement: what a run would do, nothing stored.
func (fi *FeedImport) check(st Statement, s BundleStatement) {
	r := fi.r
	switch {
	case st.Withdrawn:
		r.res.Counts["would_withdraw"]++
		return
	case len(r.fd.Kinds) > 0 && !slices.Contains(r.fd.Kinds, st.Kind):
		r.refuse(st, "kind")
		return
	case ValidTitle(st.Title) != nil:
		r.refuse(st, "title")
		return
	case st.Size > r.f.Attachments.maxSize():
		r.refuse(st, "size")
		return
	}
	if mt, err := CanonicalMediaType(st.Kind, st.MediaType); err != nil || mt != st.MediaType {
		r.refuse(st, "media type")
		return
	}
	b, why, err := fi.content(context.Background(), st)
	switch {
	case err != nil || why != "":
		r.refuse(st, "bundle: no content")
	case int64(len(b)) != st.Size || blob.ContentHash(b) != st.ContentHash:
		r.refuse(st, "content")
	default:
		if _, err := Summarize(st.Kind, st.MediaType, b); err != nil {
			r.refuse(st, "document")
			return
		}
		r.res.Counts["would_store"]++
	}
}

// Finish applies the last batch and, when the bundle's entries of the feed were read whole, moves
// each body's cursor: to the end of its range, no further than a statement verified in the bundle
// or before (stricter than a run, which caps by the whole run's), or to the last statement taken
// before a hold. It records the feed's run (a dry run only its event) and returns it.
func (fi *FeedImport) Finish(ctx context.Context, actor string, whole bool) (RunResult, error) {
	defer func() {
		if fi.dir != "" {
			_ = os.RemoveAll(fi.dir)
		}
	}()
	if err := fi.flush(ctx); err != nil {
		fi.Fail(err)
	}
	r := fi.r
	if fi.err == nil && !whole {
		fi.Fail(errors.New("attachment: the bundle's entries of the feed were not read whole"))
	}
	fctx := context.WithoutCancel(ctx)
	if fi.err == nil && !fi.dryRun {
		for body, rg := range fi.ranges {
			if fi.gap[body] || !fi.offered[body] {
				continue
			}
			to := min(rg[1], max(fi.verified[body], fi.cursors[body]))
			if fi.held[body] {
				to = fi.last[body]
			}
			if to <= fi.cursors[body] {
				continue
			}
			if err := fi.f.Store.InTenantTx(fctx, r.t.ID, nil, func(tx *store.Tx) error { return tx.SetFeedCursor(fctx, r.fd.ID, body, to) }); err != nil {
				fi.Fail(err)
				break
			}
		}
	}
	res := *r.res
	res.Ended = time.Now().UTC()
	if fi.err != nil {
		res.Error = clip(fi.err.Error())
	}
	status := store.FeedOK
	if fi.err != nil {
		status = store.FeedFailing
	}
	b, _ := json.Marshal(res)
	reasons := map[string]int{}
	for _, x := range res.Refused {
		reasons[x.Reason]++
	}
	err := fi.f.Store.InTenantTx(fctx, r.t.ID, nil, func(tx *store.Tx) error {
		if !fi.dryRun {
			if err := tx.FinishFeedRun(fctx, r.fd.ID, string(b), status, res.Error, res.Started, time.Time{}); err != nil {
				return err
			}
		}
		return tx.Event(fctx, r.t.ID, actor, "feed.run", "feed:"+r.fd.Name, map[string]any{"feed": r.fd.Name, "counts": res.Counts,
			"refused": reasons, "error": runClass(fi.err), "bundle": fi.bundle, "dry_run": fi.dryRun})
	})
	return res, err
}
