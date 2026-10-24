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
	"tenant.create":  {"name", "display_name", "storage_domain", "holder_id"},
	"tenant.holder":  {"old", "new"}, // the holder its licences must name (spec 0028)
	"tenant.suspend": {},
	"tenant.resume":  {},
	"tenant.delete":  {"channels", "suspended_at", "keys"},
	"tenant.purge":   {"rows", "passes", "late", "error"},
	"version.add":    {"version", "kind", "c_apis"},
	"version.c_apis": {"version", "c_api"},
	// reserved names (spec 0030): a version's core extensions, an administrator's
	"reserved.add":    {"version", "kind", "source", "package_type", "names", "forced", "tenants", "skipped"}, // source and type before names: a cut keeps them
	"reserved.sync":   {"error"},                                                                              // a failed community sync pass (spec 0030 part 3)
	"reserved.remove": {"name", "sources", "package_type"},
	// a name reserved in a tenant for a holder (spec 0030 part 2)
	"reserved.tenant.add":    {"name", "holder", "previous", "package_type"},
	"reserved.tenant.remove": {"name", "holder", "package_type"},
	"server.start":           {"version", "schema", "digests", "changed"},
	// a key source weaker than a vault or a KMS, at every start (spec 0026)
	"signer.source.weak": {"source", "kind", "protection"},
	// channels and keys
	"channel.create":   {"name", "kind", "package_type"},
	"channel.versions": {"added", "removed"},
	"key.add":          {"key", "fingerprint", "scheme", "state", "protection"},
	"key.activate":     {"key", "fingerprint", "from", "forced"},
	"key.retire":       {"key", "fingerprint", "from", "forced"},
	"key.resign":       {"key", "fingerprint", "signatures"},
	// attestation keys and feeds (spec 0011)
	"attestation_key.add":      {"key", "fingerprint", "scheme", "state"},
	"attestation_key.activate": {"key", "fingerprint", "from", "forced"},
	"attestation_key.retire":   {"key", "fingerprint", "from", "forced"},
	"feed.add":                 {"name", "url", "credential", "kinds", "readers", "offer", "channels", "keys", "mode", "export"},
	"feed.remove":              {"name", "documents", "export"},
	"feed.change":              {"name", "change"},
	"feed.run":                 {"feed", "counts", "refused", "error", "bundle", "dry_run"},
	// identity
	"issuer.add":              {"name", "url", "algorithms"},
	"issuer.remove":           {"name", "url"},
	"issuer.console.set":      {"issuer", "client_id", "audience"},
	"issuer.console.remove":   {"issuer", "client_id", "audience"},
	"audience.add":            {"audience"},
	"audience.remove":         {"audience"},
	"grant.add":               {"principal", "verbs", "channel", "extension", "extension_group", "package_type"},
	"grant.remove":            {"principal", "verbs", "channel", "extension", "extension_group", "package_type"},
	"attachment.add":          {"body_hash", "kind", "origin", "size"},
	"attachment.remove":       {"body_hash", "kind", "origin", "size", "statement"},
	"attachment.change":       {"body_hash", "kind", "origin", "change", "statement"},
	"extension_group.add":     {"name", "description"},
	"extension_group.remove":  {"name", "description", "members"},
	"extension_group.change":  {"description"},
	"extension_group.members": {"added", "removed", "package_type"},
	"publisher.add":           {"name", "distributor"},                           // spec 0033 phase B: a distributor's publisher made marked
	"publisher.remove":        {"name", "purged", "kept"},                        // spec 0033 phase A2: the held entries purged (how many) and kept, revoked
	"publisher.change":        {"name", "license_keys", "distributor", "before"}, // spec 0033 phase B: a distributor's mark
	"publisher.key.add":       {"publisher", "key", "fingerprint", "type", "title", "expires_at"},
	"publisher.key.remove":    {"publisher", "key", "fingerprint", "type", "title", "purge_held", "purged", "kept"},
	// releases
	"release.add":       {"release", "name", "version", "platform", "slot", "body_hash", "visibility", "current", "shadows"},
	"release.publish":   {"release", "name", "version", "platform", "slot", "body_hash", "visibility", "current", "shadows", "provenance"},
	"release.promote":   {"releases", "name", "from_channel", "version", "shadows", "provenance"},
	"release.place":     {"releases", "name", "version", "source", "placed", "early"}, // spec 0033: by hand, auto or manual; early (A2)
	"catalogue.arrival": {"build", "name", "version", "platform", "slot", "body_hash", "source", "visibility", "package_type", "available_at", "provenance"},
	"catalogue.purge":   {"build", "name", "version", "platform", "slot", "body_hash", "package_type", "reason", "key"},
	// spec 0033 phase A2: a source's hold (with how many held entries a raise held longer), or a channel's
	"catalogue.hold": {"source", "channel", "hold", "before", "held"},
	// spec 0033 phase B: a version withdrawn from a distributor's publisher (how many of the tenant's
	// channels it bumped)
	"catalogue.withdrawn": {"source", "name", "version", "reason", "builds", "channels", "package_type"},
	"follow.set":          {"source"},
	"follow.clear":        {"source"},
	"release.yank":        {"release", "name", "version", "platform"},
	"release.deprecate":   {"release", "name", "version", "platform"},
	"release.activate":    {"release", "name", "version", "platform"},
	"release.current":     {"release", "name", "version", "platform"},
	"release.public":      {"release", "name", "version", "platform"},
	"release.private":     {"release", "name", "version", "platform"},
	"release.purge":       {"release", "name", "version", "platform", "slot", "body_hash"},
	"block.add":           {"body_hash", "reason"},
	"block.remove":        {"body_hash"},
	"upstream.add":        {"name", "kind", "prefix", "channel", "mode", "fetch", "visibility", "keys", "platforms", "extensions", "credential"},
	"upstream.remove":     {"name"},
	"upstream.change":     {"name", "change", "value", "versions", "allow_reserved", "eager", "visibility", "state"},
	"upstream.release":    {"upstream", "release", "name", "version", "platform", "slot", "body_hash", "url", "key", "bundle", "trigger"},
	"upstream.rejected":   {"upstream", "duckdb_version", "platform", "name", "outcome"},
	"upstream.run":        {"upstream", "dry_run", "counts", "error", "bundle"},
	// licences (spec 0012): refused last, since a truncated event keeps its first fields
	"license.import": {"name", "license", "issuer", "holder", "plan", "expires_at", "replaced", "package_type"},
	"license.change": {"name", "license", "visibility", "package_type"},
	"license.remove": {"name", "license", "reason", "package_type"},
	"license.sync":   {"upstream", "counts", "error", "refused"},
	// bundles (spec 0014): targets last, since a truncated event keeps its first fields; a bundle's
	// licences (spec 0021) before them
	"bundle.import":          {"import", "manifest", "created_at", "exporter", "bundle_key", "input_sha256", "dry_run", "state", "licenses", "targets"},
	"bundle.reset":           {"upstream", "feed", "manifest", "created_at"},
	"bundle.upload":          {"upload", "size", "sha256", "chunk_size", "dry_run", "upstreams", "feeds", "any_tenant"},
	"bundle.upload_complete": {"upload", "retry"},
	"bundle.upload_abort":    {"upload"},
	"bundle.upload_fail":     {"upload", "import", "error"},
	"bundle.upload_expire":   {"upload", "state"},
	// the configuration's bootstrap (spec 0020 phase 4a)
	"bootstrap.apply":  {"tenant", "digest", "source", "version", "host", "created"}, // created last: a long list is cut first
	"bootstrap.accept": {"tenant", "digest", "previous"},
	"bootstrap.rearm":  {"tenant", "digest"},
	"bundle.export":    {"plan", "manifest", "manifest_sha256", "bundle_key", "counts"},
	"shadow.add":       {"name"},
	"source.set":       {"name", "source", "before", "arrival", "mode"},
	"release.source":   {"release", "name", "before", "source"}, // written before spec 0033 (slots per source); kept for old events
	"source.clear":     {"name", "before"},
	"shadow.remove":    {"name"},
	"auth.failure":     {"reason", "route", "count"},
	"authz.refused":    {"route", "verb", "status", "count"},
	"install":          {"release", "name", "version", "platform", "duckdb_version", "body_hash", "user_agent"},
	"extension.use":    {"version", "platform", "duckdb_version", "license", "status", "events"},
	"usage.forward":    {"upstream", "sent", "skipped", "failed"},
	"usage.receive":    {"day", "part", "parts", "sender", "reports", "dropped"},
	"audit.dropped":    {"counts", "sink"},
	"storage.gc":       {"domain", "builds", "bodies", "marked", "streams", "tmp", "attachments", "uploads", "chunks", "bytes", "claims", "resolved", "dry_run", "error"},
	"storage.verify": {"domain", "full", "mark", "unmark", "checked", "ok", "skipped", "marked_ok", "missing", "short", "long",
		"corrupt", "record", "unreadable", "marked", "unmarked", "bytes", "error"},
	"audit.sink":     {"sink", "state", "class", "status"},
	"request.failed": {"route", "status", "count"},
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
	case "release.promote", "release.place", "extension.use", "source.set", "source.clear":
		return "channel:<channel>/ext:<name>"
	case "upstream.release", "install":
		return release
	case "server.start", "audit.dropped":
		return "server"
	case "storage.gc", "storage.verify":
		return "domain:<domain>"
	case "signer.source.weak":
		return "source:<name>"
	case "audit.sink":
		return "sink:<name>"
	case "auth.failure", "authz.refused", "request.failed":
		return "route:<route>"
	case "bundle.import", "bundle.export", "bundle.upload", "bundle.upload_complete", "bundle.upload_abort", "bundle.upload_fail",
		"bundle.upload_expire", "bootstrap.apply", "bootstrap.accept", "bootstrap.rearm":
		return "tenant:<name>"
	case "bundle.reset":
		return "upstream:<name> or feed:<name>"
	case "reserved.add":
		return "version:<name>, reserved:<name> or sync:community"
	case "reserved.sync":
		return "sync:community"
	case "reserved.remove", "reserved.tenant.add", "reserved.tenant.remove":
		return "reserved:<name>"
	case "license.sync", "usage.forward":
		return "upstream:<name>"
	case "catalogue.hold":
		return "upstream:<name>, publisher:<name> or channel:<channel>"
	}
	if strings.HasPrefix(string(k), "publisher.key.") {
		return "publisher:<name>/key:<id>"
	}
	prefix, _, _ := strings.Cut(string(k), ".")
	return map[string]string{"tenant": "tenant:<name>", "audience": "tenant:<name>", "version": "version:<name>",
		"channel": "channel:<channel>", "key": "channel:<channel>/key:<id>", "issuer": "issuer:<name>", "grant": "grant:<id>",
		"publisher": "publisher:<name>", "release": release, "usage": "channel:<channel>", "block": "block:<body hash>", "upstream": "upstream:<name>",
		"shadow": "shadow:<name>", "extension_group": "extension-group:<name>", "attachment": "ext:<name>/attachment:<id>",
		"attestation_key": "attestation-key:<id>", "feed": "feed:<name>", "license": "license:<name>", "catalogue": "ext:<name>",
		"follow": "channel:<channel>"}[prefix]
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
