//go:build live

package gcs_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/blobtest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/gcs"
	gcpcloud "github.com/hugr-lab/duckdb-extension-repository/internal/cloud/gcp"
)

// The live tests run against the bucket deploy/gcp/up.sh makes (KISTA_LIVE_GCS_BUCKET), with
// liveTokens. Every run writes under a prefix of its own; down.sh removes the bucket with the
// project.
func live(t *testing.T) (string, oauth2.TokenSource) {
	t.Helper()
	bucket := os.Getenv("KISTA_LIVE_GCS_BUCKET")
	if bucket == "" {
		t.Skip("KISTA_LIVE_GCS_BUCKET is not set")
	}
	return bucket, liveTokens(t)
}

// liveTokens are the stack's service account: an external_account file through kista's own
// credentials (KISTA_LIVE_GCP_CREDENTIALS and KISTA_LIVE_GCP_PRINCIPAL, as in GitHub Actions), or
// an access token got by impersonation (KISTA_LIVE_GCP_TOKEN, deploy/gcp/live.sh).
func liveTokens(t *testing.T) oauth2.TokenSource {
	t.Helper()
	if f := os.Getenv("KISTA_LIVE_GCP_CREDENTIALS"); f != "" {
		ts, err := gcpcloud.TokenSource(f, os.Getenv("KISTA_LIVE_GCP_PRINCIPAL"), gcpcloud.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	if tok := os.Getenv("KISTA_LIVE_GCP_TOKEN"); tok != "" {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok, TokenType: "Bearer"})
	}
	t.Skip("neither KISTA_LIVE_GCP_CREDENTIALS nor KISTA_LIVE_GCP_TOKEN is set")
	return nil
}

func liveStore(t *testing.T, bucket string, ts oauth2.TokenSource) blob.Store {
	t.Helper()
	return newStore(t, gcs.Config{Bucket: bucket, Prefix: fmt.Sprintf("live-%d/", time.Now().UnixNano()), Tokens: ts})
}

func TestLiveContract(t *testing.T) {
	bucket, ts := live(t)
	blobtest.Run(t, blobtest.Harness{
		New: func(t *testing.T) blob.Store { return liveStore(t, bucket, ts) },
		WithTimeout: func(t *testing.T, timeout time.Duration) blob.Store {
			return newStore(t, gcs.Config{Bucket: bucket, Prefix: fmt.Sprintf("live-%d/", time.Now().UnixNano()), Tokens: ts,
				Timeout: timeout})
		},
	})
}

// Above 16 MiB a Put is a resumable upload of two chunks; the bucket is private.
func TestLiveResumable(t *testing.T) {
	bucket, ts := live(t)
	s := liveStore(t, bucket, ts)
	data := blobtest.Data(20<<20+5, 7)
	key := blobtest.Key(data)
	if err := s.Put(context.Background(), key, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	rc, err := s.Get(context.Background(), key, 16<<20-3, 10)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data[16<<20-3:16<<20+7]) {
		t.Fatal("a ranged read across the chunk boundary differs")
	}
	if readable, ok := s.Anonymous(context.Background(), key); !ok || readable {
		t.Fatalf("anonymous check: readable %v, known %v; want a private bucket", readable, ok)
	}
	if err := s.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
}

// GCS itself refuses a body whose CRC32C is not the one in the metadata, for the multipart and the
// resumable uploads kista makes; it ignores x-goog-hash on a JSON API media upload, which kista
// therefore does not use.
func TestLiveWrongHash(t *testing.T) {
	bucket, ts := live(t)
	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	up := "https://storage.googleapis.com/upload/storage/v1/b/" + bucket + "/o?uploadType="
	name := fmt.Sprintf("live-%d/wrong-hash", time.Now().UnixNano())
	meta := `{"name":"` + name + `","crc32c":"AAAAAA=="}`
	send := func(method, url, contentType string, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		req.Header.Set("Content-Type", contentType)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	mp := "--b\r\nContent-Type: application/json\r\n\r\n" + meta + "\r\n--b\r\nContent-Type: application/octet-stream\r\n\r\n" +
		"kista live test\r\n--b--\r\n"
	if resp := send(http.MethodPost, up+"multipart", "multipart/related; boundary=b", mp); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("multipart with a wrong crc32c: %d, want 400", resp.StatusCode)
	}
	resp := send(http.MethodPost, up+"resumable", "application/json", meta)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a resumable session: %d", resp.StatusCode)
	}
	if resp := send(http.MethodPut, resp.Header.Get("Location"), "application/octet-stream", "kista live test"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("resumable with a wrong crc32c: %d, want 400", resp.StatusCode)
	}
}
