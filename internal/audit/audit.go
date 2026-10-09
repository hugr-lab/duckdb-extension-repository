// Package audit holds kista's events (spec 0010): the catalogue of kinds and their fields, the
// request's context (client address, request id, the actor's display name), and building an event's
// data. Events are written by the store in the transaction of the change they record (Tx.Event).
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
)

// ServerTenant is the tenant id of server-level events: never a tenant's.
const ServerTenant = "00000000-0000-0000-0000-000000000000"

// Outcomes.
const (
	OK      = "ok"
	Refused = "refused"
	Failed  = "failed"
)

// MaxData is the most bytes an event's data takes; a larger one keeps its first fields.
const MaxData = 16 << 10

// --- the request's context ---

type ctxKey int

const requestKey ctxKey = 0

// Request is what a request tells its events: the client address, the request id, and the token's
// display name for its principal.
type Request struct {
	Client    string // the client address after the trusted proxies, not reduced
	ID        string
	ActorName string
}

// WithRequest records a request's facts for the events it causes.
func WithRequest(ctx context.Context, r Request) context.Context {
	return context.WithValue(ctx, requestKey, r)
}

// FromContext returns the request's facts (zero outside a request).
func FromContext(ctx context.Context) Request {
	r, _ := ctx.Value(requestKey).(Request)
	return r
}

// WithActorName adds the token's display name to the request's facts.
func WithActorName(ctx context.Context, name string) context.Context {
	r := FromContext(ctx)
	r.ActorName = name
	return WithRequest(ctx, r)
}

// Client reduction modes (events.client_addresses).
const (
	ClientFull      = "full"
	ClientTruncated = "truncated"
	ClientNone      = "none"
)

// ReduceClient reduces a client address for an event: an IPv4-mapped address is unmapped; IPv4 is
// cut to /24, IPv6 to /48, and an IPv6 address embedding IPv4 (6to4, NAT64, Teredo) to the
// embedded address's /24.
func ReduceClient(addr, mode string) string {
	if addr == "" || mode == ClientNone {
		return ""
	}
	a, err := netip.ParseAddr(addr)
	if err != nil {
		if ap, err2 := netip.ParseAddrPort(addr); err2 == nil {
			a = ap.Addr()
		} else {
			return ""
		}
	}
	a = a.Unmap().WithZone("")
	if mode == ClientFull {
		return a.String()
	}
	if a.Is6() {
		if v4 := egress.Embedded(a); len(v4) > 0 {
			a = v4[0]
		}
	}
	bits := 48
	if a.Is4() {
		bits = 24
	}
	p, _ := a.Prefix(bits)
	return p.String()
}

// --- the catalogue ---

// Kind is an event's kind (spec 0010's catalogue, version 1).
type Kind string

// Version is the catalogue's version, recorded with every event.
const Version = 1

// Kinds and the data fields each may carry. A field outside its kind's list never reaches an event
// (Data refuses it), so secrets cannot by construction.
var catalogue = map[Kind][]string{
	// tenants and server
	"tenant.create":  {"name", "display_name", "storage_domain"},
	"tenant.suspend": {},
	"tenant.resume":  {},
	"version.add":    {"version", "kind", "c_apis"},
	"version.c_apis": {"version", "c_api"},
	"server.start":   {"version", "schema", "digests", "changed"},
	// channels and keys
	"channel.create":   {"name", "kind"},
	"channel.versions": {"added", "removed"},
	"key.add":          {"key", "fingerprint", "scheme", "state"},
	"key.activate":     {"key", "fingerprint", "from", "forced"},
	"key.retire":       {"key", "fingerprint", "from", "forced"},
	"key.resign":       {"key", "fingerprint", "signatures"},
	// identity
	"issuer.add":              {"name", "url", "algorithms"},
	"issuer.remove":           {"name", "url"},
	"audience.add":            {"audience"},
	"audience.remove":         {"audience"},
	"grant.add":               {"principal", "verbs", "channel", "extension"},
	"grant.remove":            {"principal", "verbs", "channel", "extension"},
	"publisher.add":           {"name"},
	"publisher.remove":        {"name"},
	"publisher.github.add":    {"publisher", "credential", "owner_id", "repository_id", "workflow", "ref", "environment"},
	"publisher.github.remove": {"publisher", "credential"},
	"publisher.key.add":       {"publisher", "key", "prefix", "expires_at"},
	"publisher.key.remove":    {"publisher", "key"},
	// releases
	"release.add":       {"release", "name", "version", "platform", "slot", "body_hash", "visibility", "current", "shadows"},
	"release.publish":   {"release", "name", "version", "platform", "slot", "body_hash", "visibility", "current", "shadows", "provenance"},
	"release.promote":   {"releases", "name", "from_channel", "version", "shadows", "provenance"},
	"release.yank":      {"release", "name", "version", "platform"},
	"release.deprecate": {"release", "name", "version", "platform"},
	"release.activate":  {"release", "name", "version", "platform"},
	"release.current":   {"release", "name", "version", "platform"},
	"release.public":    {"release", "name", "version", "platform"},
	"release.private":   {"release", "name", "version", "platform"},
	"block.add":         {"body_hash", "reason"},
	"block.remove":      {"body_hash"},
	"upstream.add":      {"name", "kind", "prefix", "channel", "mode", "visibility", "keys", "platforms", "extensions"},
	"upstream.remove":   {"name"},
	"upstream.change":   {"name", "change", "value", "versions", "allow_reserved", "visibility", "state"},
	"upstream.release":  {"upstream", "release", "name", "version", "platform", "slot", "body_hash", "url", "key"},
	"upstream.rejected": {"upstream", "duckdb_version", "platform", "name", "outcome"},
	"upstream.run":      {"upstream", "dry_run", "counts", "error"},
	"upstream.pull":     {"upstream", "duckdb_version", "platform", "name"},
	"shadow.add":        {"name"},
	"shadow.remove":     {"name"},
	"auth.failure":      {"reason", "route", "count"},
	"authz.refused":     {"route", "verb", "status", "count"},
	"install":           {"release", "name", "version", "platform", "duckdb_version", "body_hash", "user_agent"},
	"audit.dropped":     {"counts", "sink"},
	"audit.sink":        {"sink", "state", "class", "status"},
	"request.failed":    {"route", "status", "count"},
}

// Kinds lists the catalogue: each kind with its fields, sorted by kind.
func Kinds() []KindInfo {
	out := make([]KindInfo, 0, len(catalogue))
	for k, f := range catalogue {
		out = append(out, KindInfo{Kind: k, Subject: SubjectForm(k), Fields: slices.Clone(f)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// KindInfo describes a kind.
type KindInfo struct {
	Kind    Kind     `json:"kind"`
	Subject string   `json:"subject"`
	Fields  []string `json:"fields"`
}

// SubjectForm is the form of a kind's subject.
func SubjectForm(k Kind) string {
	const release = "channel:<channel>/ext:<name>/release:<id>"
	switch k {
	case "release.promote":
		return "channel:<channel>/ext:<name>"
	case "upstream.release", "install":
		return release
	case "server.start", "audit.dropped":
		return "server"
	case "audit.sink":
		return "sink:<name>"
	case "auth.failure", "authz.refused", "request.failed":
		return "route:<route>"
	}
	if strings.HasPrefix(string(k), "publisher.github.") {
		return "publisher:<name>/github:<id>"
	}
	if strings.HasPrefix(string(k), "publisher.key.") {
		return "publisher:<name>/key:<id>"
	}
	prefix, _, _ := strings.Cut(string(k), ".")
	return map[string]string{"tenant": "tenant:<name>", "audience": "tenant:<name>", "version": "version:<name>",
		"channel": "channel:<channel>", "key": "channel:<channel>/key:<id>", "issuer": "issuer:<name>", "grant": "grant:<id>",
		"publisher": "publisher:<name>", "release": release, "block": "block:<body hash>", "upstream": "upstream:<name>",
		"shadow": "shadow:<name>"}[prefix]
}

// Known reports whether a kind is in the catalogue.
func Known(k Kind) bool { _, ok := catalogue[k]; return ok }

// Data encodes an event's data: only the kind's fields, at most MaxData bytes (a larger one keeps
// its first fields, in the catalogue's order, and "truncated": true). An unknown kind or field is an
// error: a programming error, found by the coverage test.
func Data(k Kind, fields map[string]any) (string, error) {
	allowed, ok := catalogue[k]
	if !ok {
		return "", fmt.Errorf("audit: unknown kind %q", k)
	}
	for f := range fields {
		if !slices.Contains(allowed, f) {
			return "", fmt.Errorf("audit: %s has no field %q", k, f)
		}
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	if len(b) <= MaxData {
		return string(b), nil
	}
	kept := map[string]any{"truncated": true}
	for _, f := range allowed {
		v, ok := fields[f]
		if !ok {
			continue
		}
		kept[f] = v
		if b, _ := json.Marshal(kept); len(b) > MaxData {
			delete(kept, f)
			break
		}
	}
	b, err = json.Marshal(kept)
	return string(b), err
}

// DisplayName is a token's display name for events: its name, preferred_username or email claim,
// printable characters only.
func DisplayName(claims map[string]any) string {
	for _, k := range []string{"name", "preferred_username", "email"} {
		if v, ok := claims[k].(string); ok {
			v = strings.Map(func(r rune) rune {
				if unicode.IsPrint(r) {
					return r
				}
				return -1
			}, v)
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	return ""
}
