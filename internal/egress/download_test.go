package egress

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestDownload(t *testing.T) {
	ctx := context.Background()
	srv, hp := newTLS(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file":
			if r.Header.Get("If-None-Match") == `"e1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"e1"`)
			_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
		case "/slow":
			w.Header().Set("Content-Length", "100000")
			_, _ = w.Write([]byte("x"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/redirect":
			http.Redirect(w, r, "/file", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	})
	port := netip.MustParseAddrPort(hp).Port()
	c := client(t, srv, Config{Allow: loopbackAllow(port), OwnHost: "kista.example"})
	base := "https://127.0.0.1:" + itoa(port)
	o := DownloadOptions{MaxBytes: 1 << 20, Timeout: 10 * time.Second}

	var buf bytes.Buffer
	d, err := c.Download(ctx, base+"/file", o, &buf)
	if err != nil || d.NotModified || d.Size != 4096 || buf.Len() != 4096 || d.ETag != `"e1"` {
		t.Fatalf("a download: %+v %v", d, err)
	}
	o.ETag = d.ETag
	buf.Reset()
	if d, err := c.Download(ctx, base+"/file", o, &buf); err != nil || !d.NotModified || buf.Len() != 0 {
		t.Fatalf("not modified: %+v %v", d, err)
	}
	o.ETag = ""
	if _, err := c.Download(ctx, base+"/nope", o, &buf); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing file: %v", err)
	}
	if _, err := c.Download(ctx, base+"/redirect", o, &buf); !errors.Is(err, ErrStatus) {
		t.Errorf("a redirect is not followed: %v", err)
	}
	small := o
	small.MaxBytes = 100
	if _, err := c.Download(ctx, base+"/file", small, &buf); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over the cap: %v", err)
	}
	slow := o
	slow.MinRate, slow.Grace = 1000, time.Second
	start := time.Now()
	if _, err := c.Download(ctx, base+"/slow", slow, &buf); !errors.Is(err, ErrConnect) || !strings.Contains(err.Error(), "too slow") ||
		time.Since(start) > 5*time.Second {
		t.Errorf("too slow: %v after %v", err, time.Since(start))
	}
	short := o
	short.Timeout = 500 * time.Millisecond
	if _, err := c.Download(ctx, base+"/slow", short, &buf); !errors.Is(err, ErrConnect) {
		t.Errorf("the timeout: %v", err)
	}
	if _, err := c.Download(ctx, "https://kista.example/x", o, &buf); !errors.Is(err, ErrRefused) {
		t.Errorf("the deployment's own host: %v", err)
	}
	if _, err := c.Download(ctx, "https://10.0.0.1/x", o, &buf); !errors.Is(err, ErrRefused) {
		t.Errorf("a private address: %v", err)
	}
}

// Spec 0032: a Retry-After is delay-seconds or an HTTP date; a past date is now, a bad or negative
// value is none.
func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for v, want := range map[string]struct {
		d  time.Duration
		ok bool
	}{
		"":                              {0, false},
		"0":                             {0, true},
		"5":                             {5 * time.Second, true},
		"-1":                            {0, false},
		"soon":                          {0, false},
		"99999999999999999999":          {0, false},
		"Fri, 23 Oct 2026 12:00:10 GMT": {10 * time.Second, true},
		"Fri, 23 Oct 2026 11:00:00 GMT": {0, true},
	} {
		if d, ok := retryAfter(v, now); d != want.d || ok != want.ok {
			t.Errorf("%q: %v %v, want %v %v", v, d, ok, want.d, want.ok)
		}
	}
}
