package release_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// retire retires a release of a channel as the retention pass does (spec 0042), with the snapshot bump.
func (en *env) retire(t *testing.T, channel, id string) {
	t.Helper()
	ch, err := en.st.GetChannel(ctx, "acme", channel)
	if err != nil {
		t.Fatal(err)
	}
	must(t, func() error {
		return en.st.InTx(ctx, "kista/channel/"+ch.ID, func(tx *store.Tx) error {
			r, err := tx.GetRelease(ctx, ch.ID, id)
			if err != nil {
				return err
			}
			if err := tx.RetireRelease(ctx, &r, "system:release-retention"); err != nil {
				return err
			}
			return tx.BumpReleaseVersion(ctx, ch.ID)
		})
	})
}

// snapshot reads a channel's snapshot as serving does.
func (en *env) snapshot(t *testing.T, channel string) *release.Snapshot {
	t.Helper()
	ch, err := en.st.GetChannel(ctx, "acme", channel)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := (&release.Snapshots{Store: en.st}).Get(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// Spec 0042: a retired release is served on neither path; a restore returns it to the state it
// came from with its seq (current again only if nothing newer is); an insert into its slot, of the
// same body or another, is refused as retired; a yank or a purge takes it; a withdrawn entry's is
// not restored; a block yanks it.
func TestRetiredReleaseChanges(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		f1, f2 := ext(t, 1000, 1, cpp("1.0")), ext(t, 1000, 2, cpp("1.1"))
		r1, _, err := en.add(t, f1, release.AddOptions{Unchecked: true, Name: "tresor"})
		if err != nil {
			t.Fatal(err)
		}
		r2, _, err := en.add(t, f2, release.AddOptions{Unchecked: true, Name: "tresor"})
		if err != nil {
			t.Fatal(err)
		}
		apply := func(id string, c release.Change) (store.Release, error) {
			return en.rel.Apply(ctx, admin, "acme", "prod", "", id, c, 0)
		}
		if _, err := apply(r1.ID, release.Deprecate); err != nil {
			t.Fatal(err)
		}
		if _, err := apply(r2.ID, release.Restore); !errors.Is(err, release.ErrState) {
			t.Fatalf("an active release restored: %v", err)
		}
		en.retire(t, "prod", r1.ID)
		en.retire(t, "prod", r2.ID)
		snap := en.snapshot(t, "prod")
		if c := snap.Flat("v2.0.0", "linux_amd64", "tresor", release.Everything); c != nil {
			t.Fatalf("the flat path serves a retired release: %+v", c)
		}
		for _, v := range []string{"1.0", "1.1"} {
			if c := snap.Versioned("v2.0.0", "linux_amd64", "tresor", v, release.Everything); c != nil {
				t.Fatalf("the versioned path serves retired %s: %+v", v, c)
			}
		}
		if snap.Live("tresor", release.Everything) {
			t.Fatal("a retired release is live")
		}
		// nothing new arrives in its slot: the same body, another body, other choices
		for name, tc := range map[string]struct {
			file []byte
			o    release.AddOptions
		}{
			"the same body":  {f2, release.AddOptions{Unchecked: true, Name: "tresor"}},
			"another body":   {ext(t, 1000, 3, cpp("1.1")), release.AddOptions{Unchecked: true, Name: "tresor"}},
			"other choices":  {f2, release.AddOptions{Unchecked: true, Name: "tresor", Private: true}},
			"a not-current":  {f1, release.AddOptions{Unchecked: true, Name: "tresor", NotCurrent: true}},
			"a C API build:": {ext(t, 1000, 4, capi("1.1", "v1.2.0")), release.AddOptions{Unchecked: true, Name: "tresor"}},
		} {
			_, _, err := en.add(t, tc.file, tc.o)
			want := release.ErrRetired
			if name == "a C API build:" {
				want = release.ErrSlot // c_struct and cpp builds of one version do not mix, as before
			}
			if !errors.Is(err, want) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// restores: deprecated and active, their seqs kept; the newer is current again
		got, err := apply(r1.ID, release.Restore)
		if err != nil || got.State != store.ReleaseDeprecated || got.Seq != r1.Seq || got.RetiredFrom != "" {
			t.Fatalf("restore to deprecated: %+v %v", got, err)
		}
		if got, err = apply(r2.ID, release.Restore); err != nil || got.State != store.ReleaseActive || got.Seq != r2.Seq {
			t.Fatalf("restore to active: %+v %v", got, err)
		}
		if c := en.snapshot(t, "prod").Flat("v2.0.0", "linux_amd64", "tresor", release.Everything); c == nil || c.ID != r2.ID {
			t.Fatalf("the restored release is current again: %+v", c)
		}
		if _, err := apply(r2.ID, release.Restore); !errors.Is(err, release.ErrState) {
			t.Fatalf("restored twice: %v", err)
		}
		// a yank takes a retired release (final), a purge another
		en.retire(t, "prod", r2.ID)
		if got, err := apply(r2.ID, release.Yank); err != nil || got.State != store.ReleaseYanked {
			t.Fatalf("yank a retired release: %+v %v", got, err)
		}
		en.retire(t, "prod", r1.ID)
		for _, c := range []release.Change{release.Activate, release.Deprecate, release.MakeCurrent} {
			if _, err := apply(r1.ID, c); !errors.Is(err, release.ErrState) {
				t.Errorf("%s on a retired release: %v", c, err)
			}
		}
		if p, err := en.rel.Purge(ctx, admin, "acme", "prod", "", r1.ID, 0); err != nil || p.ID != r1.ID {
			t.Fatalf("purge a retired release: %+v %v", p, err)
		}
		if _, _, err := en.add(t, f1, release.AddOptions{Unchecked: true, Name: "tresor"}); !errors.Is(err, release.ErrSlot) {
			t.Fatalf("its purged slot: %v", err)
		}

		// a block yanks a retired release with the body
		r3, _, err := en.add(t, ext(t, 1000, 5, cpp("1.2")), release.AddOptions{Unchecked: true, Name: "tresor"})
		if err != nil {
			t.Fatal(err)
		}
		en.retire(t, "prod", r3.ID)
		var hash string
		ch := channel(t, en)
		for _, c := range must2(en.st.ListReleases(ctx, ch.ID, "tresor")) {
			if c.ID == r3.ID {
				hash = c.BodyHash
			}
		}
		if _, _, err := en.rel.Block(ctx, admin, "acme", hash, "CVE"); err != nil {
			t.Fatal(err)
		}
		for _, c := range must2(en.st.ListReleases(ctx, ch.ID, "tresor")) {
			if c.ID == r3.ID && c.State != store.ReleaseYanked {
				t.Fatalf("the block left a retired release: %+v", c)
			}
		}

		// a withdrawn entry's retired release is not restored
		r4, _, err := en.add(t, ext(t, 1000, 6, cpp("1.3")), release.AddOptions{Unchecked: true, Name: "tresor"})
		if err != nil {
			t.Fatal(err)
		}
		en.retire(t, "prod", r4.ID)
		if err := store.ExecRaw(ctx, en.st, "UPDATE builds SET withdrawn_at = ?, withdrawn_reason = 'CVE' WHERE id = ?",
			store.TimeArg(en.st, time.Now()), r4.BuildID); err != nil {
			t.Fatal(err)
		}
		if _, err := apply(r4.ID, release.Restore); !errors.Is(err, store.ErrWithdrawn) {
			t.Fatalf("a withdrawn entry's release restored: %v", err)
		}
		// the release's extension administrators restore it, nobody else
		r5, _, err := en.add(t, ext(t, 1000, 7, cpp("1.4")), release.AddOptions{Unchecked: true, Name: "tresor"})
		if err != nil {
			t.Fatal(err)
		}
		en.retire(t, "prod", r5.ID)
		if _, err := en.rel.Apply(ctx, authz.Actor{Kind: authz.ActorPrincipal, ID: "x"}, "acme", "prod", "tresor", r5.ID, release.Restore, 0); !errors.Is(err, authz.ErrDenied) {
			t.Fatalf("a stranger's restore: %v", err)
		}
	})
}

func must2[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Spec 0042: a promotion into a retired slot, and a placement by hand, are refused as retired; a
// promotion from a retired release is refused; an auto rule never places a retired version again and
// a manual rule does not complete one.
func TestRetiredIntake(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		p := newPlacing(t, en)
		p.publish(t, cpp("1.0"))
		staged := must2(en.st.ListReleases(ctx, mustChannel(t, en, "staging").ID, "tresor"))
		if len(staged) != 1 {
			t.Fatalf("staging: %+v", staged)
		}
		promote := func() error {
			_, _, err := en.rel.Promote(ctx, admin, "acme", "prod", "tresor", release.PromoteOptions{From: "staging", Version: "1.0"})
			return err
		}
		if err := promote(); err != nil {
			t.Fatal(err)
		}
		prod := must2(en.st.ListReleases(ctx, mustChannel(t, en, "prod").ID, "tresor"))
		en.retire(t, "prod", prod[0].ID)
		if err := promote(); !errors.Is(err, release.ErrRetired) {
			t.Fatalf("a promotion into a retired slot: %v", err)
		}
		if _, _, err := en.rel.Place(ctx, admin, "acme", "prod", "tresor", release.PlaceOptions{Source: "publisher:ci", Version: "1.0"}); !errors.Is(err, release.ErrRetired) {
			t.Fatalf("a placement by hand into a retired slot: %v", err)
		}
		// from a retired release: nothing active to promote
		en.retire(t, "staging", staged[0].ID)
		if _, _, err := en.rel.Promote(ctx, admin, "acme", "empty", "tresor", release.PromoteOptions{From: "staging", Release: staged[0].ID}); !errors.Is(err, release.ErrState) {
			t.Fatalf("a promotion of a retired release: %v", err)
		}
		if _, err := en.rel.Apply(ctx, admin, "acme", "staging", "", staged[0].ID, release.Restore, 0); err != nil {
			t.Fatal(err)
		}

		// an auto rule never places the retired version again; a manual one does not complete it
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "tresor", "publisher:ci", "auto"); err != nil {
			t.Fatal(err)
		}
		p.pass(t, true)
		if got := p.prod(t); got != "1.0@v2.0.0:retired" {
			t.Fatalf("auto placed a retired version again: %q", got)
		}
		if _, err := p.rel.SetSource(ctx, admin, "acme", "prod", "tresor", "publisher:ci", "manual"); err != nil {
			t.Fatal(err)
		}
		arm := extfile.Metadata{Platform: "linux_arm64", DuckDBVersion: "v2.0.0", ExtensionVersion: "1.0", ABI: extfile.ABICPP}
		armFile := built(t, arm, "tresor_duckdb_cpp_init")
		armFile[18], armFile[19] = 183, 0
		if _, _, err := p.pubs.Add(ctx, p.pub, "acme", "staging", bytes.NewReader(armFile), publish("tresor", "1.0", "linux_arm64")); err != nil {
			t.Fatal(err)
		}
		p.pass(t, true)
		if got := p.prod(t); got != "1.0@v2.0.0:retired" {
			t.Fatalf("a manual rule completed a retired version: %q", got)
		}
	})
}

func mustChannel(t *testing.T, en *env, name string) store.Channel {
	t.Helper()
	c, err := en.st.GetChannel(ctx, "acme", name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
