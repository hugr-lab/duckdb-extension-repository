package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0023: a publisher's request is signed. It carries Kista-Statement and Kista-Signature and no
// Authorization; the statement names the method, the path and query, the body's SHA-256 and the
// signing time, and the signature is an SSH signature over it by one of the publisher's keys.

// signedSkew is how far a statement's signed-at may be from the server's clock, either way.
const signedSkew = 5 * time.Minute

// errBodyNotSigned is a body whose SHA-256 is not the statement's: the reader returns it at the
// body's end, before anything is written.
var errBodyNotSigned = errors.New("api: the body is not the signed one")

// signedHeaders reports whether a request carries a signed request's headers, and checks their
// form: each exactly once, and no Authorization beside them.
func signedHeaders(r *http.Request) (st, sig string, signed bool, problemText string) {
	sts, sigs := r.Header.Values(auth.StatementHeader), r.Header.Values(auth.SignatureHeader)
	if len(sts) == 0 && len(sigs) == 0 {
		return "", "", false, ""
	}
	switch {
	case len(sts) != 1 || len(sigs) != 1:
		return "", "", true, "a signed request carries Kista-Statement and Kista-Signature, once each"
	case len(r.Header.Values("Authorization")) > 0:
		return "", "", true, "a signed request carries no Authorization header"
	}
	return sts[0], sigs[0], true, ""
}

// parseSigned decodes a signed request and checks that its statement names this request: the method
// as sent, and the path and query exactly as sent.
func parseSigned(r *http.Request, st, sig string) (auth.SignedRequest, error) {
	sr, err := auth.ParseSignedRequest(st, sig)
	if err != nil {
		return sr, err
	}
	switch {
	case sr.Statement.Method != r.Method:
		return sr, fmt.Errorf("%w: the statement's method is not this request's", auth.ErrStatement)
	case sr.Statement.Path != r.RequestURI:
		return sr, fmt.Errorf("%w: the statement's path is not this request's path and query, as sent", auth.ErrStatement)
	}
	return sr, nil
}

// signedFailure is a signed request that is not accepted: 401, naming the key kista found and its
// clock (never which check failed), so a developer can tell a wrong key from a skewed clock.
type signedFailure struct {
	reason      string // for the auth.failure event
	fingerprint string
	now         time.Time
}

func (f *signedFailure) Error() string { return "api: the signed request is not accepted: " + f.reason }

func (f *signedFailure) write(w http.ResponseWriter) {
	note(w, func(sw *statusWriter) { sw.authFailure = f.reason })
	writeProblem(w, http.StatusUnauthorized, typeUnauthorized, fmt.Sprintf("the signed request is not accepted: it is signed by key %s; "+
		"kista's clock is %s, and signed-at must be within five minutes of it", f.fingerprint, f.now.UTC().Format(time.RFC3339)), true)
}

// identifySigned verifies a signed request against the tenant's publisher keys: signed-at within
// five minutes, a key of the tenant that has not expired, the signature by that key. The caller is
// the key's publisher, issued at signed-at (spec 0006's freshness).
func (h *Handler) identifySigned(ctx context.Context, t store.Tenant, sr auth.SignedRequest) (caller, error) {
	c := caller{tenant: t}
	now := h.o.Now()
	fail := func(reason string) (caller, error) {
		return c, &signedFailure{reason: reason, fingerprint: sr.Fingerprint, now: now}
	}
	if d := now.Sub(sr.Statement.SignedAt); d > signedSkew || d < -signedSkew {
		return fail("signed_stale")
	}
	k, p, err := h.o.Store.PublisherKeyByFingerprint(ctx, t.ID, sr.Fingerprint)
	if errors.Is(err, store.ErrNotFound) {
		return fail("signed_key")
	}
	if err != nil {
		return c, err
	}
	if !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt) {
		return fail("signed_key")
	}
	wire, err := auth.WireOf(k.PublicKey)
	if err != nil {
		return c, fmt.Errorf("api: a stored publisher key: %w", err)
	}
	if err := sr.Verify(wire); err != nil {
		return fail("signed_signature")
	}
	if now.Sub(k.LastUsedAt) > time.Hour {
		if err := h.o.Store.TouchPublisherKey(ctx, k.ID); err != nil {
			h.o.Log.Warn("api: recording a key's use", "error", err)
		}
	}
	ta, err := h.o.Auths.Get(ctx, t)
	if err != nil {
		return c, err
	}
	id := auth.Identity{Principals: auth.Principals{auth.PublisherKey(p.ID): true}, IssuedAt: sr.Statement.SignedAt}
	c.principals, c.ta, c.id, c.verified = id.Principals, ta, id, true
	c.pub = &auth.Publication{Identity: id, Provider: "key",
		Publishers: []auth.MatchedPublisher{{Publisher: p, Credential: "key:" + sr.Fingerprint, KeyID: k.ID}}}
	c.signed = &sr
	return c, nil
}

// hashedBody is a signed request's body: it hashes what is read and, at the body's end (its
// Content-Length, or EOF), returns errBodyNotSigned when the hash is not the statement's, so a
// reader that reads to the end (the spool, the JSON reader) fails before anything is written. A
// reader limited to Content-Length never asks past it, so the end is the length when one is sent.
type hashedBody struct {
	rc         io.ReadCloser
	h          hash.Hash
	want       string
	length     int64 // -1: unknown
	read       int64
	mismatched bool
}

func newHashedBody(rc io.ReadCloser, want string, length int64) *hashedBody {
	return &hashedBody{rc: rc, h: sha256.New(), want: want, length: length}
}

func (b *hashedBody) Read(p []byte) (int, error) {
	if b.mismatched {
		return 0, errBodyNotSigned
	}
	n, err := b.rc.Read(p)
	b.h.Write(p[:n])
	b.read += int64(n)
	end := b.length >= 0 && b.read >= b.length || b.length < 0 && errors.Is(err, io.EOF)
	if end && hex.EncodeToString(b.h.Sum(nil)) != b.want {
		b.mismatched = true
		return n, errBodyNotSigned
	}
	return n, err
}

func (b *hashedBody) Close() error { return b.rc.Close() }

// statement and signature are a signed request's, as they came, for the release's record.
func (c caller) statement() string {
	if c.signed == nil {
		return ""
	}
	return string(c.signed.Statement.Raw)
}

func (c caller) signature() string {
	if c.signed == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(c.signed.Signature)
}
