package serve

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
)

// Listener is one configured address.
type Listener struct {
	config.Listener
	// Net, if set, is used instead of listening on Addr (tests).
	Net net.Listener
}

// Server runs listeners over a handler.
type Server struct {
	Handler         *Handler
	Listeners       []Listener
	TrustedProxies  []netip.Prefix
	DrainTimeout    time.Duration
	ShutdownTimeout time.Duration
	Log             *slog.Logger

	bound []boundListener
}

// certReloader serves a certificate from files, re-reading them when they change (checked at most
// once a minute).
type certReloader struct {
	certFile, keyFile string
	mu                sync.Mutex
	cert              *tls.Certificate
	modCert, modKey   time.Time
	checked           time.Time
}

func newCertReloader(cfg *config.TLSConfig) (*certReloader, error) {
	c := &certReloader{certFile: cfg.CertFile, keyFile: cfg.KeyFile}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *certReloader) load() error {
	ci, err1 := os.Stat(c.certFile)
	ki, err2 := os.Stat(c.keyFile)
	if err1 != nil || err2 != nil {
		return errors.New("serve: cannot read the TLS certificate or key file")
	}
	if c.cert != nil && ci.ModTime().Equal(c.modCert) && ki.ModTime().Equal(c.modKey) {
		return nil
	}
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return errors.New("serve: the TLS certificate and key do not load")
	}
	c.cert, c.modCert, c.modKey = &cert, ci.ModTime(), ki.ModTime()
	return nil
}

func (c *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.checked) > time.Minute {
		c.checked = time.Now()
		_ = c.load() // keep the current certificate if the new files do not load
	}
	return c.cert, nil
}

// dualListener serves TLS or plain http on one port, by the first byte of each connection. The
// first byte is read in a goroutine per connection, so a client that sends nothing never holds up
// the accept loop.
type dualListener struct {
	net.Listener
	tls   *tls.Config
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
	err   error
}

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }

func newDualListener(ln net.Listener, cfg *tls.Config) *dualListener {
	d := &dualListener{Listener: ln, tls: cfg, conns: make(chan net.Conn), done: make(chan struct{})}
	go d.loop()
	return d
}

func (d *dualListener) loop() {
	for {
		c, err := d.Listener.Accept()
		if err != nil {
			d.err = err
			d.once.Do(func() { close(d.done) })
			return
		}
		go d.sniff(c)
	}
}

func (d *dualListener) sniff(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		_ = c.Close()
		return
	}
	var out net.Conn = &peekedConn{Conn: c, r: br}
	if first[0] == 0x16 { // a TLS handshake record
		out = tls.Server(out, d.tls)
	}
	select {
	case d.conns <- out:
	case <-d.done:
		_ = c.Close()
	}
}

func (d *dualListener) Accept() (net.Conn, error) {
	select {
	case c := <-d.conns:
		return c, nil
	case <-d.done:
		if d.err != nil {
			return nil, d.err
		}
		return nil, net.ErrClosed
	}
}

func (d *dualListener) Close() error {
	err := d.Listener.Close()
	d.once.Do(func() { close(d.done) })
	return err
}

// Bind opens every listener (Run calls it if it was not called).
func (s *Server) Bind() error {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.bound != nil {
		return nil
	}
	var out []boundListener
	for _, l := range s.Listeners {
		srv, ln, err := s.build(l)
		if err != nil {
			for _, o := range out {
				_ = o.ln.Close()
			}
			return err
		}
		out = append(out, boundListener{l, srv, ln})
	}
	s.bound = out
	return nil
}

type boundListener struct {
	l   Listener
	srv *http.Server
	ln  net.Listener
}

// Run serves every listener until ctx is done, then drains and shuts down.
func (s *Server) Run(ctx context.Context) error {
	if err := s.Bind(); err != nil {
		return err
	}
	var servers []*http.Server
	errs := make(chan error, len(s.bound))
	for _, b := range s.bound {
		l, srv, ln := b.l, b.srv, b.ln
		servers = append(servers, srv)
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("serve: listener %s: %w", l.Addr, err)
			}
		}()
		s.Log.Info("serve: listening", "addr", ln.Addr().String(), "scheme", l.Scheme, "behind_proxy", l.BehindProxy)
	}
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errs:
	}
	// drain: fail readiness first, so load balancers stop sending, then shut down
	s.Handler.SetStopping()
	time.Sleep(s.DrainTimeout)
	sctx, cancel := context.WithTimeout(context.Background(), s.ShutdownTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Shutdown(sctx); err != nil {
				_ = srv.Close() // abort what is still downloading
			}
		}()
	}
	wg.Wait()
	return runErr
}

func (s *Server) build(l Listener) (*http.Server, net.Listener, error) {
	srv := &http.Server{
		DisableGeneralOptionsHandler: true, // "OPTIONS *" reaches our handler, which answers 404
		ReadHeaderTimeout:            10 * time.Second,
		IdleTimeout:                  120 * time.Second,
		MaxHeaderBytes:               64 << 10,
		ErrorLog:                     slog.NewLogLogger(s.Log.Handler(), slog.LevelWarn),
		HTTP2:                        &http.HTTP2Config{MaxConcurrentStreams: 16},
	}
	ln := l.Net
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", l.Addr); err != nil {
			return nil, nil, fmt.Errorf("serve: listen %s: %w", l.Addr, err)
		}
	}
	var tlsCfg *tls.Config
	if l.TLS != nil {
		cr, err := newCertReloader(l.TLS)
		if err != nil {
			_ = ln.Close()
			return nil, nil, err
		}
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: cr.get, NextProtos: []string{"h2", "http/1.1"}}
	}
	h := s.Handler
	switch {
	case l.Scheme == "dual":
		dualTLS := tlsCfg.Clone()
		dualTLS.NextProtos = []string{"http/1.1"} // HTTP/2 needs the server's own TLS setup
		ln = newDualListener(ln, dualTLS)
		srv.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
			if _, ok := c.(*tls.Conn); ok {
				return withConn(ctx, "https", l.Addr)
			}
			return withConn(ctx, "http", l.Addr)
		}
		srv.Handler = h
	case l.Scheme == "https" && tlsCfg != nil:
		ln = tls.NewListener(ln, tlsCfg)
		srv.TLSConfig = tlsCfg
		srv.ConnContext = func(ctx context.Context, _ net.Conn) context.Context { return withConn(ctx, "https", l.Addr) }
		srv.Handler = h
	case l.Scheme == "https": // behind_proxy
		srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(h.o.TrustedProxies) > 0 {
				host, _, _ := net.SplitHostPort(r.RemoteAddr)
				if a, err := netip.ParseAddr(host); err != nil || !h.trusted(a) {
					h.log.Warn("serve: a request from a peer that is not a trusted proxy was refused", "listener", l.Addr, "peer", host)
					panic(http.ErrAbortHandler) // only the proxy may reach a behind_proxy listener
				}
			}
			sch := "http"
			if v := r.Header.Values("X-Forwarded-Proto"); len(v) == 1 && v[0] == "https" {
				sch = "https" // the header can only downgrade: anything but exactly one "https" is http
			}
			h.ServeHTTP(w, r.WithContext(withConn(r.Context(), sch, l.Addr)))
		})
	default: // http
		srv.ConnContext = func(ctx context.Context, _ net.Conn) context.Context { return withConn(ctx, "http", l.Addr) }
		srv.Handler = h
	}
	return srv, ln, nil
}
