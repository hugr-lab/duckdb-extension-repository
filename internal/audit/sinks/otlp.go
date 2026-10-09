package sinks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
)

// Poster sends a request body (egress.Client).
type Poster interface {
	Post(ctx context.Context, raw, contentType string, header http.Header, body []byte) (int, error)
}

// OTLP sends records to an OpenTelemetry collector's OTLP/HTTP logs endpoint, JSON-encoded: one
// ResourceLogs per tenant, a log record per event.
type OTLP struct {
	URL      string
	Client   Poster
	Header   http.Header       // from headers_file; never logged
	Resource map[string]string // events.resource
	Version  string            // service.version
	Instance string            // service.instance.id
}

// Send posts a batch.
func (o *OTLP) Send(ctx context.Context, rs []Record) error {
	body, err := o.encode(rs)
	if err != nil {
		return err
	}
	status, err := o.Client.Post(ctx, o.URL, "application/json", o.Header, body)
	if err != nil {
		if errors.Is(err, egress.ErrStatus) {
			return &StatusError{Status: status}
		}
		return err
	}
	return nil
}

// Close does nothing.
func (o *OTLP) Close() error { return nil }

// encode builds the OTLP JSON request (ExportLogsServiceRequest).
func (o *OTLP) encode(rs []Record) ([]byte, error) {
	var order []string
	byTenant := map[string][]Record{}
	for _, r := range rs {
		if _, ok := byTenant[r.TenantID]; !ok {
			order = append(order, r.TenantID)
		}
		byTenant[r.TenantID] = append(byTenant[r.TenantID], r)
	}
	var resources []map[string]any
	for _, t := range order {
		trs := byTenant[t]
		attrs := []kv{str("service.name", "kista"), str("service.version", o.Version), str("kista.tenant.id", t)}
		if o.Instance != "" {
			attrs = append(attrs, str("service.instance.id", o.Instance))
		}
		if trs[0].TenantName != "" {
			attrs = append(attrs, str("kista.tenant.name", trs[0].TenantName))
		}
		keys := make([]string, 0, len(o.Resource))
		for k := range o.Resource {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if !strings.HasPrefix(k, "service.") && !strings.HasPrefix(k, "kista.") {
				attrs = append(attrs, str(k, o.Resource[k]))
			}
		}
		var records []map[string]any
		for _, r := range trs {
			rec, err := logRecord(r)
			if err != nil {
				return nil, err
			}
			records = append(records, rec)
		}
		resources = append(resources, map[string]any{
			"resource":  map[string]any{"attributes": attrs},
			"scopeLogs": []any{map[string]any{"scope": map[string]any{"name": "kista.events"}, "logRecords": records}},
		})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(map[string]any{"resourceLogs": resources})
	return buf.Bytes(), err
}

func logRecord(r Record) (map[string]any, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var body any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&body); err != nil {
		return nil, err
	}
	sev, sevText := 9, "INFO"
	if r.Outcome != audit.OK {
		sev, sevText = 13, "WARN"
	}
	ts := strconv.FormatInt(r.at.UnixNano(), 10)
	attrs := []kv{str("kista.event.id", r.ID), str("kista.kind", r.Kind), str("kista.outcome", r.Outcome),
		str("kista.actor", r.Actor), str("kista.subject", r.Subject), str("kista.request", r.Request)}
	if r.Client != "" {
		attrs = append(attrs, str("client.address", r.Client))
	}
	return map[string]any{"timeUnixNano": ts, "observedTimeUnixNano": ts, "severityNumber": sev, "severityText": sevText,
		"eventName": "kista." + r.Kind, "body": anyValue(body), "attributes": attrs}, nil
}

type kv struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func str(k, v string) kv { return kv{k, map[string]any{"stringValue": v}} }

// anyValue is a JSON value as OTLP's AnyValue.
func anyValue(v any) map[string]any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		vals := make([]kv, 0, len(x))
		for _, k := range keys {
			vals = append(vals, kv{k, anyValue(x[k])})
		}
		return map[string]any{"kvlistValue": map[string]any{"values": vals}}
	case []any:
		vals := make([]map[string]any, 0, len(x))
		for _, e := range x {
			vals = append(vals, anyValue(e))
		}
		return map[string]any{"arrayValue": map[string]any{"values": vals}}
	case string:
		return map[string]any{"stringValue": x}
	case bool:
		return map[string]any{"boolValue": x}
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return map[string]any{"intValue": strconv.FormatInt(i, 10)}
		}
		f, _ := x.Float64()
		return map[string]any{"doubleValue": f}
	case nil:
		return map[string]any{}
	default:
		return map[string]any{"stringValue": fmt.Sprint(x)}
	}
}
