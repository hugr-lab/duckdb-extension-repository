package upstream

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// The export of statements (spec 0014 phase 1b): for each of the plan's feeds, what the tenant's
// export feeds of the same URL hold, as the issuer signed it. No feed is contacted.

// ExportFeed is a feed's part of an export.
type ExportFeed struct {
	URL         string         `json:"url"`
	From        string         `json:"from,omitempty"`   // the export feed it came from
	Status      string         `json:"status,omitempty"` // the export feed's, so a stale gateway shows
	LastSuccess time.Time      `json:"last_success,omitzero"`
	Counts      map[string]int `json:"counts"` // bodies, statements, withdrawals, without_content, documents
	Bytes       int64          `json:"bytes"`  // batches and documents
	Error       string         `json:"error,omitempty"`
}

// Why a feed fails as a whole, and the notes on statements carried without content.
const (
	whyNotSubscribed = "not subscribed here"
	whyNotKept       = "not kept for export"
	whyFewerKinds    = "accepts fewer kinds"
	whyNeverRan      = "never ran"
	whyFeedLimits    = "over the bundle's limits for a feed"
	whyFeedChanged   = "changed during the export: export again"
	noteRemoved      = "removed on the exporting kista"
	noteNotHeld      = "not held on the exporting kista"
	noteRefused      = "refused on the exporting kista"
	noteKind         = "a kind the site's feed does not take"
)

// Limits on a feed's part of an export.
const (
	maxFeedStatements = 100000
	maxFeedManifest   = 16 << 20
)

// xstatement is a statement an export carries.
type xstatement struct {
	stmt, sig []byte
	st        attachment.Statement
	note      string
	doc       *store.Attachment // its live document, when the bundle carries its content
}

// xbatch is a batch spooled to a temporary file.
type xbatch struct {
	off, size int64
	docs      []*store.Attachment // the documents it first names, in order
}

// xfeed is a feed being exported.
type xfeed struct {
	keys    []byte
	batches []xbatch
	m       bundle.Feed
}

// covers reports whether a gateway feed accepting have takes every kind a site's feed accepting
// want takes (none: every kind).
func covers(have, want []string) bool {
	if len(have) == 0 {
		return true
	}
	if len(want) == 0 {
		return false
	}
	for _, k := range want {
		if !slices.Contains(have, k) {
			return false
		}
	}
	return true
}

// feeds builds the plan's feeds from the tenant's export feeds, spooling their batches to spool. It
// returns the feeds that made it, in plan order, and every feed's summary.
func (x *exporter) feeds(ctx context.Context, plan *bundle.Plan, m *bundle.Manifest, spool *os.File, budget int) ([]*xfeed, []ExportFeed, error) {
	if len(plan.Feeds) == 0 {
		return nil, nil, nil
	}
	var all []store.Feed
	if err := x.s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		all, err = tx.ListFeeds(ctx, x.t.ID)
		return err
	}); err != nil {
		return nil, nil, err
	}
	var made []*xfeed
	var outs []ExportFeed
	for i := range plan.Feeds {
		pf := &plan.Feeds[i]
		out := ExportFeed{URL: pf.URL, Counts: map[string]int{}}
		start, err := spool.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, nil, err
		}
		f, err := x.feed(ctx, plan, pf, m, all, spool, &out, len(made)+1)
		if err == nil { // the whole manifest stays readable: a feed past what is left fails alone
			if b, merr := json.Marshal(f.m); merr != nil || len(b)+1 > budget {
				err = feedFails(whyFeedLimits)
			} else {
				budget -= len(b) + 1
			}
		}
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, nil, ctx.Err()
		case errors.Is(err, errFeedFails):
			// nothing of it is in the bundle: neither its counts nor its spooled batches
			out.Error, out.Counts, out.Bytes = strings.TrimPrefix(err.Error(), errFeedFails.Error()+": "), map[string]int{}, 0
			if terr := spool.Truncate(start); terr != nil {
				return nil, nil, terr
			}
		case err != nil:
			return nil, nil, err
		default:
			made = append(made, f)
		}
		outs = append(outs, out)
	}
	return made, outs, nil
}

// errFeedFails is a feed failing as a whole: the bundle carries the others.
var errFeedFails = errors.New("feed fails")

func feedFails(why string) error { return fmt.Errorf("%w: %s", errFeedFails, why) }

// feed exports one plan feed as the n-th feed of the bundle.
func (x *exporter) feed(ctx context.Context, plan *bundle.Plan, pf *bundle.PlanFeed, m *bundle.Manifest, all []store.Feed,
	spool *os.File, out *ExportFeed, n int) (*xfeed, error) {
	// the tenant's online feed of the URL that hands it on (export): a tenant has at most one per
	// issuer (spec 0045). It must take every kind the site's feed takes, and have kept a keys file
	var gw store.Feed
	why := whyNotSubscribed
	for _, f := range all {
		if f.URL != pf.URL || f.Mode != store.FeedOnline {
			continue
		}
		if !f.Export {
			if gw.ID == "" {
				why = whyNotKept
			}
			continue
		}
		gw = f
		switch {
		case !covers(f.Kinds, pf.Kinds):
			why = whyFewerKinds
		case f.KeysFileAt.IsZero():
			why = whyNeverRan
		default:
			why = ""
		}
	}
	if why != "" {
		return nil, feedFails(why)
	}
	out.LastSuccess, out.Status = gw.LastSuccessAt, gw.Status
	out.From = "feeds " + gw.Name
	xf := &xfeed{m: bundle.Feed{URL: pf.URL, Bodies: []bundle.FeedBody{}}}
	if err := x.s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		xf.keys, _, err = tx.FeedKeysFile(ctx, gw.ID)
		return err
	}); err != nil {
		return nil, err
	}
	if len(xf.keys) == 0 {
		return nil, feedFails(whyNeverRan)
	}
	listed, err := attachment.ParseKeysFile(xf.keys)
	if err != nil {
		return nil, feedFails("its keys file does not parse")
	}
	pinned := map[string]*rsa.PublicKey{}
	for fp, k := range listed {
		if slices.Contains(pf.Keys, fp) {
			pinned[fp] = k
		}
	}
	cursors, err := x.s.Store.FeedCursors(ctx, gw.ID)
	if err != nil {
		return nil, err
	}

	// the bodies: the plan's, and from 0 those of the files the bundle brings for the feed's sources
	after := map[string]int64{}
	for _, b := range pf.Bodies {
		after[b.BodyHash] = b.Seq
	}
	for _, si := range pf.Sources {
		ps := plan.Sources[si]
		for _, s := range m.Sources {
			if s.Kind != ps.Kind || s.Prefix != ps.Prefix {
				continue
			}
			for _, c := range s.Cells {
				if _, ok := after[c.BodyHash]; !ok && c.Outcome == bundle.Fetched {
					after[c.BodyHash] = 0
				}
			}
		}
	}
	bodies := make([]string, 0, len(after))
	for b := range after {
		bodies = append(bodies, b)
	}
	sort.Strings(bodies)

	var stmts []xstatement
	var used []string // the bodies whose range the feed's cursor ends
	for _, body := range bodies {
		from, upTo := after[body], cursors[body]
		if upTo <= from {
			continue // the feed did not read past the site's cursor: nothing to carry
		}
		got, end, err := x.bodyStatements(ctx, pf, pinned, gw, body, from, upTo)
		if err != nil {
			return nil, err
		}
		if end <= from {
			continue
		}
		stmts = append(stmts, got...)
		if len(stmts) > maxFeedStatements {
			return nil, feedFails(whyFeedLimits)
		}
		xf.m.Bodies = append(xf.m.Bodies, bundle.FeedBody{BodyHash: body, AfterSeq: from, Seq: end})
		used = append(used, body)
		out.Counts["bodies"]++
	}
	// the reads above are not one snapshot: a change of export (its refusals and kept statements
	// dropped) or the collector (a body's rows gone with its cursor) meanwhile would leave a range
	// that hides statements; such a feed fails, to be exported again
	if changed, err := x.changed(ctx, gw, cursors, used); err != nil {
		return nil, err
	} else if changed {
		return nil, feedFails(whyFeedChanged)
	}
	if err := x.spoolFeed(xf, stmts, spool, n, out); err != nil {
		return nil, err
	}
	if len(xf.batches) > bundle.MaxFeedEntries || out.Counts["documents"] > bundle.MaxFeedEntries {
		return nil, feedFails(whyFeedLimits)
	}
	if b, err := json.Marshal(xf.m); err != nil || len(b) > maxFeedManifest {
		return nil, feedFails(whyFeedLimits)
	}
	return xf, nil
}

// changed reports whether the feed's export setting or a used cursor moved since they were read.
func (x *exporter) changed(ctx context.Context, gw store.Feed, cursors map[string]int64, used []string) (bool, error) {
	var now store.Feed
	err := x.s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		now, err = tx.GetFeedByID(ctx, gw.ID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !now.Export || now.ExportEpoch != gw.ExportEpoch {
		return true, nil
	}
	again, err := x.s.Store.FeedCursors(ctx, gw.ID)
	if err != nil {
		return false, err
	}
	for _, body := range used {
		if again[body] < cursors[body] {
			return true, nil
		}
	}
	return false, nil
}

// bodyStatements collects a body's statements in (from, upTo] as the tenant holds them
// (attachment.HeldStatements, shared with the online relay of spec 0045): its documents of the
// issuer (whichever feed stored them), the statements the export feed kept, its
// refusals, among them the statements that wait for this kista's upgrade (spec 0046: carried
// without content, their note the waiting reason); at one seq the first of these wins. The
// range ends just below a statement that does not verify with the plan's keys, or whose live
// document cannot be read.
func (x *exporter) bodyStatements(ctx context.Context, pf *bundle.PlanFeed, pinned map[string]*rsa.PublicKey, best store.Feed,
	body string, from, upTo int64) ([]xstatement, int64, error) {
	held, err := attachment.HeldStatements(ctx, x.s.Store, x.t.ID, pf.URL, best.ID, body, from, upTo)
	if err != nil {
		return nil, 0, err
	}
	var out []xstatement
	for _, c := range held {
		q := c.Seq
		// why the exporting side holds no content for it: its refusal's reason, a removal, a document
		// it never held
		note := ""
		switch {
		case c.Removed:
			note = noteRemoved
		case c.NotHeld:
			note = noteNotHeld
		case c.Doc == nil:
			if note = bundle.Text(c.Refused, bundle.MaxDetail); note == "" {
				note = noteRefused
			}
		}
		stmt := []byte(c.Statement)
		sig, err := base64.StdEncoding.DecodeString(c.Signature)
		if err != nil {
			return out, q - 1, nil
		}
		st, err := attachment.Verify(stmt, sig, pinned)
		// authentic, with a member out of its bounds: refused for good here, and carried so the site
		// refuses it too (spec 0046); it has no document
		malformed := errors.Is(err, attachment.ErrMalformed) && c.Doc == nil
		if err != nil && !malformed || st.Issuer != pf.URL || st.BodyHash != body || st.Seq != q {
			return out, q - 1, nil // as an unverified statement holds a cursor online
		}
		xs := xstatement{stmt: stmt, sig: sig, st: st, note: note}
		switch {
		case malformed:
		case st.Withdrawn:
			xs.note = ""
		case c.Doc != nil && len(pf.Kinds) > 0 && !slices.Contains(pf.Kinds, st.Kind):
			// online, the site's feed refuses it before fetching: its content does not leave
			xs.note = noteKind
		case c.Doc != nil && c.Doc.ContentHash == st.ContentHash && c.Doc.Size == st.Size:
			err := x.s.Blob.ReadAttachment(ctx, x.t.StorageDomain, c.Doc.ID, st.ContentHash, st.Size, func([]byte) error { return nil })
			if errors.Is(err, blob.ErrCorrupt) {
				return out, q - 1, nil // its object missing or corrupt: the site moves no further
			}
			if err != nil {
				return nil, 0, err
			}
			xs.doc = c.Doc
		case c.Doc != nil:
			return out, q - 1, nil // a record that does not match its own statement
		}
		out = append(out, xs)
	}
	return out, upTo, nil
}

// spoolFeed writes a feed's statements in batches of at most bundle.MaxBatch to spool, each batch
// followed by the documents it first names, and fills the manifest's entries.
func (x *exporter) spoolFeed(xf *xfeed, stmts []xstatement, spool *os.File, n int, out *ExportFeed) error {
	ks := sha256.Sum256(xf.keys)
	xf.m.Entries = []bundle.FileRef{{Path: bundle.FeedPath(n, "attestation-keys.json"), SHA256: hex.EncodeToString(ks[:]), Size: int64(len(xf.keys))}}
	out.Bytes += int64(len(xf.keys))
	placed := map[string]bool{}
	var cur []bundle.BatchEntry
	var docs []*store.Attachment
	size := int64(2) // []
	flush := func() error {
		if len(cur) == 0 {
			return nil
		}
		b, err := bundle.MarshalBatch(cur)
		if err != nil {
			return err
		}
		off, err := spool.Seek(0, io.SeekEnd)
		if err != nil {
			return err
		}
		if _, err := spool.Write(b); err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		k := len(xf.batches) + 1
		xf.batches = append(xf.batches, xbatch{off: off, size: int64(len(b)), docs: docs})
		xf.m.Entries = append(xf.m.Entries, bundle.FileRef{Path: bundle.FeedPath(n, fmt.Sprintf("batch-%d.json", k)),
			SHA256: hex.EncodeToString(sum[:]), Size: int64(len(b))})
		out.Bytes += int64(len(b))
		for _, d := range docs {
			xf.m.Entries = append(xf.m.Entries, bundle.FileRef{Path: bundle.FeedPath(n, "documents/"+d.ContentHash), SHA256: d.ContentHash,
				Size: d.Size})
			out.Bytes += d.Size
			out.Counts["documents"]++
		}
		cur, docs, size = nil, nil, 2
		return nil
	}
	for _, s := range stmts {
		e := bundle.BatchEntry{Statement: base64.StdEncoding.EncodeToString(s.stmt), Signature: base64.StdEncoding.EncodeToString(s.sig), Note: s.note}
		eb, _ := json.Marshal(e)
		if size+int64(len(eb))+1 > bundle.MaxBatch {
			if err := flush(); err != nil {
				return err
			}
		}
		cur, size = append(cur, e), size+int64(len(eb))+1
		out.Counts["statements"]++
		switch {
		case s.st.Withdrawn:
			out.Counts["withdrawals"]++
		case s.note != "":
			out.Counts["without_content"]++
		case s.doc != nil && !placed[s.doc.ContentHash]:
			placed[s.doc.ContentHash] = true
			docs = append(docs, s.doc)
		}
	}
	return flush()
}

// writeFeeds writes the feeds' entries after the files: the keys file, then each batch from the
// spool followed by its documents, read again and checked.
func (x *exporter) writeFeeds(ctx context.Context, bw *bundle.Writer, feeds []*xfeed, spool *os.File) error {
	for _, xf := range feeds {
		e := xf.m.Entries
		if err := bw.Entry(e[0].Path, e[0].Size, strings.NewReader(string(xf.keys))); err != nil {
			return err
		}
		i := 1
		for _, b := range xf.batches {
			if err := bw.Entry(e[i].Path, b.size, io.NewSectionReader(spool, b.off, b.size)); err != nil {
				return err
			}
			i++
			for _, d := range b.docs {
				// removed meanwhile (the collector, an administrator), it fails the export: export again
				err := x.s.Blob.ReadAttachment(ctx, x.t.StorageDomain, d.ID, d.ContentHash, d.Size, func(content []byte) error {
					return bw.Entry(e[i].Path, int64(len(content)), bytes.NewReader(content))
				})
				if err != nil {
					return fmt.Errorf("upstream: feed %s: %s: %w", xf.m.URL, e[i].Path, err)
				}
				i++
			}
		}
	}
	return nil
}
