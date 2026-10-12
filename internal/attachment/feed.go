package attachment

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// The feed's source side (spec 0011 phase 1b): statements issued when a document is marked
// feedable (and withdrawals when it is un-marked or removed), and the feed route that answers a
// subscriber's bodies with them.

// Feed limits.
const (
	MaxFeedBodies = 500     // bodies per feed request
	MaxFeedAnswer = 1 << 20 // an answer's statements, encoded
)

// Issuer is a tenant's stable identity in its statements: its URL under the deployment's public URL.
func Issuer(publicURL, tenant string) string {
	return strings.TrimSuffix(publicURL, "/") + "/api/v1/tenants/" + tenant
}

// sourceOrigin is who stands behind a document at its source: the tenant, or publisher:<name>.
func sourceOrigin(rec store.Attachment) string {
	if rec.Origin != store.AttachedByPublisher {
		return store.AttachedByTenant
	}
	_, names, _ := strings.Cut(strings.TrimPrefix(rec.AttachedBy, "publisher:"), "/")
	o := "publisher:" + names
	if len(o) > 80 {
		o = o[:80]
	}
	return o
}

// issue numbers and signs a statement about a tenant's own document (an offer, or its withdrawal)
// in the transaction that changes it, with the tenant's active attestation key.
func (s *Service) issue(ctx context.Context, tx *store.Tx, t store.Tenant, rec store.Attachment, withdrawn bool) (int64, error) {
	if s.PublicURL == "" || s.Keys == nil {
		return 0, fmt.Errorf("%w: this deployment issues no statements (serve.public_url)", store.ErrInvalid)
	}
	keys, err := tx.AttestationKeys(ctx, t.ID)
	if err != nil {
		return 0, err
	}
	var active *store.Key
	for i := range keys {
		if keys[i].State == store.KeyActive {
			active = &keys[i]
		}
	}
	if active == nil {
		return 0, fmt.Errorf("%w: the tenant has no active attestation key", store.ErrInvalid)
	}
	sg, err := s.Keys.OpenAttestationSigner(ctx, *active)
	if err != nil {
		return 0, err
	}
	seq, err := tx.NextStatementSeq(ctx, t.ID)
	if err != nil {
		return 0, err
	}
	now := tx.Now()
	st := Statement{Issuer: Issuer(s.PublicURL, t.Name), Key: active.Fingerprint, IssuerID: rec.IssuerID, Seq: seq,
		BodyHash: rec.BodyHash, IssuedAt: Timestamp(now), Withdrawn: withdrawn}
	if !withdrawn {
		st.Kind, st.MediaType, st.ContentHash, st.Size, st.Title = rec.Kind, rec.MediaType, rec.ContentHash, rec.Size, rec.Title
		st.Summary, st.Supersedes, st.Origin, st.Audience = json.RawMessage(rec.Summary), rec.Supersedes, sourceOrigin(rec), rec.Visibility
		if !rec.ValidUntil.IsZero() {
			st.ValidUntil = Timestamp(rec.ValidUntil)
		}
	}
	b, err := st.Canonical()
	if err != nil {
		return 0, err
	}
	if len(b) > MaxStatement {
		return 0, fmt.Errorf("%w: the statement would be %d bytes", store.ErrInvalid, len(b))
	}
	sig, err := Sign(ctx, sg, b)
	if err != nil {
		return 0, err
	}
	return seq, tx.InsertStatement(ctx, store.IssuedStatement{TenantID: t.ID, Seq: seq, IssuerID: rec.IssuerID, BodyHash: rec.BodyHash,
		Withdrawn: withdrawn, Statement: b, Signature: sig, IssuedAt: now})
}

// FeedBody is a body a subscriber asks about, with the last seq it received for it (0: none, so
// it receives the body's whole history).
type FeedBody struct {
	BodyHash string `json:"body_hash"`
	AfterSeq int64  `json:"after_seq"`
}

// WireStatement is a statement as the feed sends it: its exact bytes and signature, base64.
type WireStatement struct {
	Statement string `json:"statement"`
	Signature string `json:"signature"`
}

// HighWater is how far a body's statements were read: a subscriber that stored what came before it
// may move its cursor there (statements left out were about documents no longer offered).
type HighWater struct {
	BodyHash string `json:"body_hash"`
	Seq      int64  `json:"seq"`
}

// FeedAnswer is the feed route's answer: statements in seq order per body, the bodies read through,
// and more when it stopped early (the subscriber asks again from its cursors).
type FeedAnswer struct {
	Statements []WireStatement `json:"statements"`
	Bodies     []HighWater     `json:"bodies"`
	More       bool            `json:"more"`
}

// feedChannels are the ids of the channels where an actor holds feed.
func (s *Service) feedChannels(ctx context.Context, a authz.Actor, tenant string) ([]string, error) {
	chs, err := s.Store.ListChannels(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range chs {
		ok, err := s.allowed(ctx, a, authz.VerbFeed, authz.Resource{Tenant: tenant, Channel: c.Name})
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, c.ID)
		}
	}
	if len(out) == 0 {
		return nil, authz.ErrDenied
	}
	return out, nil
}

// Feed answers a subscriber's bodies with the statements issued about them after its cursors: only
// for bodies released in the channels its feed grant covers (any other body answers like one
// without documents), only documents still offered, and withdrawals.
func (s *Service) Feed(ctx context.Context, a authz.Actor, tenant string, bodies []FeedBody) (FeedAnswer, error) {
	out := FeedAnswer{Statements: []WireStatement{}, Bodies: []HighWater{}}
	if len(bodies) > MaxFeedBodies {
		return out, fmt.Errorf("%w: at most %d bodies a request", store.ErrInvalid, MaxFeedBodies)
	}
	seen := map[string]bool{}
	var hashes []string
	for _, b := range bodies {
		if !hexHash(b.BodyHash) || b.AfterSeq < 0 || seen[b.BodyHash] {
			return out, fmt.Errorf("%w: a body is a distinct 64-hex body_hash with after_seq ≥ 0", store.ErrInvalid)
		}
		seen[b.BodyHash] = true
		hashes = append(hashes, b.BodyHash)
	}
	channels, err := s.feedChannels(ctx, a, tenant)
	if err != nil {
		return out, err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return out, err
	}
	inScope, err := s.Store.ReleasedIn(ctx, t.ID, channels, hashes)
	if err != nil {
		return out, err
	}
	size := 64
	for _, b := range bodies {
		if !inScope[b.BodyHash] {
			continue
		}
		after, high := b.AfterSeq, b.AfterSeq
		for {
			page, err := s.Store.BodyStatements(ctx, t.ID, b.BodyHash, after, 100)
			if err != nil {
				return out, err
			}
			for _, st := range page {
				after = st.Seq
				if !st.Withdrawn {
					offered, err := s.Store.StatementOffered(ctx, t.ID, st.IssuerID, st.Seq)
					if err != nil {
						return out, err
					}
					if !offered {
						high = st.Seq
						continue
					}
				}
				w := WireStatement{Statement: base64.StdEncoding.EncodeToString(st.Statement), Signature: base64.StdEncoding.EncodeToString(st.Signature)}
				n := len(w.Statement) + len(w.Signature) + 40
				if size+n > MaxFeedAnswer {
					out.More = true
					if high > b.AfterSeq {
						out.Bodies = append(out.Bodies, HighWater{BodyHash: b.BodyHash, Seq: high})
					}
					return out, nil
				}
				size += n
				out.Statements = append(out.Statements, w)
				high = st.Seq
			}
			if len(page) < 100 {
				break
			}
		}
		if high > b.AfterSeq {
			out.Bodies = append(out.Bodies, HighWater{BodyHash: b.BodyHash, Seq: high})
			size += 100
		}
	}
	return out, nil
}

// FeedDocument hands a document a subscriber's statement names, and its content, to use: a tenant's
// own document, still offered, of a body released in the channels the caller's feed grant covers;
// any other answers like a missing one.
func (s *Service) FeedDocument(ctx context.Context, a authz.Actor, tenant, issuerID string, use func(store.Attachment, []byte) error) error {
	channels, err := s.feedChannels(ctx, a, tenant)
	if err != nil {
		return err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return err
	}
	missing := fmt.Errorf("%w: document %s", store.ErrNotFound, issuerID)
	rec, err := s.Store.GetAttachment(ctx, t.ID, issuerID)
	if errors.Is(err, store.ErrNotFound) {
		return missing
	}
	if err != nil {
		return err
	}
	if rec.Issuer != "" || !rec.Feedable || !rec.RemovedAt.IsZero() {
		return missing
	}
	latest, err := s.Store.LatestStatement(ctx, t.ID, rec.IssuerID)
	if errors.Is(err, store.ErrNotFound) {
		return missing
	}
	if err != nil {
		return err
	}
	if latest.Withdrawn {
		return missing
	}
	inScope, err := s.Store.ReleasedIn(ctx, t.ID, channels, []string{rec.BodyHash})
	if err != nil {
		return err
	}
	if !inScope[rec.BodyHash] {
		return missing
	}
	return s.Blob.ReadAttachment(ctx, t.StorageDomain, rec.ID, rec.ContentHash, rec.Size, func(b []byte) error { return use(rec, b) })
}
