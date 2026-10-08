// Package transport is the HTTP transport of kista's remote object stores (spec 0005): no proxy
// from the environment, TLS 1.2 or later with the system roots or one CA bundle, and a timeout on
// connecting, on response headers, and on every read or write that makes no progress.
package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"os"
	"time"
)

// New returns the transport. caFile, if set, is the only trusted CA bundle.
func New(timeout time.Duration, caFile string) (*http.Transport, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, errors.New("blob: cannot read ca_file")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("blob: ca_file holds no certificate")
		}
		tlsCfg.RootCAs = pool
	}
	dialer := &net.Dialer{Timeout: timeout}
	return &http.Transport{
		Proxy: nil, // never from the environment
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &idleConn{Conn: c, idle: timeout}, nil
		},
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		MaxIdleConnsPerHost:   16,
		// a pooled connection has a read pending (the transport's read loop), whose deadline was
		// set when it went idle: close it well before that deadline can fire under a new request
		IdleConnTimeout: timeout / 2,
	}, nil
}

// NoRedirects is a CheckRedirect that stops at the first response.
func NoRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// idleConn fails a connection that makes no progress for idle: a stalled server cannot hold a
// request (an upload it stops reading, a download it stops sending) longer than that. Every read
// and every write moves the deadline of both directions: while a request body is being written, the
// transport's read loop is already blocked in a read, and its deadline must not run out under an
// upload that is still progressing.
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(p []byte) (int, error) {
	_ = c.SetDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	_ = c.SetDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(p)
}
