package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// ErrNotFound is a 404 answer.
var ErrNotFound = errors.New("egress: not found")

// DownloadOptions bound a download (spec 0009).
type DownloadOptions struct {
	ETag     string        // sent as If-None-Match when set
	MaxBytes int64         // the largest body read
	Timeout  time.Duration // the whole download
	MinRate  int64         // bytes per second, after Grace
	Grace    time.Duration // before the rate floor applies (default 30s)
	// Header is sent with the request (an upstream's Authorization, spec 0009 phase 3); a redirect
	// is never followed, so it reaches the URL's host only.
	Header http.Header
}

// Download is a download's outcome: NotModified for a 304 (nothing written), otherwise the body's
// size and the answer's ETag.
type Download struct {
	NotModified bool
	ETag        string
	Size        int64
}

// Download fetches a URL into w, through the same checks as Get, streaming: a 200 answer of at
// most MaxBytes, read at no less than MinRate once Grace has passed, within Timeout. A 304 answers
// NotModified; a 404 is ErrNotFound.
func (c *Client) Download(ctx context.Context, raw string, o DownloadOptions, w io.Writer) (Download, error) {
	u, err := c.checkURL(raw)
	if err != nil {
		return Download{}, err
	}
	if o.Grace == 0 {
		o.Grace = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Download{}, fmt.Errorf("%w: not an absolute URL", ErrRefused)
	}
	for k, vs := range o.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if o.ETag != "" {
		req.Header.Set("If-None-Match", o.ETag)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		var oe *net.OpError
		if errors.Is(err, ErrRefused) || errors.As(err, &oe) && errors.Is(oe.Err, ErrRefused) {
			return Download{}, fmt.Errorf("%w: %s", ErrRefused, u.Hostname())
		}
		return Download{}, fmt.Errorf("%w: %s", ErrConnect, u.Hostname())
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return Download{NotModified: true, ETag: resp.Header.Get("ETag")}, nil
	case http.StatusNotFound:
		return Download{}, fmt.Errorf("%w: %s", ErrNotFound, u.Hostname())
	case http.StatusUnauthorized, http.StatusForbidden:
		return Download{}, &AuthError{Status: resp.StatusCode, Host: u.Hostname()}
	default:
		return Download{}, fmt.Errorf("%w %d from %s", ErrStatus, resp.StatusCode, u.Hostname())
	}
	// the rate floor: a watchdog cancels a download that falls behind
	var n atomic.Int64
	start := time.Now()
	slow := atomic.Bool{}
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				if el := now.Sub(start) - o.Grace; o.MinRate > 0 && el > 0 && float64(n.Load()) < float64(o.MinRate)*el.Seconds() {
					slow.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	size, err := io.Copy(w, &countReader{r: io.LimitReader(resp.Body, o.MaxBytes+1), n: &n})
	switch {
	case slow.Load():
		return Download{}, fmt.Errorf("%w: %s is too slow", ErrConnect, u.Hostname())
	case err != nil:
		return Download{}, fmt.Errorf("%w: %s", ErrConnect, u.Hostname())
	case size > o.MaxBytes:
		return Download{}, fmt.Errorf("%w: %s", ErrTooLarge, u.Hostname())
	}
	return Download{ETag: resp.Header.Get("ETag"), Size: size}, nil
}

type countReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n.Add(int64(k))
	return k, err
}

// PortAllowed reports whether a port can be dialed at all: 443, or a port an allowlist entry
// names (the dial still checks each address).
func (c *Client) PortAllowed(port uint16) bool {
	if port == 443 {
		return true
	}
	for _, al := range c.allow {
		for _, p := range al.Ports {
			if p == port {
				return true
			}
		}
	}
	return c.loopback
}
