package telemetry_test

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/hugr-lab/duckdb-extension-repository/internal/telemetry"
)

var ctx = context.Background()

func collect(t *testing.T, r *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

// Spec 0010 phase 2b: metrics name only the tenants listed, never a principal or an address, and
// the optional labels only when configured.
func TestMetrics(t *testing.T) {
	r := sdkmetric.NewManualReader()
	mp, shutdown, err := telemetry.Setup(ctx, telemetry.Options{Version: "v-test", Instance: "i", Reader: r})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)
	m, err := telemetry.New(mp, telemetry.Tenants{Names: map[string]bool{"acme": true}}, telemetry.Labels{Platform: true}, telemetry.Gauges{
		DownloadsActive: func() int64 { return 3 },
		EventsDropped:   func() int64 { return 7 },
		EventsPending:   func(context.Context) (map[string]int64, error) { return map[string]int64{"collector": 5}, nil },
		// a failing gauge is left out, never the whole collection
		UpstreamCells: func(context.Context) (map[string]int64, error) { return nil, errors.New("the store is down") },
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Downloaded(ctx, telemetry.Download{TenantID: "t-acme", TenantName: "acme", Channel: "prod", Extension: "acl", Version: "1.0",
		Platform: "linux_amd64", DuckDBVersion: "v2.0.0", Authenticated: true})
	m.Downloaded(ctx, telemetry.Download{TenantID: "t-beta", TenantName: "beta", Channel: "secret", Extension: "private_ext",
		Platform: "linux_amd64"})
	m.Request(ctx, "GET", "extension", 200, 30*time.Millisecond)
	m.Request(ctx, "BREW", "extension", 200, 30*time.Millisecond)
	got := collect(t, r)
	sum := got["kista.downloads"].Data.(metricdata.Sum[int64])
	if len(sum.DataPoints) != 2 {
		t.Fatalf("download series: %+v", sum.DataPoints)
	}
	for _, dp := range sum.DataPoints {
		tenant, named := dp.Attributes.Value("kista.tenant.id")
		_, ver := dp.Attributes.Value("kista.extension.version")
		_, plat := dp.Attributes.Value("kista.platform")
		if ver {
			t.Fatalf("an unconfigured label: %v", dp.Attributes)
		}
		switch {
		case named && tenant.AsString() == "t-acme":
			if !plat || dp.Value != 1 {
				t.Fatalf("acme's series: %v", dp.Attributes)
			}
		case !named:
			if dp.Attributes.Len() != 1 || dp.Attributes.HasValue("kista.extension") {
				t.Fatalf("an unlisted tenant named: %v", dp.Attributes)
			}
		default:
			t.Fatalf("a series: %v", dp.Attributes)
		}
	}
	h := got["http.server.request.duration"].Data.(metricdata.Histogram[float64])
	if len(h.DataPoints) != 2 || h.DataPoints[0].Count != 1 {
		t.Fatalf("durations (an unknown method is _OTHER): %+v", h.DataPoints)
	}
	if v, _ := h.DataPoints[0].Attributes.Value("http.route"); v.AsString() != "extension" {
		t.Fatalf("route: %v", h.DataPoints[0].Attributes)
	}
	if g := got["kista.downloads.active"].Data.(metricdata.Gauge[int64]); g.DataPoints[0].Value != 3 {
		t.Fatalf("active: %+v", g)
	}
	if c := got["kista.events.dropped"].Data.(metricdata.Sum[int64]); c.DataPoints[0].Value != 7 {
		t.Fatalf("dropped: %+v", c)
	}
	p := got["kista.events.pending"].Data.(metricdata.Gauge[int64])
	if v, _ := p.DataPoints[0].Attributes.Value(attribute.Key("kista.sink")); v.AsString() != "collector" || p.DataPoints[0].Value != 5 {
		t.Fatalf("pending: %+v", p)
	}
	if _, ok := got["kista.uploads.active"]; ok {
		t.Fatal("an instrument without its source")
	}
}

// clearEnv unsets every OTEL_ variable the tests touch.
func clearEnv(t *testing.T) {
	for _, v := range []string{"OTEL_SDK_DISABLED", "OTEL_METRICS_EXPORTER", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "OTEL_EXPORTER_OTLP_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE", "OTEL_RESOURCE_ATTRIBUTES"} {
		t.Setenv(v, "")
	}
}

// Without an endpoint nothing is exported; a protocol other than http/protobuf is refused.
func TestSetupFromEnvironment(t *testing.T) {
	clearEnv(t)
	if telemetry.Enabled() {
		t.Fatal("enabled without an endpoint")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	if !telemetry.Enabled() {
		t.Fatal("an endpoint")
	}
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	if telemetry.Enabled() {
		t.Fatal("OTEL_METRICS_EXPORTER=none")
	}
	t.Setenv("OTEL_METRICS_EXPORTER", "")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	if _, _, err := telemetry.Setup(ctx, telemetry.Options{}); err == nil {
		t.Fatal("grpc")
	}
	// the metrics' own protocol wins over the shared one
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "http/protobuf")
	if _, shutdown, err := telemetry.Setup(ctx, telemetry.Options{}); err != nil {
		t.Fatalf("metrics over http/protobuf, traces over grpc: %v", err)
	} else {
		_ = shutdown(ctx)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "")
	t.Setenv("OTEL_METRICS_EXPORTER", "prometheus")
	if _, _, err := telemetry.Setup(ctx, telemetry.Options{}); err == nil {
		t.Fatal("another exporter")
	}
	t.Setenv("OTEL_METRICS_EXPORTER", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment.name=prod,broken")
	if _, shutdown, err := telemetry.Setup(ctx, telemetry.Options{}); err != nil {
		t.Fatalf("a partly malformed resource: %v", err)
	} else {
		_ = shutdown(ctx)
	}
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if telemetry.Enabled() {
		t.Fatal("disabled")
	}
}

// The exporter posts OTLP protobuf to the environment's endpoint on shutdown's flush.
func TestExport(t *testing.T) {
	got := make(chan string, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Method + " " + r.URL.Path + " " + r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	mp, shutdown, err := telemetry.Setup(ctx, telemetry.Options{Version: "v"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := telemetry.New(mp, telemetry.Tenants{}, telemetry.Labels{}, telemetry.Gauges{})
	if err != nil {
		t.Fatal(err)
	}
	m.Request(ctx, "GET", "api", 200, time.Millisecond)
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case req := <-got:
		if req != "POST /v1/metrics application/x-protobuf" {
			t.Fatalf("the export: %s", req)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing exported")
	}
}

// The SDK's client takes the collector's CA from the environment (a private CA).
func TestExportPrivateCA(t *testing.T) {
	got := make(chan bool, 10)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", ca)
	mp, shutdown, err := telemetry.Setup(ctx, telemetry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := telemetry.New(mp, telemetry.Tenants{}, telemetry.Labels{}, telemetry.Gauges{})
	m.Request(ctx, "GET", "healthz", 200, time.Millisecond)
	if err := shutdown(ctx); err != nil {
		t.Fatalf("export over TLS with the CA from the environment: %v", err)
	}
	select {
	case <-got:
	default:
		t.Fatal("nothing exported")
	}
}
