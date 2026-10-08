package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/serve"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Serve runs kista serve until ctx is done (spec 0006): the store, the blob service, the handler,
// the listeners, readiness, the public check of storage domains, and the re-signer if enabled.
func Serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if len(cfg.Serve.Listeners) == 0 {
		return errors.New("app: serve.listeners is empty")
	}
	st, err := OpenStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	svc, err := NewServices(cfg, st, authz.ServerAdmin{})
	if err != nil {
		return err
	}
	bs, err := BlobService(ctx, cfg, st, log)
	if err != nil {
		return err
	}
	defer func() { _ = bs.Close() }()
	svc.WithBlob(bs)
	lim := cfg.ServeLimits()
	eg, err := Egress(cfg)
	if err != nil {
		return err
	}
	h := serve.NewHandler(st, svc.Keys, bs, serve.Options{
		PublicURL: cfg.Serve.PublicURL, Verifier: &auth.Verifier{Fetch: eg},
		MaxDownloads: lim.MaxDownloads, MaxDownloadsPerClient: lim.MaxDownloadsPerClient, MinRate: int64(lim.MinRate),
		WriteIdleTimeout: lim.WriteIdleTimeout, TrustedProxies: cfg.TrustedProxyPrefixes(), Log: log,
	})
	var ls []serve.Listener
	for _, l := range cfg.Serve.Listeners {
		ls = append(ls, serve.Listener{Listener: l})
	}
	srv := &serve.Server{Handler: h, Listeners: ls, TrustedProxies: cfg.TrustedProxyPrefixes(),
		DrainTimeout: lim.DrainTimeout, ShutdownTimeout: lim.ShutdownTimeout, Log: log}
	// bind every listener before any background work: a port in use fails before a lease is taken
	if err := srv.Bind(); err != nil {
		return err
	}
	bg, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { stop(); wg.Wait() }() // the re-signer releases its lease before the store closes
	wg.Add(2)
	go func() { defer wg.Done(); readiness(bg, st, h) }()
	go func() { defer wg.Done(); publicCheck(bg, bs, h) }()
	if cfg.Serve.Resign {
		host, _ := os.Hostname()
		if len(host) > 120 {
			host = host[:120] // leases.holder is 200 characters
		}
		r := &release.Resigner{Service: svc.Releases, Holder: host + "/" + store.NewID(), Log: log}
		wg.Add(1)
		go func() { defer wg.Done(); r.Run(bg) }()
	}
	return srv.Run(ctx)
}

// readiness recomputes /readyz every 5 seconds: the store reachable and its schema readable.
func readiness(ctx context.Context, st *store.Store, h *serve.Handler) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		h.SetReady(st.Ready(cctx) == nil)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// publicCheck finds storage domains readable without credentials every 10 minutes.
func publicCheck(ctx context.Context, bs interface {
	PublicDomains(context.Context) []string
}, h *serve.Handler) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.SetPublicDomains(bs.PublicDomains(ctx))
		}
	}
}
