// Package gcs is the Google Cloud Storage store (spec 0005 phase 3b): the JSON API over REST, with
// no Google SDK. An object up to 16 MiB is one upload; a larger one is a resumable upload in 16 MiB
// chunks, visible only when complete. Every upload carries the object's CRC32C, and the object GCS
// reports must have kista's CRC32C and size.
package gcs

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/transport"
	gcpcloud "github.com/hugr-lab/duckdb-extension-repository/internal/cloud/gcp"
)

// Endpoint is GCS's.
const Endpoint = "https://storage.googleapis.com"

// Config configures a store.
type Config struct {
	Bucket string
	Prefix string
	// Endpoint replaces GCS's (fake-gcs-server in the tests, in development); empty in production.
	Endpoint string
	Tokens   oauth2.TokenSource
	// Timeout bounds connecting, headers and every read or write without progress (default 30s);
	// Stat, Delete and the list pages are bounded as a whole by it as well.
	Timeout   time.Duration
	Transport http.RoundTripper // tests
	// ChunkSize is a resumable upload's chunk, a multiple of 256 KiB (default 16 MiB; tests lower it).
	ChunkSize int64
}

// Store is a bucket and prefix.
type Store struct {
	cfg     Config
	base    *url.URL
	hc      *http.Client
	timeout time.Duration
	chunk   int64
}

// New builds a store. It does not contact the service.
func New(cfg Config) (*Store, error) {
	if cfg.Bucket == "" || cfg.Tokens == nil {
		return nil, errors.New("blob/gcs: a bucket and credentials are required")
	}
	if err := blob.ValidPrefix(cfg.Prefix); err != nil {
		return nil, err
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = Endpoint
	}
	base, err := url.Parse(strings.TrimSuffix(cfg.Endpoint, "/"))
	if err != nil || base.Host == "" || base.Path != "" || base.User != nil || base.RawQuery != "" {
		return nil, errors.New("blob/gcs: invalid endpoint")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	chunk := cfg.ChunkSize
	if chunk == 0 {
		chunk = 16 << 20
	}
	if chunk%(256<<10) != 0 || chunk <= 0 {
		return nil, errors.New("blob/gcs: the chunk size must be a multiple of 256 KiB")
	}
	tr := cfg.Transport
	if tr == nil {
		t, err := transport.New(cfg.Timeout, "")
		if err != nil {
			return nil, err
		}
		tr = t
	}
	return &Store{cfg: cfg, base: base, timeout: cfg.Timeout, chunk: chunk,
		hc: &http.Client{Transport: tr, CheckRedirect: transport.NoRedirects}}, nil
}

// ID implements blob.Store: the endpoint's host, the bucket and the prefix.
func (s *Store) ID() string {
	return "gcs:" + strings.ToLower(s.base.Host) + "/" + s.cfg.Bucket + "/" + s.cfg.Prefix
}

func (s *Store) name(key string) string { return s.cfg.Prefix + key }

// escape escapes an object name for a path segment, "/" as %2F.
func escape(name string) string { return strings.ReplaceAll(url.PathEscape(name), "/", "%2F") }

func (s *Store) bucketURL() string { return s.base.String() + "/storage/v1/b/" + escape(s.cfg.Bucket) }

func (s *Store) objectURL(key string, query string) string {
	u := s.bucketURL() + "/o/" + escape(s.name(key))
	if query != "" {
		u += "?" + query
	}
	return u
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// crc32c is the base64 big-endian CRC32C of r's first size bytes, as GCS writes it.
func crc32c(r io.ReaderAt, size int64) (string, error) {
	h := crc32.New(castagnoli)
	if _, err := io.Copy(h, io.NewSectionReader(r, 0, size)); err != nil {
		return "", errors.New("blob/gcs: the body could not be read")
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], h.Sum32())
	return base64.StdEncoding.EncodeToString(b[:]), nil
}

// object is the resource GCS answers an upload with.
type object struct {
	Size       string `json:"size"`
	CRC32C     string `json:"crc32c"`
	Generation string `json:"generation"`
}

// Put implements blob.Store.
func (s *Store) Put(ctx context.Context, key string, r io.ReaderAt, size int64) error {
	if err := blob.ValidKey(key); err != nil {
		return err
	}
	sum, err := crc32c(r, size)
	if err != nil {
		return err
	}
	up := s.base.String() + "/upload/storage/v1/b/" + escape(s.cfg.Bucket) + "/o?name=" + url.QueryEscape(s.name(key))
	var got object
	if size <= s.chunk {
		// multipart, not media: GCS checks a crc32c given in the metadata, and ignores x-goog-hash
		// on a JSON API media upload (verified live)
		var rnd [16]byte
		_, _ = crand.Read(rnd[:])
		boundary := hex.EncodeToString(rnd[:])
		meta, _ := json.Marshal(map[string]string{"crc32c": sum, "contentType": "application/octet-stream"})
		head := "--" + boundary + "\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n" + string(meta) +
			"\r\n--" + boundary + "\r\nContent-Type: application/octet-stream\r\n\r\n"
		tail := "\r\n--" + boundary + "--\r\n"
		resp, err := s.do(ctx, 3, func() (*http.Request, error) {
			body := io.MultiReader(strings.NewReader(head), io.NewSectionReader(r, 0, size), strings.NewReader(tail))
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, up+"&uploadType=multipart", body)
			if err != nil {
				return nil, err
			}
			req.ContentLength = int64(len(head)) + size + int64(len(tail))
			req.Header.Set("Content-Type", "multipart/related; boundary="+boundary)
			return req, nil
		}, http.StatusOK)
		if err != nil {
			return err
		}
		err = decode(resp, &got)
		if err != nil {
			return err
		}
	} else if got, err = s.resumable(ctx, up+"&uploadType=resumable", r, size, sum); err != nil {
		return err
	}
	if got.CRC32C != sum || got.Size != strconv.FormatInt(size, 10) {
		s.deleteGeneration(context.WithoutCancel(ctx), key, got.Generation)
		return errors.New("blob/gcs: the stored object is not what was sent (CRC32C or size)")
	}
	return nil
}

// deleteGeneration removes the object an upload made, and no later one (ifGenerationMatch).
func (s *Store) deleteGeneration(ctx context.Context, key, generation string) {
	if _, err := strconv.ParseInt(generation, 10, 64); err != nil {
		return // no generation to pin: leaving it is safer than removing another writer's
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if resp, err := s.do(ctx, 3, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodDelete, s.objectURL(key, "ifGenerationMatch="+generation), nil)
	}, http.StatusNoContent); err == nil {
		resp.Body.Close()
	}
}

// resumable uploads in chunks: a 308 continues from what GCS acknowledged, a failed chunk is
// followed by a status query, and a failed upload is cancelled with a fresh deadline.
func (s *Store) resumable(ctx context.Context, start string, r io.ReaderAt, size int64, sum string) (object, error) {
	meta, _ := json.Marshal(map[string]string{"crc32c": sum, "contentType": "application/octet-stream"})
	resp, err := s.do(ctx, 3, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, start, bytes.NewReader(meta))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		req.Header.Set("X-Upload-Content-Type", "application/octet-stream")
		req.Header.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))
		return req, nil
	}, http.StatusOK)
	if err != nil {
		return object{}, err
	}
	resp.Body.Close()
	session, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || session.Scheme != s.base.Scheme || session.Host != s.base.Host || session.User != nil ||
		session.EscapedPath() != "/upload/storage/v1/b/"+escape(s.cfg.Bucket)+"/o" || session.Query().Get("upload_id") == "" {
		return object{}, errors.New("blob/gcs: the upload session is not on the store's upload path")
	}
	done := false
	defer func() {
		if !done {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
			defer cancel()
			if req, err := http.NewRequestWithContext(cctx, http.MethodDelete, session.String(), nil); err == nil {
				if resp, err := s.authorized(req); err == nil { // 499: cancelled
					resp.Body.Close()
				}
			}
		}
	}()
	var obj object
	off, failures, stalls := int64(0), 0, 0
	// every round either advances, or spends a failure or a stall; this bounds a server that keeps
	// acknowledging a byte at a time
	rounds := 2*int((size+s.chunk-1)/s.chunk) + 8
	for range rounds {
		n := min(s.chunk, size-off)
		resp, err := s.chunkPut(ctx, session.String(), r, off, n, size)
		for err != nil {
			// a broken or failed chunk: back off, then ask where the session is before sending again
			var fe finalErr
			if errors.As(err, &fe) {
				return object{}, fe.err
			}
			if failures++; failures > 3 || ctx.Err() != nil {
				return object{}, err
			}
			wait := time.Duration(1<<failures)*200*time.Millisecond + time.Duration(rand.IntN(200))*time.Millisecond
			var re retryAfter
			if errors.As(err, &re) && re.d > 0 {
				wait = re.d
			}
			select {
			case <-ctx.Done():
				return object{}, describeNet(ctx.Err())
			case <-time.After(wait):
			}
			resp, err = s.chunkPut(ctx, session.String(), nil, 0, 0, size)
		}
		switch resp.StatusCode {
		case http.StatusOK, http.StatusCreated:
			done = true
			return obj, decode(resp, &obj)
		case 308: // Resume Incomplete: continue after the last byte GCS kept
			next, ok := acknowledged(resp.Header.Get("Range"))
			resp.Body.Close()
			if !ok || next > size {
				return object{}, errors.New("blob/gcs: the upload session acknowledged an impossible range")
			}
			if next <= off { // no progress: a session that keeps refusing bytes is given up
				if stalls++; stalls > 3 {
					return object{}, errors.New("blob/gcs: the upload session makes no progress")
				}
			} else {
				failures = 0 // the budget is per chunk
			}
			off = next
		}
	}
	return object{}, errors.New("blob/gcs: the upload session takes too many rounds")
}

// finalErr is a chunk refused for good (a 4xx other than 408 and 429): no retry, no status query.
type finalErr struct{ err error }

func (f finalErr) Error() string { return f.err.Error() }

// retryAfter is a chunk refused with a Retry-After (up to 10s).
type retryAfter struct {
	err error
	d   time.Duration
}

func (r retryAfter) Error() string { return r.err.Error() }
func (r retryAfter) Unwrap() error { return r.err }

// chunkPut sends bytes off..off+n of r to the session (n = 0 and r = nil: a status query). It
// returns a 200, 201 or 308 response, or an error.
func (s *Store) chunkPut(ctx context.Context, session string, r io.ReaderAt, off, n, size int64) (*http.Response, error) {
	var b io.Reader = http.NoBody
	rng := fmt.Sprintf("bytes */%d", size)
	if r != nil && n > 0 {
		b = io.NewSectionReader(r, off, n)
		rng = fmt.Sprintf("bytes %d-%d/%d", off, off+n-1, size)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, session, b)
	if err != nil {
		return nil, errors.New("blob/gcs: an invalid request")
	}
	req.ContentLength = n
	req.Header.Set("Content-Range", rng)
	resp, err := s.authorized(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, 308:
		return resp, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	err = describe(resp.StatusCode, body)
	if secs, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && secs > 0 && secs <= 10 {
		return nil, retryAfter{err, time.Duration(secs) * time.Second}
	}
	if c := resp.StatusCode; c < 500 && c != http.StatusRequestTimeout && c != http.StatusTooManyRequests {
		return nil, finalErr{err}
	}
	return nil, err
}

// acknowledged reads a 308's Range (bytes=0-N): the next offset is N+1; none means 0.
func acknowledged(h string) (int64, bool) {
	if h == "" {
		return 0, true
	}
	rest, ok := strings.CutPrefix(h, "bytes=0-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	return n + 1, err == nil && n >= 0
}

// body is a download's body: read errors are described.
type body struct{ rc io.ReadCloser }

func (b body) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if err != nil && err != io.EOF {
		err = describeNet(err)
	}
	return n, err
}

func (b body) Close() error { return b.rc.Close() }

// Get implements blob.Store: one ranged media download.
func (s *Store) Get(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if err := blob.ValidReadKey(key); err != nil {
		return nil, err
	}
	if off < 0 {
		return nil, errors.New("blob/gcs: negative offset")
	}
	if n == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	ranged := off > 0 || n > 0
	want := http.StatusOK
	if ranged {
		want = http.StatusPartialContent
	}
	resp, err := s.do(ctx, 3, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.objectURL(key, "alt=media"), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept-Encoding", "identity") // never transcoded, nor gunzipped by Go
		switch {
		case n > 0:
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
		case off > 0:
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
		}
		return req, nil
	}, want)
	if errors.Is(err, errRangeNotSatisfiable) {
		return io.NopCloser(strings.NewReader("")), nil // the offset is the size
	}
	if err != nil {
		return nil, err
	}
	if ranged && !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", off)) {
		resp.Body.Close()
		return nil, errors.New("blob/gcs: the store did not honour the range")
	}
	return body{resp.Body}, nil
}

// Stat implements blob.Store.
func (s *Store) Stat(ctx context.Context, key string) (int64, error) {
	if err := blob.ValidReadKey(key); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	var meta struct {
		Size string `json:"size"`
	}
	if err := s.getJSON(ctx, s.objectURL(key, "fields=size"), &meta); err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(meta.Size, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("blob/gcs: the store answered without a size")
	}
	return n, nil
}

// Delete implements blob.Store.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := blob.ValidKey(key); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	resp, err := s.do(ctx, 3, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodDelete, s.objectURL(key, ""), nil)
	}, http.StatusNoContent)
	if errors.Is(err, blob.ErrNotFound) {
		return nil
	}
	if err == nil {
		resp.Body.Close()
	}
	return err
}

// List implements blob.Store.
func (s *Store) List(ctx context.Context, prefix string, fn func(string, int64, time.Time) error) error {
	if err := blob.ValidListPrefix(prefix); err != nil {
		return err
	}
	token := ""
	for {
		q := url.Values{"prefix": {s.name(prefix)}, "fields": {"items(name,size,updated),nextPageToken"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			Items []struct {
				Name    string    `json:"name"`
				Size    string    `json:"size"`
				Updated time.Time `json:"updated"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		pctx, cancel := context.WithTimeout(ctx, s.timeout)
		err := s.getJSON(pctx, s.bucketURL()+"/o?"+q.Encode(), &page)
		cancel()
		if err != nil {
			return err
		}
		for _, it := range page.Items {
			key := strings.TrimPrefix(it.Name, s.cfg.Prefix)
			n, perr := strconv.ParseInt(it.Size, 10, 64)
			if blob.ValidKey(key) != nil || perr != nil {
				continue
			}
			if err := fn(key, n, it.Updated); err != nil {
				return err
			}
		}
		if page.NextPageToken == "" {
			return nil
		}
		if page.NextPageToken == token {
			return errors.New("blob/gcs: the store repeats a page")
		}
		token = page.NextPageToken
	}
}

// Anonymous implements blob.Store: an unauthenticated read of the key's metadata. 200, or 404 for a
// missing object (or bucket), means public; 401 and 403 private; anything else is unknown.
func (s *Store) Anonymous(ctx context.Context, key string) (bool, bool) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.objectURL(key, "fields=size"), nil)
	if err != nil {
		return false, false
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return false, false
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNotFound:
		return true, true
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, true
	}
	return false, false
}

func (s *Store) getJSON(ctx context.Context, u string, out any) error {
	resp, err := s.do(ctx, 3, func() (*http.Request, error) { return http.NewRequestWithContext(ctx, http.MethodGet, u, nil) },
		http.StatusOK)
	if err != nil {
		return err
	}
	return decode(resp, out)
}

func decode(resp *http.Response, out any) error {
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
		return describeNet(err)
	}
	return nil
}

// authorized sends a request with a bearer token.
func (s *Store) authorized(req *http.Request) (*http.Response, error) {
	tok, err := gcpcloud.Token(req.Context(), s.cfg.Tokens)
	switch {
	case errors.Is(err, gcpcloud.ErrPrincipal):
		return nil, fmt.Errorf("blob/gcs: %w", gcpcloud.ErrPrincipal)
	case req.Context().Err() != nil:
		return nil, describeNet(req.Context().Err())
	case err != nil:
		return nil, errCredentials
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, describeNet(err)
	}
	return resp, nil
}

var (
	errCredentials         = errors.New("blob/gcs: the Google credentials could not be obtained")
	errRangeNotSatisfiable = errors.New("blob/gcs: the range is past the object's end")
)

// do sends a request, built anew for each attempt, and returns the response when its status is want
// (a 201 counts as a 200); up to attempts tries on 429, 5xx and connection failures, honouring a
// Retry-After up to 10s.
func (s *Store) do(ctx context.Context, attempts int, build func() (*http.Request, error), want int) (*http.Response, error) {
	var lastErr error
	wait := time.Duration(0)
	for attempt := range attempts {
		if attempt > 0 {
			if wait == 0 {
				wait = time.Duration(1<<attempt)*100*time.Millisecond + time.Duration(rand.IntN(100))*time.Millisecond
			}
			select {
			case <-ctx.Done():
				return nil, describeNet(ctx.Err())
			case <-time.After(wait):
			}
			wait = 0
		}
		req, err := build()
		if err != nil {
			return nil, errors.New("blob/gcs: an invalid request")
		}
		resp, err := s.authorized(req)
		if err != nil {
			if errors.Is(err, errCredentials) || ctx.Err() != nil {
				return nil, err
			}
			lastErr = err
			continue
		}
		if resp.StatusCode == want || want == http.StatusOK && resp.StatusCode == http.StatusCreated ||
			want == http.StatusNoContent && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			return nil, errRangeNotSatisfiable
		}
		lastErr = describe(resp.StatusCode, b)
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return nil, lastErr
		}
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 && secs <= 10 {
			wait = time.Duration(secs) * time.Second
		}
	}
	return nil, lastErr
}

var reasonRe = regexp.MustCompile(`^[A-Za-z]{1,40}$`)

// describe keeps the status and Google's reason word; never a message, bucket or object name.
func describe(status int, body []byte) error {
	if status == http.StatusNotFound {
		return blob.ErrNotFound
	}
	var e struct {
		Error struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	reason := "?"
	if len(e.Error.Errors) > 0 && reasonRe.MatchString(e.Error.Errors[0].Reason) {
		reason = e.Error.Errors[0].Reason
	}
	return fmt.Errorf("blob/gcs: the store answered %d %s", status, reason)
}

// describeNet describes a transport failure without its URL.
func describeNet(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("blob/gcs: the store did not answer in time: %w", context.DeadlineExceeded)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("blob/gcs: %w", context.Canceled)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.New("blob/gcs: the store stopped responding")
	}
	return errors.New("blob/gcs: the store could not be reached, or the connection broke")
}
