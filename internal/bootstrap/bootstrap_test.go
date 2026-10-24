package bootstrap_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bootstrap"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

var ctx = context.Background()

const idpURL = "https://login.example/tid/v2.0"

type fakeIDP struct {
	key *rsa.PrivateKey
	// fail answers calls from..to (counted from 1) with an error: an IdP down for a while
	mu              sync.Mutex
	calls, from, to int
}

func (f *fakeIDP) Get(_ context.Context, url string) ([]byte, http.Header, error) {
	f.mu.Lock()
	f.calls++
	down := f.calls >= f.from && f.calls <= f.to
	f.mu.Unlock()
	if down {
		return nil, nil, errors.New("connection refused")
	}
	switch url {
	case idpURL + "/.well-known/openid-configuration":
		return []byte(`{"issuer":"` + idpURL + `","jwks_uri":"` + idpURL + `/jwks"}`), http.Header{}, nil
	case idpURL + "/jwks":
		b64 := base64.RawURLEncoding.EncodeToString
		b, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k1",
			"n": b64(f.key.N.Bytes()), "e": b64(big.NewInt(int64(f.key.E)).Bytes())}}})
		return b, http.Header{}, nil
	}
	return nil, nil, errors.New("not found")
}

type env struct {
	st  *store.Store
	d   bootstrap.Deps
	dir string
	idp *fakeIDP
}

func newEnv(t *testing.T, e storetest.Engine) *env {
	t.Helper()
	st := e.Open(t)
	dir := t.TempDir()
	for _, f := range []string{"a.pem", "att.pem", "other.pem"} {
		if err := signer.GenerateKeyFile(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	az := authz.ServerAdmin{}
	idp := &fakeIDP{key: k}
	return &env{st: st, dir: dir, idp: idp, d: bootstrap.Deps{Store: st,
		Tenants: &tenants.Service{Store: st, Authz: az, HasDomain: func(d string) bool { return d == "default" }},
		Auth:    &tenants.AuthAdmin{Store: st, Authz: az, Fetch: idp, PublicURL: "https://kista.example"},
		Keys:    &keys.Service{Store: st, Signers: signer.Resolver{FileDir: dir, AllowFile: true}, Authz: az},
		Log:     slog.New(slog.DiscardHandler)}}
}

func section() config.Bootstrap {
	return config.Bootstrap{
		Versions:       []config.BootstrapVersion{{Version: "v1.4.1", Kind: "release", CAPIs: []string{"v1.2.0"}, CoreExtensions: []string{"json", "parquet"}}},
		Tenant:         config.BootstrapTenant{Name: "acme", DisplayName: "Acme", Audiences: []string{"api://kista-acme"}},
		Channels:       []config.BootstrapChannel{{Name: "prod", Kind: store.ChannelSigned, Versions: []string{"v1.4.1"}, Key: "file:a.pem"}},
		AttestationKey: "file:att.pem",
		Issuer: &config.BootstrapIssuer{Name: "entra", URL: idpURL, RequiredClaims: map[string]string{"tid": "tid"}, RolesClaim: "roles",
			Console: &config.BootstrapConsole{ClientID: "console-app", Audience: "api://kista-acme"}},
		Grants: []config.BootstrapGrant{{Principal: "role:entra|kista-admin", Verbs: []string{"admin"}}},
	}
}

var serveOpts = bootstrap.Options{Actor: authz.Bootstrap, Source: "env:KISTA_CONFIG", Version: "test", Host: "h1", Holder: "h1/1", Retry: 1}

func (en *env) events(t *testing.T, kind string) []store.Event {
	t.Helper()
	var out []store.Event
	tenants, _ := en.st.ListTenants(ctx)
	for _, id := range append([]string{audit.ServerTenant}, func() []string {
		var ids []string
		for _, x := range tenants {
			ids = append(ids, x.ID)
		}
		return ids
	}()...) {
		evs, err := en.st.ListEvents(ctx, id, store.EventFilter{Kind: kind, Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, evs...)
	}
	return out
}

// Spec 0020 phase 4a: a first start makes every object of the section through the services, each
// with its event as system:bootstrap, and records the bootstrap; the next start applies nothing.
func TestBootstrapApplies(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			b := section()
			res, err := bootstrap.Run(ctx, en.d, b, serveOpts)
			if err != nil || !res.Applied {
				t.Fatalf("apply: %+v %v", res, err)
			}
			tn, err := en.st.GetTenant(ctx, "acme")
			if err != nil || tn.DisplayName != "Acme" {
				t.Fatalf("the tenant: %+v %v", tn, err)
			}
			ch, err := en.st.GetChannel(ctx, "acme", "prod")
			if err != nil {
				t.Fatal(err)
			}
			ks, _ := en.st.ListKeys(ctx, ch.ID)
			if len(ks) != 1 || ks[0].State != store.KeyActive || ks[0].SignerRef != "file:a.pem" {
				t.Fatalf("the channel key: %+v", ks)
			}
			if vs, _ := en.st.ChannelVersions(ctx, ch.ID); len(vs) != 1 || vs[0] != "v1.4.1" {
				t.Fatalf("channel versions: %v", vs)
			}
			ta, _ := en.st.GetTenantAuth(ctx, tn.ID)
			if len(ta.Issuers) != 1 || len(ta.Audiences) != 1 || len(ta.Grants) != 1 || ta.Grants[0].Principal() != "role:entra|kista-admin" {
				t.Fatalf("auth: %+v", ta)
			}
			if atts, _ := en.st.AttestationKeys(ctx, tn.ID); len(atts) != 1 || atts[0].State != store.KeyActive {
				t.Fatalf("the attestation key: %+v", atts)
			}
			for _, kind := range []string{"tenant.create", "channel.create", "key.add", "issuer.add", "grant.add", "issuer.console.set"} {
				evs := en.events(t, kind)
				if len(evs) != 1 || evs[0].Actor != "system:bootstrap" {
					t.Fatalf("%s: %+v", kind, evs)
				}
			}
			ap := en.events(t, "bootstrap.apply")
			if len(ap) != 1 || ap[0].Subject != "tenant:acme" || ap[0].Actor != "system:bootstrap" {
				t.Fatalf("bootstrap.apply: %+v", ap)
			}
			var data map[string]any
			_ = json.Unmarshal([]byte(ap[0].Data), &data)
			if data["source"] != "env:KISTA_CONFIG" || data["digest"] == "" || data["host"] != "h1" {
				t.Fatalf("bootstrap.apply data: %v", data)
			}
			rec, err := en.st.GetBootstrap(ctx)
			if err != nil || rec.Tenant != "acme" || rec.Actor != "system:bootstrap" {
				t.Fatalf("the record: %+v %v", rec, err)
			}

			// the next start: nothing applied
			res, err = bootstrap.Run(ctx, en.d, b, serveOpts)
			if err != nil || !res.Already || res.Applied || res.Changed {
				t.Fatalf("again: %+v %v", res, err)
			}
			if len(en.events(t, "tenant.create")) != 1 {
				t.Fatal("applied twice")
			}

			// a grant revoked since stays revoked
			if err := en.d.Auth.RemoveGrant(ctx, authz.OSActor(), "acme", ta.Grants[0].ID); err != nil {
				t.Fatal(err)
			}
			if _, err := bootstrap.Run(ctx, en.d, b, serveOpts); err != nil {
				t.Fatal(err)
			}
			if ta, _ := en.st.GetTenantAuth(ctx, tn.ID); len(ta.Grants) != 0 {
				t.Fatal("a revoked grant came back")
			}

			// a changed section: serve notes it once; the CLI refuses until it is accepted
			b.Grants = append(b.Grants, config.BootstrapGrant{Principal: "role:entra|kista-audit", Verbs: []string{"audit"}})
			res, err = bootstrap.Run(ctx, en.d, b, serveOpts)
			if err != nil || !res.Changed || res.Applied {
				t.Fatalf("changed, serve: %+v %v", res, err)
			}
			cli := bootstrap.Options{Actor: authz.OSActor(), Holder: "cli"}
			if _, err := bootstrap.Run(ctx, en.d, b, cli); !errors.Is(err, bootstrap.ErrChanged) {
				t.Fatalf("changed, CLI: %v", err)
			}
			if err := bootstrap.Accept(ctx, en.d, b, authz.OSActor()); err != nil {
				t.Fatal(err)
			}
			if res, err := bootstrap.Run(ctx, en.d, b, cli); err != nil || res.Changed {
				t.Fatalf("accepted: %+v %v", res, err)
			}
			if len(en.events(t, "bootstrap.accept")) != 1 {
				t.Fatal("no bootstrap.accept")
			}

			// re-armed, it makes what is absent again: the revoked grant too
			if err := bootstrap.Rearm(ctx, en.d, authz.OSActor()); err != nil {
				t.Fatal(err)
			}
			if len(en.events(t, "bootstrap.rearm")) != 1 {
				t.Fatal("no bootstrap.rearm")
			}
			res, err = bootstrap.Run(ctx, en.d, b, bootstrap.Options{Actor: authz.OSActor(), Holder: "cli", Adopt: true, Retry: 1})
			if err != nil || !res.Applied {
				t.Fatalf("re-armed: %+v %v", res, err)
			}
			if ta, _ := en.st.GetTenantAuth(ctx, tn.ID); len(ta.Grants) != 2 {
				t.Fatalf("re-armed grants: %+v", ta.Grants)
			}
		})
	}
}

// A key version is no change; a section's digest is its parsed content's.
func TestBootstrapDigest(t *testing.T) {
	a, b := section(), section()
	a.Channels[0].Key, b.Channels[0].Key = "azure:kista-channel/0123456789abcdef0123456789abcdef", "azure:kista-channel/fedcba9876543210fedcba9876543210"
	da, _, _ := bootstrap.Digest(a)
	db, _, _ := bootstrap.Digest(b)
	if da != db {
		t.Fatal("a key version changed the digest")
	}
	b.Tenant.DisplayName = "Other"
	if dc, _, _ := bootstrap.Digest(b); dc == da {
		t.Fatal("a change kept the digest")
	}
	// spec 0032: a mirror without a fetch mode digests as before it existed (an applied section
	// stays applied after the upgrade)
	m := section()
	m.Upstreams = []config.BootstrapUpstream{{Name: "core", Kind: "duckdb-core", Channel: m.Channels[0].Name, Platforms: []string{"linux_amd64"},
		Extensions: []config.BootstrapUpstreamEntry{{Name: "httpfs"}}}}
	if _, js, err := bootstrap.Digest(m); err != nil || strings.Contains(js, "fetch") || strings.Contains(js, "eager") {
		t.Fatalf("a mirror's section: %s %v", js, err)
	}
}

// Spec 0034: a grant's package_type is optional, never filled in by Defaulted and left out of its
// key when it is the default for the grant's shape: a section applied before keeps its digest (the
// one v0.1.4 computed for it) and its plan's grants.
func TestBootstrapDigestPackageTypes(t *testing.T) {
	b := section()
	b.Grants = append(b.Grants, config.BootstrapGrant{Principal: "role:entra|ops", Verbs: []string{"install"}, Channel: "prod"},
		config.BootstrapGrant{Principal: "role:entra|dev", Verbs: []string{"publish", "promote"}, Extension: "tresor"})
	const before = "2a62a40c48d65d821ea3e1a652de415c4474a9d7fb05f229b6b7a54c07020d68" // origin/main's Digest of this section
	d, js, err := bootstrap.Digest(b)
	if err != nil || d != before || strings.Contains(js, "package_type") {
		t.Fatalf("the digest moved: %s %v\n%s", d, err, js)
	}
	keys := []string{"role:entra|kista-admin admin", "role:entra|ops install on prod", "role:entra|dev promote,publish on */tresor"}
	for i, g := range b.Grants {
		if g.Key() != keys[i] {
			t.Fatalf("key %q, was %q", g.Key(), keys[i])
		}
		// the default for the shape, named, is the same grant
		g.PackageType = pkgtype.Any
		if g.Extension != "" {
			g.PackageType = pkgtype.DuckDBExtension
		}
		if g.Key() != keys[i] {
			t.Fatalf("the default named: %q", g.Key())
		}
	}
	typed := config.BootstrapGrant{Principal: "role:entra|ops", Verbs: []string{"install"}, PackageType: pkgtype.DuckDBExtension}
	if typed.Key() != "role:entra|ops install of duckdb_extension" {
		t.Fatalf("a typed grant's key: %q", typed.Key())
	}
	b.Grants = append(b.Grants, typed)
	if d2, _, _ := bootstrap.Digest(b); d2 == d {
		t.Fatal("a typed grant kept the digest")
	}
}

// Spec 0034: a grant on a channel naming the channel's type is stored as one of every type there; the
// next run reads it as the same grant (nothing to make).
func TestBootstrapChannelTypeGrant(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			b := section()
			b.Grants = append(b.Grants, config.BootstrapGrant{Principal: "role:entra|ops", Verbs: []string{"install"}, Channel: "prod",
				PackageType: pkgtype.DuckDBExtension})
			if res, err := bootstrap.Run(ctx, en.d, b, serveOpts); err != nil || !res.Applied {
				t.Fatalf("applied: %+v %v", res, err)
			}
			p, err := bootstrap.Check(ctx, en.d, b, bootstrap.Options{})
			if err != nil || p.Changes() {
				t.Fatalf("the plan after: %+v %v", p, err)
			}
		})
	}
}

// Objects that exist and differ are refused with nothing written; a tenant the bootstrap did not
// make is changed only with -adopt.
func TestBootstrapRefuses(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			cli := authz.OSActor()
			cases := []struct {
				name  string
				setup func(t *testing.T, en *env, b *config.Bootstrap)
			}{
				{"another display name", func(t *testing.T, en *env, b *config.Bootstrap) {
					must(t, func() error { _, err := en.d.Tenants.CreateTenant(ctx, cli, "acme", "Other", "", ""); return err })
				}},
				{"a suspended tenant", func(t *testing.T, en *env, b *config.Bootstrap) {
					must(t, func() error { _, err := en.d.Tenants.CreateTenant(ctx, cli, "acme", "Acme", "", ""); return err })
					must(t, func() error {
						_, err := en.d.Tenants.SetTenantState(ctx, cli, "acme", store.TenantSuspended, 0)
						return err
					})
				}},
				{"an audience of another tenant", func(t *testing.T, en *env, b *config.Bootstrap) {
					must(t, func() error { _, err := en.d.Tenants.CreateTenant(ctx, cli, "other", "", "", ""); return err })
					must(t, func() error { return en.d.Auth.AddAudience(ctx, cli, "other", "api://kista-acme") })
				}},
				{"a version of another kind", func(t *testing.T, en *env, b *config.Bootstrap) {
					must(t, func() error {
						_, _, err := en.d.Tenants.AddVersion(ctx, cli, "v1.4.1", "dev", nil, tenants.CoreExtensions{Names: []string{"json"}})
						return err
					})
				}},
				{"a key another channel holds", func(t *testing.T, en *env, b *config.Bootstrap) {
					must(t, func() error { _, err := en.d.Tenants.CreateTenant(ctx, cli, "other", "", "", ""); return err })
					must(t, func() error {
						_, err := en.d.Tenants.CreateChannel(ctx, cli, "other", "prod", store.ChannelSigned, "")
						return err
					})
					must(t, func() error { _, err := en.d.Keys.Add(ctx, cli, "other", "prod", "file:a.pem", true); return err })
				}},
				{"a channel with another key", func(t *testing.T, en *env, b *config.Bootstrap) {
					must(t, func() error { _, err := en.d.Tenants.CreateTenant(ctx, cli, "acme", "Acme", "", ""); return err })
					must(t, func() error {
						_, err := en.d.Tenants.CreateChannel(ctx, cli, "acme", "prod", store.ChannelSigned, "")
						return err
					})
					must(t, func() error { _, err := en.d.Keys.Add(ctx, cli, "acme", "prod", "file:other.pem", true); return err })
				}},
				{"an issuer whose required claims differ", func(t *testing.T, en *env, b *config.Bootstrap) {
					must(t, func() error { _, err := en.d.Tenants.CreateTenant(ctx, cli, "acme", "Acme", "", ""); return err })
					is, _ := b.Issuer.IssuerRecord()
					is.RequiredClaims = map[string]string{}
					must(t, func() error { _, err := en.d.Auth.AddIssuer(ctx, cli, "acme", is); return err })
				}},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					en := newEnv(t, e)
					b := section()
					c.setup(t, en, &b)
					before := len(en.events(t, ""))
					_, err := bootstrap.Run(ctx, en.d, b, bootstrap.Options{Actor: authz.OSActor(), Holder: "cli", Adopt: true, Retry: 1})
					if !errors.Is(err, bootstrap.ErrRefused) {
						t.Fatalf("not refused: %v", err)
					}
					if after := len(en.events(t, "")); after != before {
						t.Fatalf("a refusal wrote %d events", after-before)
					}
					if _, err := en.st.GetBootstrap(ctx); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("a refusal recorded the bootstrap: %v", err)
					}
				})
			}
		})
	}
}

// A tenant made through the API is changed only with -adopt; one that matches entirely is
// recorded with nothing made.
func TestBootstrapAdopts(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			cli := authz.OSActor()
			must(t, func() error { _, err := en.d.Tenants.CreateTenant(ctx, cli, "acme", "Acme", "", ""); return err })
			b := section()
			if _, err := bootstrap.Run(ctx, en.d, b, serveOpts); !errors.Is(err, bootstrap.ErrRefused) {
				t.Fatalf("serve created in an existing tenant: %v", err)
			}
			res, err := bootstrap.Run(ctx, en.d, b, bootstrap.Options{Actor: cli, Holder: "cli", Adopt: true, Retry: 1})
			if err != nil || !res.Applied {
				t.Fatalf("adopt: %+v %v", res, err)
			}
			// everything present and the same: recorded, nothing made
			if err := bootstrap.Rearm(ctx, en.d, cli); err != nil {
				t.Fatal(err)
			}
			n := len(en.events(t, "tenant.create"))
			res, err = bootstrap.Run(ctx, en.d, b, serveOpts)
			if err != nil || !res.Applied || res.Plan.Changes() || len(en.events(t, "tenant.create")) != n {
				t.Fatalf("all the same: %+v %v", res, err)
			}
		})
	}
}

// A failure that may pass (a signer not there yet) leaves what was made and no record; the next
// start converges. -dry-run writes nothing.
func TestBootstrapConverges(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			b := section()
			b.AttestationKey = "file:later.pem"
			p, err := bootstrap.Check(ctx, en.d, section(), bootstrap.Options{})
			if err != nil || !p.Changes() || len(p.Refusals()) != 0 {
				t.Fatalf("the plan: %+v %v", p, err)
			}
			res, err := bootstrap.Run(ctx, en.d, section(), bootstrap.Options{Actor: authz.Bootstrap, DryRun: true})
			if err != nil || res.Applied || len(res.Plan.Items) == 0 {
				t.Fatalf("dry run: %+v %v", res, err)
			}
			if ts, _ := en.st.ListTenants(ctx); len(ts) != 0 {
				t.Fatal("a dry run wrote")
			}
			if _, err := bootstrap.Run(ctx, en.d, b, serveOpts); err == nil || errors.Is(err, bootstrap.ErrRefused) {
				t.Fatalf("a missing signer: %v", err)
			}
			if _, err := en.st.GetBootstrap(ctx); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("recorded after a failure")
			}
			if err := signer.GenerateKeyFile(filepath.Join(en.dir, "later.pem")); err != nil {
				t.Fatal(err)
			}
			res, err = bootstrap.Run(ctx, en.d, b, serveOpts)
			if err != nil || !res.Applied {
				t.Fatalf("converged: %+v %v", res, err)
			}
		})
	}
}

// Replicas starting together apply the bootstrap once.
func TestBootstrapOnce(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			var wg sync.WaitGroup
			results := make([]bootstrap.Result, 3)
			errs := make([]error, 3)
			for i := range 3 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					o := serveOpts
					o.Holder = "replica-" + string(rune('a'+i))
					results[i], errs[i] = bootstrap.Run(ctx, en.d, section(), o)
				}()
			}
			wg.Wait()
			applied := 0
			for i := range 3 {
				if errs[i] != nil {
					t.Fatalf("replica %d: %v", i, errs[i])
				}
				if results[i].Applied {
					applied++
				}
			}
			if applied != 1 || len(en.events(t, "tenant.create")) != 1 {
				t.Fatalf("applied %d times", applied)
			}
		})
	}
}

func must(t *testing.T, f func() error) {
	t.Helper()
	if err := f(); err != nil {
		t.Fatal(err)
	}
}

// A failure after the first write (the IdP down between the check and issuer add) leaves a pending
// record; the next start goes on with the tenant it made, and applies.
func TestBootstrapResumes(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			en.idp.from, en.idp.to = 3, 3 // the check's discovery and JWKS answer; issuer add's discovery does not
			_, err := bootstrap.Run(ctx, en.d, section(), serveOpts)
			if err == nil || errors.Is(err, bootstrap.ErrRefused) {
				t.Fatalf("the first start: %v", err)
			}
			if _, err := en.st.GetTenant(ctx, "acme"); err != nil {
				t.Fatalf("nothing was made before the failure: %v", err)
			}
			if rec, err := en.st.GetBootstrap(ctx); err != nil || rec.State != store.BootstrapPending {
				t.Fatalf("the record after a failure: %+v %v", rec, err)
			}
			res, err := bootstrap.Run(ctx, en.d, section(), serveOpts)
			if err != nil || !res.Applied || !res.Plan.Resumed {
				t.Fatalf("the next start: %+v %v", res, err)
			}
			if n := len(en.events(t, "tenant.create")); n != 1 {
				t.Fatalf("tenant.create %d times", n)
			}
			if rec, _ := en.st.GetBootstrap(ctx); rec.State != store.BootstrapApplied {
				t.Fatalf("the record: %+v", rec)
			}
		})
	}
}

// Grants that differ only by scope are two grants; a channel's missing versions are added; a C API
// of the same major is never changed.
func TestBootstrapDetails(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			b := section()
			b.Channels = append(b.Channels, config.BootstrapChannel{Name: "stage", Kind: store.ChannelSigned, Versions: []string{"v1.4.1"}, Key: "file:other.pem"})
			b.Grants = append(b.Grants, config.BootstrapGrant{Principal: "role:entra|dev", Verbs: []string{"install"}, Channel: "prod"},
				config.BootstrapGrant{Principal: "role:entra|dev", Verbs: []string{"install"}, Channel: "stage"})
			if res, err := bootstrap.Run(ctx, en.d, b, serveOpts); err != nil || !res.Applied {
				t.Fatalf("apply: %+v %v", res, err)
			}
			tn, _ := en.st.GetTenant(ctx, "acme")
			if ta, _ := en.st.GetTenantAuth(ctx, tn.ID); len(ta.Grants) != 3 {
				t.Fatalf("grants: %+v", ta.Grants)
			}
			// re-armed with a version more for prod: only that one is added
			cli := bootstrap.Options{Actor: authz.OSActor(), Holder: "cli", Adopt: true, Retry: 1}
			must(t, func() error { return bootstrap.Rearm(ctx, en.d, authz.OSActor()) })
			b.Versions = append(b.Versions, config.BootstrapVersion{Version: "v1.4.2", Kind: "release", CoreExtensions: []string{"json"}})
			b.Channels[0].Versions = append(b.Channels[0].Versions, "v1.4.2")
			if res, err := bootstrap.Run(ctx, en.d, b, cli); err != nil || !res.Applied {
				t.Fatalf("a version added: %+v %v", res, err)
			}
			ch, _ := en.st.GetChannel(ctx, "acme", "prod")
			if vs, _ := en.st.ChannelVersions(ctx, ch.ID); len(vs) != 2 {
				t.Fatalf("channel versions: %v", vs)
			}
			// a version's core extensions it did not name (spec 0030): added and reserved
			must(t, func() error { return bootstrap.Rearm(ctx, en.d, authz.OSActor()) })
			b.Versions[1].CoreExtensions = []string{"json", "kista_boot_core"}
			b.Versions[1].CoreExtensionsCommit = "abcdef0"
			res, err := bootstrap.Run(ctx, en.d, b, cli)
			if err != nil || !res.Applied || !strings.Contains(fmt.Sprint(res.Plan.Items), "core extensions kista_boot_core") {
				t.Fatalf("core extensions added: %+v %v", res, err)
			}
			if l, _ := en.st.ListReservedNames(ctx, pkgtype.DuckDBExtension, "kista_boot_core"); len(l) != 1 || l[0].Kind != store.ReservedCore ||
				!slices.Equal(l[0].Sources, []string{"version:v1.4.2@abcdef0"}) {
				t.Fatalf("reserved: %+v", l)
			}
			// another C API of a major the version has: refused
			must(t, func() error { return bootstrap.Rearm(ctx, en.d, authz.OSActor()) })
			b.Versions[0].CAPIs = []string{"v1.3.0"}
			if _, err := bootstrap.Run(ctx, en.d, b, cli); !errors.Is(err, bootstrap.ErrRefused) || !strings.Contains(err.Error(), "C API v1.3.0") {
				t.Fatalf("another C API: %v", err)
			}
			// a new version without its core extensions: refused
			b.Versions[0].CAPIs = []string{"v1.2.0"}
			b.Versions = append(b.Versions, config.BootstrapVersion{Version: "v1.4.3", Kind: "release"})
			if _, err := bootstrap.Run(ctx, en.d, b, cli); !errors.Is(err, bootstrap.ErrRefused) || !strings.Contains(err.Error(), "core_extensions") {
				t.Fatalf("a version without core extensions: %v", err)
			}
		})
	}
}

// A replica waits for the lease's holder, and gives up in time; a dry run takes no lease and
// writes nothing; serve notes a changed section once.
func TestBootstrapLease(t *testing.T) {
	en := newEnv(t, storetest.Engines(t)[0])
	if ok, err := en.st.AcquireLease(ctx, store.BootstrapLease, "other", time.Hour); err != nil || !ok {
		t.Fatal(err)
	}
	o := serveOpts
	o.Wait = 3 * time.Second
	if _, err := bootstrap.Run(ctx, en.d, section(), o); err == nil || !strings.Contains(err.Error(), "did not finish in time") {
		t.Fatalf("waiting: %v", err)
	}
	start := time.Now()
	res, err := bootstrap.Run(ctx, en.d, section(), bootstrap.Options{Actor: authz.Bootstrap, DryRun: true})
	if err != nil || len(res.Plan.Items) == 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("a dry run under another's lease: %+v %v", res, err)
	}
	if ts, _ := en.st.ListTenants(ctx); len(ts) != 0 {
		t.Fatal("a dry run wrote")
	}
	must(t, func() error { return en.st.ReleaseLease(ctx, store.BootstrapLease, "other") })
	if _, err := bootstrap.Run(ctx, en.d, section(), serveOpts); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	en.d.Log = slog.New(slog.NewTextHandler(&logs, nil))
	b := section()
	b.Tenant.DisplayName = "Acme Corp"
	for range 2 {
		if res, err := bootstrap.Run(ctx, en.d, b, serveOpts); err != nil || !res.Changed {
			t.Fatalf("changed: %+v %v", res, err)
		}
	}
	if n := strings.Count(logs.String(), "the section changed"); n != 1 {
		t.Fatalf("warned %d times", n)
	}
	// the CLI names what changed
	if _, err := bootstrap.Run(ctx, en.d, b, bootstrap.Options{Actor: authz.OSActor(), Holder: "cli"}); err == nil || !strings.Contains(err.Error(), "changed: tenant") {
		t.Fatalf("the CLI: %v", err)
	}
}
