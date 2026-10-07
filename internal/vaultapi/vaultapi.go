// Package vaultapi is a thin HTTP client for HashiCorp Vault and OpenBao (spec 0004): TLS 1.2+ with
// an optional private CA, no redirects, no environment variables, login through a projected token
// (kubernetes or jwt) or a Vault Agent token file, lazy re-login at 2/3 of the token's lease, at
// most 3 attempts per call, and errors that never carry response bodies.
package vaultapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Auth kinds.
const (
	AuthKubernetes = "kubernetes"
	AuthJWT        = "jwt"
	AuthTokenFile  = "token_file"
)

// Config configures a client.
type Config struct {
	Address   string // scheme://host[:port]
	Namespace string
	CAFile    string
	AuthKind  string
	AuthMount string // default: the auth kind's name
	Role      string
	TokenFile string
	// AllowHTTP permits plain http (only on loopback; config validation enforces profile dev).
	AllowHTTP bool
	// Timeout per attempt; default 10s.
	Timeout time.Duration
	// HTTPClient replaces the transport (tests).
	HTTPClient *http.Client
}

// Error is a Vault error: the status and a short message. It never includes a response body
// beyond Vault's own error strings, cut short.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("vault: status %d", e.Status)
	}
	return fmt.Sprintf("vault: status %d: %s", e.Status, e.Msg)
}

// ErrUnreachable is returned when Vault does not answer.
var ErrUnreachable = errors.New("vault: did not answer")

// Client talks to one Vault address.
type Client struct {
	cfg  Config
	base *url.URL
	http *http.Client

	mu         sync.Mutex
	token      string
	renewAt    time.Time
	expires    time.Time
	lastForced time.Time
	login1     chan struct{} // one login at a time, waitable with a context
	now        func() time.Time
}

// loginBackoff delays the next login after a failed one; forcedEvery bounds re-logins forced by a
// 403, so a real permission denial does not mint a token per call.
const (
	loginBackoff = 30 * time.Second
	forcedEvery  = 30 * time.Second
)

// New builds a client. It refuses http unless AllowHTTP and the host is loopback.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.Address)
	if err != nil || u.Host == "" {
		return nil, errors.New("vault: invalid address")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !cfg.AllowHTTP || !loopback(u.Hostname()) {
			return nil, errors.New("vault: plain http is allowed only on loopback in development")
		}
	default:
		return nil, errors.New("vault: the address must be https")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.AuthMount == "" {
		cfg.AuthMount = cfg.AuthKind
	}
	hc := cfg.HTTPClient
	if hc == nil {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.CAFile != "" {
			pem, err := os.ReadFile(cfg.CAFile)
			if err != nil {
				return nil, fmt.Errorf("vault: reading ca_file: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("vault: ca_file holds no certificate")
			}
			tlsCfg.RootCAs = pool
		}
		hc = &http.Client{Transport: &http.Transport{
			TLSClientConfig:     tlsCfg,
			Proxy:               nil, // never from the environment
			DialContext:         (&net.Dialer{Timeout: cfg.Timeout}).DialContext,
			TLSHandshakeTimeout: cfg.Timeout,
		}}
	}
	// never follow redirects
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{cfg: cfg, base: u, http: hc, now: time.Now, login1: make(chan struct{}, 1)}, nil
}

func loopback(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// readTokenFile reads a token file that must be a regular file with no permissions for group or
// others (checked on the opened file; a symlink, as Kubernetes projected volumes use, is followed),
// and not empty.
func readTokenFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("vault: token file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("vault: token file: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return "", errors.New("vault: the token file must be a regular file with mode 0600 or narrower")
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return "", fmt.Errorf("vault: token file: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("vault: the token file is empty")
	}
	return tok, nil
}

// current returns the cached token if it is still valid, and whether a renewal is due.
func (c *Client) current() (tok string, valid, due bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	valid = c.token != "" && now.Before(c.expires)
	return c.token, valid, !valid || !now.Before(c.renewAt)
}

// authToken returns a token to use. A renewal is due at 2/3 of the lease: while the old token is
// still valid, a caller that finds a login already running keeps using the old token, and a failed
// login keeps it (and backs off) instead of failing the call. failed, when set, is the token a 403
// was answered with: a re-login is forced unless another caller already replaced it, at most once
// per forcedEvery.
func (c *Client) authToken(ctx context.Context, failed string) (string, error) {
	tok, valid, due := c.current()
	force := failed != "" && failed == tok
	if force {
		c.mu.Lock()
		if c.now().Sub(c.lastForced) < forcedEvery {
			force = false
		} else {
			c.lastForced = c.now()
		}
		c.mu.Unlock()
		if !force {
			return tok, nil
		}
	} else if failed != "" {
		return tok, nil // someone else already replaced the token
	}
	if !due && !force {
		return tok, nil
	}
	// one login at a time; with a valid token in hand, do not wait for someone else's login
	select {
	case c.login1 <- struct{}{}:
	default:
		if valid && !force {
			return tok, nil
		}
		select {
		case c.login1 <- struct{}{}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	defer func() { <-c.login1 }()
	// someone may have logged in while we waited
	if tok2, valid2, due2 := c.current(); valid2 && !due2 && tok2 != failed {
		return tok2, nil
	}
	// the login gets its own deadline, so a slow auth endpoint cannot use up the caller's
	lctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	newTok, ttl, err := c.login(lctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if err != nil {
		if c.token != "" && now.Before(c.expires) {
			c.renewAt = minTime(now.Add(loginBackoff), c.expires)
			return c.token, nil
		}
		return "", err
	}
	if ttl <= 0 {
		// no lease (a token file managed by Vault Agent): re-read it regularly
		ttl = 90 * time.Second
	}
	c.token, c.expires, c.renewAt = newTok, now.Add(ttl), now.Add(ttl*2/3)
	return newTok, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (c *Client) login(ctx context.Context) (string, time.Duration, error) {
	switch c.cfg.AuthKind {
	case AuthTokenFile:
		tok, err := readTokenFile(c.cfg.TokenFile)
		return tok, 0, err
	case AuthKubernetes, AuthJWT:
		jwt, err := readTokenFile(c.cfg.TokenFile)
		if err != nil {
			return "", 0, err
		}
		var out struct {
			Auth *struct {
				ClientToken   string `json:"client_token"`
				LeaseDuration int    `json:"lease_duration"`
			} `json:"auth"`
		}
		body := map[string]string{"role": c.cfg.Role, "jwt": jwt}
		if _, err := c.do(ctx, "", http.MethodPost, "auth/"+c.cfg.AuthMount+"/login", body, &out, 1); err != nil {
			var ve *Error
			if errors.As(err, &ve) {
				return "", 0, fmt.Errorf("vault: login refused (status %d)", ve.Status)
			}
			return "", 0, err
		}
		if out.Auth == nil || out.Auth.ClientToken == "" {
			return "", 0, errors.New("vault: login returned no token")
		}
		return out.Auth.ClientToken, time.Duration(out.Auth.LeaseDuration) * time.Second, nil
	}
	return "", 0, errors.New("vault: unknown auth kind")
}

// Do sends an authenticated request to /v1/<path> and decodes the JSON answer into out. The call
// makes at most 3 attempts in total; a 403 may use one of them on a request with a new token.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	tok, err := c.authToken(ctx, "")
	if err != nil {
		return err
	}
	used, err := c.do(ctx, tok, method, path, body, out, 3)
	var ve *Error
	if errors.As(err, &ve) && ve.Status == http.StatusForbidden && used < 3 {
		if tok2, lerr := c.authToken(ctx, tok); lerr == nil && tok2 != tok {
			_, err = c.do(ctx, tok2, method, path, body, out, 3-used)
		}
	}
	return err
}

// do makes up to attempts attempts (retrying transport failures, 5xx and 429) and returns how many it
// used.
func (c *Client) do(ctx context.Context, token, method, path string, body, out any, attempts int) (int, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return 0, err
		}
	}
	u := *c.base
	u.Path = "/v1/" + path
	var lastErr error
	attempt := 0
	for ; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return attempt, ctx.Err()
			case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
			}
		}
		actx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
		req, err := http.NewRequestWithContext(actx, method, u.String(), bytes.NewReader(payload))
		if err != nil {
			cancel()
			return attempt + 1, err
		}
		if token != "" {
			req.Header.Set("X-Vault-Token", token)
		}
		if c.cfg.Namespace != "" {
			req.Header.Set("X-Vault-Namespace", c.cfg.Namespace)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			cancel()
			lastErr = ErrUnreachable
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if rerr != nil {
			lastErr = ErrUnreachable
			continue
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			lastErr = vaultError(resp.StatusCode, data)
			continue
		}
		if resp.StatusCode >= 300 {
			return attempt + 1, vaultError(resp.StatusCode, data)
		}
		if out == nil || len(data) == 0 {
			return attempt + 1, nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return attempt + 1, errors.New("vault: unreadable answer")
		}
		return attempt + 1, nil
	}
	return attempt, lastErr
}

// vaultError keeps Vault's own error strings, cut to 200 characters; nothing else from the body.
func vaultError(status int, data []byte) error {
	var e struct {
		Errors []string `json:"errors"`
	}
	_ = json.Unmarshal(data, &e)
	msg := strings.Join(e.Errors, "; ")
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return &Error{Status: status, Msg: msg}
}
