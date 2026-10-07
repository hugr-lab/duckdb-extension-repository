// Package s3 is the S3-compatible object store (spec 0005, phase 1): AWS S3, Cloudflare R2, MinIO
// and others, through minio-go. Credentials come only from kista's own provider (static key files);
// minio-go's IAM, chain, environment and file providers are never used. The transport ignores proxy
// variables, requires TLS 1.2+ off loopback, never follows redirects; the region is always explicit.
package s3

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/encrypt"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
)

// Config configures a store.
type Config struct {
	Endpoint      string // host[:port]
	Secure        bool   // https; false only on loopback in development (validated by config)
	Bucket        string
	Prefix        string
	Region        string
	PathStyle     bool
	SSE           string // none | s3 | kms
	KMSKeyID      string
	AccessKeyFile string
	SecretKeyFile string
	CAFile        string // trust only this CA bundle; empty: the system roots
	// Timeout bounds the dial, the TLS handshake, the wait for response headers, and every read or
	// write on a connection that makes no progress (default 30s); Stat and Delete are bounded as a
	// whole by it as well.
	Timeout time.Duration
	// Transport replaces the HTTP transport (tests).
	Transport http.RoundTripper
}

// Store is an S3-compatible bucket and prefix.
type Store struct {
	c       *minio.Client
	core    minio.Core
	cfg     Config
	tr      http.RoundTripper
	timeout time.Duration
}

// errCredentials marks a credential file that cannot be used; its message names no secret.
var errCredentials = errors.New("blob/s3: credentials")

// fileProvider reads static credentials from two files on every retrieval (so rotated secrets are
// picked up), never from the environment.
type fileProvider struct{ access, secret string }

func readSecret(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%w: cannot open a credential file", errCredentials)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("%w: cannot stat a credential file", errCredentials)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%w: a credential file must be a regular file with mode 0600 or narrower", errCredentials)
	}
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", fmt.Errorf("%w: cannot read a credential file", errCredentials)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%w: a credential file is empty", errCredentials)
	}
	return v, nil
}

func (p fileProvider) RetrieveWithCredContext(*credentials.CredContext) (credentials.Value, error) {
	return p.Retrieve()
}

func (p fileProvider) Retrieve() (credentials.Value, error) {
	a, err := readSecret(p.access)
	if err != nil {
		return credentials.Value{}, err
	}
	s, err := readSecret(p.secret)
	if err != nil {
		return credentials.Value{}, err
	}
	return credentials.Value{AccessKeyID: a, SecretAccessKey: s, SignerType: credentials.SignatureV4}, nil
}

func (fileProvider) IsExpired() bool { return true } // re-read each time; files are small

// New builds a store. It does not contact the service.
func New(cfg Config) (*Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.Region == "" {
		return nil, errors.New("blob/s3: endpoint, bucket and region are required")
	}
	if err := blob.ValidPrefix(cfg.Prefix); err != nil {
		return nil, err
	}
	if cfg.AccessKeyFile == "" || cfg.SecretKeyFile == "" {
		return nil, errors.New("blob/s3: access_key_file and secret_key_file are required")
	}
	switch {
	case cfg.SSE == "kms" && cfg.KMSKeyID == "":
		return nil, errors.New("blob/s3: sse kms needs kms_key_id")
	case cfg.SSE != "kms" && cfg.KMSKeyID != "":
		return nil, errors.New("blob/s3: kms_key_id is only for sse kms")
	case cfg.SSE != "" && cfg.SSE != "none" && cfg.SSE != "s3" && cfg.SSE != "kms":
		return nil, fmt.Errorf("blob/s3: unknown sse %q", cfg.SSE)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	tr := cfg.Transport
	if tr == nil {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.CAFile != "" {
			pem, err := os.ReadFile(cfg.CAFile)
			if err != nil {
				return nil, errors.New("blob/s3: cannot read ca_file")
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("blob/s3: ca_file holds no certificate")
			}
			tlsCfg.RootCAs = pool
		}
		dialer := &net.Dialer{Timeout: cfg.Timeout}
		idle := cfg.Timeout
		tr = &http.Transport{
			Proxy: nil, // never from the environment
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := dialer.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				return &idleConn{Conn: c, idle: idle}, nil
			},
			TLSClientConfig:       tlsCfg,
			TLSHandshakeTimeout:   cfg.Timeout,
			ResponseHeaderTimeout: cfg.Timeout,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
		}
	}
	lookup := minio.BucketLookupDNS
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:        credentials.New(fileProvider{cfg.AccessKeyFile, cfg.SecretKeyFile}),
		Secure:       cfg.Secure,
		Transport:    tr,
		Region:       cfg.Region,
		BucketLookup: lookup,
		MaxRetries:   3,
	})
	if err != nil {
		return nil, errors.New("blob/s3: invalid endpoint")
	}
	return &Store{c: c, core: minio.Core{Client: c}, cfg: cfg, tr: tr, timeout: cfg.Timeout}, nil
}

// idleConn fails a read or write that makes no progress for idle: a stalled server cannot hold a
// request (an upload it stops reading, a download it stops sending) longer than that.
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(p []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	_ = c.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(p)
}

// ID implements blob.Store: the endpoint lowercased and without its default port, the bucket and
// the prefix. The lookup style does not change which objects are reached.
func (s *Store) ID() string {
	host := strings.ToLower(s.cfg.Endpoint)
	if s.cfg.Secure {
		host = strings.TrimSuffix(host, ":443")
	} else {
		host = strings.TrimSuffix(host, ":80")
	}
	return "s3:" + host + "/" + s.cfg.Bucket + "/" + s.cfg.Prefix
}

func (s *Store) object(key string) string { return s.cfg.Prefix + key }

func (s *Store) sse() (encrypt.ServerSide, error) {
	switch s.cfg.SSE {
	case "", "none":
		return nil, nil
	case "s3":
		return encrypt.NewSSE(), nil
	case "kms":
		return encrypt.NewSSEKMS(s.cfg.KMSKeyID, nil)
	}
	return nil, fmt.Errorf("blob/s3: unknown sse %q", s.cfg.SSE)
}

// Put implements blob.Store. minio-go uses a single PUT for small objects and a multipart upload
// (reading parts through ReaderAt) for large ones, aborting it on error.
func (s *Store) Put(ctx context.Context, key string, r io.ReaderAt, size int64) error {
	if err := blob.ValidKey(key); err != nil {
		return err
	}
	sse, err := s.sse()
	if err != nil {
		return err
	}
	_, err = s.c.PutObject(ctx, s.cfg.Bucket, s.object(key), io.NewSectionReader(r, 0, size), size, minio.PutObjectOptions{
		ContentType: "application/octet-stream", ServerSideEncryption: sse, PartSize: partSize,
	})
	if err != nil && size > partSize {
		// minio-go aborts a failed multipart upload with the call's context, which sends nothing
		// once that context is done; abort here with a fresh deadline.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
		_ = s.c.RemoveIncompleteUpload(actx, s.cfg.Bucket, s.object(key))
		cancel()
	}
	return describe(err)
}

const partSize = 16 << 20

// body is a GET's body: read errors are described, never the SDK's (which carry the URL).
type body struct{ rc io.ReadCloser }

func (b body) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if err != nil && err != io.EOF {
		err = describe(err)
	}
	return n, err
}

func (b body) Close() error { return b.rc.Close() }

// Get implements blob.Store: one ranged GET (minio.Core: no HEAD first, no If-Match pinning).
func (s *Store) Get(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if err := blob.ValidKey(key); err != nil {
		return nil, err
	}
	if off < 0 {
		return nil, fmt.Errorf("blob/s3: negative offset")
	}
	opts := minio.GetObjectOptions{} // server-managed SSE needs no headers on GET
	switch {
	case n == 0:
		return io.NopCloser(strings.NewReader("")), nil
	case n > 0:
		if err := opts.SetRange(off, off+n-1); err != nil {
			return nil, err
		}
	case off > 0:
		if err := opts.SetRange(off, 0); err != nil {
			return nil, err
		}
	}
	rc, _, _, err := s.core.GetObject(ctx, s.cfg.Bucket, s.object(key), opts)
	if err != nil {
		return nil, describe(err)
	}
	return body{rc}, nil
}

// Stat implements blob.Store.
func (s *Store) Stat(ctx context.Context, key string) (int64, error) {
	if err := blob.ValidKey(key); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	info, err := s.c.StatObject(ctx, s.cfg.Bucket, s.object(key), minio.StatObjectOptions{})
	if err != nil {
		return 0, describe(err)
	}
	return info.Size, nil
}

// Delete implements blob.Store.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := blob.ValidKey(key); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return describe(s.c.RemoveObject(ctx, s.cfg.Bucket, s.object(key), minio.RemoveObjectOptions{}))
}

// List implements blob.Store.
func (s *Store) List(ctx context.Context, prefix string, fn func(string, int64, time.Time) error) error {
	if err := blob.ValidListPrefix(prefix); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for obj := range s.c.ListObjects(ctx, s.cfg.Bucket, minio.ListObjectsOptions{Prefix: s.object(prefix), Recursive: true}) {
		if obj.Err != nil {
			return describe(obj.Err)
		}
		key := strings.TrimPrefix(obj.Key, s.cfg.Prefix)
		if blob.ValidKey(key) != nil {
			continue
		}
		if err := fn(key, obj.Size, obj.LastModified); err != nil {
			return err
		}
	}
	return nil
}

// Anonymous implements blob.Store: an unsigned HEAD of the key, with the store's own transport.
func (s *Store) Anonymous(ctx context.Context, key string) (bool, bool) {
	scheme := "http"
	if s.cfg.Secure {
		scheme = "https"
	}
	var u string
	if s.cfg.PathStyle {
		u = scheme + "://" + s.cfg.Endpoint + "/" + s.cfg.Bucket + "/" + s.object(key)
	} else {
		u = scheme + "://" + s.cfg.Bucket + "." + s.cfg.Endpoint + "/" + s.object(key)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return false, false
	}
	hc := &http.Client{Transport: s.tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := hc.Do(req)
	if err != nil {
		return false, false
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, true
	case http.StatusForbidden, http.StatusUnauthorized:
		return false, true
	}
	return false, false
}

var (
	codeRe = regexp.MustCompile(`^[A-Za-z]{1,64}$`)
	reqRe  = regexp.MustCompile(`^[A-Z0-9]{8,40}$`) // AWS, MinIO, SeaweedFS: upper-case hex or alnum
)

// describe keeps the status, an S3 error code and a request id of the expected shapes; never the
// resource, key, host id or message.
func describe(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("blob/s3: the store did not answer in time: %w", context.DeadlineExceeded)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("blob/s3: %w", context.Canceled)
	}
	er := minio.ToErrorResponse(err)
	if er.Code == minio.NoSuchKey {
		return blob.ErrNotFound
	}
	if er.StatusCode != 0 {
		code, req := er.Code, er.RequestID
		if !codeRe.MatchString(code) {
			code = "?"
		}
		if !reqRe.MatchString(req) {
			req = "?"
		}
		return fmt.Errorf("blob/s3: the store answered %d %s (request %s)", er.StatusCode, code, req)
	}
	if errors.Is(err, errCredentials) {
		return err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.New("blob/s3: the store stopped responding")
	}
	return errors.New("blob/s3: the store could not be reached, or the connection broke")
}
