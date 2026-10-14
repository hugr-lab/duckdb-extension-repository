package s3_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/s3"
)

// With an AWS identity every request carries x-amz-expected-bucket-owner, signed (spec 0005 phase
// 3a). The S3 server (KISTA_TEST_S3, which checks SigV4) sees it through a TLS proxy: a single PUT,
// a multipart upload, a ranged GET, Stat, List and Delete all verify.
func TestExpectedOwner(t *testing.T) {
	u := server(t)
	ensureBucket(t, u)
	target, _ := url.Parse("http://" + u.Host)
	var mu sync.Mutex
	seen := map[string]int{}
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Expected-Bucket-Owner") != "123456789012" ||
			!strings.Contains(r.Header.Get("Authorization"), "x-amz-expected-bucket-owner") {
			t.Errorf("%s %s without the signed expected owner", r.Method, r.URL)
		}
		mu.Lock()
		seen[r.Method]++
		mu.Unlock()
		proxy.ServeHTTP(w, r) // the Host header is kept: the signature verifies upstream
	}))
	defer front.Close()
	pw, _ := u.User.Password()
	cfg := s3.Config{
		Endpoint: strings.TrimPrefix(front.URL, "https://"), Secure: true, Bucket: strings.Trim(u.Path, "/"), Region: "us-east-1",
		PathStyle: true, SSE: "none", Prefix: "owner-" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "-") + "/",
		AWS: credentials.NewStaticCredentialsProvider(u.User.Username(), pw, ""), ExpectedOwner: "123456789012",
		Transport: front.Client().Transport,
	}
	st, err := s3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	small, big := []byte("a small body"), make([]byte, 17<<20) // the large one is a multipart upload
	_, _ = rand.Read(big)
	keys := map[string]string{"small": "streams/" + strings.Repeat("a", 64), "big": "streams/" + strings.Repeat("b", 64)}
	for name, body := range map[string][]byte{"small": small, "big": big} {
		key := keys[name]
		if err := st.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		if n, err := st.Stat(ctx, key); err != nil || n != int64(len(body)) {
			t.Fatalf("stat %s: %d %v", key, n, err)
		}
		rc, err := st.Get(ctx, key, 3, 5)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, body[3:8]) {
			t.Fatalf("get %s: %q", key, got)
		}
	}
	n := 0
	if err := st.List(ctx, "streams/", func(string, int64, time.Time) error { n++; return nil }); err != nil || n != 2 {
		t.Fatalf("list: %d %v", n, err)
	}
	for _, key := range keys {
		if err := st.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	if seen[http.MethodPut] < 3 || seen[http.MethodPost] < 2 || seen[http.MethodGet] < 3 || seen[http.MethodHead] < 2 || seen[http.MethodDelete] < 2 {
		t.Fatalf("requests seen: %v", seen)
	}
}

// An expected owner needs an AWS identity over https.
func TestExpectedOwnerRules(t *testing.T) {
	aws := credentials.NewStaticCredentialsProvider("a", "b", "")
	for _, c := range []s3.Config{
		{Endpoint: "s3.example", Secure: true, Bucket: "b", Region: "eu-central-1", ExpectedOwner: "123456789012", AccessKeyFile: "/a", SecretKeyFile: "/s"},
		{Endpoint: "s3.example", Secure: false, Bucket: "b", Region: "eu-central-1", ExpectedOwner: "123456789012", AWS: aws},
		{Endpoint: "s3.example", Secure: true, Bucket: "b", Region: "eu-central-1", AWS: aws, AccessKeyFile: "/a", SecretKeyFile: "/s"},
		{Endpoint: "s3.example", Secure: true, Bucket: "b", Region: "eu-central-1", AWS: aws},
	} {
		if _, err := s3.New(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}
