package release_test

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/fs"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

var (
	ctx   = context.Background()
	admin = authz.Actor{Kind: authz.ActorOS, ID: "1:test"}
)

func TestMain(m *testing.M) {
	code := m.Run()
	storetest.Cleanup()
	os.Exit(code)
}

type env struct {
	st   *store.Store
	keys *keys.Service
	rel  *release.Service
	ten  *tenants.Service
	dir  string
}

func newEnv(t *testing.T, e storetest.Engine) *env {
	t.Helper()
	st := e.Open(t)
	dir := t.TempDir()
	ks := &keys.Service{Store: st, Signers: signer.Resolver{FileDir: dir, AllowFile: true}, Authz: authz.ServerAdmin{}}
	fsStore, err := fs.Open(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	bs, err := blob.NewService(ctx, st, []blob.Domain{{Name: "default", Kind: "fs", Store: fsStore}},
		blob.Options{SpoolDir: filepath.Join(t.TempDir(), "spool"), MaxBody: 64 << 20, MaxIngests: 4, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bs.Close() })
	en := &env{st: st, keys: ks, dir: dir,
		rel: &release.Service{Store: st, Blob: bs, Signers: ks, Authz: authz.ServerAdmin{}},
		ten: &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}}
	must(t, func() error { _, err := en.ten.CreateTenant(ctx, admin, "acme", "", ""); return err })
	must(t, func() error {
		_, err := en.ten.AddVersion(ctx, admin, "v2.0.0", "release", []string{"v1.5.6", "v2.0.0"})
		return err
	})
	for name, kind := range map[string]string{"prod": store.ChannelSigned, "mirror": store.ChannelPassthrough, "empty": store.ChannelSigned} {
		must(t, func() error { _, err := en.ten.CreateChannel(ctx, admin, "acme", name, kind); return err })
	}
	must(t, func() error {
		_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "prod", []string{"v2.0.0"}, nil)
		return err
	})
	en.addKey(t, "prod", "a.pem", true)
	return en
}

func must(t *testing.T, f func() error) {
	t.Helper()
	if err := f(); err != nil {
		t.Fatal(err)
	}
}

func (en *env) addKey(t *testing.T, channel, file string, active bool) store.Key {
	t.Helper()
	if err := signer.GenerateKeyFile(filepath.Join(en.dir, file)); err != nil {
		t.Fatal(err)
	}
	k, err := en.keys.Add(ctx, admin, "acme", channel, "file:"+file, active)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// ext builds an extension file: n deterministic bytes, then the footer.
func ext(t *testing.T, n int, seed uint64, m extfile.Metadata) []byte {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	block, err := extfile.EncodeMetadata(m)
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, extfile.MetadataPrefix...)
	b = append(b, block[:]...)
	return append(b, make([]byte, extfile.SignatureSize)...)
}

func cpp(ver string) extfile.Metadata {
	return extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: ver, ABI: extfile.ABICPP}
}

func capi(ver, c string) extfile.Metadata {
	return extfile.Metadata{Platform: "linux_amd64", CAPIVersion: c, ExtensionVersion: ver, ABI: extfile.ABICStruct}
}

func (en *env) add(t *testing.T, file []byte, o release.AddOptions) (store.Release, bool, error) {
	t.Helper()
	return en.rel.Add(ctx, admin, "acme", "prod", bytes.NewReader(file), o)
}

func each(t *testing.T, f func(t *testing.T, en *env)) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) { f(t, newEnv(t, e)) })
	}
}

// verifies checks that the release's signature by key verifies over the file's body.
func (en *env) verifies(t *testing.T, r store.Release, k store.Key, file []byte) bool {
	t.Helper()
	sig, err := en.st.Signature(ctx, r.ID, k.ID)
	if err != nil {
		return false
	}
	f, err := extfile.Open(bytes.NewReader(file), int64(len(file)), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := x509.ParsePKIXPublicKey(k.PublicKey)
	_, ok := extfile.Verify(f.Hash, sig, []*rsa.PublicKey{pub.(*rsa.PublicKey)})
	return ok
}

func channel(t *testing.T, en *env) store.Channel {
	t.Helper()
	c, err := en.st.GetChannel(ctx, "acme", "prod")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAdd(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		ka := activeKey(t, en)
		file := ext(t, 3000, 1, cpp("1.0"))
		r, existed, err := en.add(t, file, release.AddOptions{Name: "tresor"})
		if err != nil || existed {
			t.Fatalf("add: %v %v", existed, err)
		}
		if r.Seq != 1 || r.Slot != "v2.0.0" || r.Visibility != store.Public || r.State != store.ReleaseActive {
			t.Fatalf("release %+v", r)
		}
		if !en.verifies(t, r, ka, file) {
			t.Fatal("the signature does not verify")
		}
		c := channel(t, en)
		if c.ServingKeyID != ka.ID || c.ReleaseVersion != 1 {
			t.Fatalf("channel after add: %+v", c)
		}

		// the same body and choices: the existing release, nothing new
		r2, existed, err := en.add(t, file, release.AddOptions{Name: "tresor"})
		if err != nil || !existed || r2.ID != r.ID {
			t.Fatalf("re-add: %v %v", existed, err)
		}
		for _, o := range []release.AddOptions{{Name: "tresor", Private: true}, {Name: "tresor", NotCurrent: true}} {
			if _, _, err := en.add(t, file, o); !errors.Is(err, release.ErrState) {
				t.Fatalf("re-add with other choices %+v: %v", o, err)
			}
		}
		// another body in the slot
		if _, _, err := en.add(t, ext(t, 3000, 2, cpp("1.0")), release.AddOptions{Name: "tresor"}); !errors.Is(err, release.ErrSlot) {
			t.Fatalf("another body in the slot: %v", err)
		}
		// c_struct and cpp builds of one version do not mix
		if _, _, err := en.add(t, ext(t, 3000, 3, capi("1.0", "v1.2.0")), release.AddOptions{Name: "tresor"}); !errors.Is(err, release.ErrSlot) {
			t.Fatalf("mixing ABIs: %v", err)
		}
		// a c_struct build per C API major
		c1, _, err := en.add(t, ext(t, 3000, 4, capi("1.0", "v1.2.0")), release.AddOptions{Name: "demo"})
		if err != nil || c1.Slot != "capi:1" {
			t.Fatalf("c_struct v1: %+v %v", c1, err)
		}
		c2, _, err := en.add(t, ext(t, 3000, 5, capi("1.0", "v2.0.0")), release.AddOptions{Name: "demo", NotCurrent: true})
		if err != nil || c2.Slot != "capi:2" || c2.Seq != 0 {
			t.Fatalf("c_struct v2, not current: %+v %v", c2, err)
		}

		for name, tc := range map[string]struct {
			file    []byte
			o       release.AddOptions
			channel string
			want    error
		}{
			"alias name":           {ext(t, 100, 6, cpp("1.0")), release.AddOptions{Name: "postgres"}, "prod", store.ErrInvalid},
			"device name":          {ext(t, 100, 6, cpp("1.0")), release.AddOptions{Name: "com1"}, "prod", store.ErrInvalid},
			"upper name":           {ext(t, 100, 6, cpp("1.0")), release.AddOptions{Name: "Tresor"}, "prod", store.ErrInvalid},
			"unserved version":     {ext(t, 100, 6, extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.1.0", ExtensionVersion: "1", ABI: extfile.ABICPP}), release.AddOptions{Name: "x"}, "prod", store.ErrInvalid},
			"wasm":                 {ext(t, 100, 6, extfile.Metadata{Platform: "wasm_eh", DuckDBVersion: "v2.0.0", ExtensionVersion: "1", ABI: extfile.ABICPP}), release.AddOptions{Name: "x"}, "prod", store.ErrInvalid},
			"bad C API":            {ext(t, 100, 6, capi("1.0", "1.2")), release.AddOptions{Name: "x"}, "prod", store.ErrInvalid},
			"long name":            {ext(t, 100, 6, cpp("1.0")), release.AddOptions{Name: strings.Repeat("a", 65)}, "prod", store.ErrInvalid},
			"dotdot version":       {ext(t, 100, 6, cpp("..")), release.AddOptions{Name: "x"}, "prod", extfile.ErrMalformed},
			"version with a space": {ext(t, 100, 6, cpp("1 0")), release.AddOptions{Name: "x"}, "prod", extfile.ErrMalformed},
			"upper platform":       {ext(t, 100, 6, extfile.Metadata{Platform: "Linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: "1", ABI: extfile.ABICPP}), release.AddOptions{Name: "x"}, "prod", store.ErrInvalid},
			"bad DuckDB version":   {ext(t, 100, 6, extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0", ExtensionVersion: "1", ABI: extfile.ABICPP}), release.AddOptions{Name: "x"}, "prod", store.ErrInvalid},
			"passthrough channel":  {ext(t, 100, 6, cpp("1.0")), release.AddOptions{Name: "x"}, "mirror", release.ErrState},
			"no active key":        {ext(t, 100, 6, cpp("1.0")), release.AddOptions{Name: "x"}, "empty", release.ErrState},
		} {
			_, _, err := en.rel.Add(ctx, admin, "acme", tc.channel, bytes.NewReader(tc.file), tc.o)
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: got %v, want %v", name, err, tc.want)
			}
		}
		if _, _, err := en.rel.Add(ctx, authz.Actor{Kind: authz.ActorPrincipal, ID: "x"}, "acme", "prod", bytes.NewReader(file), release.AddOptions{Name: "tresor"}); !errors.Is(err, authz.ErrDenied) {
			t.Fatalf("unauthorised add: %v", err)
		}
	})
}

func activeKey(t *testing.T, en *env) store.Key {
	t.Helper()
	ks, err := en.keys.List(ctx, admin, "acme", "prod")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range ks {
		if k.State == store.KeyActive {
			return k
		}
	}
	t.Fatal("no active key")
	return store.Key{}
}

func TestApply(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		r1, _, err := en.add(t, ext(t, 1000, 1, cpp("1.0")), release.AddOptions{Name: "tresor"})
		if err != nil {
			t.Fatal(err)
		}
		r2, _, err := en.add(t, ext(t, 1000, 2, cpp("1.1")), release.AddOptions{Name: "tresor", Private: true})
		if err != nil || r2.Seq != 2 {
			t.Fatalf("second: %+v %v", r2, err)
		}
		apply := func(id string, c release.Change) (store.Release, error) {
			return en.rel.Apply(ctx, admin, "acme", "prod", "", id, c, 0)
		}
		if r, err := apply(r1.ID, release.MakeCurrent); err != nil || r.Seq != 3 {
			t.Fatalf("current: %+v %v", r, err)
		}
		if r, err := apply(r2.ID, release.SetPublic); err != nil || r.Visibility != store.Public {
			t.Fatalf("public: %+v %v", r, err)
		}
		if _, err := apply(r2.ID, release.Activate); !errors.Is(err, release.ErrState) {
			t.Fatalf("activate an active release: %v", err)
		}
		if r, err := apply(r2.ID, release.Deprecate); err != nil || r.State != store.ReleaseDeprecated {
			t.Fatalf("deprecate: %+v %v", r, err)
		}
		if _, err := apply(r2.ID, release.MakeCurrent); !errors.Is(err, release.ErrState) {
			t.Fatalf("make a deprecated release current: %v", err)
		}
		if r, err := apply(r2.ID, release.Activate); err != nil || r.State != store.ReleaseActive {
			t.Fatalf("activate: %+v %v", r, err)
		}
		if r, err := apply(r2.ID, release.Yank); err != nil || r.State != store.ReleaseYanked {
			t.Fatalf("yank: %+v %v", r, err)
		}
		for _, c := range []release.Change{release.Yank, release.Activate, release.SetPrivate, release.MakeCurrent} {
			if _, err := apply(r2.ID, c); !errors.Is(err, release.ErrState) {
				t.Errorf("%s on a yanked release: %v", c, err)
			}
		}
		if _, err := apply("no-such-id", release.Yank); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing release: %v", err)
		}
		if c := channel(t, en); c.ReleaseVersion != 7 { // 2 adds and 5 changes
			t.Fatalf("release_version %d", c.ReleaseVersion)
		}
		if rs, err := en.rel.List(ctx, admin, "acme", "prod", "tresor"); err != nil || len(rs) != 2 {
			t.Fatalf("list: %d %v", len(rs), err)
		}
	})
}

// A rotation: the new key signs new releases at once, the re-signer covers the old ones, then the
// serving key moves; the old key cannot be retired before.
func TestRotation(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		ka := activeKey(t, en)
		f1 := ext(t, 1000, 1, cpp("1.0"))
		r1, _, err := en.add(t, f1, release.AddOptions{Name: "tresor"})
		if err != nil {
			t.Fatal(err)
		}
		kb := en.addKey(t, "prod", "b.pem", false)
		if _, err := en.keys.Activate(ctx, admin, "acme", "prod", kb.ID, 0, true); err != nil {
			t.Fatal(err)
		}
		f2 := ext(t, 1000, 2, cpp("1.1"))
		r2, _, err := en.add(t, f2, release.AddOptions{Name: "tresor"})
		if err != nil {
			t.Fatal(err)
		}
		if !en.verifies(t, r2, ka, f2) || !en.verifies(t, r2, kb, f2) {
			t.Fatal("a release added during a rotation is not signed by the serving and the active key")
		}
		if en.verifies(t, r1, kb, f1) {
			t.Fatal("an old release has the new key's signature before re-signing")
		}
		for _, force := range []bool{false, true} {
			if _, err := en.keys.Retire(ctx, admin, "acme", "prod", ka.ID, 0, force); !errors.Is(err, keys.ErrState) {
				t.Fatalf("retiring the serving key (force %v): %v", force, err)
			}
		}
		ids, err := en.st.ChannelsToResign(ctx)
		if err != nil || len(ids) != 1 {
			t.Fatalf("channels to re-sign: %v %v", ids, err)
		}
		signed, moved, err := en.rel.Resign(ctx, ids[0], nil)
		if err != nil || signed != 1 || !moved {
			t.Fatalf("resign: %d %v %v", signed, moved, err)
		}
		if !en.verifies(t, r1, kb, f1) || channel(t, en).ServingKeyID != kb.ID {
			t.Fatal("after re-signing")
		}
		if ids, _ := en.st.ChannelsToResign(ctx); len(ids) != 0 {
			t.Fatalf("still to re-sign: %v", ids)
		}
		if signed, moved, err := en.rel.Resign(ctx, channel(t, en).ID, nil); err != nil || signed != 0 || moved {
			t.Fatalf("a second resign: %d %v %v", signed, moved, err)
		}
		if _, err := en.keys.Retire(ctx, admin, "acme", "prod", ka.ID, 0, true); err != nil {
			t.Fatalf("retiring the old key after the move: %v", err)
		}
		// a new release after the retirement is signed by the remaining key only
		f3 := ext(t, 1000, 3, cpp("1.2"))
		r3, _, err := en.add(t, f3, release.AddOptions{Name: "tresor"})
		if err != nil || !en.verifies(t, r3, kb, f3) || en.verifies(t, r3, ka, f3) {
			t.Fatalf("after retirement: %v", err)
		}
	})
}

// Adds racing a re-sign never leave a release without the serving key's signature.
func TestRotationRace(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		if _, _, err := en.add(t, ext(t, 500, 1, cpp("0.1")), release.AddOptions{Name: "tresor"}); err != nil {
			t.Fatal(err)
		}
		kb := en.addKey(t, "prod", "b.pem", false)
		if _, err := en.keys.Activate(ctx, admin, "acme", "prod", kb.ID, 0, true); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, err := en.add(t, ext(t, 500, uint64(10+i), cpp("1."+string(rune('0'+i)))), release.AddOptions{Name: "tresor"})
				errs <- err
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(5 * time.Millisecond)
			_, _, err := en.rel.Resign(ctx, channel(t, en).ID, nil)
			errs <- err
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		c := channel(t, en)
		missing, _, err := en.st.UnsignedReleases(ctx, c.ID, c.ServingKeyID, 0)
		if err != nil || len(missing) != 0 {
			t.Fatalf("%d releases without the serving key's signature: %v", len(missing), err)
		}
	})
}
