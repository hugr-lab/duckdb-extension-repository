package store_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Extension groups (spec 0017): groups, members, grants on groups apart from grants, the limits.
func TestPackageGroups(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		is := store.Issuer{TenantID: tn.ID, Name: "corp", URL: "https://idp.example", CreatedBy: "os:1:t"}
		pub := store.Publisher{TenantID: tn.ID, Name: "ci", CreatedBy: "os:1:t"}
		g := store.PackageGroup{TenantID: tn.ID, Name: "platform", Description: "hugr", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.InsertIssuer(ctx, &is); err != nil {
				return err
			}
			if err := tx.InsertPublisher(ctx, &pub); err != nil {
				return err
			}
			return tx.InsertPackageGroup(ctx, &g)
		})
		for name, bad := range map[string]store.PackageGroup{
			"bad name":    {TenantID: tn.ID, Name: "Platform"},
			"control":     {TenantID: tn.ID, Name: "x", Description: "a\nb"},
			"long":        {TenantID: tn.ID, Name: "x", Description: string(make([]byte, 257))},
			"format char": {TenantID: tn.ID, Name: "x", Description: "a\u200bb"},
		} {
			b := bad
			if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertPackageGroup(ctx, &b) }); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			return tx.InsertPackageGroup(ctx, &store.PackageGroup{TenantID: tn.ID, Name: "platform"})
		}); !errors.Is(err, store.ErrExists) {
			t.Fatalf("the same name: %v", err)
		}

		// members: added and removed by name, skipping what is already so
		change := func(add, remove []string) (added, removed []string) {
			t.Helper()
			inTx(t, s, func(tx *store.Tx) error {
				var err error
				added, removed, err = tx.ChangeGroupMembers(ctx, g, add, remove, "os:1:t")
				return err
			})
			return added, removed
		}
		if a, r := change([]string{"tresor", "acl", "tresor"}, []string{"nope"}); !slices.Equal(a, []string{"tresor", "acl"}) || r != nil {
			t.Fatalf("added %v removed %v", a, r)
		}
		if a, r := change([]string{"acl", "hugr_node"}, []string{"tresor"}); !slices.Equal(a, []string{"hugr_node"}) || !slices.Equal(r, []string{"tresor"}) {
			t.Fatalf("added %v removed %v", a, r)
		}
		ms, err := s.GroupMembers(ctx, g.ID)
		if err != nil || len(ms) != 2 || ms[0].Name != "acl" || ms[1].Name != "hugr_node" || ms[0].AddedBy != "os:1:t" {
			t.Fatalf("members %+v %v", ms, err)
		}

		// grants on the group live apart from grants; the duplicate check; audit and both targets refused
		gg := store.Grant{TenantID: tn.ID, IssuerID: is.ID, Kind: store.PrincipalRole, Value: "team", ChannelID: ch.ID,
			PackageGroupID: g.ID, Verbs: []string{"install"}, CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertGrant(ctx, &gg) })
		dup := gg
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertGrant(ctx, &dup) }); !errors.Is(err, store.ErrExists) {
			t.Fatalf("the same grant: %v", err)
		}
		other := store.Grant{TenantID: tn.ID, IssuerID: is.ID, Kind: store.PrincipalRole, Value: "team", Verbs: []string{"install"}}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertGrant(ctx, &other) }) // the same principal tenant-wide: another grant
		pg := store.Grant{TenantID: tn.ID, Kind: store.PrincipalPublisher, PublisherID: pub.ID, PackageGroupID: g.ID,
			Verbs: []string{"publish"}, CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertGrant(ctx, &pg) })
		for name, bad := range map[string]store.Grant{
			"audit": {TenantID: tn.ID, IssuerID: is.ID, Kind: "role", Value: "x", PackageGroupID: g.ID, Verbs: []string{"audit"}},
			// spec 0011: feed on the tenant or a channel only
			"feed on a group":      {TenantID: tn.ID, IssuerID: is.ID, Kind: "role", Value: "x", PackageGroupID: g.ID, Verbs: []string{"feed"}},
			"feed on an extension": {TenantID: tn.ID, IssuerID: is.ID, Kind: "role", Value: "x", Package: "acl", Verbs: []string{"feed"}},
			"feed issuer-wide":     {TenantID: tn.ID, IssuerID: is.ID, Kind: "issuer", Verbs: []string{"feed"}},
			"both":                 {TenantID: tn.ID, IssuerID: is.ID, Kind: "role", Value: "x", Package: "acl", PackageGroupID: g.ID, Verbs: []string{"install"}},
		} {
			b := bad
			if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertGrant(ctx, &b) }); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}

		// the tenant's auth: group grants apart, with their members; the lock-out count ignores them
		ta, err := s.GetTenantAuth(ctx, tn.ID)
		if err != nil || len(ta.Grants) != 1 || len(ta.GroupGrants) != 2 {
			t.Fatalf("auth %+v %v", ta, err)
		}
		for _, x := range ta.GroupGrants {
			if x.PackageGroupName != "platform" || !x.Members["acl"] || x.Members["tresor"] || x.Package != "" {
				t.Fatalf("group grant %+v", x)
			}
		}
		if ta.GroupGrants[0].ChannelName != "prod" || ta.Groups[g.ID].Name != "platform" || ta.Version == 0 {
			t.Fatalf("auth %+v", ta)
		}
		admin := store.Grant{TenantID: tn.ID, IssuerID: is.ID, Kind: "role", Value: "owners", PackageGroupID: g.ID, Verbs: []string{"admin"}}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertGrant(ctx, &admin) })
		inTx(t, s, func(tx *store.Tx) error {
			if n, err := tx.TenantAdminGrants(ctx, tn.ID); err != nil || n != 0 {
				return fmt.Errorf("tenant admin grants %d %w", n, err)
			}
			return nil
		})
		// every write moved the version
		before := ta.Version
		change([]string{"duckdb_acl"}, nil)
		if ta, _ = s.GetTenantAuth(ctx, tn.ID); ta.Version <= before || !ta.GroupGrants[0].Members["duckdb_acl"] {
			t.Fatalf("after a member change: %d %+v", ta.Version, ta.GroupGrants[0].Members)
		}
		gr, err := s.GetPackageGroup(ctx, tn.ID, "platform")
		if err != nil || gr.Members != 3 || gr.Grants != 3 || gr.Version != 1 {
			t.Fatalf("group %+v %v", gr, err)
		}
		if gs, err := s.ListPackageGroups(ctx, tn.ID, "acl"); err != nil || len(gs) != 1 {
			t.Fatalf("groups holding acl %v %v", gs, err)
		}
		if gs, err := s.ListPackageGroups(ctx, tn.ID, "tresor"); err != nil || len(gs) != 0 {
			t.Fatalf("groups holding tresor %v %v", gs, err)
		}

		// a grant id is found in either table
		inTx(t, s, func(tx *store.Tx) error { return tx.DeleteGrant(ctx, tn.ID, admin.ID) })
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.DeleteGrant(ctx, tn.ID, admin.ID) }); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a removed grant: %v", err)
		}
		// removing the publisher, then the issuer, removes their grants on groups too
		inTx(t, s, func(tx *store.Tx) error { return tx.DeletePublisher(ctx, tn.ID, "ci") })
		inTx(t, s, func(tx *store.Tx) error {
			if granted, err := tx.GroupGranted(ctx, g); err != nil || !granted {
				return fmt.Errorf("granted %v %w", granted, err)
			}
			return tx.DeleteIssuer(ctx, tn.ID, "corp")
		})
		if ta, _ = s.GetTenantAuth(ctx, tn.ID); len(ta.Grants)+len(ta.GroupGrants) != 0 {
			t.Fatalf("grants left %+v", ta)
		}

		// description and removal compare the version; removal takes the members
		inTx(t, s, func(tx *store.Tx) error { return tx.SetGroupDescription(ctx, &gr, "the platform", 1) })
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.SetGroupDescription(ctx, &gr, "x", 1) }); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a stale description: %v", err)
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.DeletePackageGroup(ctx, gr, 1) }); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("a stale removal: %v", err)
		}
		inTx(t, s, func(tx *store.Tx) error { return tx.DeletePackageGroup(ctx, gr, 2) })
		if _, err := s.GetPackageGroup(ctx, tn.ID, "platform"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a removed group: %v", err)
		}
		if ms, _ := s.GroupMembers(ctx, g.ID); len(ms) != 0 {
			t.Fatalf("members left %v", ms)
		}
	})
}

func TestPackageGroupLimits(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		g := store.PackageGroup{TenantID: tn.ID, Name: "big", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPackageGroup(ctx, &g) })
		names := func(from, n int) []string {
			var out []string
			for i := from; i < from+n; i++ {
				out = append(out, fmt.Sprintf("x%04d", i))
			}
			return out
		}
		inTx(t, s, func(tx *store.Tx) error {
			_, _, err := tx.ChangeGroupMembers(ctx, g, names(0, store.MaxGroupMembers), nil, "os:1:t")
			return err
		})
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			_, _, err := tx.ChangeGroupMembers(ctx, g, names(store.MaxGroupMembers, 1), nil, "os:1:t")
			return err
		}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("one name too many: %v", err)
		}
		// a full group still takes a swap: removals count first
		inTx(t, s, func(tx *store.Tx) error {
			_, _, err := tx.ChangeGroupMembers(ctx, g, names(store.MaxGroupMembers, 1), names(0, 1), "os:1:t")
			return err
		})
		for i := 1; i < store.MaxPackageGroups; i++ {
			x := store.PackageGroup{TenantID: tn.ID, Name: fmt.Sprintf("g%d", i)}
			inTx(t, s, func(tx *store.Tx) error { return tx.InsertPackageGroup(ctx, &x) })
		}
		if err := s.InTx(ctx, "", func(tx *store.Tx) error {
			return tx.InsertPackageGroup(ctx, &store.PackageGroup{TenantID: tn.ID, Name: "one-more"})
		}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("a group too many: %v", err)
		}
	})
}

// GetTenantAuth is one state while writers change it: each write below adds one member and moves
// the version by one, so the members read must match the version read.
func TestTenantAuthConsistentRead(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		g := store.PackageGroup{TenantID: tn.ID, Name: "platform", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertPackageGroup(ctx, &g) })
		base, err := s.GetTenantAuth(ctx, tn.ID)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			for i := range 30 {
				err := s.InTx(ctx, "kista/tenant-auth/"+tn.ID, func(tx *store.Tx) error {
					_, _, err := tx.ChangeGroupMembers(ctx, g, []string{fmt.Sprintf("x%02d", i)}, nil, "os:1:t")
					return err
				})
				if err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
		for reading := true; reading; {
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
				reading = false
			default:
			}
			ta, err := s.GetTenantAuth(ctx, tn.ID)
			if err != nil {
				t.Fatal(err)
			}
			if n := int64(len(ta.Groups[g.ID].Members)); n != ta.Version-base.Version {
				t.Fatalf("%d members at version %d (base %d)", n, ta.Version, base.Version)
			}
		}
	})
}

// Every write to a table GetTenantAuth reads moves the version; MaxGrants counts both tables; two
// writers at a group's limit cannot pass it together.
func TestPackageGroupWrites(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, _ := fixture(t, s)
		is := store.Issuer{TenantID: tn.ID, Name: "corp", URL: "https://idp.example", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error { return tx.InsertIssuer(ctx, &is) })
		version := func() int64 {
			t.Helper()
			ta, err := s.GetTenantAuth(ctx, tn.ID)
			if err != nil {
				t.Fatal(err)
			}
			return ta.Version
		}
		moved := func(name string, fn func(tx *store.Tx) error) {
			t.Helper()
			before := version()
			inTx(t, s, fn)
			if version() <= before {
				t.Errorf("%s did not move the version", name)
			}
		}
		g := store.PackageGroup{TenantID: tn.ID, Name: "platform", CreatedBy: "os:1:t"}
		moved("a group", func(tx *store.Tx) error { return tx.InsertPackageGroup(ctx, &g) })
		gg := store.Grant{TenantID: tn.ID, IssuerID: is.ID, Kind: "role", Value: "team", PackageGroupID: g.ID, Verbs: []string{"install"}}
		moved("a group grant", func(tx *store.Tx) error { return tx.InsertGrant(ctx, &gg) })
		moved("a group grant's removal", func(tx *store.Tx) error { return tx.DeleteGrant(ctx, tn.ID, gg.ID) })
		moved("a group's removal", func(tx *store.Tx) error { return tx.DeletePackageGroup(ctx, g, 0) })

		// MaxGrants counts grants on groups with the others, in both insert paths
		g = store.PackageGroup{TenantID: tn.ID, Name: "platform", CreatedBy: "os:1:t"}
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.InsertPackageGroup(ctx, &g); err != nil {
				return err
			}
			for i := range store.MaxGrants - 1 {
				x := store.Grant{TenantID: tn.ID, IssuerID: is.ID, Kind: "subject", Value: fmt.Sprintf("u%d", i), Verbs: []string{"install"}}
				if err := tx.InsertGrant(ctx, &x); err != nil {
					return err
				}
			}
			x := store.Grant{TenantID: tn.ID, IssuerID: is.ID, Kind: "role", Value: "team", PackageGroupID: g.ID, Verbs: []string{"install"}}
			return tx.InsertGrant(ctx, &x)
		})
		for name, x := range map[string]store.Grant{
			"a grant":          {TenantID: tn.ID, IssuerID: is.ID, Kind: "subject", Value: "one-more", Verbs: []string{"install"}},
			"a grant on group": {TenantID: tn.ID, IssuerID: is.ID, Kind: "role", Value: "x", PackageGroupID: g.ID, Verbs: []string{"install"}},
		} {
			if err := s.InTx(ctx, "", func(tx *store.Tx) error { return tx.InsertGrant(ctx, &x) }); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s beyond MaxGrants: %v", name, err)
			}
		}

		// two writers, each adding one name to a group one short of its limit, under the auth lock
		var names []string
		for i := range store.MaxGroupMembers - 1 {
			names = append(names, fmt.Sprintf("x%04d", i))
		}
		inTx(t, s, func(tx *store.Tx) error {
			_, _, err := tx.ChangeGroupMembers(ctx, g, names, nil, "os:1:t")
			return err
		})
		errs := make(chan error, 2)
		for _, n := range []string{"last_a", "last_b"} {
			go func() {
				errs <- s.InTx(ctx, "kista/tenant-auth/"+tn.ID, func(tx *store.Tx) error {
					_, _, err := tx.ChangeGroupMembers(ctx, g, []string{n}, nil, "os:1:t")
					return err
				})
			}()
		}
		var refused int
		for range 2 {
			if err := <-errs; errors.Is(err, store.ErrInvalid) {
				refused++
			} else if err != nil {
				t.Fatal(err)
			}
		}
		if ms, _ := s.GroupMembers(ctx, g.ID); refused != 1 || len(ms) != store.MaxGroupMembers {
			t.Fatalf("refused %d, members %d", refused, len(ms))
		}
	})
}
