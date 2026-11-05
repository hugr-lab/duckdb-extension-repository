package store_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0043: a channel's purged slots of a name, of its source and those purged before slots had a
// source ('*'), never another source's, name's or channel's, by version, platform and slot.
func TestPurgedSlots(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		other := store.Channel{TenantID: tn.ID, Name: "staging", Kind: store.ChannelSigned}
		inTx(t, s, func(tx *store.Tx) error { return tx.CreateChannel(ctx, &other) })
		now := store.TimeArg(s, time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
		n := 0
		purge := func(channelID, source, name, version, platform, slot, abi string) {
			t.Helper()
			n++
			if err := store.ExecRaw(ctx, s, "INSERT INTO purged_slots (channel_id, source, name, package_version, platform, slot, abi, body_hash, seq, "+
				"release_id, purged_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)", channelID, source, name, version, platform, slot, abi,
				strings.Repeat("a", 64), fmt.Sprintf("r%d", n), now); err != nil {
				t.Fatal(err)
			}
		}
		purge(ch.ID, "tenant", "relay", "1.1", "osx_arm64", "v2.0.0", "cpp")
		purge(ch.ID, "tenant", "relay", "1.1", "linux_amd64", "v2.0.0", "cpp")
		purge(ch.ID, "*", "relay", "1.0", "linux_amd64", "v1.4.0", "cpp") // before slots had a source
		purge(ch.ID, "upstream:x", "relay", "2.0", "linux_amd64", "v2.0.0", "cpp")
		purge(ch.ID, "tenant", "beacon", "1.0", "linux_amd64", "v2.0.0", "cpp")
		purge(other.ID, "tenant", "relay", "3.0", "linux_amd64", "v2.0.0", "cpp")
		got, err := s.PurgedSlots(ctx, ch.ID, "tenant", "relay")
		if err != nil {
			t.Fatal(err)
		}
		want := []store.PurgedSlot{{PackageVersion: "1.0", Platform: "linux_amd64", Slot: "v1.4.0", ABI: "cpp"},
			{PackageVersion: "1.1", Platform: "linux_amd64", Slot: "v2.0.0", ABI: "cpp"}, {PackageVersion: "1.1", Platform: "osx_arm64", Slot: "v2.0.0", ABI: "cpp"}}
		if !slices.Equal(got, want) {
			t.Fatalf("relay's purged slots in prod: %+v", got)
		}
		if got, err := s.PurgedSlots(ctx, other.ID, "tenant", "beacon"); err != nil || len(got) != 0 {
			t.Fatalf("none: %+v %v", got, err)
		}
	})
}

func oe(channel, next string) store.OrderEdge { return store.OrderEdge{Channel: channel, Next: next} }

// Spec 0043: the channels' order is replaced whole, per package type; a cycle (of two channels, of
// three), a channel its own next, two channels sharing a next, a channel named twice, a passthrough
// channel, another type's channel and another tenant's are refused, the order unchanged; an
// unchanged order changes nothing; an empty one clears it.
func TestChannelOrder(t *testing.T) {
	defer pkgtype.RegisterForTest("adbc_driver")()
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		// an older replica's reaper does not know the table, which references channels
		if n, err := store.CountRaw(ctx, s, "schema_migrations", "version = 52 AND min_reader = 52"); err != nil || n != 1 {
			t.Fatalf("migration 52 raises min_reader: %d %v", n, err)
		}
		tn, _ := fixture(t, s) // prod
		other := store.Tenant{Name: "globex", DisplayName: "Globex"}
		inTx(t, s, func(tx *store.Tx) error {
			if err := tx.CreateTenant(ctx, &other); err != nil {
				return err
			}
			for _, c := range []store.Channel{{TenantID: tn.ID, Name: "nightly", Kind: store.ChannelSigned},
				{TenantID: tn.ID, Name: "staging", Kind: store.ChannelSigned}, {TenantID: tn.ID, Name: "core", Kind: store.ChannelPassthrough},
				{TenantID: tn.ID, Name: "drivers", Kind: store.ChannelSigned, PackageType: "adbc_driver"},
				{TenantID: tn.ID, Name: "drivers-prod", Kind: store.ChannelSigned, PackageType: "adbc_driver"},
				{TenantID: other.ID, Name: "elsewhere", Kind: store.ChannelSigned}} {
				if err := tx.CreateChannel(ctx, &c); err != nil {
					return err
				}
			}
			return nil
		})
		set := func(typ string, edges ...store.OrderEdge) ([]store.OrderEdge, bool, error) {
			t.Helper()
			var before []store.OrderEdge
			var changed bool
			err := s.InTenantTxForUpdate(ctx, tn.ID, nil, func(tx *store.Tx) error {
				var err error
				before, changed, err = tx.SetChannelOrder(ctx, tn.ID, typ, edges, "os:1:t")
				return err
			})
			return before, changed, err
		}
		order := func(typ string) []store.OrderEdge {
			t.Helper()
			o, err := s.ChannelOrder(ctx, tn.ID, typ)
			if err != nil {
				t.Fatal(err)
			}
			return o
		}
		const def = "duckdb_extension"
		chain := []store.OrderEdge{{Channel: "staging", Next: "prod"}, {Channel: "nightly", Next: "staging"}}
		if before, changed, err := set(def, chain...); err != nil || !changed || len(before) != 0 {
			t.Fatalf("a first chain: %v %v %v", before, changed, err)
		}
		want := []store.OrderEdge{{Channel: "nightly", Next: "staging"}, {Channel: "staging", Next: "prod"}}
		if got := order(def); !slices.Equal(got, want) {
			t.Fatalf("the chain, by channel: %v", got)
		}
		if before, changed, err := set(def, want...); err != nil || changed || !slices.Equal(before, want) {
			t.Fatalf("the same order again changes nothing: %v %v %v", before, changed, err)
		}
		for _, c := range []struct {
			name  string
			typ   string
			edges []store.OrderEdge
			says  string
		}{
			{"a cycle of two", def, []store.OrderEdge{oe("nightly", "staging"), oe("staging", "nightly")}, "cycle"},
			{"a cycle of three", def, []store.OrderEdge{oe("nightly", "staging"), oe("staging", "prod"), oe("prod", "nightly")}, "cycle"},
			{"its own next", def, []store.OrderEdge{oe("prod", "prod")}, "its own next"},
			{"two channels sharing a next", def, []store.OrderEdge{oe("nightly", "prod"), oe("staging", "prod")}, "twice as a next"},
			{"a channel named twice", def, []store.OrderEdge{oe("nightly", "staging"), oe("nightly", "prod")}, "twice as a channel"},
			{"a passthrough channel", def, []store.OrderEdge{oe("core", "prod")}, "passthrough"},
			{"another type's channel", def, []store.OrderEdge{oe("staging", "drivers")}, "serves adbc_driver"},
			{"another tenant's channel", def, []store.OrderEdge{oe("staging", "elsewhere")}, "not a channel of the tenant"},
			{"an unknown channel", def, []store.OrderEdge{oe("nope", "prod")}, "not a channel of the tenant"},
			{"the default type's channel in another type's order", "adbc_driver", []store.OrderEdge{oe("drivers", "prod")}, "serves duckdb_extension"},
		} {
			if _, _, err := set(c.typ, c.edges...); !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), c.says) {
				t.Errorf("%s: %v", c.name, err)
			}
		}
		if got := order(def); !slices.Equal(got, want) {
			t.Fatalf("a refused order changed nothing: %v", got)
		}
		// another type's order is its own: set beside the default type's, cleared apart
		drivers := []store.OrderEdge{{Channel: "drivers", Next: "drivers-prod"}}
		if _, changed, err := set("adbc_driver", drivers...); err != nil || !changed {
			t.Fatalf("another type's order: %v %v", changed, err)
		}
		if got := order("adbc_driver"); !slices.Equal(got, drivers) || !slices.Equal(order(def), want) {
			t.Fatalf("two types' orders: %v %v", got, order(def))
		}
		// a shorter chain replaces the whole order of the type
		if before, changed, err := set(def, store.OrderEdge{Channel: "staging", Next: "prod"}); err != nil || !changed || !slices.Equal(before, want) {
			t.Fatalf("a replacement: %v %v %v", before, changed, err)
		}
		// a reversed chain (each channel's next moves): the unique key holds within the transaction
		if _, _, err := set(def, store.OrderEdge{Channel: "prod", Next: "staging"}, store.OrderEdge{Channel: "staging", Next: "nightly"}); err != nil {
			t.Fatal(err)
		}
		if _, changed, err := set(def); err != nil || !changed || len(order(def)) != 0 {
			t.Fatalf("an empty order clears it: %v %v", changed, err)
		}
		if got := order("adbc_driver"); !slices.Equal(got, drivers) {
			t.Fatalf("clearing one type's order leaves another's: %v", got)
		}
		if _, changed, err := set(def); err != nil || changed {
			t.Fatalf("clearing an empty order changes nothing: %v %v", changed, err)
		}
	})
}
