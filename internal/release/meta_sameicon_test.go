package release_test

import (
	"bytes"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
)

// Spec 0036 (post-PR review): the icon the entry has, sent again as bytes, is uploaded again when
// its object is missing (the write repairs it), and when the entry's icon changed meanwhile the
// write uploads its bytes rather than answering a conflict its client could not have caused.
func TestEntryMetaSameIcon(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		metaEnv(t, en)
		rel, c := counted(t, en)
		ic := newIcon(t, 64)
		v, err := rel.SetEntryMeta(ctx, admin, "acme", "", "tenant", "acl", release.MetaWrite{Icon: ic})
		if err != nil {
			t.Fatal(err)
		}
		if err := en.blobs.Delete(ctx, blob.IconKey(v.IconSHA256)); err != nil {
			t.Fatal(err)
		}
		if _, err := rel.SetEntryMeta(ctx, admin, "acme", "", "tenant", "acl", release.MetaWrite{Icon: ic, Summary: str("x")}); err != nil ||
			c.puts.Load() != 2 {
			t.Fatalf("a missing object: %v, %d puts", err, c.puts.Load())
		}
		if got, err := rel.EntryIcon(ctx, admin, "acme", "", "tenant", "acl"); err != nil || !bytes.Equal(got, ic.Data) {
			t.Fatalf("the repaired icon: %v", err)
		}
		// the entry's icon changed between the checks and the transaction
		other := newIcon(t, 96)
		release.SetBeforeMetaCommit(rel, func() {
			release.SetBeforeMetaCommit(rel, nil)
			if _, err := rel.SetEntryMeta(ctx, admin, "acme", "", "tenant", "acl", release.MetaWrite{Icon: other}); err != nil {
				t.Error(err)
			}
		})
		v, err = rel.SetEntryMeta(ctx, admin, "acme", "", "tenant", "acl", release.MetaWrite{Icon: ic, Summary: str("y")})
		if err != nil || v.IconSHA256 != blob.ContentHash(ic.Data) || v.Summary != "y" {
			t.Fatalf("a write whose icon changed meanwhile: %+v %v", v, err)
		}
	})
}
