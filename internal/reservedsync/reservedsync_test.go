package reservedsync_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reservedsync"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

const commit = "250f2d603d5b2fc121ec7d1e2067668383e2f192"

// tarball is duckdb/community-extensions as git archive writes it.
func tarball(t *testing.T, withCommit bool, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if withCommit {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": commit}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range names {
		body := "extension:\n  name: " + n + "\n"
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "community-extensions-HEAD/extensions/" + n + "/description.yml",
			Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCommunitySync(t *testing.T) {
	ctx := context.Background()
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			t.Cleanup(reserved.SetForTest(reserved.Seed(), 0))
			st := e.Open(t)
			var body atomic.Value
			body.Store(tarball(t, true, "h3", "kista_comm_a", "kista_comm_b"))
			var status, hits atomic.Int32
			status.Store(http.StatusOK)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(int(status.Load()))
				w.Write(body.Load().([]byte))
			}))
			defer srv.Close()
			eg, err := egress.New(egress.Config{AllowLoopbackHTTP: true})
			if err != nil {
				t.Fatal(err)
			}
			s := &reservedsync.Sync{Store: st, Fetch: eg, URL: srv.URL + "/tar.gz/HEAD", Every: time.Hour, Holder: "a",
				Log: slog.New(slog.DiscardHandler)}
			v0, _ := st.ReservedVersion(ctx)
			added, err := s.Once(ctx)
			if err != nil || !slices.Equal(added, []string{"kista_comm_a", "kista_comm_b"}) {
				t.Fatalf("first pass: %v %v", added, err)
			}
			l, _ := st.ListReservedNames(ctx, pkgtype.DuckDBExtension, "kista_comm_a")
			if len(l) != 1 || l[0].Kind != store.ReservedCommunity || !slices.Equal(l[0].Sources, []string{"community-sync@" + commit}) {
				t.Fatalf("reserved: %+v", l)
			}
			if v, _ := st.ReservedVersion(ctx); v != v0+1 {
				t.Fatalf("version %d after %d", v, v0)
			}
			if evs, _ := st.ListEvents(ctx, audit.ServerTenant, store.EventFilter{Kind: "reserved.add"}); len(evs) != 1 || evs[0].Actor != reservedsync.Actor {
				t.Fatalf("events: %+v", evs)
			}
			// again: nothing new; h3 (in the seed) never got this source
			if added, err := s.Once(ctx); err != nil || len(added) != 0 {
				t.Fatalf("second pass: %v %v", added, err)
			}
			if l, _ := st.ListReservedNames(ctx, pkgtype.DuckDBExtension, "h3"); len(l) != 1 || slices.Contains(l[0].Sources, "community-sync@"+commit) {
				t.Fatalf("h3: %+v", l)
			}
			// another replica within the interval: no fetch
			before := hits.Load()
			other := *s
			other.Holder = "b"
			if added, err := other.Once(ctx); err != nil || len(added) != 0 || hits.Load() != before {
				t.Fatalf("another replica: %v %v, %d fetches", added, err, hits.Load()-before)
			}
			// a name a tenant publishes under is skipped; too many new names are refused
			tn := store.Tenant{Name: "acme"}
			ch := store.Channel{Name: "prod", Kind: store.ChannelSigned}
			if err := st.InTx(ctx, "", func(tx *store.Tx) error {
				if err := tx.CreateTenant(ctx, &tn); err != nil {
					return err
				}
				ch.TenantID = tn.ID
				if err := tx.CreateChannel(ctx, &ch); err != nil {
					return err
				}
				b := store.Build{TenantID: tn.ID, Name: "kista_theirs", PackageVersion: "1.0", Platform: "linux_amd64", ABI: store.ABICPP,
					DuckDBVersion: "v2.0.0", BodyHash: strings.Repeat("a", 64), Origin: store.OriginAdmin, CreatedBy: "os:1:t"}
				if err := tx.FindOrInsertBuild(ctx, &b); err != nil {
					return err
				}
				r := store.Release{TenantID: tn.ID, ChannelID: ch.ID, BuildID: b.ID, Name: b.Name, PackageVersion: b.PackageVersion,
					Platform: b.Platform, Slot: b.Slot(), State: store.ReleaseActive, Visibility: store.Public}
				return tx.InsertRelease(ctx, &r, "os:1:t")
			}); err != nil {
				t.Fatal(err)
			}
			body.Store(tarball(t, true, "kista_theirs", "kista_comm_d"))
			if added, err := s.Once(ctx); err != nil || !slices.Equal(added, []string{"kista_comm_d"}) {
				t.Fatalf("a tenant's name: %v %v", added, err)
			}
			if l, _ := st.ListReservedNames(ctx, pkgtype.DuckDBExtension, "kista_theirs"); len(l) != 0 {
				t.Fatalf("a tenant's name was reserved: %+v", l)
			}
			many := make([]string, reservedsync.MaxNew+1)
			for i := range many {
				many[i] = fmt.Sprintf("kista_many_%03d", i)
			}
			body.Store(tarball(t, true, many...))
			if added, err := s.Once(ctx); err == nil || len(added) != 0 {
				t.Fatalf("too many new names: %d %v", len(added), err)
			}
			// a failed read, a tarball without its commit: an error, nothing changed
			v1, _ := st.ReservedVersion(ctx)
			status.Store(http.StatusBadGateway)
			if _, err := s.Once(ctx); err == nil {
				t.Fatal("a 502 was read")
			}
			status.Store(http.StatusOK)
			body.Store(tarball(t, false, "kista_comm_c"))
			if _, err := s.Once(ctx); err == nil {
				t.Fatal("a tarball without its commit was read")
			}
			if v, _ := st.ReservedVersion(ctx); v != v1 {
				t.Fatal("a failed pass changed the list")
			}
			// Run records a failed pass as an event; it ends with its context
			rctx, cancel := context.WithCancel(ctx)
			other.Holder = "c"
			if err := st.ReleaseLease(ctx, "kista/reserved/community-sync", "a"); err != nil {
				t.Fatal(err)
			}
			go func() { time.Sleep(500 * time.Millisecond); cancel() }()
			other.Run(rctx)
			if evs, _ := st.ListEvents(ctx, audit.ServerTenant, store.EventFilter{Kind: "reserved.sync"}); len(evs) != 1 ||
				evs[0].Outcome != "failed" || !strings.Contains(evs[0].Data, `"error":"fetch"`) {
				t.Fatalf("the failed pass's event: %+v", evs)
			}
		})
	}
}

// TestCommunitySyncNetwork reads the real duckdb/community-extensions (KISTA_TEST_NETWORK=1).
func TestCommunitySyncNetwork(t *testing.T) {
	if os.Getenv("KISTA_TEST_NETWORK") != "1" {
		t.Skip("KISTA_TEST_NETWORK is not set")
	}
	t.Cleanup(reserved.SetForTest(reserved.Seed(), 0))
	st := storetest.Engines(t)[0].Open(t)
	eg, err := egress.New(egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	s := &reservedsync.Sync{Store: st, Fetch: eg, URL: config.DefaultCommunitySyncURL, Every: time.Hour, Holder: "net",
		Log: slog.New(slog.DiscardHandler)}
	names, commit, err := s.Read(context.Background())
	if err != nil || len(commit) != 40 || len(names) < 300 || !slices.Contains(names, "h3") {
		t.Fatalf("read: %d names at %q: %v", len(names), commit, err)
	}
	if _, err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
}
