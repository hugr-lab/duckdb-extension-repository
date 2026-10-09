package app

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/audit/sinks"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// EventSinks builds the configured sinks' senders (spec 0010): OTLP through an egress client with
// the sink's own allowlist added to egress.allow, JSON lines to stdout or a file.
func EventSinks(cfg config.Config, instance string) ([]sinks.Sink, error) {
	ev := cfg.EventSettings()
	var out []sinks.Sink
	for _, s := range ev.Sinks {
		sk := sinks.Sink{Name: s.Name, Selection: sinks.SelectionOf(s), Unmasked: s.Unmasked}
		switch s.Kind {
		case "otlp":
			c, err := egressWith(cfg, s.Allow, cmp.Or(s.Timeout, 10*time.Second))
			if err != nil {
				return nil, fmt.Errorf("app: events sink %s: %w", s.Name, err)
			}
			h, err := headersFile(s.HeadersFile)
			if err != nil {
				return nil, fmt.Errorf("app: events sink %s: %w", s.Name, err)
			}
			sk.Sender = &sinks.OTLP{URL: s.URL, Client: c, Header: h, Resource: ev.Resource, Version: Version, Instance: instance}
		case "jsonl":
			sk.Sender = &sinks.JSONL{Path: s.Path, MaxSize: int64(s.MaxSize), Keep: s.Keep}
		}
		out = append(out, sk)
	}
	return out, nil
}

// headersFile reads "Name: value" lines (blank lines and # comments skipped); errors never carry
// a value.
func headersFile(path string) (http.Header, error) {
	h := http.Header{}
	if path == "" {
		return h, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read headers_file")
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || strings.ContainsAny(k, " \t") || strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("headers_file line %d is not Name: value", n)
		}
		ck := textproto.CanonicalMIMEHeaderKey(k)
		if ck == "Content-Type" || ck == "Content-Length" || ck == "Host" {
			return nil, fmt.Errorf("headers_file line %d sets %s", n, ck)
		}
		h.Add(ck, v)
	}
	return h, sc.Err()
}

// securitySettings are the configuration sections server.start digests (spec 0010).
func securitySettings(cfg config.Config) map[string]any {
	return map[string]any{"profile": cfg.Profile, "signers": cfg.Signers, "blob": cfg.Blob, "serve": cfg.Serve,
		"egress": cfg.Egress, "auth": cfg.Auth, "publish": cfg.Publish, "upstreams": cfg.Upstreams, "events": cfg.Events}
}

// ServerStart records server.start: kista's version, the schema level, the digests of the
// security-relevant settings, and the names of those changed since the previous start.
func ServerStart(ctx context.Context, cfg config.Config, st *store.Store) error {
	digests := map[string]string{}
	for k, v := range securitySettings(cfg) {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		digests[k] = hex.EncodeToString(sum[:8])
	}
	changed := []string{}
	prev, err := st.LastEvent(ctx, audit.ServerTenant, "server.start")
	switch {
	case err == nil:
		var d struct {
			Digests map[string]string `json:"digests"`
		}
		_ = json.Unmarshal([]byte(prev.Data), &d)
		for k, v := range digests {
			if d.Digests[k] != v {
				changed = append(changed, k)
			}
		}
		slices.Sort(changed)
	case errors.Is(err, store.ErrNotFound):
	default:
		return err
	}
	level, err := st.SchemaLevel()
	if err != nil {
		return err
	}
	e, err := st.NewEvent(ctx, "", "system:serve", "server.start", audit.OK, "server",
		map[string]any{"version": Version, "schema": level, "digests": digests, "changed": changed})
	if err != nil {
		return err
	}
	return st.InsertEvents(ctx, []store.Event{e})
}
