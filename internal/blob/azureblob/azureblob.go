// Package azureblob is the Azure Blob Storage object store (spec 0005, phase 2). It authenticates
// with an Entra token credential (spec 0004's internal/cloud/azure: managed or workload identity);
// a shared account key is accepted only for the Azurite emulator in development (enforced by
// config). SAS tokens and connection strings are never accepted. The transport ignores proxy
// variables, never follows redirects, and bounds connecting, headers and idle reads and writes.
package azureblob

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	azblob "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/transport"
)

// Config configures a store. Exactly one of Credential and AccountKeyFile is set.
type Config struct {
	// Endpoint is the account's blob endpoint: https://<account>.blob.<cloud suffix>, or, for
	// Azurite, http://127.0.0.1:10000/<account>.
	Endpoint       string
	Account        string
	Container      string
	Prefix         string
	Credential     azcore.TokenCredential
	AccountKeyFile string        // Azurite only (config allows it with profile dev on loopback)
	Timeout        time.Duration // connect, headers and idle reads/writes; default 30s
	// Transport replaces the HTTP transport (tests).
	Transport http.RoundTripper
}

// Store is a container and prefix.
type Store struct {
	c       *container.Client
	cfg     Config
	tr      http.RoundTripper
	timeout time.Duration
}

var (
	accountRe   = regexp.MustCompile(`^[a-z0-9]{3,24}$`)
	containerRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{1,61})[a-z0-9]$`)
)

// New builds a store. It does not contact the service.
func New(cfg Config) (*Store, error) {
	if !accountRe.MatchString(cfg.Account) {
		return nil, errors.New("blob/azureblob: an account name is 3-24 lowercase letters and digits")
	}
	if !containerRe.MatchString(cfg.Container) || strings.Contains(cfg.Container, "--") {
		return nil, errors.New("blob/azureblob: a container name is 3-63 lowercase letters, digits and single dashes")
	}
	if err := blob.ValidPrefix(cfg.Prefix); err != nil {
		return nil, err
	}
	if (cfg.Credential == nil) == (cfg.AccountKeyFile == "") {
		return nil, errors.New("blob/azureblob: exactly one of an Entra credential and an account key file")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Opaque != "" || u.User != nil ||
		u.Fragment != "" || u.Scheme != "https" && u.Scheme != "http" {
		return nil, errors.New("blob/azureblob: invalid endpoint")
	}
	if u.Scheme == "http" && cfg.Credential != nil {
		return nil, errors.New("blob/azureblob: an Entra token is never sent over http")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	tr := cfg.Transport
	if tr == nil {
		t, err := transport.New(cfg.Timeout, "")
		if err != nil {
			return nil, err
		}
		tr = t
	}
	opts := &container.ClientOptions{ClientOptions: azcore.ClientOptions{
		// at most 3 attempts; no per-try limit, which would also cut a long download: the
		// transport bounds idle time instead
		// (a Retry-After above MaxRetryDelay is not retried at all, so leave room for throttling)
		Retry:     policy.RetryOptions{MaxRetries: 2, MaxRetryDelay: 10 * time.Second},
		Transport: &http.Client{Transport: tr, CheckRedirect: transport.NoRedirects},
	}}
	containerURL := strings.TrimSuffix(cfg.Endpoint, "/") + "/" + cfg.Container
	var c *container.Client
	if cfg.Credential != nil {
		c, err = container.NewClient(containerURL, cfg.Credential, opts)
	} else {
		var key string
		key, err = readKey(cfg.AccountKeyFile)
		if err != nil {
			return nil, err
		}
		var cred *container.SharedKeyCredential
		cred, err = container.NewSharedKeyCredential(cfg.Account, key)
		if err != nil {
			return nil, errors.New("blob/azureblob: the account key is not valid base64")
		}
		c, err = container.NewClientWithSharedKeyCredential(containerURL, cred, opts)
	}
	if err != nil {
		return nil, errors.New("blob/azureblob: invalid endpoint or container")
	}
	return &Store{c: c, cfg: cfg, tr: tr, timeout: cfg.Timeout}, nil
}

func readKey(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("blob/azureblob: cannot open the account key file")
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return "", errors.New("blob/azureblob: the account key file must be a regular file with mode 0600 or narrower")
	}
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return "", errors.New("blob/azureblob: cannot read the account key file")
	}
	return strings.TrimSpace(string(b)), nil
}

// ID implements blob.Store: the endpoint lowercased and without its default port, the container
// and the prefix.
func (s *Store) ID() string {
	u, _ := url.Parse(s.cfg.Endpoint)
	host := strings.ToLower(u.Host)
	host = strings.TrimSuffix(host, map[string]string{"https": ":443", "http": ":80"}[u.Scheme])
	path := strings.TrimSuffix(u.Path, "/")
	return "azureblob:" + host + path + "/" + s.cfg.Container + "/" + s.cfg.Prefix
}

func (s *Store) name(key string) string { return s.cfg.Prefix + key }

// blockSize is the size of a staged block; an object up to it is one Put Blob.
const blockSize = 16 << 20

// blockID derives a block id from the key (for streams, the stream hash) and the block index, so
// two replicas writing one key stage identical blocks under identical ids and neither commit
// invalidates the other's. This relies on one content per key, which kista's keys guarantee:
// streams are content-addressed, tmp/ keys are unique, and the marker is small (one Put Blob).
func blockID(key string, i int) string {
	h := sha256.New()
	h.Write([]byte(key))
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(i))
	h.Write(n[:])
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Put implements blob.Store: one Put Blob for a small object; for a large one, staged blocks
// committed as a block list. Either becomes visible only when complete. Staged blocks of a failed
// upload are never visible and are discarded by the service after a week.
func (s *Store) Put(ctx context.Context, key string, r io.ReaderAt, size int64) error {
	if err := blob.ValidKey(key); err != nil {
		return err
	}
	bb := s.c.NewBlockBlobClient(s.name(key))
	ct := "application/octet-stream"
	headers := &azblob.HTTPHeaders{BlobContentType: &ct}
	if size <= blockSize {
		_, err := bb.Upload(ctx, streaming.NopCloser(io.NewSectionReader(r, 0, size)), &blockblob.UploadOptions{HTTPHeaders: headers})
		return describe(err)
	}
	var ids []string
	for i, off := 0, int64(0); off < size; i, off = i+1, off+blockSize {
		n := min(int64(blockSize), size-off)
		id := blockID(key, i)
		if _, err := bb.StageBlock(ctx, id, streaming.NopCloser(io.NewSectionReader(r, off, n)), nil); err != nil {
			return describe(err)
		}
		ids = append(ids, id)
	}
	_, err := bb.CommitBlockList(ctx, ids, &blockblob.CommitBlockListOptions{HTTPHeaders: headers})
	return describe(err)
}

// body is a download's body: read errors are described, never the SDK's.
type body struct{ rc io.ReadCloser }

func (b body) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if err != nil && err != io.EOF {
		err = describe(err)
	}
	return n, err
}

func (b body) Close() error { return b.rc.Close() }

// Get implements blob.Store.
func (s *Store) Get(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if err := blob.ValidKey(key); err != nil {
		return nil, err
	}
	if off < 0 {
		return nil, errors.New("blob/azureblob: negative offset")
	}
	if n == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	rng := azblob.HTTPRange{Offset: off}
	if n > 0 {
		rng.Count = n
	}
	resp, err := s.c.NewBlobClient(s.name(key)).DownloadStream(ctx, &azblob.DownloadStreamOptions{Range: rng})
	if err != nil {
		return nil, describe(err)
	}
	if resp.Body == nil {
		return nil, errors.New("blob/azureblob: the store answered without a body")
	}
	// a server (or something in between) that ignores the range would send bytes from offset 0
	if off > 0 || n > 0 {
		if resp.ContentRange == nil || !strings.HasPrefix(*resp.ContentRange, fmt.Sprintf("bytes %d-", off)) {
			resp.Body.Close()
			return nil, errors.New("blob/azureblob: the store did not honour the range")
		}
	}
	return body{resp.Body}, nil
}

// Stat implements blob.Store.
func (s *Store) Stat(ctx context.Context, key string) (int64, error) {
	if err := blob.ValidKey(key); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	resp, err := s.c.NewBlobClient(s.name(key)).GetProperties(ctx, nil)
	if err != nil {
		return 0, describe(err)
	}
	if resp.ContentLength == nil {
		return 0, errors.New("blob/azureblob: the store answered without a length")
	}
	return *resp.ContentLength, nil
}

// Delete implements blob.Store.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := blob.ValidKey(key); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	_, err := s.c.NewBlobClient(s.name(key)).Delete(ctx, nil)
	if err = describe(err); errors.Is(err, blob.ErrNotFound) {
		return nil
	}
	return err
}

// List implements blob.Store.
func (s *Store) List(ctx context.Context, prefix string, fn func(string, int64, time.Time) error) error {
	if err := blob.ValidListPrefix(prefix); err != nil {
		return err
	}
	p := s.name(prefix)
	pager := s.c.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: &p})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return describe(err)
		}
		for _, it := range page.Segment.BlobItems {
			if it == nil || it.Name == nil || it.Properties == nil || it.Properties.ContentLength == nil {
				continue
			}
			key := strings.TrimPrefix(*it.Name, s.cfg.Prefix)
			if blob.ValidKey(key) != nil {
				continue
			}
			var mod time.Time
			if it.Properties.LastModified != nil {
				mod = *it.Properties.LastModified
			}
			if err := fn(key, *it.Properties.ContentLength, mod); err != nil {
				return err
			}
		}
	}
	return nil
}

// Anonymous implements blob.Store: an unsigned HEAD of the key with the store's own transport.
// A private container answers 404 ResourceNotFound (or 409 when the account forbids public
// access); a public one answers 200, or 404 BlobNotFound when the key is missing, which still
// means anonymous reads reach the container.
func (s *Store) Anonymous(ctx context.Context, key string) (bool, bool) {
	u := strings.TrimSuffix(s.cfg.Endpoint, "/") + "/" + s.cfg.Container + "/" + s.name(key)
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return false, false
	}
	req.Header.Set("x-ms-version", "2021-12-02")
	resp, err := (&http.Client{Transport: s.tr, CheckRedirect: transport.NoRedirects}).Do(req)
	if err != nil {
		return false, false
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, true
	case http.StatusNotFound:
		return resp.Header.Get("x-ms-error-code") == string(bloberror.BlobNotFound), true
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict:
		return false, true
	}
	return false, false
}

var (
	errorCode = regexp.MustCompile(`^[A-Z][A-Za-z]{1,63}$`)
	requestID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

func clean(re *regexp.Regexp, s string) string {
	if re.MatchString(s) {
		return s
	}
	return "?"
}

// describe keeps the status, an Azure error code (letters only) and the request id (a GUID); never a
// body, URL, account or key. The SDK's own error strings carry the URL and are never wrapped.
func describe(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("blob/azureblob: the store did not answer in time: %w", context.DeadlineExceeded)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("blob/azureblob: %w", context.Canceled)
	}
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return blob.ErrNotFound
	}
	var re *azcore.ResponseError
	if errors.As(err, &re) {
		reqID := ""
		if re.RawResponse != nil {
			reqID = re.RawResponse.Header.Get("x-ms-request-id")
		}
		return fmt.Errorf("blob/azureblob: the store answered %d %s (request %s)", re.StatusCode,
			clean(errorCode, re.ErrorCode), clean(requestID, reqID))
	}
	var af *azidentity.AuthenticationFailedError
	if errors.As(err, &af) {
		return errors.New("blob/azureblob: the Azure credential did not get a token")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.New("blob/azureblob: the store stopped responding")
	}
	return errors.New("blob/azureblob: the store could not be reached, the connection broke, or no Azure credential is available")
}
