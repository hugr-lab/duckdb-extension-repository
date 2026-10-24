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
	"fmt"
	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
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
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/fs"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
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
		"CreateTenant": {"tenant.create"}, "SetTenantState": {"tenant.suspend", "tenant.resume"}, "SetTenantHolder": {"tenant.holder"}, "Delete": {"tenant.delete"}, "Show": nil,
		"AddVersion": {"version.add"}, "AddVersionCAPI": {"version.c_apis"}, "AddVersionCore": {"reserved.add"}, "CreateChannel": {"channel.create"},
		"SetChannelVersions": {"channel.versions"}, "Reserve": {"reserved.add"}, "Unreserve": {"reserved.remove"}, "ListReserved": nil,
		"ReserveInTenant": {"reserved.tenant.add"}, "ReleaseInTenant": {"reserved.tenant.remove"}, "ListInTenant": nil,
		"ListTenants": nil, "GetTenant": nil, "ListVersions": nil, "ListChannels": nil, "ChannelVersions": nil,
	},
	"tenants.AuthAdmin": {
		"AddIssuer": {"issuer.add"}, "RemoveIssuer": {"issuer.remove"}, "AddAudience": {"audience.add"},
		"RemoveAudience": {"audience.remove"}, "AddGrant": {"grant.add"}, "RemoveGrant": {"grant.remove"},
		"AddPublisher": {"publisher.add"}, "RemovePublisher": {"publisher.remove"}, "SetPublisherLicenseKeys": {"publisher.change"},
		"AddPublisherKey": {"publisher.key.add"}, "RemovePublisherKey": {"publisher.key.remove"},
		"SetConsoleClient": {"issuer.console.set"}, "RemoveConsoleClient": {"issuer.console.remove"}, "ConsoleIssuers": nil,
		"ListIssuers": nil, "ListAudiences": nil, "ListGrants": nil, "ListPublishers": nil, "ListPublisherKeys": nil, "TenantPublisherKeys": nil,
		"CreateExtensionGroup": {"extension_group.add"}, "SetExtensionGroupDescription": {"extension_group.change"},
		"ChangeExtensionGroupMembers": {"extension_group.members"},
		"RemoveExtensionGroup":        {"extension_group.members", "extension_group.remove"},
		"ListExtensionGroups":         nil, "GetExtensionGroup": nil, "ExtensionGroupMembers": nil,
	},
	"keys.Service": {
		"Add": {"key.add"}, "Activate": {"key.activate"}, "Retire": {"key.retire"},
		"List": nil, "Events": nil, "PublicWellKnown": nil, "WellKnownOf": nil, "WellKnown": nil, "OpenSigner": nil,
		"AddAttestation": {"attestation_key.add"}, "ActivateAttestation": {"attestation_key.activate"},
		"RetireAttestation": {"attestation_key.retire"}, "ListAttestation": nil, "PublicAttestation": nil, "OpenAttestationSigner": nil,
	},
	"release.Service": {
		"Add": {"release.add", "release.publish", "shadow.add", "source.set"}, "Promote": {"release.promote", "shadow.add", "source.set"},
		"SetSource": {"source.set"}, "ClearSource": {"source.clear"}, "Sources": nil, "Source": nil, "Apply": {"release.yank",
			"release.deprecate", "release.activate", "release.current", "release.public", "release.private"},
		"Block": {"block.add"}, "Unblock": {"block.remove"}, "Ingest": {"upstream.release", "source.set"}, "Resign": {"key.resign"},
		"Purge": {"release.purge"}, "List": nil, "ListBlocks": nil, "GetBlock": nil, "DuckDBCore": nil,
		"Place": {"release.place", "shadow.add", "source.set"}, "PlaceRule": {"release.place", "shadow.add"}, "Catalogue": nil,
		"CatalogueVersions": nil,
	},
	"upstream.Service": {
		"Add": {"upstream.add"}, "Remove": {"upstream.remove", "license.remove"}, "Set": {"upstream.change"}, "PutEntry": {"upstream.change"},
		"RemoveEntry": {"upstream.change"}, "AddPlatform": {"upstream.change"}, "RemovePlatform": {"upstream.change"},
		"AddKey": {"upstream.change"}, "RemoveKey": {"upstream.change"}, "RemoveShadow": {"shadow.remove"},
		"SetCredential": {"upstream.change"},
		"SetUsage":      {"upstream.change"},
		"SetFetchMode":  {"upstream.change"},
		"RunUpstream":   {"upstream.run", "upstream.release", "upstream.rejected"},
		"Import":        {"bundle.import", "upstream.run", "upstream.release", "upstream.rejected", "feed.run"},
		"ResetBundle":   {"bundle.reset"}, "ResetFeedBundle": {"bundle.reset"}, "Imports": nil,
		"CreateUpload": {"bundle.upload"}, "CompleteUpload": {"bundle.upload_complete"}, "AbortUpload": {"bundle.upload_abort"},
		"PutUploadChunk": nil, "GetUpload": nil, "ListUploads": nil, "UploadExpiresAt": nil, "WakeUploads": nil, "GetImport": nil, "Export": {"bundle.export"},
		"Sync": nil, "List": nil, "Get": nil, "Cells": nil, "WithAuthz": nil, "Shadows": nil, "Plan": nil,
		"CheckAddAll": nil, "CheckSource": nil, // read only (spec 0024): the bootstrap's check
	},
	"attachment.Service": {
		"Attach": {"attachment.add"}, "SetFlags": {"attachment.change"}, "Remove": {"attachment.remove"},
		"RemoveBlockedDocument": {"attachment.remove"},
		"List":                  nil, "Get": nil, "Content": nil, "ReadContent": nil, "BlockedDocuments": nil,
		"Feed": nil, "FeedDocument": nil,
	},
	"licenses.Service": {
		"Import": {"license.import"}, "Remove": {"license.remove"}, "SetVisibility": {"license.change"},
		"RunUpstream": {"license.sync", "license.remove"}, "ExpireOnce": {"license.remove"},
		"Sync": nil, "List": nil, "Get": nil, "Token": nil, "LastSync": nil, "LicenseKeys": nil, "Serve": nil, "WithAuthz": nil,
		"Exportable": nil, "ApplyBundle": nil, // a bundle's licences are recorded by the import's bundle.import
	},
	"attachment.Feeds": {
		"Add": {"feed.add"}, "Change": {"feed.change"}, "Remove": {"feed.remove"}, "RunFeed": {"feed.run"},
		"List": nil, "Get": nil, "Sync": nil, "ImportFeed": nil, // the import's run is recorded by Import
	},
}

// publisherKeyLine is a public key (spec 0023's vectors).
const publisherKeyLine = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIvmAKxjbOkVN7MhsYLLHONcPMZXvhAR+JPGbBHEmWgz ci ed25519"

// TestRegistry fails when a service gains an exported method the registry does not list.
func TestRegistry(t *testing.T) {
	for name, v := range map[string]any{"tenants.Service": &tenants.Service{}, "tenants.AuthAdmin": &tenants.AuthAdmin{},
		"keys.Service": &keys.Service{}, "release.Service": &release.Service{}, "upstream.Service": &upstream.Service{},
		"attachment.Service": &attachment.Service{}, "attachment.Feeds": &attachment.Feeds{}, "licenses.Service": &licenses.Service{}} {
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
	ops []namedOp       // every operation run, to run again on a deleting tenant
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
	en.ops = append(en.ops, namedOp{name, op})
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
	t, err := en.st.GetTenantAny(ctx, "acme")
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
	ks := &keys.Service{Store: st, Signers: keysource.NewRegistry(signer.Resolver{FileDir: dir, AllowFile: true}), Authz: authz.ServerAdmin{}}
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
		adm: &tenants.AuthAdmin{Store: st, Authz: authz.ServerAdmin{}, Fetch: &fakeIDP{idpKey}, PublicURL: "https://kista.example"},
		up: &upstream.Service{Store: st, Releases: rel, Blob: bs, Authz: authz.ServerAdmin{}, MaxBody: 1 << 20, MaxIngests: 4, Fetch: repoFetch{},
			Log: slog.New(slog.DiscardHandler)},
	}
	rctx := audit.WithRequest(ctx, audit.Request{Client: "198.51.100.9", ID: "r1"})
	do := func(f func() error) func() error { return f }

	en.expect("tenants.Service.CreateTenant", do(func() error { _, err := en.ten.CreateTenant(rctx, admin, "acme", "Acme", "", ""); return err }), "tenant.create")
	en.expect("tenants.Service.AddVersion", do(func() error {
		_, _, err := en.ten.AddVersion(rctx, admin, "v2.0.0", "release", []string{"v1.5.6"}, tenants.CoreExtensions{Names: []string{"json"}})
		return err
	}), "version.add")
	en.expect("tenants.Service.AddVersionCAPI", do(func() error { _, err := en.ten.AddVersionCAPI(rctx, admin, "v2.0.0", "v2.0.0"); return err }), "version.c_apis")
	en.expect("tenants.Service.Reserve", do(func() error {
		_, err := en.ten.Reserve(rctx, admin, []string{"kista_audit_admin"}, store.ReservedCommunity, false)
		return err
	}), "reserved.add")
	en.expect("tenants.Service.Unreserve", do(func() error { return en.ten.Unreserve(rctx, admin, "kista_audit_admin") }), "reserved.remove")
	en.expect("tenants.Service.AddVersionCore", do(func() error {
		_, err := en.ten.AddVersionCore(rctx, admin, "v2.0.0", tenants.CoreExtensions{Names: []string{"kista_audit_core"}})
		return err
	}), "reserved.add")
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
	// spec 0028: set, then cleared (the licences below are of no particular holder)
	for _, h := range []string{"org-1", ""} {
		en.expect("tenants.Service.SetTenantHolder", do(func() error { _, err := en.ten.SetTenantHolder(rctx, admin, "acme", h, 0); return err }),
			"tenant.holder")
	}
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
	en.expect("tenants.AuthAdmin.SetConsoleClient", do(func() error {
		_, err := en.adm.SetConsoleClient(rctx, admin, "acme", "corp", "", store.ConsoleClient{ClientID: "kista-console",
			Scopes: []string{"openid"}, Audience: "api://acme"})
		return err
	}), "issuer.console.set")
	en.expect("tenants.AuthAdmin.RemoveConsoleClient", do(func() error { return en.adm.RemoveConsoleClient(rctx, admin, "acme", "corp", "") }),
		"issuer.console.remove")
	en.expect("tenants.AuthAdmin.RemoveAudience", do(func() error { return en.adm.RemoveAudience(rctx, admin, "acme", "api://acme") }), "audience.remove")
	var grant string
	en.expect("tenants.AuthAdmin.AddGrant", do(func() error {
		g, err := en.adm.AddGrant(rctx, admin, "acme", "subject:corp|alice", []string{"audit"}, tenants.GrantOn{})
		grant = g.ID
		return err
	}), "grant.add")
	en.expect("tenants.AuthAdmin.RemoveGrant", do(func() error { return en.adm.RemoveGrant(rctx, admin, "acme", grant) }), "grant.remove")
	// extension groups (spec 0017)
	en.expect("tenants.AuthAdmin.CreateExtensionGroup", do(func() error {
		_, err := en.adm.CreateExtensionGroup(rctx, admin, "acme", "platform", "hugr")
		return err
	}), "extension_group.add")
	en.expect("tenants.AuthAdmin.SetExtensionGroupDescription", do(func() error {
		_, err := en.adm.SetExtensionGroupDescription(rctx, admin, "acme", "platform", "the hugr platform", 0)
		return err
	}), "extension_group.change")
	en.expect("tenants.AuthAdmin.ChangeExtensionGroupMembers", do(func() error {
		_, _, err := en.adm.ChangeExtensionGroupMembers(rctx, admin, "acme", "platform", []string{"tresor", "acl"}, nil)
		return err
	}), "extension_group.members")
	en.expect("tenants.AuthAdmin.AddGrant", do(func() error {
		g, err := en.adm.AddGrant(rctx, admin, "acme", "subject:corp|alice", []string{"install"}, tenants.GrantOn{ExtensionGroup: "platform"})
		grant = g.ID
		return err
	}), "grant.add")
	en.expect("tenants.AuthAdmin.RemoveGrant", do(func() error { return en.adm.RemoveGrant(rctx, admin, "acme", grant) }), "grant.remove")
	en.expect("tenants.AuthAdmin.RemoveExtensionGroup", do(func() error { return en.adm.RemoveExtensionGroup(rctx, admin, "acme", "platform", 0) }),
		"extension_group.members", "extension_group.remove")
	en.expect("tenants.AuthAdmin.AddPublisher", do(func() error { _, err := en.adm.AddPublisher(rctx, admin, "acme", "ci"); return err }), "publisher.add")
	en.expect("tenants.Service.ReserveInTenant", do(func() error {
		_, err := en.ten.ReserveInTenant(rctx, admin, "acme", "kista_held", "enterest")
		return err
	}), "reserved.tenant.add")
	en.expect("tenants.Service.ReleaseInTenant", do(func() error { return en.ten.ReleaseInTenant(rctx, admin, "acme", "kista_held") }),
		"reserved.tenant.remove")
	var pubKey string
	en.expect("tenants.AuthAdmin.AddPublisherKey", do(func() error {
		k, err := en.adm.AddPublisherKey(rctx, admin, "acme", "ci", publisherKeyLine, time.Now().Add(24*time.Hour))
		pubKey = k.ID
		return err
	}), "publisher.key.add")
	en.expect("tenants.AuthAdmin.RemovePublisherKey", do(func() error { return en.adm.RemovePublisherKey(rctx, admin, "acme", "ci", pubKey) }), "publisher.key.remove")
	en.expect("tenants.AuthAdmin.SetPublisherLicenseKeys", do(func() error {
		_, err := en.adm.SetPublisherLicenseKeys(rctx, admin, "acme", "ci", true)
		return err
	}), "publisher.change")
	en.expect("tenants.AuthAdmin.RemovePublisher", do(func() error { return en.adm.RemovePublisher(rctx, admin, "acme", "ci") }), "publisher.remove")
	// releases
	var relID string
	en.expect("release.Service.Add", do(func() error {
		r, _, err := en.rel.Add(rctx, admin, "acme", "staging", bytes.NewReader(ext(t, nil, "tresor", "1.0")), release.AddOptions{Name: "tresor"})
		relID = r.ID
		return err
	}), "source.set", "release.add") // spec 0031: the first release of a name sets its source
	en.expect("release.Service.Add", do(func() error {
		_, _, err := en.rel.Add(rctx, admin, "acme", "staging", bytes.NewReader(ext(t, nil, "acl", "1.0")),
			release.AddOptions{Name: "acl", Publish: &release.Publication{Version: "1.0", Platform: "linux_amd64", Provenance: `{"run":"7"}`}})
		return err
	}), "source.set", "release.publish")
	en.expect("release.Service.Promote", do(func() error {
		_, _, err := en.rel.Promote(rctx, admin, "acme", "prod", "tresor", release.PromoteOptions{From: "staging", Version: "1.0"})
		return err
	}), "source.set", "release.promote")
	// spec 0033: a version placed by hand from the catalogue, then a newer one by the pass
	en.expect("release.Service.Place", do(func() error {
		_, _, err := en.rel.Place(rctx, admin, "acme", "prod", "acl", release.PlaceOptions{Source: "tenant", Version: "1.0"})
		return err
	}), "source.set", "release.place")
	en.expect("release.Service.Add", do(func() error {
		_, _, err := en.rel.Add(rctx, admin, "acme", "staging", bytes.NewReader(ext(t, nil, "acl", "2.0")), release.AddOptions{Name: "acl"})
		return err
	}), "release.add")
	en.expect("release.Service.PlaceRule", do(func() error {
		prod, err := st.GetChannel(ctx, "acme", "prod")
		if err != nil {
			return err
		}
		n, failure, err := en.rel.PlaceRule(rctx, store.DueRule{TenantID: prod.TenantID, ChannelID: prod.ID, Name: "acl", Source: "tenant", Mode: "auto"}, nil)
		if err == nil && (n != 1 || failure != "") {
			err = fmt.Errorf("placed %d: %s", n, failure)
		}
		return err
	}), "release.place")
	en.expect("release.Service.SetSource", do(func() error {
		_, err := en.rel.SetSource(rctx, admin, "acme", "prod", "parquet", "tenant", "")
		return err
	}), "source.set")
	en.expect("release.Service.ClearSource", do(func() error { return en.rel.ClearSource(rctx, admin, "acme", "prod", "parquet") }), "source.clear")
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
	}), "source.set", "release.add", "shadow.add")
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
		{"SetFetchMode", func() error {
			_, err := en.up.SetFetchMode(rctx, admin, "acme", "core", store.FetchLazy, 0)
			return err
		}},
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
	en.expect("upstream.Service.SetUsage", do(func() error {
		_, err := en.up.SetUsage(rctx, admin, "acme", "repo", store.UsageOff, 0)
		return err
	}), "upstream.change")
	en.expect("upstream.Service.Remove", do(func() error { return en.up.Remove(rctx, admin, "acme", "core", 0) }), "upstream.remove")
	// licences (spec 0012): an import, its visibility, its removal
	licKey, err := vectors.LoadKey("../license/testdata", vectors.TrustedKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.LicenseKey(ctx, st, en.tenantID(), "tresor", store.KeySourcePublisher, "p", &licKey.PublicKey); err != nil {
		t.Fatal(err)
	}
	lic := &licenses.Service{Store: st, Authz: authz.ServerAdmin{}, Enabled: true, Log: slog.New(slog.DiscardHandler)}
	licTok := vectors.Sign(licKey, vectors.Header(licKey), fmt.Sprintf(
		`{"v":1,"iss":"https://issuer.example","jti":"l1","sub":"acme","ext":"tresor","plan":"pro","iat":%d,"exp":%d}`,
		base.Unix()-60, base.Unix()+30*86400))
	en.expect("licenses.Service.Import", do(func() error { _, err := lic.Import(rctx, admin, "acme", licTok, licenses.Create); return err }),
		"license.import")
	en.expect("licenses.Service.SetVisibility", do(func() error {
		_, err := lic.SetVisibility(rctx, admin, "acme", "tresor", store.Public, 0)
		return err
	}), "license.change")
	en.expect("licenses.Service.Remove", do(func() error { return lic.Remove(rctx, admin, "acme", "tresor", 0) }), "license.remove")
	en.expect("upstream.Service.Add", do(func() error {
		_, err := en.up.Add(rctx, admin, "acme", upstream.Spec{Name: "offline", Kind: store.UpstreamCore, Mode: store.ModeBundle,
			Channel: "staging", Platforms: []string{"linux_amd64"}, Entries: []store.UpstreamEntry{{Name: "inet"}}})
		return err
	}), "upstream.add")
	en.expect("upstream.Service.ResetBundle", do(func() error { _, err := en.up.ResetBundle(rctx, admin, "acme", "offline"); return err }),
		"bundle.reset")
	// attachments (spec 0011): on acl's body, then on it blocked
	att := &attachment.Service{Store: st, Blob: bs, Authz: authz.ServerAdmin{}}
	aclBody := ext(t, nil, "acl", "1.0")
	ah, _, _ := extfile.HashBody(bytes.NewReader(aclBody[:len(aclBody)-extfile.SignatureSize]), 1<<30)
	notes := func() (store.Attachment, error) {
		return att.Attach(rctx, admin, "acme", "acl", attachment.AttachInput{BodyHash: ah.String(), Kind: store.KindNotes,
			MediaType: attachment.Markdown, Title: "Notes", Content: []byte("# checked")})
	}
	var doc store.Attachment
	en.expect("attachment.Service.Attach", do(func() (err error) { doc, err = notes(); return err }), "attachment.add")
	readers := store.VisibleReaders
	en.expect("attachment.Service.SetFlags", do(func() error {
		_, err := att.SetFlags(rctx, admin, "acme", "acl", doc.ID, &readers, nil, doc.Version)
		return err
	}), "attachment.change")
	// attestation keys and the feed (spec 0011 phase 1b): a document offered, then withdrawn by its removal
	var attKeys []string
	for _, f := range []string{"att1.pem", "att2.pem"} {
		if err := signer.GenerateKeyFile(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	en.expect("keys.Service.AddAttestation", do(func() error {
		k, err := en.ks.AddAttestation(rctx, admin, "acme", "file:att1.pem", true)
		attKeys = append(attKeys, k.ID)
		return err
	}), "attestation_key.add")
	en.expect("keys.Service.AddAttestation", do(func() error {
		k, err := en.ks.AddAttestation(rctx, admin, "acme", "file:att2.pem", false)
		attKeys = append(attKeys, k.ID)
		return err
	}), "attestation_key.add")
	en.expect("keys.Service.ActivateAttestation", do(func() error {
		_, err := en.ks.ActivateAttestation(rctx, admin, "acme", attKeys[1], 0, false)
		return err
	}), "attestation_key.activate")
	en.expect("keys.Service.RetireAttestation", do(func() error {
		_, err := en.ks.RetireAttestation(rctx, admin, "acme", attKeys[0], 0, false)
		return err
	}), "attestation_key.retire")
	att.Keys, att.PublicURL = en.ks, "https://kista.example"
	feedable := true
	en.expect("attachment.Service.SetFlags", do(func() error {
		_, err := att.SetFlags(rctx, admin, "acme", "acl", doc.ID, nil, &feedable, 0)
		return err
	}), "attachment.change")
	en.expect("attachment.Service.Remove", do(func() error { return att.Remove(rctx, admin, "acme", "acl", doc.ID) }), "attachment.remove")
	if ev := en.newest(1)[0]; !strings.Contains(ev.Data, `"statement":2`) {
		t.Errorf("a removal of an offered document issues its withdrawal: %s", ev.Data)
	}
	feedURL := "https://source.example/api/v1/tenants/upstream"
	fcreds, err := credential.New([]config.Credential{{Name: "feed", Tenants: []string{"acme"}, Prefixes: []string{"https://source.example/"},
		Kind: "token_file", TokenFile: "/nonexistent"}}, credential.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	feeds := &attachment.Feeds{Store: st, Authz: authz.ServerAdmin{}, Attachments: att, Fetch: repoFetch{}, Credentials: fcreds}
	en.expect("attachment.Feeds.Add", do(func() error {
		_, err := feeds.Add(rctx, admin, "acme", attachment.FeedSpec{Name: "reviews", URL: feedURL, Credential: "feed", Keys: []string{repoFingerprint}})
		return err
	}), "feed.add")
	paused := store.FeedPaused
	en.expect("attachment.Feeds.Change", do(func() error {
		_, err := feeds.Change(rctx, admin, "acme", "reviews", attachment.FeedChange{State: &paused}, 0)
		return err
	}), "feed.change")
	en.expect("attachment.Feeds.Remove", do(func() error { return feeds.Remove(rctx, admin, "acme", "reviews", true, 0) }), "feed.remove")
	en.expect("attachment.Service.Attach", do(func() (err error) { doc, err = notes(); return err }), "attachment.add")
	en.expect("release.Service.Block", do(func() error { _, _, err := en.rel.Block(rctx, admin, "acme", ah.String(), "CVE"); return err }), "block.add")
	en.expect("attachment.Service.RemoveBlockedDocument", do(func() error {
		return att.RemoveBlockedDocument(rctx, admin, "acme", ah.String(), doc.ID)
	}), "attachment.remove")
	// spec 0019, the race: a deletion that starts after an operation's checks, before its first
	// transaction; the operation is refused as a deleting tenant, or writes nothing
	serverWide := map[string]bool{"tenants.Service.CreateTenant": true, "tenants.Service.AddVersion": true, "tenants.Service.AddVersionCAPI": true,
		"tenants.Service.AddVersionCore": true, "tenants.Service.Reserve": true, "tenants.Service.Unreserve": true}
	tid := en.tenantID()
	chans, err := st.ListChannels(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	var chanIDs []string
	for _, c := range chans {
		chanIDs = append(chanIDs, c.ID)
	}
	rows := func() map[string]int64 {
		r, err := store.TenantRows(ctx, st, tid, chanIDs)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	refused := 0
	for _, o := range en.ops {
		if serverWide[o.name] || o.name == "tenants.Service.Delete" {
			continue
		}
		before, evs := rows(), en.count()
		store.SetTxHook(func() {
			store.SetTxHook(nil)
			if err := store.ExecRaw(ctx, st, "UPDATE tenants SET deleting_at = ? WHERE id = ?", store.TimeArg(st, base), tid); err != nil {
				t.Error(err)
			}
		})
		err := o.op()
		store.SetTxHook(nil)
		if err := store.ExecRaw(ctx, st, "UPDATE tenants SET deleting_at = NULL WHERE id = ?", tid); err != nil {
			t.Fatal(err)
		}
		after := rows()
		if errors.Is(err, store.ErrDeleting) {
			refused++
		}
		for table, n := range after {
			if n != before[table] {
				t.Errorf("%s, its tenant closed before its transaction: %s changed from %d to %d rows (%v)", o.name, table, before[table], n, err)
			}
		}
		if n := en.count(); n != evs {
			t.Errorf("%s, its tenant closed before its transaction: %d events (%v)", o.name, n-evs, err)
		}
	}
	if refused == 0 {
		t.Error("no operation met the deletion in its transaction")
	}

	// spec 0019: once the tenant's deletion started, every write to it is refused as a deleting
	// tenant and changes nothing; the server-wide ones are not the tenant's
	en.expect("tenants.Service.SetTenantState", do(func() error {
		_, err := en.ten.SetTenantState(rctx, admin, "acme", store.TenantSuspended, 0)
		return err
	}), "tenant.suspend")
	en.expect("tenants.Service.Delete", do(func() error { _, _, err := en.ten.Delete(rctx, admin, "acme", 0, 0); return err }), "tenant.delete")
	before := en.count()
	for _, o := range en.ops {
		if serverWide[o.name] {
			continue
		}
		err := o.op()
		var refusal *tenants.Refusal
		switch {
		case o.name == "tenants.Service.Delete":
			if !errors.As(err, &refusal) {
				t.Errorf("deleting a deleting tenant again: %v", err)
			}
		case !errors.Is(err, store.ErrDeleting):
			t.Errorf("%s on a deleting tenant: %v", o.name, err)
		}
	}
	if after := en.count(); after != before {
		t.Errorf("writes refused on a deleting tenant still wrote %d events", after-before)
	}
	// every registered writer ran (Ingest, RunUpstream, Import and Export: in internal/upstream's tests, which
	// check their events)
	elsewhere := map[string]bool{"release.Service.Ingest": true, "upstream.Service.RunUpstream": true, "attachment.Feeds.RunFeed": true,
		"upstream.Service.Import": true, "upstream.Service.Export": true, "upstream.Service.ResetFeedBundle": true,
		"upstream.Service.CreateUpload": true, "upstream.Service.CompleteUpload": true, "upstream.Service.AbortUpload": true,
		// the relay and the expiry pass: in internal/upstream's licence tests
		"licenses.Service.RunUpstream": true, "licenses.Service.ExpireOnce": true}
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
		if e.Kind == "key.add" && !bytes.Contains([]byte(e.Data), []byte(`"protection":"file"`)) {
			t.Errorf("key.add without the key's protection (spec 0026): %s", e.Data)
		}
		if e.Actor != "system:resign" && e.Client != "198.51.100.0/24" {
			t.Errorf("%s: client %q", e.Kind, e.Client)
		}
	}
}

type namedOp struct {
	name string
	op   func() error
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

func (repoFetch) PostJSON(context.Context, string, http.Header, []byte, int64) ([]byte, error) {
	return nil, egress.ErrNotFound
}
