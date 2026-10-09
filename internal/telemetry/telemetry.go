// Package telemetry is kista's OpenTelemetry metrics (spec 0010 phase 2b), set up from the standard
// environment as the hugr platform's other services (OTEL_EXPORTER_OTLP_*, OTEL_SERVICE_NAME,
// OTEL_RESOURCE_ATTRIBUTES, OTEL_METRIC_EXPORT_INTERVAL, OTEL_METRICS_EXPORTER, OTEL_SDK_DISABLED):
// nothing is exported unless an endpoint is set; OTLP over HTTP with protobuf only. Which tenants
// metrics may name, and which labels downloads carry, are kista's configuration: no principal, no
// address, no unlisted tenant ever is an attribute.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Name is the instrumentation scope.
const Name = "github.com/hugr-lab/duckdb-extension-repository"

// Enabled reports whether metrics are exported: an OTLP endpoint for them (or for every signal),
// OTEL_METRICS_EXPORTER not none, the SDK not disabled.
func Enabled() bool {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") || strings.EqualFold(os.Getenv("OTEL_METRICS_EXPORTER"), "none") {
		return false
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != ""
}

// checkEnv refuses what kista does not export: another exporter than OTLP, another protocol than
// http/protobuf (the metrics' own setting first, then the shared one).
func checkEnv() error {
	if e := os.Getenv("OTEL_METRICS_EXPORTER"); e != "" && !strings.EqualFold(e, "otlp") && !strings.EqualFold(e, "none") {
		return fmt.Errorf("telemetry: OTEL_METRICS_EXPORTER is %s: kista exports otlp only", e)
	}
	v, p := "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
	if p == "" {
		v, p = "OTEL_EXPORTER_OTLP_PROTOCOL", os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if p != "" && p != "http/protobuf" {
		return fmt.Errorf("telemetry: %s is %s: kista exports http/protobuf only", v, p)
	}
	return nil
}

// Options are what kista's configuration decides; the rest is the environment's.
type Options struct {
	Version   string // service.version
	Instance  string // service.instance.id: the replica
	MaxSeries int    // per instrument; beyond, one overflow series counts (default 10,000)
	Log       *slog.Logger
	// Reader replaces the OTLP exporter (tests).
	Reader sdkmetric.Reader
}

// Setup returns the meter provider and what flushes it at the end: a no-op one when metrics are not
// exported.
func Setup(ctx context.Context, o Options) (metric.MeterProvider, func(context.Context) error, error) {
	none := func(context.Context) error { return nil }
	if o.Log == nil {
		o.Log = slog.Default()
	}
	reader := o.Reader
	if reader == nil {
		if !Enabled() {
			return noop.NewMeterProvider(), none, nil
		}
		if err := checkEnv(); err != nil {
			return nil, nil, err
		}
		// the SDK's own client: the endpoint, headers, timeout, CA and client certificate from the
		// OTEL_EXPORTER_OTLP_(METRICS_)* environment, as the platform's other services
		exp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("telemetry: metrics: %w", err)
		}
		// the interval from OTEL_METRIC_EXPORT_INTERVAL, 60 seconds by default
		reader = sdkmetric.NewPeriodicReader(exp)
		// the SDK's errors (an export failing) in kista's log, at most once a minute
		var last atomic.Int64
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
			if now := time.Now().Unix(); now-last.Load() >= 60 {
				last.Store(now)
				o.Log.Warn("telemetry: exporting metrics", "error", err)
			}
		}))
	}
	// kista's defaults first: OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES, read after, win
	attrs := []attribute.KeyValue{semconv.ServiceName("kista"), semconv.ServiceVersion(o.Version)}
	if o.Instance != "" {
		attrs = append(attrs, semconv.ServiceInstanceID(o.Instance))
	}
	res, err := resource.New(ctx, resource.WithAttributes(attrs...), resource.WithFromEnv(), resource.WithTelemetrySDK())
	if errors.Is(err, resource.ErrPartialResource) {
		o.Log.Warn("telemetry: OTEL_RESOURCE_ATTRIBUTES is partly malformed; the rest is used", "error", err)
	} else if err != nil {
		return nil, nil, fmt.Errorf("telemetry: the resource: %w", err)
	}
	limit := o.MaxSeries
	if limit == 0 {
		limit = 10000
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(reader), sdkmetric.WithCardinalityLimit(limit))
	return mp, mp.Shutdown, nil
}

// Tenants are the tenants whose ids, channels and extensions metrics may name (every tenant with
// All; none by default).
type Tenants struct {
	All   bool
	Names map[string]bool
}

func (t Tenants) named(name string) bool { return t.All || t.Names[name] }

// Labels are the optional labels of kista.downloads.
type Labels struct{ Version, Platform, DuckDBVersion bool }

// Download is a counted download as metrics see it.
type Download struct {
	TenantID, TenantName, Channel, Extension, Version, Platform, DuckDBVersion string
	Authenticated                                                              bool
}

// Gauges read what the observable instruments report; each may be nil. A failing read is logged and
// its value left out: never the whole export.
type Gauges struct {
	Log             *slog.Logger
	DownloadsActive func() int64
	UploadsActive   func() int64
	PullQueue       func() int64
	EventsDropped   func() int64                                        // cumulative
	EventsPending   func(ctx context.Context) (map[string]int64, error) // per sink
	UpstreamCells   func(ctx context.Context) (map[string]int64, error) // per outcome
}

// Metrics are kista's instruments.
type Metrics struct {
	tenants   Tenants
	labels    Labels
	downloads metric.Int64Counter
	duration  metric.Float64Histogram
	reg       metric.Registration
}

// New makes the instruments on a meter provider.
func New(mp metric.MeterProvider, tenants Tenants, labels Labels, g Gauges) (*Metrics, error) {
	m := &Metrics{tenants: tenants, labels: labels}
	meter := mp.Meter(Name)
	var err error
	if m.downloads, err = meter.Int64Counter("kista.downloads", metric.WithUnit("{download}"),
		metric.WithDescription("Extension downloads served")); err != nil {
		return nil, err
	}
	if m.duration, err = meter.Float64Histogram("http.server.request.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of HTTP server requests"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10, 30, 60)); err != nil {
		return nil, err
	}
	type cb struct {
		inst metric.Int64Observable
		f    func(context.Context, metric.Observer, metric.Int64Observable) error
	}
	var cbs []cb
	value := func(f func() int64) func(context.Context, metric.Observer, metric.Int64Observable) error {
		return func(_ context.Context, o metric.Observer, i metric.Int64Observable) error {
			o.ObserveInt64(i, f())
			return nil
		}
	}
	logger := g.Log
	if logger == nil {
		logger = slog.Default()
	}
	per := func(key string, f func(context.Context) (map[string]int64, error)) func(context.Context, metric.Observer, metric.Int64Observable) error {
		return func(ctx context.Context, o metric.Observer, i metric.Int64Observable) error {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			vals, err := f(ctx)
			if err != nil {
				logger.Warn("telemetry: reading a gauge", "gauge", key, "error", err)
				return nil
			}
			for k, v := range vals {
				o.ObserveInt64(i, v, metric.WithAttributes(attribute.String(key, k)))
			}
			return nil
		}
	}
	for _, d := range []struct {
		name, unit, desc string
		counter          bool
		f                func(context.Context, metric.Observer, metric.Int64Observable) error
		on               bool
	}{
		{"kista.downloads.active", "{download}", "Downloads being served", false, value(g.DownloadsActive), g.DownloadsActive != nil},
		{"kista.uploads.active", "{upload}", "Uploads being received", false, value(g.UploadsActive), g.UploadsActive != nil},
		{"kista.upstream.pull.queue", "{miss}", "Pull-through misses waiting", false, value(g.PullQueue), g.PullQueue != nil},
		{"kista.events.dropped", "{event}", "Events dropped by the asynchronous writer", true, value(g.EventsDropped), g.EventsDropped != nil},
		{"kista.events.pending", "{event}", "Events waiting for a sink", false, per("kista.sink", g.EventsPending), g.EventsPending != nil},
		{"kista.upstream.cells", "{cell}", "Upstream cells by outcome", false, per("kista.outcome", g.UpstreamCells), g.UpstreamCells != nil},
	} {
		if !d.on {
			continue
		}
		var inst metric.Int64Observable
		if d.counter {
			inst, err = meter.Int64ObservableCounter(d.name, metric.WithUnit(d.unit), metric.WithDescription(d.desc))
		} else {
			inst, err = meter.Int64ObservableGauge(d.name, metric.WithUnit(d.unit), metric.WithDescription(d.desc))
		}
		if err != nil {
			return nil, err
		}
		cbs = append(cbs, cb{inst, d.f})
	}
	if len(cbs) > 0 {
		insts := make([]metric.Observable, len(cbs))
		for i, c := range cbs {
			insts[i] = c.inst
		}
		if m.reg, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
			for _, c := range cbs {
				_ = c.f(ctx, o, c.inst) // a gauge logs its own failure
			}
			return nil
		}, insts...); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Downloaded counts a download: the tenant, channel and extension only for a tenant metrics may
// name; version, platform and DuckDB version only when configured.
func (m *Metrics) Downloaded(ctx context.Context, d Download) {
	if m == nil {
		return
	}
	attrs := []attribute.KeyValue{attribute.Bool("kista.authenticated", d.Authenticated)}
	if m.tenants.named(d.TenantName) {
		attrs = append(attrs, attribute.String("kista.tenant.id", d.TenantID), attribute.String("kista.channel", d.Channel),
			attribute.String("kista.extension", d.Extension))
		if m.labels.Version {
			attrs = append(attrs, attribute.String("kista.extension.version", d.Version))
		}
		if m.labels.Platform {
			attrs = append(attrs, attribute.String("kista.platform", d.Platform))
		}
		if m.labels.DuckDBVersion {
			attrs = append(attrs, attribute.String("kista.duckdb.version", d.DuckDBVersion))
		}
	}
	m.downloads.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// Request records a request's duration: route is a route's pattern, never a path.
func (m *Metrics) Request(ctx context.Context, method, route string, status int, d time.Duration) {
	if m == nil {
		return
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
	default:
		method = "_OTHER"
	}
	m.duration.Record(ctx, d.Seconds(), metric.WithAttributes(attribute.String("http.request.method", method),
		attribute.String("http.route", route), attribute.Int("http.response.status_code", status)))
}
