package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/idna"
)

// Errors, one per class; messages carry the host and nothing else.
var (
	ErrRefused  = errors.New("egress: refused")
	ErrConnect  = errors.New("egress: cannot connect")
	ErrStatus   = errors.New("egress: unexpected status")
	ErrTooLarge = errors.New("egress: response too large")
)

// Config configures a client.
type Config struct {
	Allow             []Allow
	AllowLoopbackHTTP bool   // development only (config enforces it)
	OwnHost           string // the deployment's public host: never fetched
	Proxy             *url.URL
	CAFile            string
	Timeout           time.Duration // total per request; default 10s
	MaxBytes          int64         // default 1 MiB
	// Resolve replaces DNS resolution (tests).
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
}

// Client fetches URLs on a tenant's behalf.
type Client struct {
	allow    []Allow
	loopback bool
	ownHost  string
	proxy    *url.URL
	timeout  time.Duration
	maxBytes int64
	resolve  func(ctx context.Context, host string) ([]netip.Addr, error)
	hc       *http.Client
}

// New returns a client.
func New(cfg Config) (*Client, error) {
	c := &Client{allow: cfg.Allow, loopback: cfg.AllowLoopbackHTTP, ownHost: normalHost(cfg.OwnHost),
		proxy: cfg.Proxy, timeout: cfg.Timeout, maxBytes: cfg.MaxBytes, resolve: cfg.Resolve}
	if c.timeout == 0 {
		c.timeout = 10 * time.Second
	}
	if c.maxBytes == 0 {
		c.maxBytes = 1 << 20
	}
	if c.resolve == nil {
		c.resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, errors.New("egress: cannot read ca_file")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("egress: ca_file holds no certificate")
		}
		tlsCfg.RootCAs = pool
	}
	c.hc = &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil, // never from the environment; an explicit proxy is dialed below
			DialContext:           c.dial,
			TLSClientConfig:       tlsCfg,
			TLSHandshakeTimeout:   c.timeout,
			ResponseHeaderTimeout: c.timeout,
			DisableCompression:    true, // the size cap applies to what is read
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c, nil
}

// dial resolves the host itself, checks every address, and connects to a checked one: directly, or
// through the explicit proxy with CONNECT to that address. A Control hook re-checks the address the
// socket actually connects to.
func (c *Client) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrRefused
	}
	p, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, ErrRefused
	}
	port := uint16(p)
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else if addrs, err = c.resolve(ctx, host); err != nil {
		// one answer for "does not resolve" and "resolves inside": neither tells a tenant administrator
		// which internal names exist
		return nil, fmt.Errorf("%w: %s cannot be reached", ErrRefused, host)
	}
	var checked []netip.Addr
	for _, a := range addrs {
		if c.check(a, port) || c.loopback && a.Unmap().IsLoopback() {
			checked = append(checked, a.Unmap())
		}
	}
	if len(checked) == 0 || len(checked) != len(addrs) {
		// one refused address refuses the host: a name that resolves to an internal address too is
		// not trusted for its public ones either
		return nil, fmt.Errorf("%w: %s cannot be reached", ErrRefused, host)
	}
	d := &net.Dialer{Timeout: c.timeout, Control: func(_, addr string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(addr)
		if err != nil {
			return ErrRefused
		}
		if c.proxy != nil {
			return nil // the proxy's own address; the target was checked above
		}
		if !c.check(ap.Addr(), ap.Port()) && (!c.loopback || !ap.Addr().Unmap().IsLoopback()) {
			return ErrRefused
		}
		return nil
	}}
	target := netip.AddrPortFrom(checked[0], port).String()
	if c.proxy == nil {
		for _, a := range checked { // each checked address in turn (a dual-stack host without IPv6 routing)
			conn, err := d.DialContext(ctx, network, netip.AddrPortFrom(a, port).String())
			if err == nil {
				return conn, nil
			}
			if ctx.Err() != nil {
				break
			}
		}
		return nil, fmt.Errorf("%w: %s", ErrConnect, host)
	}
	conn, err := d.DialContext(ctx, network, c.proxy.Host)
	if err != nil {
		return nil, fmt.Errorf("%w: the egress proxy", ErrConnect)
	}
	_ = conn.SetDeadline(time.Now().Add(c.timeout))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: the egress proxy", ErrConnect)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != http.StatusOK || br.Buffered() > 0 {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: the egress proxy refused %s", ErrConnect, host)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// checkURL refuses what egress never fetches: other schemes, userinfo, the deployment's own host,
// ports other than 443 unless allowlisted (the dial checks the port per address).
func (c *Client) checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" {
		return nil, fmt.Errorf("%w: not an absolute URL", ErrRefused)
	}
	switch u.Scheme {
	case "https":
	case "http":
		a, err := netip.ParseAddr(u.Hostname())
		if !c.loopback || (err != nil || !a.Unmap().IsLoopback()) && !strings.EqualFold(u.Hostname(), "localhost") {
			return nil, fmt.Errorf("%w: %s is not https", ErrRefused, u.Hostname())
		}
	default:
		return nil, fmt.Errorf("%w: %s is not https", ErrRefused, u.Hostname())
	}
	if strings.Contains(u.Hostname(), "%") {
		return nil, fmt.Errorf("%w: a zoned address", ErrRefused)
	}
	if c.ownHost != "" && normalHost(u.Hostname()) == c.ownHost {
		return nil, fmt.Errorf("%w: %s is this deployment", ErrRefused, u.Hostname())
	}
	return u, nil
}

// normalHost is a host name as resolution sees it: the port dropped, IDNA-mapped, lowercase,
// without a trailing dot.
func normalHost(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	if a, err := idna.Lookup.ToASCII(h); err == nil {
		h = a
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// Get fetches a URL: a 200 answer of at most MaxBytes, its body and headers.
func (c *Client) Get(ctx context.Context, raw string) ([]byte, http.Header, error) {
	u, err := c.checkURL(raw)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: not an absolute URL", ErrRefused)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		var refused bool
		var oe *net.OpError
		refused = errors.Is(err, ErrRefused) || errors.As(err, &oe) && errors.Is(oe.Err, ErrRefused)
		if refused {
			return nil, nil, fmt.Errorf("%w: %s", ErrRefused, u.Hostname())
		}
		return nil, nil, fmt.Errorf("%w: %s", ErrConnect, u.Hostname())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("%w from %s", ErrStatus, u.Hostname())
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrConnect, u.Hostname())
	}
	if int64(len(b)) > c.maxBytes {
		return nil, nil, fmt.Errorf("%w: %s", ErrTooLarge, u.Hostname())
	}
	return b, resp.Header, nil
}
