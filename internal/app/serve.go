package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/api"
	"github.com/hugr-lab/duckdb-extension-repository/internal/audit/buffer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/audit/sinks"
	"github.com/hugr-lab/duckdb-extension-repository/internal/audit/writer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/serve"
	"github.com/hugr-lab/duckdb-extension-repository/internal/stats"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/telemetry"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
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
	providers := svc.Auth.Providers
	if recs, err := st.IssuerURLs(ctx); err != nil {
		log.Error("serve: reading issuer records", "error", err)
	} else {
		for _, r := range recs {
			if providers.Has(r[2]) {
				log.Warn("serve: an issuer record has a trusted-publishing provider's URL and is not used; add a publisher instead",
					"tenant_id", r[0], "issuer", r[1], "url", r[2])
			}
		}
	}
	// events (spec 0010): refusals through the asynchronous writer; the sinks' registry, loaded
	// again for serve's own resolver, which refreshes
	events := &writer.Writer{Store: st, Log: log}
	resolver, err := sinks.Attach(ctx, st, cfg.Events.Sinks)
	if errors.Is(err, store.ErrNoSinkBit) {
		log.Error("serve: an event sink has no bit yet; removed sinks' bits are freed 10 minutes after they leave every configuration", "error", err)
	} else if err != nil {
		return err
	}
	host, _ := os.Hostname()
	if len(host) > 120 {
		host = host[:120] // leases.holder is 200 characters
	}
	instance := host + "/" + store.NewID()
	eventSinks, err := EventSinks(cfg, instance)
	if err != nil {
		return err
	}
	if err := ServerStart(ctx, cfg, st); err != nil {
		log.Error("serve: recording server.start", "error", err)
	}
	maxBody, maxIngests := cfg.BlobLimits()
	perActor, perTenant := cfg.PublishLimits()
	mgmt := svc.WithAuthz(grants)
	apiHandler := api.New(api.Options{Store: st, Snapshots: snaps, Auths: auths, Verifier: verifier,
		PublicURL: cfg.Serve.PublicURL, Rate: cfg.Serve.APIRate, Burst: cfg.Serve.APIBurst, Log: log, KistaVersion: Version,
		Server: server, Providers: providers, Authz: grants, Tenants: mgmt.Tenants, Auth: mgmt.Auth, Keys: mgmt.Keys,
		Releases: mgmt.Releases, Upstreams: mgmt.Upstreams, MaxBody: maxBody, MinRate: int64(lim.MinRate), PublishPerActor: perActor,
		PublishPerTenant: perTenant, PublishMax: max(1, maxIngests-1), AdminTokenMaxAge: cfg.AdminTokenMaxAge(), Events: events})
	// pull-through (spec 0009 phase 2): misses of callers holding install, fetched in the background
	puller := &upstream.Puller{Service: svc.Upstreams, Holder: host + "/" + store.NewID(), Log: log,
		NegativeTTL: cfg.UpstreamLimits().NegativeTTL}
	// download statistics (spec 0010 phase 2a): counts every minute, installers once a day
	downloads := &stats.Counter{Store: st, Log: log}
	// OpenTelemetry metrics (spec 0010 phase 2b): where they go is the OTEL_* environment's
	mp, flushMetrics, err := telemetry.Setup(ctx, telemetry.Options{Version: Version, Instance: instance,
		MaxSeries: cfg.TelemetrySettings().Metrics.MaxSeries, Log: log})
	if err != nil {
		return err
	}
	defer func() {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := flushMetrics(fctx); err != nil {
			log.Error("serve: flushing metrics", "error", err)
		}
	}()
	var served atomic.Pointer[serve.Handler] // the gauge may be read before the handler is made
	tenants := metricTenants(cfg)
	metrics, err := telemetry.New(mp, tenants, metricLabels(cfg), telemetry.Gauges{
		Log: log,
		DownloadsActive: func() int64 {
			if h := served.Load(); h != nil {
				return h.DownloadsActive()
			}
			return 0
		},
		UploadsActive: apiHandler.UploadsActive,
		PullQueue:     puller.Queued,
		EventsDropped: events.Dropped,
		EventsPending: func(ctx context.Context) (map[string]int64, error) {
			counts, err := st.PendingCounts(ctx, resolver.Masks())
			if err != nil {
				return nil, err
			}
			return sinkNames(cfg, tenants, counts), nil
		},
		UpstreamCells: st.CellOutcomes,
	})
	if err != nil {
		return err
	}
	if telemetry.Enabled() {
		log.Info("serve: exporting OpenTelemetry metrics")
	}
	h := serve.NewHandler(st, svc.Keys, bs, serve.Options{Puller: puller, Events: events, Downloads: downloads, Metrics: metrics,
		PublicURL: cfg.Serve.PublicURL, Verifier: verifier, Server: server, Providers: providers, Auths: auths, Snapshots: snaps, API: apiHandler,
		MaxDownloads: lim.MaxDownloads, MaxDownloadsPerClient: lim.MaxDownloadsPerClient, MinRate: int64(lim.MinRate),
		WriteIdleTimeout: lim.WriteIdleTimeout, TrustedProxies: cfg.TrustedProxyPrefixes(), Log: log,
	})
	served.Store(h)
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
		r := &release.Resigner{Service: svc.Releases, Holder: host + "/" + store.NewID(), Log: log}
		wg.Add(1)
		go func() { defer wg.Done(); r.Run(bg) }()
	}
	// upstream runs (spec 0009): every replica polls; a run's lease keeps it on one
	// downloads a crash left behind are swept: the directory is this replica's (blob.spool_dir)
	_ = os.RemoveAll(svc.Upstreams.TempDir)
	if err := os.MkdirAll(svc.Upstreams.TempDir, 0o700); err != nil {
		return fmt.Errorf("app: the upstream download directory: %w", err)
	}
	svc.Upstreams.Log = log
	ev := cfg.EventSettings()
	pruner := &buffer.Pruner{Store: st, Holder: host + "/" + store.NewID(), Retention: ev.Retention, MaxRowsPerTenant: ev.MaxRowsPerTenant, Log: log}
	wg.Add(1)
	go func() { defer wg.Done(); pruner.Run(bg) }()
	// the writer outlives the listeners: refusals while draining are written
	evCtx, evStop := context.WithCancel(context.WithoutCancel(ctx))
	evDone := make(chan struct{})
	go func() { defer close(evDone); events.Run(evCtx) }()
	defer func() { evStop(); <-evDone }()
	cntDone := make(chan struct{})
	go func() { defer close(cntDone); downloads.Run(evCtx) }() // counts of draining downloads too
	defer func() { evStop(); <-cntDone }()
	// storage garbage collection (spec 0016): the resolver always, full passes every gc.interval
	collector := Collector(cfg, st, bs, instance, log)
	wg.Add(1)
	go func() { defer wg.Done(); collector.Run(bg, cfg.GCSettings().Interval) }()
	daily := &stats.Daily{Store: st, Holder: instance, Retention: cfg.StatisticsSettings().Retention, EventRetention: ev.Retention, Log: log}
	wg.Add(1)
	go func() { defer wg.Done(); daily.Run(bg) }()
	m := &sinks.Manager{Store: st, Resolver: resolver, Sinks: eventSinks, Holder: instance, Log: log}
	wg.Add(1)
	go func() { defer wg.Done(); m.Run(bg) }()
	ur := &upstream.Runner{Service: svc.Upstreams, Holder: host + "/" + store.NewID(), Log: log}
	wg.Add(2)
	go func() { defer wg.Done(); ur.Run(bg) }()
	go func() { defer wg.Done(); puller.Run(bg) }()
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

// metricTenants are the tenants metrics may name (telemetry.metrics.tenants).
func metricTenants(cfg config.Config) telemetry.Tenants {
	t := telemetry.Tenants{Names: map[string]bool{}}
	for _, n := range cfg.Telemetry.Metrics.Tenants {
		if n == "*" {
			t.All = true
		} else {
			t.Names[n] = true
		}
	}
	return t
}

// metricLabels are kista.downloads' optional labels (telemetry.metrics.labels).
func metricLabels(cfg config.Config) telemetry.Labels {
	ls := cfg.Telemetry.Metrics.Labels
	return telemetry.Labels{Version: slices.Contains(ls, "version"), Platform: slices.Contains(ls, "platform"),
		DuckDBVersion: slices.Contains(ls, "duckdb_version")}
}

// sinkNames names a sink's pending count only when metrics may name every tenant it takes (or it
// takes the server's events only); the others count together as "_other" (never a sink's name): a
// sink's name may be a tenant's.
func sinkNames(cfg config.Config, tenants telemetry.Tenants, counts map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for _, s := range cfg.Events.Sinks {
		n, ok := counts[s.Name]
		if !ok {
			continue
		}
		named := !slices.Contains(s.Tenants, "*") || tenants.All
		for _, t := range s.Tenants {
			if t != "*" && !tenants.All && !tenants.Names[t] {
				named = false
			}
		}
		if named {
			out[s.Name] += n
		} else {
			out["_other"] += n // never a sink's name
		}
	}
	return out
}
