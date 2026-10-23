// Package bootstrap applies the configuration's bootstrap section once (spec 0020 phase 4a): the
// DuckDB versions and one tenant with its audiences, channels, keys, issuer, console client and
// grants, made through the services the admin commands call. It checks everything before any
// write, never changes what exists, and records that it was applied.
package bootstrap

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream/upspec"
)

// ErrRefused is a bootstrap that would change what exists: nothing is written.
var ErrRefused = errors.New("bootstrap: refused")

// ErrChanged is a section that differs from the one applied (kista admin bootstrap exits 1).
var ErrChanged = errors.New("bootstrap: the section changed after it was applied")

// Deps are the services the bootstrap acts through.
type Deps struct {
	Store   *store.Store
	Tenants *tenants.Service
	Auth    *tenants.AuthAdmin
	Keys    *keys.Service
	// Upstreams makes the section's upstreams (spec 0024).
	Upstreams *upstream.Service
	Log       *slog.Logger
}

// Options say how one run goes.
type Options struct {
	Actor  authz.Actor
	DryRun bool // the plan only: no lease, no write
	Adopt  bool // create in an existing tenant the bootstrap did not make
	// Source, Version and Host go into bootstrap.apply: where the configuration came from, which
	// kista applied it, and where.
	Source, Version, Host string
	Holder                string        // the lease holder
	Wait                  time.Duration // for another replica's apply (default 4m)
	Retry                 time.Duration // for a failure that may pass (default 4m)
}

// Action is what the bootstrap does with one object.
type Action string

const (
	Create  Action = "create"
	Add     Action = "add" // a version's C API or a channel's DuckDB versions
	Same    Action = "same"
	Refused Action = "refused"
)

// Item is one object of the plan.
type Item struct {
	Kind, Name string
	Action     Action
	Detail     string
}

func (i Item) String() string {
	s := fmt.Sprintf("%-8s %-16s %s", i.Action, i.Kind, i.Name)
	if i.Detail != "" {
		s += ": " + i.Detail
	}
	return s
}

// Plan is the bootstrap's check of every object it names.
type Plan struct {
	Items        []Item
	TenantExists bool
	// Resumed: the tenant is one an earlier, unfinished run of the bootstrap made.
	Resumed bool
}

// Refusals are the plan's refused items.
func (p Plan) Refusals() []Item {
	var out []Item
	for _, i := range p.Items {
		if i.Action == Refused {
			out = append(out, i)
		}
	}
	return out
}

// Changes reports whether the plan writes anything.
func (p Plan) Changes() bool {
	return slices.ContainsFunc(p.Items, func(i Item) bool { return i.Action == Create || i.Action == Add })
}

// tenantChanges reports whether the plan writes anything in the tenant (versions are the server's).
func (p Plan) tenantChanges() bool {
	return slices.ContainsFunc(p.Items, func(i Item) bool { return (i.Action == Create || i.Action == Add) && i.Kind != "version" })
}

func (p Plan) action(kind, name string) Action {
	for _, i := range p.Items {
		if i.Kind == kind && i.Name == name {
			return i.Action
		}
	}
	return ""
}

// Result is what a run did.
type Result struct {
	Plan    Plan
	Applied bool // this run recorded the bootstrap
	Already bool // it was applied before (by this or another replica)
	Changed bool // the section differs from the one applied
}

// azureVersion and gcpVersion are the key versions signer references carry: a version a redeploy
// renders anew is no change of the section.
var (
	azureVersion = regexp.MustCompile(`/[0-9a-f]{32}$`)
	gcpVersion   = regexp.MustCompile(`/cryptoKeyVersions/\d+$`)
)

func unversioned(ref string) string {
	return gcpVersion.ReplaceAllString(azureVersion.ReplaceAllString(ref, ""), "")
}

// Digest is the SHA-256 of the defaulted section's JSON (a struct, so the YAML's key order does not
// matter), with key versions left out of signer references. It answers the section's JSON too.
func Digest(b config.Bootstrap) (string, string, error) {
	c := b.Defaulted()
	c.Channels = slices.Clone(c.Channels)
	for i := range c.Channels {
		c.Channels[i].Key = unversioned(c.Channels[i].Key)
	}
	c.AttestationKey = unversioned(c.AttestationKey)
	data, err := json.Marshal(c)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(data)
	section, err := json.Marshal(b.Defaulted())
	return hex.EncodeToString(sum[:]), string(section), err
}

// Run applies the bootstrap unless it was applied, under the bootstrap lease. With DryRun it only
// checks, and reports whether it was applied. A refusal is ErrRefused, with nothing written.
func Run(ctx context.Context, d Deps, b config.Bootstrap, o Options) (Result, error) {
	digest, section, err := Digest(b)
	if err != nil {
		return Result{}, err
	}
	if o.DryRun {
		res, _, err := applied(ctx, d, digest, section, o)
		if err != nil && !errors.Is(err, ErrChanged) {
			return res, err
		}
		p, cerr := Check(ctx, d, b, o)
		res.Plan = p
		return res, cmpErr(cerr, err)
	}
	if res, done, err := applied(ctx, d, digest, section, o); done || err != nil {
		return res, err
	}
	lctx, release, done, err := lease(ctx, d, o)
	if err != nil {
		return Result{}, err
	}
	if done {
		res, done, err := applied(ctx, d, digest, section, o)
		if !done && err == nil {
			err = errors.New("bootstrap: another replica held the lease and recorded nothing")
		}
		return res, err
	}
	defer release()
	// another replica may have finished between the first look and the lease
	if res, done, err := applied(lctx, d, digest, section, o); done || err != nil {
		return res, err
	}
	deadline := time.Now().Add(orDefault(o.Retry, 4*time.Minute))
	for wait := time.Second; ; wait = min(wait*2, 30*time.Second) {
		res, err := apply(lctx, d, b, digest, section, o)
		if err == nil || !transient(err) || time.Now().Add(wait).After(deadline) {
			return res, err
		}
		d.Log.Warn("bootstrap: trying again", "error", err, "in", wait)
		select {
		case <-lctx.Done():
			return res, cmpErr(context.Cause(lctx), err)
		case <-time.After(wait):
		}
	}
}

func cmpErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// transient is a failure that may pass: a signer or an issuer not reachable yet (a role not yet
// propagated), the network, a busy database. Anything else is not tried again.
func transient(err error) bool {
	var ne net.Error
	return errors.Is(err, keys.ErrSigner) || errors.Is(err, tenants.ErrIssuerFetch) || errors.Is(err, upstream.ErrSourceFetch) ||
		errors.Is(err, store.ErrBusy) || errors.As(err, &ne)
}

// applied looks at the record. A pending one is not done. When the bootstrap was applied, a section
// that changed since is noted once (serve) or is ErrChanged (the CLI), listing what changed.
func applied(ctx context.Context, d Deps, digest, section string, o Options) (Result, bool, error) {
	rec, err := d.Store.GetBootstrap(ctx)
	if errors.Is(err, store.ErrNotFound) || err == nil && rec.State == store.BootstrapPending {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, true, err
	}
	res := Result{Already: true, Changed: rec.Digest != digest}
	if !res.Changed {
		return res, true, nil
	}
	if o.Actor.Kind == authz.ActorSystem {
		if rec.SeenDigest != digest {
			d.Log.Warn("bootstrap: the section changed after it was applied; change the tenant through the API", "tenant", rec.Tenant)
			if err := d.Store.SeeBootstrap(ctx, digest); err != nil {
				return res, true, err
			}
		}
		return res, true, nil
	}
	return res, true, fmt.Errorf("%w (applied %s by %s; changed: %s): change the tenant through the API, or kista admin bootstrap -accept",
		ErrChanged, rec.AppliedAt.UTC().Format(time.RFC3339), rec.Actor, changedFields(rec.Section, section))
}

// changedFields names the top-level fields of the section that differ from the applied one.
func changedFields(old, cur string) string {
	var a, b map[string]json.RawMessage
	_ = json.Unmarshal([]byte(old), &a)
	_ = json.Unmarshal([]byte(cur), &b)
	var out []string
	for _, k := range slices.Sorted(maps.Keys(b)) {
		if string(a[k]) != string(b[k]) {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return "key versions only"
	}
	return strings.Join(out, ", ")
}

// lease takes the bootstrap lease; its context ends when a renewal fails, so a replica that lost
// the lease stops writing. When another replica holds it, lease waits for that replica's record
// (done) or for the lease to be free.
func lease(ctx context.Context, d Deps, o Options) (context.Context, func(), bool, error) {
	const ttl = time.Minute
	deadline := time.Now().Add(orDefault(o.Wait, 4*time.Minute))
	for {
		ok, err := d.Store.AcquireLease(ctx, store.BootstrapLease, o.Holder, ttl)
		if err != nil {
			return nil, nil, false, err
		}
		if ok {
			lctx, cancel := context.WithCancelCause(ctx)
			go func() {
				t := time.NewTicker(ttl / 3)
				defer t.Stop()
				for {
					select {
					case <-lctx.Done():
						return
					case <-t.C:
						if ok, err := d.Store.RenewLease(lctx, store.BootstrapLease, o.Holder, ttl); err != nil || !ok {
							cancel(fmt.Errorf("bootstrap: the lease was lost: %v", err))
							return
						}
					}
				}
			}()
			return lctx, func() {
				cancel(nil)
				_ = d.Store.ReleaseLease(context.WithoutCancel(ctx), store.BootstrapLease, o.Holder)
			}, false, nil
		}
		if rec, err := d.Store.GetBootstrap(ctx); err == nil && rec.State == store.BootstrapApplied {
			return nil, nil, true, nil
		}
		if time.Now().After(deadline) {
			return nil, nil, false, errors.New("bootstrap: another replica holds the lease; it did not finish in time")
		}
		select {
		case <-ctx.Done():
			return nil, nil, false, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// open opens and probes a signer reference for its purpose, answering its fingerprint.
func (d Deps) open(ctx context.Context, ref, purpose string) (string, error) {
	var sg signer.Signer
	var err error
	if po, ok := d.Keys.Signers.(keys.PurposeOpener); ok {
		sg, err = po.OpenPurpose(ctx, ref, purpose)
	} else {
		sg, err = d.Keys.Signers.Open(ctx, ref)
	}
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", keys.ErrSigner, ref, err)
	}
	if err := extfile.CheckKey(sg.Public()); err != nil {
		return "", fmt.Errorf("%w: %s: %w", store.ErrInvalid, ref, err)
	}
	if err := keys.Probe(ctx, sg); err != nil {
		return "", fmt.Errorf("%w: %s: %w", keys.ErrSigner, ref, err)
	}
	return extfile.Fingerprint(sg.Public()), nil
}

// Check compares every object the section names with what exists, writing nothing. Signers are
// opened and probed, and a new issuer's discovery document and JWKS are fetched.
func Check(ctx context.Context, d Deps, b config.Bootstrap, o Options) (Plan, error) {
	var p Plan
	item := func(kind, name string, a Action, detail string, args ...any) {
		p.Items = append(p.Items, Item{Kind: kind, Name: name, Action: a, Detail: fmt.Sprintf(detail, args...)})
	}
	if rec, err := d.Store.GetBootstrap(ctx); err == nil && rec.State == store.BootstrapPending {
		if rec.Tenant != b.Tenant.Name {
			item("bootstrap", rec.Tenant, Refused, "a bootstrap of tenant %s did not finish: kista admin bootstrap -rearm", rec.Tenant)
			return p, nil
		}
		p.Resumed = true
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return p, err
	}
	// versions: a C API per major, never changed
	have, err := d.Store.ListDuckDBVersions(ctx)
	if err != nil {
		return p, err
	}
	known := map[string]store.DuckDBVersion{}
	for _, v := range have {
		known[v.Name] = v
	}
	reservedKinds, _, err := d.Store.ReservedNames(ctx)
	if err != nil {
		return p, err
	}
	for _, v := range b.Versions {
		cur, ok := known[v.Version]
		if !ok {
			if len(v.CoreExtensions) == 0 {
				item("version", v.Version, Refused, "a new version needs its core_extensions (kista duckdb core-extensions)")
			} else {
				item("version", v.Version, Create, "")
			}
			continue
		}
		if cur.Kind != v.Kind {
			item("version", v.Version, Refused, "it is %s, not %s", cur.Kind, v.Kind)
			continue
		}
		var missing, other []string
		for _, c := range v.CAPIs {
			i := slices.IndexFunc(cur.CAPIs, func(x store.CAPI) bool { return strings.HasPrefix(c, fmt.Sprintf("v%d.", x.Major)) })
			switch {
			case i < 0:
				missing = append(missing, c)
			case cur.CAPIs[i].String() != c:
				other = append(other, fmt.Sprintf("%s (it has %s)", c, cur.CAPIs[i]))
			}
		}
		var adds []string
		if len(missing) > 0 {
			adds = append(adds, "C APIs "+strings.Join(missing, ", "))
		}
		if core := missingCore(v.CoreExtensions, reservedKinds); len(core) > 0 {
			adds = append(adds, "core extensions "+strings.Join(core, ", "))
		}
		switch {
		case len(other) > 0:
			item("version", v.Version, Refused, "C API %s", strings.Join(other, ", "))
		case len(adds) > 0:
			item("version", v.Version, Add, "%s", strings.Join(adds, "; "))
		default:
			item("version", v.Version, Same, "")
		}
	}
	// the tenant
	b = b.Defaulted()
	tb := b.Tenant
	t, err := d.Store.GetTenantAny(ctx, tb.Name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		item("tenant", tb.Name, Create, "")
	case err != nil:
		return p, err
	default:
		p.TenantExists = true
		switch {
		case !t.DeletingAt.IsZero():
			item("tenant", tb.Name, Refused, "it is deleting or deleted: its name is kept for good")
		case t.State != store.TenantActive:
			item("tenant", tb.Name, Refused, "it is %s", t.State)
		case t.DisplayName != tb.DisplayName || t.StorageDomain != tb.Domain:
			item("tenant", tb.Name, Refused, "its display name or domain differs (%q, %s)", t.DisplayName, t.StorageDomain)
		default:
			item("tenant", tb.Name, Same, "")
		}
	}
	tenantID := ""
	if p.TenantExists {
		tenantID = t.ID
	}
	for _, aud := range tb.Audiences {
		owner, err := d.Store.AudienceTenant(ctx, aud)
		switch {
		case err != nil:
			return p, err
		case owner == "":
			item("audience", aud, Create, "")
		case owner == tenantID:
			item("audience", aud, Same, "")
		default:
			item("audience", aud, Refused, "it is another tenant's")
		}
	}
	// channels, their versions and keys; a fingerprint once in the whole section
	fingerprints := map[string]string{}
	for _, cb := range b.Channels {
		var ch store.Channel
		exists := false
		if p.TenantExists {
			ch, err = d.Store.GetChannel(ctx, tb.Name, cb.Name)
			switch {
			case errors.Is(err, store.ErrNotFound):
			case err != nil:
				return p, err
			default:
				exists = true
			}
		}
		switch {
		case !exists:
			item("channel", cb.Name, Create, "")
		case ch.Kind != cb.Kind:
			item("channel", cb.Name, Refused, "it is %s, not %s", ch.Kind, cb.Kind)
		default:
			item("channel", cb.Name, Same, "")
		}
		var serves []string
		if exists {
			if serves, err = d.Store.ChannelVersions(ctx, ch.ID); err != nil {
				return p, err
			}
		}
		var add []string
		for _, v := range cb.Versions {
			if _, ok := known[v]; !ok && !slices.ContainsFunc(b.Versions, func(x config.BootstrapVersion) bool { return x.Version == v }) {
				item("versions", cb.Name, Refused, "DuckDB version %s is neither known nor in versions", v)
				continue
			}
			if !slices.Contains(serves, v) {
				add = append(add, v)
			}
		}
		if len(add) > 0 {
			item("versions", cb.Name, Add, "%s", strings.Join(add, ", "))
		}
		if cb.Key == "" {
			continue
		}
		fp, err := d.open(ctx, cb.Key, keysource.PurposeChannel)
		if err != nil {
			return p, err
		}
		if other, dup := fingerprints[fp]; dup {
			item("key", cb.Name, Refused, "%s is %s's key too", fp, other)
			continue
		}
		fingerprints[fp] = "channel " + cb.Name
		var chKeys []store.Key
		if exists {
			if chKeys, err = d.Store.ListKeys(ctx, ch.ID); err != nil {
				return p, err
			}
		}
		owner, err := d.Store.FingerprintKey(ctx, fp)
		if err != nil {
			return p, err
		}
		switch {
		case owner != "" && slices.ContainsFunc(chKeys, func(x store.Key) bool { return x.ID == owner && x.State == store.KeyActive }):
			item("key", cb.Name, Same, "%s", fp)
		case owner != "":
			item("key", cb.Name, Refused, "%s is another key's, or not this channel's active one", fp)
		case len(chKeys) > 0:
			item("key", cb.Name, Refused, "the channel has keys, none of them this one active")
		default:
			item("key", cb.Name, Create, "%s", fp)
		}
	}
	// the attestation key
	if b.AttestationKey != "" {
		fp, err := d.open(ctx, b.AttestationKey, keysource.PurposeAttestation)
		if err != nil {
			return p, err
		}
		var atts []store.Key
		if p.TenantExists {
			if atts, err = d.Store.AttestationKeys(ctx, tenantID); err != nil {
				return p, err
			}
		}
		owner, err := d.Store.FingerprintKey(ctx, fp)
		if err != nil {
			return p, err
		}
		switch other, dup := fingerprints[fp]; {
		case dup:
			item("attestation key", tb.Name, Refused, "%s is %s's key too", fp, other)
		case owner != "" && slices.ContainsFunc(atts, func(x store.Key) bool { return x.ID == owner && x.State == store.KeyActive }):
			item("attestation key", tb.Name, Same, "%s", fp)
		case owner != "":
			item("attestation key", tb.Name, Refused, "%s is another key's", fp)
		case len(atts) > 0:
			item("attestation key", tb.Name, Refused, "the tenant has attestation keys, none of them this one active")
		default:
			item("attestation key", tb.Name, Create, "%s", fp)
		}
	}
	// the issuer, its console client, grants
	var ta store.TenantAuth
	if p.TenantExists {
		if ta, err = d.Store.GetTenantAuth(ctx, tenantID); err != nil {
			return p, err
		}
	}
	if ib := b.Issuer; ib != nil {
		want, err := ib.IssuerRecord()
		if err == nil {
			err = auth.CheckIssuer(&want, d.Auth.AllowHTTP)
		}
		if err != nil {
			return p, fmt.Errorf("%w: %w", store.ErrInvalid, err)
		}
		var cur *store.Issuer
		for i := range ta.Issuers {
			switch is := &ta.Issuers[i]; {
			case is.Name == ib.Name:
				cur = is
			case strings.TrimRight(is.URL, "/") == strings.TrimRight(ib.URL, "/"):
				item("issuer", ib.Name, Refused, "the tenant's issuer %s has its URL", is.Name)
			}
		}
		switch {
		case cur == nil:
			// fetched now, as issuer add fetches them: an issuer not reachable yet is found before
			// any write
			jwks := want.JWKSURI
			if jwks == "" {
				if jwks, err = auth.Discover(ctx, d.Auth.Fetch, want.URL); err != nil {
					return p, fmt.Errorf("%w: discovery for %s: %w", tenants.ErrIssuerFetch, want.URL, err)
				}
			}
			if n, err := auth.CheckJWKS(ctx, d.Auth.Fetch, jwks); err != nil || n == 0 {
				return p, fmt.Errorf("%w: the JWKS of %s: %v (%d keys)", tenants.ErrIssuerFetch, want.URL, err, n)
			}
			item("issuer", ib.Name, Create, "")
		case issuerDiff(*cur, want) != "":
			item("issuer", ib.Name, Refused, "its %s differs", issuerDiff(*cur, want))
		default:
			item("issuer", ib.Name, Same, "")
		}
		if cc := ib.Console; cc != nil {
			var have *store.ConsoleClient
			if cur != nil {
				iss, ccs, err := d.Store.ConsoleClients(ctx, tenantID)
				if err != nil {
					return p, err
				}
				for i := range iss {
					if iss[i].ID == cur.ID {
						have = &ccs[i]
					}
				}
			}
			switch {
			case have == nil:
				item("console", ib.Name, Create, "%s", cc.ClientID)
			case have.ClientID != cc.ClientID || have.Audience != cc.Audience || have.AudienceParameter != cc.AudienceParameter ||
				!slices.Equal(have.Scopes, cc.ScopeList()):
				item("console", ib.Name, Refused, "another console client is set (%s)", have.ClientID)
			default:
				item("console", ib.Name, Same, "%s", cc.ClientID)
			}
		}
	}
	for _, g := range b.Grants {
		if hasGrant(ta, g) {
			item("grant", g.Key(), Same, "")
		} else {
			item("grant", g.Key(), Create, "")
		}
	}
	// a tenant the bootstrap did not make, without -adopt, is refused below whatever its upstreams
	// are: the sources are not read then, so the refusal is the answer, not a wait on the network
	readSources := !p.TenantExists || p.Resumed || o.Adopt
	if err := checkUpstreams(ctx, d, b, o, p.TenantExists, readSources, item); err != nil {
		return p, err
	}
	// adoption: nothing is made in a tenant the bootstrap did not make, unless asked
	if p.TenantExists && !p.Resumed && p.tenantChanges() && !o.Adopt {
		item("tenant", tb.Name, Refused, "it exists and the bootstrap would change it: kista admin bootstrap -adopt")
	}
	return p, nil
}

// issuerDiff names the first field the existing issuer has otherwise than the section's (checked,
// so defaulted as issuer add defaults it).
func issuerDiff(cur, want store.Issuer) string {
	switch {
	case strings.TrimRight(cur.URL, "/") != strings.TrimRight(want.URL, "/"):
		return "url"
	case cur.JWKSURI != want.JWKSURI:
		return "jwks_uri"
	case !slices.Equal(cur.Algorithms, want.Algorithms):
		return "algorithms"
	case !maps.Equal(cur.RequiredClaims, want.RequiredClaims) && (len(want.RequiredClaims) > 0 || len(cur.RequiredClaims) > 0):
		return "required_claims"
	case !slices.Equal(cur.RolesClaim, want.RolesClaim):
		return "roles_claim"
	case !slices.Equal(cur.GroupsClaim, want.GroupsClaim):
		return "groups_claim"
	case !slices.Equal(cur.ClientClaim, want.ClientClaim):
		return "client_claim"
	case cur.MaxTokenLifetime != want.MaxTokenLifetime:
		return "max_lifetime"
	}
	return ""
}

// hasGrant reports whether the tenant holds the grant: its principal, verbs and scope.
func hasGrant(ta store.TenantAuth, g config.BootstrapGrant) bool {
	for _, x := range ta.Grants {
		y := config.BootstrapGrant{Principal: x.Principal(), Verbs: x.Verbs, Channel: x.ChannelName, Extension: x.Extension}
		if x.ExtensionGroupID == "" && y.Key() == g.Key() {
			return true
		}
	}
	return false
}

// apply checks, then records the bootstrap pending, makes what the plan creates and records it
// applied. A refusal writes nothing.
func apply(ctx context.Context, d Deps, b config.Bootstrap, digest, section string, o Options) (Result, error) {
	p, err := Check(ctx, d, b, o)
	res := Result{Plan: p}
	if err != nil {
		return res, err
	}
	if r := p.Refusals(); len(r) > 0 {
		lines := make([]string, len(r))
		for i, x := range r {
			lines[i] = x.String()
		}
		return res, fmt.Errorf("%w: %s", ErrRefused, strings.Join(lines, "; "))
	}
	a := o.Actor
	b = b.Defaulted()
	tb := b.Tenant
	// pending before the first write: a run after a failure goes on with the tenant it made
	if !p.Resumed && p.Changes() {
		rec := store.Bootstrap{Tenant: tb.Name, Digest: digest, SeenDigest: digest, Section: section, Actor: a.String()}
		if err := d.Store.InsertBootstrap(ctx, &rec); err != nil {
			return res, err
		}
	}
	created := map[string][]string{}
	made := func(kind, name string) { created[kind] = append(created[kind], name) }
	for _, v := range b.Versions {
		switch p.action("version", v.Version) {
		case Create:
			core := tenants.CoreExtensions{Names: v.CoreExtensions, Commit: v.CoreExtensionsCommit}
			if _, _, err := d.Tenants.AddVersion(ctx, a, v.Version, v.Kind, v.CAPIs, core); err != nil {
				return res, err
			}
			made("version", v.Version)
		case Add:
			cur, err := d.Store.ListDuckDBVersions(ctx)
			if err != nil {
				return res, err
			}
			for _, c := range v.CAPIs {
				if !slices.ContainsFunc(cur, func(x store.DuckDBVersion) bool {
					return x.Name == v.Version && slices.ContainsFunc(x.CAPIs, func(y store.CAPI) bool { return y.String() == c })
				}) {
					if _, err := d.Tenants.AddVersionCAPI(ctx, a, v.Version, c); err != nil {
						return res, err
					}
					made("c_api", v.Version+" "+c)
				}
			}
			if len(v.CoreExtensions) > 0 {
				added, err := d.Tenants.AddVersionCore(ctx, a, v.Version, tenants.CoreExtensions{Names: v.CoreExtensions, Commit: v.CoreExtensionsCommit})
				if err != nil {
					return res, err
				}
				for _, n := range added {
					made("core_extension", v.Version+" "+n)
				}
			}
		}
	}
	if p.action("tenant", tb.Name) == Create {
		if _, err := d.Tenants.CreateTenant(ctx, a, tb.Name, tb.DisplayName, tb.Domain, ""); err != nil {
			return res, err
		}
		made("tenant", tb.Name)
	}
	for _, aud := range tb.Audiences {
		if p.action("audience", aud) == Create {
			if err := d.Auth.AddAudience(ctx, a, tb.Name, aud); err != nil {
				return res, err
			}
			made("audience", aud)
		}
	}
	for _, cb := range b.Channels {
		if p.action("channel", cb.Name) == Create {
			if _, err := d.Tenants.CreateChannel(ctx, a, tb.Name, cb.Name, cb.Kind); err != nil {
				return res, err
			}
			made("channel", cb.Name)
		}
		if p.action("versions", cb.Name) == Add {
			ch, err := d.Store.GetChannel(ctx, tb.Name, cb.Name)
			if err != nil {
				return res, err
			}
			serves, err := d.Store.ChannelVersions(ctx, ch.ID)
			if err != nil {
				return res, err
			}
			var add []string
			for _, v := range cb.Versions {
				if !slices.Contains(serves, v) {
					add = append(add, v)
				}
			}
			if len(add) > 0 {
				if _, err := d.Tenants.SetChannelVersions(ctx, a, tb.Name, cb.Name, add, nil); err != nil {
					return res, err
				}
				made("channel versions", cb.Name+" "+strings.Join(add, ","))
			}
		}
		if cb.Key != "" && p.action("key", cb.Name) == Create {
			if _, err := d.Keys.Add(ctx, a, tb.Name, cb.Name, cb.Key, true); err != nil {
				return res, err
			}
			made("key", cb.Name)
		}
	}
	if b.AttestationKey != "" && p.action("attestation key", tb.Name) == Create {
		if _, err := d.Keys.AddAttestation(ctx, a, tb.Name, b.AttestationKey, true); err != nil {
			return res, err
		}
		made("attestation key", tb.Name)
	}
	if ib := b.Issuer; ib != nil {
		if p.action("issuer", ib.Name) == Create {
			is, err := ib.IssuerRecord()
			if err != nil {
				return res, err
			}
			if _, err := d.Auth.AddIssuer(ctx, a, tb.Name, is); err != nil {
				return res, err
			}
			made("issuer", ib.Name)
		}
		if cc := ib.Console; cc != nil && p.action("console", ib.Name) == Create {
			c := store.ConsoleClient{ClientID: cc.ClientID, Scopes: cc.ScopeList(), Audience: cc.Audience, AudienceParameter: cc.AudienceParameter}
			if _, err := d.Auth.SetConsoleClient(ctx, a, tb.Name, ib.Name, "", c); err != nil {
				return res, err
			}
			made("console", ib.Name)
		}
	}
	for _, g := range b.Grants {
		if p.action("grant", g.Key()) == Create {
			on := tenants.GrantOn{Channel: g.Channel, Extension: g.Extension}
			if _, err := d.Auth.AddGrant(ctx, a, tb.Name, g.Principal, g.Verbs, on); err != nil {
				return res, err
			}
			made("grant", g.Key())
		}
	}
	// 10. the upstreams (spec 0024): due for a first run once made
	for _, ub := range b.Upstreams {
		if p.action("upstream", ub.Name) == Create {
			if _, err := d.Upstreams.Add(ctx, a, tb.Name, upstreamSpec(ub)); err != nil {
				return res, err
			}
			made("upstream", ub.Name)
		}
	}
	t, err := d.Store.GetTenant(ctx, tb.Name)
	if err != nil {
		return res, err
	}
	rec := store.Bootstrap{Tenant: tb.Name, Digest: digest, SeenDigest: digest, Section: section, Actor: a.String()}
	err = d.Store.InTx(ctx, "", func(tx *store.Tx) error {
		if !p.Resumed && !p.Changes() {
			// everything was there already: recorded with nothing made
			if err := tx.InsertPendingBootstrap(ctx, &rec); err != nil {
				return err
			}
		}
		if err := tx.ApplyBootstrap(ctx, &rec); err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "bootstrap.apply", "tenant:"+t.Name, map[string]any{"tenant": t.Name,
			"digest": digest, "source": o.Source, "version": o.Version, "host": o.Host, "created": created})
	})
	if errors.Is(err, store.ErrExists) {
		res.Already = true
		return res, nil
	}
	if err != nil {
		return res, err
	}
	res.Applied = true
	return res, nil
}

// Accept takes a changed section as the applied one, without applying it (kista admin bootstrap
// -accept).
func Accept(ctx context.Context, d Deps, b config.Bootstrap, a authz.Actor) error {
	digest, section, err := Digest(b)
	if err != nil {
		return err
	}
	rec, err := d.Store.GetBootstrap(ctx)
	if err != nil {
		return err
	}
	if rec.State != store.BootstrapApplied {
		return fmt.Errorf("%w: the bootstrap did not finish: nothing to accept", store.ErrConflict)
	}
	return d.Store.InTx(ctx, "", func(tx *store.Tx) error {
		if err := tx.AcceptBootstrap(ctx, digest, section); err != nil {
			return err
		}
		return tx.Event(ctx, tenantID(ctx, d, rec.Tenant), a.String(), "bootstrap.accept", "tenant:"+rec.Tenant,
			map[string]any{"tenant": rec.Tenant, "digest": digest, "previous": rec.Digest})
	})
}

// Rearm deletes the record, so the next apply checks the section again (kista admin bootstrap
// -rearm): with -adopt, it makes what is absent.
func Rearm(ctx context.Context, d Deps, a authz.Actor) error {
	rec, err := d.Store.GetBootstrap(ctx)
	if err != nil {
		return err
	}
	return d.Store.InTx(ctx, "", func(tx *store.Tx) error {
		if _, err := tx.DeleteBootstrap(ctx); err != nil {
			return err
		}
		return tx.Event(ctx, tenantID(ctx, d, rec.Tenant), a.String(), "bootstrap.rearm", "tenant:"+rec.Tenant,
			map[string]any{"tenant": rec.Tenant, "digest": rec.Digest})
	})
}

// tenantID is a tenant's id for an event; "" (the server's) when it is gone.
func tenantID(ctx context.Context, d Deps, name string) string {
	t, err := d.Store.GetTenant(ctx, name)
	if err != nil {
		return ""
	}
	return t.ID
}

// upstreamSpec is a section's upstream as upstream add takes it.
func upstreamSpec(u config.BootstrapUpstream) upstream.Spec {
	return upstream.Spec{Name: u.Name, Kind: u.Kind, Prefix: u.Prefix, Channel: u.Channel, Mode: u.Mode,
		Visibility: u.Visibility, Credential: u.Credential, FetchMode: u.Fetch, Keys: u.Keys, Platforms: u.Platforms, Entries: u.StoreEntries()}
}

// checkUpstreams plans the section's upstreams (spec 0024): one that exists is the same or refused;
// an absent one is checked as upstream add checks it, its source's .well-known read, so a refusal is
// found before any write. In a channel that exists, against what it holds, all of them together
// (CheckAddAll); in one the bootstrap is to make, the upstream itself (CheckSource), its collisions
// and limits having been checked at load. A source not reachable for now is
// upstream.ErrSourceFetch, which the bootstrap tries again.
func checkUpstreams(ctx context.Context, d Deps, b config.Bootstrap, o Options, tenantExists, readSources bool,
	item func(kind, name string, a Action, detail string, args ...any)) error {
	if len(b.Upstreams) == 0 {
		return nil
	}
	kinds := map[string]string{}
	for _, ch := range b.Channels {
		kinds[ch.Name] = ch.Kind
	}
	var tenantID string
	if tenantExists {
		t, err := d.Store.GetTenantAny(ctx, b.Tenant.Name)
		if err != nil {
			return err
		}
		if t.State != store.TenantActive || !t.DeletingAt.IsZero() {
			return nil // the tenant itself is refused: nothing more to check in it
		}
		tenantID = t.ID
	}
	var inChannels []config.BootstrapUpstream
	for _, ub := range b.Upstreams {
		sp := upstreamSpec(ub)
		var chID string
		if tenantExists {
			sc, err := d.Store.GetServeChannel(ctx, b.Tenant.Name, ub.Channel)
			switch {
			case err == nil:
				chID, kinds[ub.Channel] = sc.Channel.ID, sc.Channel.Kind
			case !errors.Is(err, store.ErrNotFound):
				return err
			}
		}
		var cur *store.Upstream
		if tenantID != "" {
			err := d.Store.InTx(ctx, "", func(tx *store.Tx) error {
				u, err := tx.GetUpstream(ctx, tenantID, ub.Name)
				if err == nil {
					cur = &u
				}
				return err
			})
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		if cur != nil {
			if why := upstreamDiff(*cur, ub, chID); why != "" {
				item("upstream", ub.Name, Refused, "its %s differs", why)
			} else {
				item("upstream", ub.Name, Same, "")
			}
			continue
		}
		if !readSources {
			item("upstream", ub.Name, Create, "%s %s (not checked: the tenant needs -adopt)", ub.Kind, ub.Channel)
			continue
		}
		if chID != "" {
			inChannels = append(inChannels, ub)
			continue
		}
		_, err := d.Upstreams.CheckSource(ctx, b.Tenant.Name, sp, kinds[ub.Channel])
		if err := classify(err, ub, item); err != nil {
			return err
		}
	}
	// the tenant's count with the new ones (an adopted tenant holds upstreams of its own)
	if tenantID != "" {
		var have []store.Upstream
		if err := d.Store.InTx(ctx, "", func(tx *store.Tx) error {
			var err error
			have, err = tx.ListUpstreams(ctx, tenantID)
			return err
		}); err != nil {
			return err
		}
		absent := 0
		for _, ub := range b.Upstreams {
			if !slices.ContainsFunc(have, func(u store.Upstream) bool { return u.Name == ub.Name }) {
				absent++
			}
		}
		if len(have)+absent > store.MaxUpstreams {
			item("upstream", "*", Refused, "the tenant would hold %d upstreams (at most %d)", len(have)+absent, store.MaxUpstreams)
		}
	}
	// the upstreams of channels that exist: checked together, each as if those before it were
	// added, so two of the section's never pass alone and collide in the apply
	for len(inChannels) > 0 {
		sps := make([]upstream.Spec, len(inChannels))
		for i, ub := range inChannels {
			sps[i] = upstreamSpec(ub)
		}
		i, err := d.Upstreams.CheckAddAll(ctx, o.Actor, b.Tenant.Name, sps)
		if err == nil {
			for _, ub := range inChannels {
				if err := classify(nil, ub, item); err != nil {
					return err
				}
			}
			break
		}
		if err := classify(err, inChannels[i], item); err != nil {
			return err
		}
		inChannels = slices.Delete(inChannels, i, i+1)
	}
	return nil
}

// classify makes a check's answer a plan item: created, or refused; a source not reachable for now
// (or anything else) is the bootstrap's error.
func classify(err error, ub config.BootstrapUpstream, item func(kind, name string, a Action, detail string, args ...any)) error {
	name := ub.Name
	switch {
	case err == nil:
		item("upstream", name, Create, "%s %s", ub.Kind, ub.Channel)
	case errors.Is(err, upstream.ErrSourceFetch):
		return err
	case errors.Is(err, store.ErrInvalid), errors.Is(err, upstream.ErrCollision), errors.Is(err, store.ErrExists),
		errors.Is(err, upstream.ErrNoKeys), errors.Is(err, store.ErrNotFound):
		item("upstream", name, Refused, "%v", err)
	default:
		return err
	}
	return nil
}

// upstreamDiff names the first field an existing upstream has otherwise than the section's
// (defaulted, so normalised as upstream add normalises it); more keys, platforms or entries than the
// section lists are the administrator's.
func upstreamDiff(cur store.Upstream, want config.BootstrapUpstream, channelID string) string {
	switch {
	case cur.Kind != want.Kind:
		return "kind"
	case cur.Prefix != want.Prefix:
		return "prefix"
	case channelID == "" || cur.ChannelID != channelID:
		return "channel"
	case cur.Mode != want.Mode:
		return "mode"
	case cur.Mode == store.ModeMirror && cur.FetchMode != cmp.Or(want.Fetch, store.FetchEager):
		return "fetch"
	case cur.Visibility != want.Visibility:
		return "visibility"
	case cur.Credential != want.Credential:
		return "credential"
	}
	for _, k := range want.Keys {
		if !slices.Contains(cur.Keys, k) {
			return "keys (" + k + " is not pinned)"
		}
	}
	for _, pl := range want.Platforms {
		if !slices.Contains(cur.Platforms, pl) {
			return "platforms (" + pl + " is not fetched)"
		}
	}
	for _, e := range want.StoreEntries() {
		i := slices.IndexFunc(cur.Entries, func(x store.UpstreamEntry) bool { return x.Name == e.Name })
		if i < 0 {
			return "allowlist (" + e.Name + " is not on it)"
		}
		if have := cur.Entries[i]; !slices.Equal(upspec.Dedup(have.Versions), e.Versions) || have.AllowReserved != e.AllowReserved ||
			have.Eager != e.Eager {
			return "allowlist entry " + e.Name
		}
	}
	return ""
}

// missingCore lists the names of a version's core extensions the server does not hold as core.
func missingCore(names []string, kinds map[string]string) []string {
	var out []string
	for _, n := range names {
		if kinds[n] != store.ReservedCore && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}
