package audit_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/fs"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/symbols/symtest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

var (
	ctx   = context.Background()
	admin = authz.Actor{Kind: authz.ActorOS, ID: "1:test"}
)

const idpURL = "https://idp.example"

type fakeIDP struct{ key *rsa.PrivateKey }

func (f *fakeIDP) Get(_ context.Context, url string) ([]byte, http.Header, error) {
	switch url {
	case idpURL + "/.well-known/openid-configuration":
		return []byte(`{"issuer":"` + idpURL + `","jwks_uri":"` + idpURL + `/jwks"}`), http.Header{}, nil
	case idpURL + "/jwks":
		b, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k1",
			"n": b64u(f.key.N.Bytes()), "e": b64u(big.NewInt(int64(f.key.E)).Bytes())}}})
		return b, http.Header{}, nil
	}
	return nil, nil, errors.New("not found")
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// writes is the registry of the services' exported methods (spec 0010): each writing method with
// the kinds of the events it emits, every other one marked as reading. A new method fails the test
// until it is listed.
var writes = map[string]map[string][]audit.Kind{
	"tenants.Service": {
		"CreateTenant": {"tenant.create"}, "SetTenantState": {"tenant.suspend", "tenant.resume"},
		"AddVersion": {"version.add"}, "AddVersionCAPI": {"version.c_apis"}, "CreateChannel": {"channel.create"},
		"SetChannelVersions": {"channel.versions"},
		"ListTenants":        nil, "GetTenant": nil, "ListVersions": nil, "ListChannels": nil, "ChannelVersions": nil,
	},
	"tenants.AuthAdmin": {
		"AddIssuer": {"issuer.add"}, "RemoveIssuer": {"issuer.remove"}, "AddAudience": {"audience.add"},
		"RemoveAudience": {"audience.remove"}, "AddGrant": {"grant.add"}, "RemoveGrant": {"grant.remove"},
		"AddPublisher": {"publisher.add"}, "RemovePublisher": {"publisher.remove"},
		"AddGitHubCredential": {"publisher.github.add"}, "RemoveGitHubCredential": {"publisher.github.remove"},
		"AddAPIKey": {"publisher.key.add"}, "RemoveAPIKey": {"publisher.key.remove"},
		"ListIssuers": nil, "ListAudiences": nil, "ListGrants": nil, "ListPublishers": nil, "ListAPIKeys": nil,
	},
	"keys.Service": {
		"Add": {"key.add"}, "Activate": {"key.activate"}, "Retire": {"key.retire"},
		"List": nil, "Events": nil, "PublicWellKnown": nil, "WellKnownOf": nil, "WellKnown": nil, "OpenSigner": nil,
	},
	"release.Service": {
		"Add": {"release.add", "release.publish", "shadow.add"}, "Promote": {"release.promote", "shadow.add"}, "Apply": {"release.yank",
			"release.deprecate", "release.activate", "release.current", "release.public", "release.private"},
		"Block": {"block.add"}, "Unblock": {"block.remove"}, "Ingest": {"upstream.release"}, "Resign": {"key.resign"},
		"Purge": {"release.purge"}, "List": nil, "ListBlocks": nil, "GetBlock": nil, "DuckDBCore": nil,
	},
	"upstream.Service": {
		"Add": {"upstream.add"}, "Remove": {"upstream.remove"}, "Set": {"upstream.change"}, "PutEntry": {"upstream.change"},
		"RemoveEntry": {"upstream.change"}, "AddPlatform": {"upstream.change"}, "RemovePlatform": {"upstream.change"},
		"AddKey": {"upstream.change"}, "RemoveKey": {"upstream.change"}, "RemoveShadow": {"shadow.remove"},
		"SetCredential": {"upstream.change"},
		"RunUpstream":   {"upstream.run", "upstream.release", "upstream.rejected"},
		"Sync":          nil, "List": nil, "Get": nil, "Cells": nil, "WithAuthz": nil, "Shadows": nil,
	},
}

// TestRegistry fails when a service gains an exported method the registry does not list.
func TestRegistry(t *testing.T) {
	for name, v := range map[string]any{"tenants.Service": &tenants.Service{}, "tenants.AuthAdmin": &tenants.AuthAdmin{},
		"keys.Service": &keys.Service{}, "release.Service": &release.Service{}, "upstream.Service": &upstream.Service{}} {
		typ := reflect.TypeOf(v)
		for i := range typ.NumMethod() {
			m := typ.Method(i).Name
			if _, ok := writes[name][m]; !ok {
				t.Errorf("%s.%s is not in the events registry: list it with its event kinds, or nil if it only reads", name, m)
			}
		}
	}
	for _, ms := range writes {
		for _, kinds := range ms {
			for _, k := range kinds {
				if !audit.Known(k) {
					t.Errorf("%s is not in the catalogue", k)
				}
			}
		}
	}
}

type env struct {
	hit map[string]bool // registry methods run
	st  *store.Store
	ten *tenants.Service
	adm *tenants.AuthAdmin
	ks  *keys.Service
	rel *release.Service
	up  *upstream.Service
	dir string
	t   *testing.T
}

// expect runs an operation and checks it wrote exactly one event per wanted kind, in any order.
func (en *env) expect(name string, op func() error, want ...audit.Kind) {
	en.t.Helper()
	en.hit[name] = true
	before := en.count()
	if err := op(); err != nil {
		en.t.Fatalf("%s: %v", name, err)
	}
	evs := en.newest(en.count() - before)
	if len(evs) != len(want) {
		en.t.Fatalf("%s wrote %d events, want %v: %+v", name, len(evs), want, evs)
	}
	got := map[string]int{}
	for _, e := range evs {
		got[e.Kind]++
		if e.Actor == "" || e.Subject == "" || e.Outcome != audit.OK || e.Request == "" {
			en.t.Errorf("%s: an incomplete event %+v", name, e)
		}
	}
	for _, k := range want {
		if got[string(k)] == 0 {
			en.t.Errorf("%s: no %s event: %+v", name, k, evs)
		}
		got[string(k)]--
	}
}

func (en *env) all() []store.Event {
	var out []store.Event
	for _, tenant := range []string{audit.ServerTenant, en.tenantID()} {
		evs, err := en.st.ListEvents(ctx, tenant, store.EventFilter{Limit: 100000})
		if err != nil {
			en.t.Fatal(err)
		}
		out = append(out, evs...)
	}
	return out
}

func (en *env) tenantID() string {
	t, err := en.st.GetTenant(ctx, "acme")
	if err != nil {
		return "none"
	}
	return t.ID
}

func (en *env) count() int { return len(en.all()) }

func (en *env) newest(n int) []store.Event {
	evs := en.all()
	slicesSortByAt(evs)
	if n <= 0 {
		return nil
	}
	return evs[len(evs)-n:]
}

func slicesSortByAt(evs []store.Event) {
	for i := 1; i < len(evs); i++ {
		for j := i; j > 0 && (evs[j].At.Before(evs[j-1].At) || evs[j].At.Equal(evs[j-1].At) && evs[j].ID < evs[j-1].ID); j-- {
			evs[j], evs[j-1] = evs[j-1], evs[j]
		}
	}
}

func ext(t *testing.T, key *rsa.PrivateKey, name, ver string) []byte {
	t.Helper()
	block, err := extfile.EncodeMetadata(extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: ver, ABI: extfile.ABICPP})
	if err != nil {
		t.Fatal(err)
	}
	b := append(symtest.ELF(name+"_duckdb_cpp_init"), []byte(ver)...)
	b = append(b, extfile.MetadataPrefix...)
	b = append(b, block[:]...)
	sig := make([]byte, extfile.SignatureSize)
	if key != nil {
		h, _, _ := extfile.HashBody(bytes.NewReader(b), 1<<30)
		sig, _ = rsa.SignPKCS1v15(nil, key, crypto.SHA256, h[:])
	}
	return append(b, sig...)
}

// TestCoverage runs every writing operation once and checks its events.
func TestCoverage(t *testing.T) {
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	n := 0
	st.Now = func() time.Time { n++; return base.Add(time.Duration(n) * time.Millisecond) } // distinct, ordered times
	dir := t.TempDir()
	ks := &keys.Service{Store: st, Signers: signer.Resolver{FileDir: dir, AllowFile: true}, Authz: authz.ServerAdmin{}}
	fsStore, _ := fs.Open(filepath.Join(t.TempDir(), "blobs"))
	bs, err := blob.NewService(ctx, st, []blob.Domain{{Name: "default", Kind: "fs", Store: fsStore}},
		blob.Options{SpoolDir: filepath.Join(t.TempDir(), "spool"), MaxBody: 1 << 20, MaxIngests: 4, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bs.Close() })
	idpKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	rel := &release.Service{Store: st, Blob: bs, Signers: ks, Authz: authz.ServerAdmin{}}
	en := &env{st: st, ks: ks, dir: dir, t: t, rel: rel, hit: map[string]bool{},
		ten: &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }},
		adm: &tenants.AuthAdmin{Store: st, Authz: authz.ServerAdmin{}, Fetch: &fakeIDP{idpKey}, PublicURL: "https://kista.example",
			Providers: auth.Providers{{Name: "github", URL: "https://token.actions.githubusercontent.com"}}},
		up: &upstream.Service{Store: st, Releases: rel, Blob: bs, Authz: authz.ServerAdmin{}, MaxBody: 1 << 20, MaxIngests: 4, Fetch: repoFetch{},
			Log: slog.New(slog.DiscardHandler)},
	}
	rctx := audit.WithRequest(ctx, audit.Request{Client: "198.51.100.9", ID: "r1"})
	do := func(f func() error) func() error { return f }

	en.expect("tenants.Service.CreateTenant", do(func() error { _, err := en.ten.CreateTenant(rctx, admin, "acme", "Acme", ""); return err }), "tenant.create")
	en.expect("tenants.Service.AddVersion", do(func() error {
		_, err := en.ten.AddVersion(rctx, admin, "v2.0.0", "release", []string{"v1.5.6"})
		return err
	}), "version.add")
	en.expect("tenants.Service.AddVersionCAPI", do(func() error { _, err := en.ten.AddVersionCAPI(rctx, admin, "v2.0.0", "v2.0.0"); return err }), "version.c_apis")
	for _, ch := range []string{"prod", "staging"} {
		en.expect("tenants.Service.CreateChannel", do(func() error { _, err := en.ten.CreateChannel(rctx, admin, "acme", ch, store.ChannelSigned); return err }),
			"channel.create")
		en.expect("tenants.Service.SetChannelVersions", do(func() error {
			_, err := en.ten.SetChannelVersions(rctx, admin, "acme", ch, []string{"v2.0.0"}, nil)
			return err
		}), "channel.versions")
	}
	en.expect("tenants.Service.SetTenantState", do(func() error {
		_, err := en.ten.SetTenantState(rctx, admin, "acme", store.TenantSuspended, 0)
		return err
	}),
		"tenant.suspend")
	en.expect("tenants.Service.SetTenantState", do(func() error { _, err := en.ten.SetTenantState(rctx, admin, "acme", store.TenantActive, 0); return err }),
		"tenant.resume")
	// keys
	var keyIDs []string
	for _, f := range []string{"a.pem", "b.pem", "s.pem"} {
		if err := signer.GenerateKeyFile(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	en.expect("keys.Service.Add", do(func() error {
		k, err := en.ks.Add(rctx, admin, "acme", "prod", "file:a.pem", true)
		keyIDs = append(keyIDs, k.ID)
		return err
	}), "key.add")
	en.expect("keys.Service.Add", do(func() error { _, err := en.ks.Add(rctx, admin, "acme", "staging", "file:s.pem", true); return err }), "key.add")
	en.expect("keys.Service.Add", do(func() error {
		k, err := en.ks.Add(rctx, admin, "acme", "prod", "file:b.pem", false)
		keyIDs = append(keyIDs, k.ID)
		return err
	}), "key.add")
	// identity
	en.expect("tenants.AuthAdmin.AddIssuer", do(func() error {
		_, err := en.adm.AddIssuer(rctx, admin, "acme", store.Issuer{Name: "gone", URL: idpURL})
		return err
	}), "issuer.add")
	en.expect("tenants.AuthAdmin.RemoveIssuer", do(func() error { return en.adm.RemoveIssuer(rctx, admin, "acme", "gone", "") }), "issuer.remove")
	en.expect("tenants.AuthAdmin.AddIssuer", do(func() error {
		_, err := en.adm.AddIssuer(rctx, admin, "acme", store.Issuer{Name: "corp", URL: idpURL, RequiredClaims: map[string]string{"tid": "t1"}})
		return err
	}), "issuer.add")
	en.expect("tenants.AuthAdmin.AddAudience", do(func() error { return en.adm.AddAudience(rctx, admin, "acme", "api://acme") }), "audience.add")
	en.expect("tenants.AuthAdmin.RemoveAudience", do(func() error { return en.adm.RemoveAudience(rctx, admin, "acme", "api://acme") }), "audience.remove")
	var grant string
	en.expect("tenants.AuthAdmin.AddGrant", do(func() error {
		g, err := en.adm.AddGrant(rctx, admin, "acme", "subject:corp|alice", []string{"audit"}, "", "")
		grant = g.ID
		return err
	}), "grant.add")
	en.expect("tenants.AuthAdmin.RemoveGrant", do(func() error { return en.adm.RemoveGrant(rctx, admin, "acme", grant) }), "grant.remove")
	en.expect("tenants.AuthAdmin.AddPublisher", do(func() error { _, err := en.adm.AddPublisher(rctx, admin, "acme", "ci"); return err }), "publisher.add")
	var cred, apiKey string
	en.expect("tenants.AuthAdmin.AddGitHubCredential", do(func() error {
		c, err := en.adm.AddGitHubCredential(rctx, admin, "acme", "ci", store.GitHubCredential{OwnerID: "1", RepositoryID: "2", Workflow: "o/r/.github/workflows/w.yml"})
		cred = c.ID
		return err
	}), "publisher.github.add")
	en.expect("tenants.AuthAdmin.RemoveGitHubCredential", do(func() error { return en.adm.RemoveGitHubCredential(rctx, admin, "acme", "ci", cred) }), "publisher.github.remove")
	en.expect("tenants.AuthAdmin.AddAPIKey", do(func() error {
		k, _, err := en.adm.AddAPIKey(rctx, admin, "acme", "ci", time.Now().Add(24*time.Hour))
		apiKey = k.ID
		return err
	}), "publisher.key.add")
	en.expect("tenants.AuthAdmin.RemoveAPIKey", do(func() error { return en.adm.RemoveAPIKey(rctx, admin, "acme", "ci", apiKey) }), "publisher.key.remove")
	en.expect("tenants.AuthAdmin.RemovePublisher", do(func() error { return en.adm.RemovePublisher(rctx, admin, "acme", "ci") }), "publisher.remove")
	// releases
	var relID string
	en.expect("release.Service.Add", do(func() error {
		r, _, err := en.rel.Add(rctx, admin, "acme", "staging", bytes.NewReader(ext(t, nil, "tresor", "1.0")), release.AddOptions{Name: "tresor"})
		relID = r.ID
		return err
	}), "release.add")
	en.expect("release.Service.Add", do(func() error {
		_, _, err := en.rel.Add(rctx, admin, "acme", "staging", bytes.NewReader(ext(t, nil, "acl", "1.0")),
			release.AddOptions{Name: "acl", Publish: &release.Publication{Version: "1.0", Platform: "linux_amd64", Provenance: `{"run":"7"}`}})
		return err
	}), "release.publish")
	en.expect("release.Service.Promote", do(func() error {
		_, _, err := en.rel.Promote(rctx, admin, "acme", "prod", "tresor", release.PromoteOptions{From: "staging", Version: "1.0"})
		return err
	}), "release.promote")
	for _, c := range []release.Change{release.SetPrivate, release.SetPublic, release.MakeCurrent, release.Deprecate, release.Activate, release.Yank} {
		kind := map[release.Change]audit.Kind{release.SetPrivate: "release.private", release.SetPublic: "release.public",
			release.MakeCurrent: "release.current", release.Deprecate: "release.deprecate", release.Activate: "release.activate",
			release.Yank: "release.yank"}[c]
		en.expect("release.Service.Apply", do(func() error {
			_, err := en.rel.Apply(rctx, admin, "acme", "staging", "tresor", relID, c, 0)
			return err
		}), kind)
	}
	en.expect("release.Service.Purge", do(func() error {
		_, err := en.rel.Purge(rctx, admin, "acme", "staging", "tresor", relID, 0) // yanked just above
		return err
	}), "release.purge")
	body := ext(t, nil, "blocked", "1.0")
	h, _, _ := extfile.HashBody(bytes.NewReader(body[:len(body)-extfile.SignatureSize]), 1<<30)
	en.expect("release.Service.Block", do(func() error { _, _, err := en.rel.Block(rctx, admin, "acme", h.String(), "CVE"); return err }), "block.add")
	en.expect("release.Service.Unblock", do(func() error { return en.rel.Unblock(rctx, admin, "acme", h.String()) }), "block.remove")
	// key rotation: activate, re-sign (the serving key moves), retire the old one
	en.ks.MinTrusted, en.ks.MinDemoted = 0, 0
	en.expect("keys.Service.Activate", do(func() error { _, err := en.ks.Activate(rctx, admin, "acme", "prod", keyIDs[1], 0, false); return err }), "key.activate")
	prod, _ := st.GetChannel(ctx, "acme", "prod")
	en.expect("release.Service.Resign", do(func() error { _, _, err := en.rel.Resign(rctx, prod.ID, nil); return err }), "key.resign")
	en.expect("keys.Service.Retire", do(func() error { _, err := en.ks.Retire(rctx, admin, "acme", "prod", keyIDs[0], 0, false); return err }), "key.retire")
	// a core name replaced records a shadow with the release
	en.expect("release.Service.Add", do(func() error {
		_, _, err := en.rel.Add(rctx, admin, "acme", "prod", bytes.NewReader(ext(t, nil, "json", "9.0")), release.AddOptions{Name: "json"})
		return err
	}), "release.add", "shadow.add")
	en.expect("upstream.Service.RemoveShadow", do(func() error { return en.up.RemoveShadow(rctx, admin, "acme", "json") }), "shadow.remove")
	// upstreams
	en.expect("upstream.Service.Add", do(func() error {
		_, err := en.up.Add(rctx, admin, "acme", upstream.Spec{Name: "core", Kind: store.UpstreamCore, Channel: "staging",
			Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "parquet"}}})
		return err
	}), "upstream.add")
	for _, c := range []struct {
		name string
		op   func() error
	}{
		{"PutEntry", func() error {
			_, err := en.up.PutEntry(rctx, admin, "acme", "core", store.UpstreamEntry{Name: "inet"})
			return err
		}},
		{"RemoveEntry", func() error { return en.up.RemoveEntry(rctx, admin, "acme", "core", "inet") }},
		{"AddPlatform", func() error { return en.up.AddPlatform(rctx, admin, "acme", "core", "osx_arm64") }},
		{"RemovePlatform", func() error { return en.up.RemovePlatform(rctx, admin, "acme", "core", "osx_arm64") }},
		{"Set", func() error { _, err := en.up.Set(rctx, admin, "acme", "core", store.Public, "", 0); return err }},
	} {
		en.expect("upstream.Service."+c.name, c.op, "upstream.change")
	}
	en.expect("upstream.Service.Add", do(func() error {
		_, err := en.up.Add(rctx, admin, "acme", upstream.Spec{Name: "repo", Kind: store.UpstreamRepo, Prefix: "https://repo.example",
			Channel: "prod", Keys: []string{repoFingerprint}, Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "zzz"}}})
		return err
	}), "upstream.add")
	other := "sha256:" + strings.Repeat("ab", 32)
	en.expect("upstream.Service.AddKey", do(func() error { return en.up.AddKey(rctx, admin, "acme", "repo", other) }), "upstream.change")
	en.expect("upstream.Service.RemoveKey", do(func() error { return en.up.RemoveKey(rctx, admin, "acme", "repo", other) }), "upstream.change")
	creds, err := credential.New([]config.Credential{{Name: "repo-token", Tenants: []string{"acme"}, Prefixes: []string{"https://repo.example"},
		Kind: "token_file", TokenFile: "/nonexistent"}}, credential.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	en.up.Credentials = creds
	en.expect("upstream.Service.SetCredential", do(func() error {
		_, err := en.up.SetCredential(rctx, admin, "acme", "repo", "repo-token", 0)
		return err
	}), "upstream.change")
	en.expect("upstream.Service.Remove", do(func() error { return en.up.Remove(rctx, admin, "acme", "core", 0) }), "upstream.remove")
	// every registered writer ran (Ingest and RunUpstream: in internal/upstream's tests, which
	// check their events)
	elsewhere := map[string]bool{"release.Service.Ingest": true, "upstream.Service.RunUpstream": true}
	for svc, ms := range writes {
		for m, kinds := range ms {
			if kinds != nil && !en.hit[svc+"."+m] && !elsewhere[svc+"."+m] {
				t.Errorf("%s.%s is registered as writing but never run here", svc, m)
			}
		}
	}
	// the request's facts reach the events; no secret field does
	for _, e := range en.all() {
		if e.Request != "r1" && e.Actor != "system:resign" {
			t.Errorf("%s: request %q", e.Kind, e.Request)
		}
		if e.Kind == "key.add" && bytes.Contains([]byte(e.Data), []byte("a.pem")) {
			t.Errorf("a signer reference in an event: %s", e.Data)
		}
		if e.Actor != "system:resign" && e.Client != "198.51.100.0/24" {
			t.Errorf("%s: client %q", e.Kind, e.Client)
		}
	}
}

// repoFetch serves a repository upstream's .well-known file with one key.
type repoFetch struct{}

var repoKey, _ = rsa.GenerateKey(rand.Reader, 2048)

var repoFingerprint = extfile.Fingerprint(&repoKey.PublicKey)

func (repoFetch) Get(_ context.Context, url string) ([]byte, http.Header, error) {
	der, _ := x509.MarshalPKIXPublicKey(&repoKey.PublicKey)
	b, _ := json.Marshal(map[string]any{"signature_keys": []string{base64.StdEncoding.EncodeToString(der)}})
	return b, http.Header{}, nil
}

func (repoFetch) Download(context.Context, string, egress.DownloadOptions, io.Writer) (egress.Download, error) {
	return egress.Download{}, egress.ErrNotFound
}

func (repoFetch) PortAllowed(uint16) bool { return true }
