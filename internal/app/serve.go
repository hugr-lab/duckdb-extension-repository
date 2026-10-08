package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/api"
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
	verifier := &auth.Verifier{Fetch: eg}
	server, err := ServerAuth(ctx, cfg, st)
	if err != nil {
		return err
	}
	auths, snaps := &auth.TenantAuths{Store: st}, &release.Snapshots{Store: st}
	grants := authz.Grants{Store: st, Auths: auths}
	maxBody, maxIngests := cfg.BlobLimits()
	perActor, perTenant := cfg.PublishLimits()
	mgmt := svc.WithAuthz(grants)
	apiHandler := api.New(api.Options{Store: st, Snapshots: snaps, Auths: auths, Verifier: verifier,
		PublicURL: cfg.Serve.PublicURL, Rate: cfg.Serve.APIRate, Burst: cfg.Serve.APIBurst, Log: log, KistaVersion: Version,
		Server: server, Authz: grants, Tenants: mgmt.Tenants, Auth: mgmt.Auth, Keys: mgmt.Keys,
		Releases: mgmt.Releases, MaxBody: maxBody, MinRate: int64(lim.MinRate), PublishPerActor: perActor,
		PublishPerTenant: perTenant, PublishMax: max(1, maxIngests-1), AdminTokenMaxAge: cfg.AdminTokenMaxAge()})
	h := serve.NewHandler(st, svc.Keys, bs, serve.Options{
		PublicURL: cfg.Serve.PublicURL, Verifier: verifier, Server: server, Auths: auths, Snapshots: snaps, API: apiHandler,
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

// ServerAuth builds server identity (spec 0007): the server issuers with a verifier and an egress
// client of their own, and their audiences, which no tenant may hold (a conflict refuses to start).
// Without server issuers no server token is accepted.
func ServerAuth(ctx context.Context, cfg config.Config, st *store.Store) (*auth.Server, error) {
	issuers := cfg.ServerIssuers()
	if len(issuers) == 0 {
		return nil, nil
	}
	auds := cfg.ServerAudiences()
	assigned, err := st.AssignedAudiences(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range assigned {
		if slices.Contains(auds, a) {
			return nil, fmt.Errorf("app: %s is a server audience and a tenant's assigned audience; remove one", a)
		}
	}
	eg, err := ServerEgress(cfg)
	if err != nil {
		return nil, err
	}
	return &auth.Server{Issuers: issuers, Audiences: auds, Admins: cfg.ServerAdmins(), Verifier: &auth.Verifier{Fetch: eg}}, nil
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
