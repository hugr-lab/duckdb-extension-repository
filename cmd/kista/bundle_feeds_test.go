package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
)

type rsaSigner struct{ k *rsa.PrivateKey }

func (s rsaSigner) Sign(_ context.Context, h extfile.BodyHash) ([]byte, error) {
	return rsa.SignPKCS1v15(rand.Reader, s.k, crypto.SHA256, h[:])
}
func (s rsaSigner) Public() *rsa.PublicKey { return &s.k.PublicKey }
func (s rsaSigner) ID() string             { return "test" }

// feedFixture writes an unsigned bundle carrying one feed (a review with its document, a
// withdrawal, a statement without content) and no source, and its plan; planSeq is the plan's
// cursor for the body, pinned the key the plan pins.
func feedFixture(t *testing.T, dir, bundleFP string, planSeq int64, pinned string) (plan, unsigned, fp string) {
	t.Helper()
	ak, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fp = extfile.Fingerprint(&ak.PublicKey)
	if pinned == "" {
		pinned = fp
	}
	pemStr, _ := extfile.MarshalPublicKeyPEM(&ak.PublicKey)
	keysFile, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"fingerprint": fp, "public_key": pemStr}}})
	const url = "https://gw.example.org/api/v1/tenants/reviews"
	body := strings.Repeat("b", 64)
	doc := []byte(`{"schema":"kista.review.v1","verdict":"approved"}`)
	sign := func(seq int64, mod func(*attachment.Statement)) bundle.BatchEntry {
		s := attachment.Statement{Issuer: url, Key: fp, IssuerID: "d" + string(rune('0'+seq)), Seq: seq, BodyHash: body, Kind: "review",
			MediaType: attachment.Review, ContentHash: blob.ContentHash(doc), Size: int64(len(doc)), Title: "Review",
			IssuedAt: "2026-10-03T12:00:00Z", Origin: "tenant", Audience: "administrators"}
		if mod != nil {
			mod(&s)
		}
		b, err := s.Canonical()
		if err != nil {
			t.Fatal(err)
		}
		sig, err := attachment.Sign(context.Background(), rsaSigner{ak}, b)
		if err != nil {
			t.Fatal(err)
		}
		return bundle.BatchEntry{Statement: base64.StdEncoding.EncodeToString(b), Signature: base64.StdEncoding.EncodeToString(sig)}
	}
	noContent := sign(3, func(s *attachment.Statement) { s.IssuerID = "d9" })
	noContent.Note = "kind on the exporting kista"
	batch, _ := bundle.MarshalBatch([]bundle.BatchEntry{sign(1, nil), sign(2, func(s *attachment.Statement) {
		s.IssuerID, s.Withdrawn, s.Kind, s.MediaType, s.ContentHash, s.Size, s.Title = "d0", true, "", "", "", 0, ""
	}), noContent})
	ref := func(p string, b []byte) bundle.FileRef {
		return bundle.FileRef{Path: p, SHA256: sha(b), Size: int64(len(b))}
	}
	entries := []bundle.FileRef{ref("feeds/1/attestation-keys.json", keysFile), ref("feeds/1/batch-1.json", batch),
		ref("feeds/1/documents/"+sha(doc), doc)}
	data := [][]byte{keysFile, batch, doc}
	now := time.Now().UTC().Truncate(time.Second)
	m := bundle.Manifest{Format: bundle.ManifestFormat, ID: "0192f0c4-1b2a-7c3d-8e4f-0123456789ab", CreatedAt: now,
		Exporter: bundle.Exporter{Version: "kista test"}, Plan: bundle.PlanRef{Tenant: "acme", CreatedAt: now}, Sources: []bundle.Source{},
		Feeds: []bundle.Feed{{URL: url, Entries: entries, Bodies: []bundle.FeedBody{{BodyHash: body, AfterSeq: 0, Seq: 3}}}}}
	p := bundle.Plan{Format: bundle.PlanFormat, CreatedAt: now, Tenant: "acme", BundleKeys: []string{bundleFP}, Sources: []bundle.PlanSource{},
		Feeds: []bundle.PlanFeed{{URL: url, Keys: []string{pinned}, Kinds: []string{}, Sources: []int{},
			Bodies: []bundle.PlanBody{{BodyHash: body, Seq: planSeq}}}}}
	planData, _ := json.Marshal(p)
	plan = filepath.Join(dir, "plan.json")
	os.WriteFile(plan, planData, 0o644)
	m.Plan.SHA256 = sha(planData)
	md, _ := json.Marshal(m)
	var out bytes.Buffer
	w := bundle.NewWriter(&out)
	if err := bundle.WriteHead(w, md, nil); err != nil {
		t.Fatal(err)
	}
	for i, e := range entries {
		w.Entry(e.Path, e.Size, bytes.NewReader(data[i]))
	}
	w.Close()
	unsigned = filepath.Join(dir, "b.unsigned")
	os.WriteFile(unsigned, out.Bytes(), 0o644)
	return plan, unsigned, fp
}

// Spec 0014 phase 1b: kista bundle inspect and sign over a bundle's feed entries.
func TestBundleCLIFeeds(t *testing.T) {
	dir := t.TempDir()
	run := func(want int, args ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != want {
			t.Fatalf("kista %v: exit %d, want %d\n%s%s", args, code, want, out.String(), errb.String())
		}
		return out.String() + errb.String()
	}
	keyFile := filepath.Join(dir, "bundle-key.pem")
	bfp := strings.TrimSpace(run(0, "bundle", "keygen", "-o", keyFile))
	plan, unsigned, afp := feedFixture(t, dir, bfp, 0, "")
	signed := filepath.Join(dir, "b.kbundle")
	run(0, "bundle", "sign", "-key", keyFile, "-plan", plan, "-in", unsigned, "-o", signed)
	got := run(0, "bundle", "inspect", signed, "-plan", plan)
	for _, want := range []string{"consistent with the plan's pins", "key " + afp + " pinned",
		"feeds: 1, statements 3 (withdrawals 1, without content 1), documents 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("inspect: no %q in\n%s", want, got)
		}
	}
	// the plan pins another key: the statements do not verify with what it pins
	d := t.TempDir()
	p2, u2, _ := feedFixture(t, d, bfp, 0, "sha256:"+strings.Repeat("1", 64))
	s2 := filepath.Join(d, "b.kbundle")
	run(0, "bundle", "sign", "-key", keyFile, "-plan", p2, "-in", u2, "-o", s2)
	if got := run(1, "bundle", "inspect", s2, "-plan", p2); !strings.Contains(got, "does not verify") {
		t.Errorf("an unpinned attestation key: %s", got)
	}
	// a range that does not start at the plan's cursor: sign refuses it, inspect reports it
	d = t.TempDir()
	p3, u3, _ := feedFixture(t, d, bfp, 2, "")
	if got := run(1, "bundle", "sign", "-key", keyFile, "-plan", p3, "-in", u3, "-o", filepath.Join(d, "x")); !strings.Contains(got, "cursor") {
		t.Errorf("sign: %s", got)
	}
	run(1, "bundle", "inspect", u3, "-plan", p3)
}
