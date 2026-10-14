package s3

import (
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

type recorder struct{ reqs []*http.Request }

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.reqs = append(r.reqs, req)
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
}

// The owner transport signs only requests to the configured host, already SigV4-signed with a
// plain payload hash, and sends one session token.
func TestOwnerTransport(t *testing.T) {
	rec := &recorder{}
	tr := newOwnerTransport(rec, "123456789012", "eu-central-1", "s3.eu-central-1.amazonaws.com", "kista",
		credentials.NewStaticCredentialsProvider("AKID", "secret", "session-token"))
	req := func(url, payload string, auth bool) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, url, nil)
		if auth {
			r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKID/20261012/eu-central-1/s3/aws4_request, SignedHeaders=host, Signature=x")
		}
		r.Header.Set("X-Amz-Date", "20261012T100000Z")
		r.Header.Set("X-Amz-Content-Sha256", payload)
		r.Header.Set("X-Amz-Security-Token", "session-token")
		return r
	}
	const empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	for name, r := range map[string]*http.Request{
		"virtual-hosted": req("https://kista.s3.eu-central-1.amazonaws.com/streams/x", empty, true),
		"path style":     req("https://s3.eu-central-1.amazonaws.com/kista/streams/x", "UNSIGNED-PAYLOAD", true),
	} {
		if _, err := tr.RoundTrip(r); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sent := rec.reqs[len(rec.reqs)-1]
		auth := sent.Header.Get("Authorization")
		if sent.Header.Get(expectedOwner) != "123456789012" || !strings.Contains(auth, "x-amz-expected-bucket-owner") ||
			!strings.Contains(auth, "x-amz-security-token") || strings.Contains(auth, "Signature=x") ||
			len(sent.Header.Values("X-Amz-Security-Token")) != 1 {
			t.Fatalf("%s: %v", name, sent.Header)
		}
	}
	date := req("https://kista.s3.eu-central-1.amazonaws.com/x", empty, true)
	date.Header.Del("X-Amz-Date")
	for name, r := range map[string]*http.Request{
		"another host":      req("https://kista.s3.dualstack.eu-central-1.amazonaws.com/x", empty, true),
		"us-east-1 default": req("https://kista.s3.us-east-1.amazonaws.com/x", empty, true),
		"http":              req("http://kista.s3.eu-central-1.amazonaws.com/x", empty, true),
		"unsigned":          req("https://kista.s3.eu-central-1.amazonaws.com/x", empty, false),
		"streaming":         req("https://kista.s3.eu-central-1.amazonaws.com/x", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD", true),
		"trailer":           req("https://kista.s3.eu-central-1.amazonaws.com/x", "STREAMING-UNSIGNED-PAYLOAD-TRAILER", true),
		"no date":           date,
	} {
		n := len(rec.reqs)
		if _, err := tr.RoundTrip(r); err == nil || len(rec.reqs) != n {
			t.Errorf("%s: sent", name)
		}
	}
}
