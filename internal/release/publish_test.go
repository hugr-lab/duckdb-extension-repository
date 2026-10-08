package release_test

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/symbols/symtest"
)

// built is an extension file whose binary exports the given entry points (an x86-64 ELF), so it
// passes spec 0008's checks for linux_amd64.
func built(t *testing.T, m extfile.Metadata, exports ...string) []byte {
	t.Helper()
	block, err := extfile.EncodeMetadata(m)
	if err != nil {
		t.Fatal(err)
	}
	b := append(symtest.ELF(exports...), extfile.MetadataPrefix...)
	b = append(b, block[:]...)
	return append(b, make([]byte, extfile.SignatureSize)...)
}

func publish(name, version, platform string) release.AddOptions {
	return release.AddOptions{Name: name, Publish: &release.Publication{Version: version, Platform: platform, Provenance: `{"run":"1"}`}}
}

func (en *env) addTo(t *testing.T, channel string, file []byte, o release.AddOptions) (store.Release, bool, error) {
	t.Helper()
	return en.rel.Add(ctx, admin, "acme", channel, bytes.NewReader(file), o)
}

func TestPublishChecks(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		good := built(t, cpp("1.0"), "tresor_duckdb_cpp_init")
		for name, tc := range map[string]struct {
			file []byte
			o    release.AddOptions
		}{
			"declared version":   {good, publish("tresor", "1.1", "linux_amd64")},
			"declared platform":  {good, publish("tresor", "1.0", "linux_arm64")},
			"another name":       {good, publish("acl", "1.0", "linux_amd64")},
			"two extensions":     {built(t, cpp("1.0"), "tresor_duckdb_cpp_init", "acl_init_c_api"), publish("tresor", "1.0", "linux_amd64")},
			"the C API v1 entry": {built(t, capi("1.0", "v1.2.0"), "tresor_duckdb_cpp_init"), publish("tresor", "1.0", "linux_amd64")},
			"not a binary":       {ext(t, 3000, 1, cpp("1.0")), publish("tresor", "1.0", "linux_amd64")},
			"unchecked is ignored for publications": {ext(t, 3000, 2, cpp("1.0")),
				release.AddOptions{Name: "tresor", Unchecked: true, Publish: &release.Publication{Version: "1.0", Platform: "linux_amd64"}}},
		} {
			if _, _, err := en.add(t, tc.file, tc.o); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// no metadata prefix
		noPrefix := bytes.Replace(good, extfile.MetadataPrefix, make([]byte, len(extfile.MetadataPrefix)), 1)
		if _, _, err := en.add(t, noPrefix, publish("tresor", "1.0", "linux_amd64")); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("no prefix: %v", err)
		}

		r, existed, err := en.add(t, good, publish("tresor", "1.0", "linux_amd64"))
		if err != nil || existed || r.Origin != store.OriginPublication || r.Provenance != `{"run":"1"}` {
			t.Fatalf("publish: %+v %v %v", r, existed, err)
		}
		if again, existed, err := en.add(t, good, publish("tresor", "1.0", "linux_amd64")); err != nil || !existed || again.ID != r.ID {
			t.Fatalf("publish again: %v %v", existed, err)
		}
		c, err := built(t, capi("2.0", "v2.0.0"), "tresor_init_c_api_v2"), error(nil)
		if _, _, err = en.add(t, c, publish("tresor", "2.0", "linux_amd64")); err != nil {
			t.Fatalf("the C API v2 entry: %v", err)
		}
		// C API v1 (accepted by v2.0.0's v1.5.6 maximum) and C_STRUCT_UNSTABLE
		if _, _, err = en.add(t, built(t, capi("2.0", "v1.2.0"), "tresor_init_c_api"), publish("tresor", "2.0", "linux_amd64")); err != nil {
			t.Fatalf("the C API v1 entry: %v", err)
		}
		unstable := extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: "3.0", ABI: extfile.ABICStructUnstable}
		if _, _, err = en.add(t, built(t, unstable, "acl_init_c_api_v2"), publish("acl", "3.0", "linux_amd64")); err != nil {
			t.Fatalf("C_STRUCT_UNSTABLE: %v", err)
		}
		if _, _, err := en.add(t, built(t, cpp("1.0"), "tresor_init_c_api"), publish("tresor", "1.0", "linux_amd64")); err == nil ||
			!strings.Contains(err.Error(), "tresor_init_c_api") {
			t.Fatalf("the wrong ABI's entry: %v", err)
		}
		// the operator's add checks too, unless told not to
		if _, _, err := en.add(t, ext(t, 3000, 3, cpp("3.0")), release.AddOptions{Name: "tresor"}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("an unchecked add without -unchecked: %v", err)
		}
	})
}

// staging is a second signed channel serving v2.0.0, with a key.
func (en *env) staging(t *testing.T) {
	t.Helper()
	must(t, func() error {
		_, err := en.ten.CreateChannel(ctx, admin, "acme", "staging", store.ChannelSigned)
		return err
	})
	must(t, func() error {
		_, err := en.ten.SetChannelVersions(ctx, admin, "acme", "staging", []string{"v2.0.0"}, nil)
		return err
	})
	en.addKey(t, "staging", "s.pem", true)
}

func TestPromote(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.staging(t)
		en.addKey(t, "empty", "e.pem", true)
		amd := built(t, cpp("1.0"), "tresor_duckdb_cpp_init")
		arm := extfile.Metadata{Platform: "linux_arm64", DuckDBVersion: "v2.0.0", ExtensionVersion: "1.0", ABI: extfile.ABICPP}
		armFile := built(t, arm, "tresor_duckdb_cpp_init")
		// an arm64 ELF is needed for linux_arm64: patch the machine of the x86-64 fixture
		armFile[18], armFile[19] = 183, 0
		var ids []string
		for _, f := range []struct {
			file     []byte
			platform string
		}{{amd, "linux_amd64"}, {armFile, "linux_arm64"}} {
			o := publish("tresor", "1.0", f.platform)
			o.Private = true
			r, _, err := en.addTo(t, "staging", f.file, o)
			if err != nil {
				t.Fatalf("publish %s: %v", f.platform, err)
			}
			ids = append(ids, r.ID)
		}
		// a version: every platform, visibility kept (private)
		rs, existed, err := en.rel.Promote(ctx, admin, "acme", "prod", "tresor", release.PromoteOptions{From: "staging", Version: "1.0"})
		if err != nil || existed || len(rs) != 2 {
			t.Fatalf("promote a version: %v %v %v", rs, existed, err)
		}
		for _, r := range rs {
			if r.Origin != store.OriginPromotion || r.Visibility != store.Private || r.ChannelID == "" {
				t.Fatalf("a promoted release: %+v", r)
			}
		}
		if _, existed, err := en.rel.Promote(ctx, admin, "acme", "prod", "tresor", release.PromoteOptions{From: "staging", Release: ids[0]}); err != nil || !existed {
			t.Fatalf("promote again: %v %v", existed, err)
		}
		for name, tc := range map[string]struct {
			o    release.PromoteOptions
			want error
		}{
			"neither":        {release.PromoteOptions{From: "staging"}, store.ErrInvalid},
			"both":           {release.PromoteOptions{From: "staging", Release: ids[0], Version: "1.0"}, store.ErrInvalid},
			"to itself":      {release.PromoteOptions{From: "empty", Version: "1.0"}, store.ErrInvalid},
			"missing":        {release.PromoteOptions{From: "staging", Version: "9.9"}, store.ErrNotFound},
			"another source": {release.PromoteOptions{From: "nope", Version: "1.0"}, store.ErrNotFound},
			"passthrough":    {release.PromoteOptions{From: "mirror", Version: "1.0"}, store.ErrNotFound},
			"no source":      {release.PromoteOptions{Version: "1.0"}, store.ErrInvalid},
		} {
			if _, _, err := en.rel.Promote(ctx, admin, "acme", "empty", "tresor", tc.o); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// the target must serve the build's DuckDB version (empty serves none)
		if _, _, err := en.rel.Promote(ctx, admin, "acme", "empty", "tresor", release.PromoteOptions{From: "staging", Release: ids[0]}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a target that does not serve v2.0.0: %v", err)
		}
		// an unchecked build is not promoted, until the same body is published (and so checked)
		ub := built(t, cpp("2.0"), "tresor_duckdb_cpp_init")
		u, _, err := en.addTo(t, "staging", ub, release.AddOptions{Name: "tresor", Unchecked: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := en.rel.Promote(ctx, admin, "acme", "prod", "tresor", release.PromoteOptions{From: "staging", Release: u.ID}); !errors.Is(err, release.ErrState) {
			t.Fatalf("an unchecked build: %v", err)
		}
		if _, existed, err := en.addTo(t, "staging", ub, publish("tresor", "2.0", "linux_amd64")); err != nil || !existed {
			t.Fatalf("publishing the unchecked body: %v %v", existed, err)
		}
		if _, _, err := en.rel.Promote(ctx, admin, "acme", "prod", "tresor", release.PromoteOptions{From: "staging", Release: u.ID,
			Private: true}); err != nil {
			t.Fatalf("promoting it once checked: %v", err)
		}
		// all or none: a version whose one platform's slot holds another body in the target
		v3 := built(t, cpp("3.0"), "tresor_duckdb_cpp_init")
		v3arm := built(t, extfile.Metadata{Platform: "linux_arm64", DuckDBVersion: "v2.0.0", ExtensionVersion: "3.0", ABI: extfile.ABICPP},
			"tresor_duckdb_cpp_init")
		v3arm[18] = 183
		for _, f := range []struct {
			ch       string
			file     []byte
			platform string
		}{{"staging", v3, "linux_amd64"}, {"staging", v3arm, "linux_arm64"}} {
			if _, _, err := en.addTo(t, f.ch, f.file, publish("tresor", "3.0", f.platform)); err != nil {
				t.Fatal(err)
			}
		}
		conflict := built(t, cpp("3.0"), "tresor_duckdb_cpp_init", "helper")
		if _, _, err := en.addTo(t, "prod", conflict, publish("tresor", "3.0", "linux_amd64")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := en.rel.Promote(ctx, admin, "acme", "prod", "tresor", release.PromoteOptions{From: "staging", Version: "3.0"}); !errors.Is(err, release.ErrSlot) {
			t.Fatalf("a version with a conflicting slot: %v", err)
		}
		rs3, err := en.rel.List(ctx, admin, "acme", "prod", "tresor")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs3 {
			if r.ExtVersion == "3.0" && r.Platform == "linux_arm64" {
				t.Fatal("all or none: the arm64 release was inserted")
			}
		}
		if _, err := en.rel.Apply(ctx, admin, "acme", "staging", "", u.ID, release.Deprecate, 0); err != nil {
			t.Fatal(err)
		}
		if _, _, err := en.rel.Promote(ctx, admin, "acme", "prod", "tresor", release.PromoteOptions{From: "staging", Release: u.ID}); !errors.Is(err, release.ErrState) {
			t.Fatalf("a deprecated release: %v", err)
		}
	})
}

func TestBlock(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.staging(t)
		file := built(t, cpp("1.0"), "tresor_duckdb_cpp_init")
		f, err := extfile.Open(bytes.NewReader(file), int64(len(file)), 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		hash := f.Hash.String()
		for _, ch := range []string{"prod", "staging"} {
			if _, _, err := en.addTo(t, ch, file, publish("tresor", "1.0", "linux_amd64")); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := en.rel.Block(ctx, admin, "acme", "nothex", "x"); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a bad hash: %v", err)
		}
		b, existed, err := en.rel.Block(ctx, admin, "acme", hash, "CVE-1")
		if err != nil || existed || b.Reason != "CVE-1" {
			t.Fatalf("block: %+v %v %v", b, existed, err)
		}
		for _, ch := range []string{"prod", "staging"} {
			rs, err := en.rel.List(ctx, admin, "acme", ch, "tresor")
			if err != nil || len(rs) != 1 || rs[0].State != store.ReleaseYanked {
				t.Fatalf("%s after the block: %+v %v", ch, rs, err)
			}
		}
		// blocked: not published, added or promoted again, anywhere in the tenant
		en.addKey(t, "empty", "e.pem", true)
		if _, _, err := en.addTo(t, "empty", file, release.AddOptions{Name: "tresor"}); !errors.Is(err, release.ErrBlocked) {
			t.Fatalf("add a blocked body: %v", err)
		}
		if _, _, err := en.rel.Promote(ctx, admin, "acme", "empty", "tresor", release.PromoteOptions{From: "prod", Version: "1.0"}); !errors.Is(err, release.ErrState) {
			t.Fatalf("promote a blocked (yanked) body: %v", err)
		}
		if _, existed, _ := en.rel.Block(ctx, admin, "acme", hash, "again"); !existed {
			t.Fatal("blocking twice: not reported as existing")
		}
		if bs, err := en.rel.ListBlocks(ctx, admin, "acme"); err != nil || len(bs) != 1 {
			t.Fatalf("blocks: %v %v", bs, err)
		}
		if err := en.rel.Unblock(ctx, admin, "acme", hash); err != nil {
			t.Fatal(err)
		}
		if err := en.rel.Unblock(ctx, admin, "acme", hash); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unblock twice: %v", err)
		}
		// a pre-emptive block of a body nobody holds
		if _, _, err := en.rel.Block(ctx, admin, "acme", "00"+hash[2:], "ahead"); err != nil {
			t.Fatal(err)
		}
	})
}

// A block and publications racing: whatever the order, the body is not left served.
func TestBlockRace(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.staging(t)
		for i := range 4 {
			ver := []string{"1.0", "1.1", "1.2", "1.3"}[i]
			file := built(t, cpp(ver), "tresor_duckdb_cpp_init")
			f, _ := extfile.Open(bytes.NewReader(file), int64(len(file)), 1<<30)
			var wg sync.WaitGroup
			wg.Add(3)
			errs := make(chan error, 3)
			go func() {
				defer wg.Done()
				_, _, err := en.addTo(t, "prod", file, publish("tresor", ver, "linux_amd64"))
				errs <- err
			}()
			go func() {
				defer wg.Done()
				_, _, err := en.addTo(t, "staging", file, publish("tresor", ver, "linux_amd64"))
				errs <- err
			}()
			go func() {
				defer wg.Done()
				_, _, err := en.rel.Block(ctx, admin, "acme", f.Hash.String(), "race")
				errs <- err
			}()
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil && !errors.Is(err, release.ErrBlocked) {
					t.Fatalf("an error other than blocked: %v", err)
				}
			}
			for _, ch := range []string{"prod", "staging"} {
				rs, err := en.rel.List(ctx, admin, "acme", ch, "tresor")
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range rs {
					if r.ExtVersion == ver && r.State != store.ReleaseYanked {
						t.Fatalf("%s %s is %s after a block", ch, ver, r.State)
					}
				}
			}
		}
	})
}
