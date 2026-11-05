package upstream_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
	"github.com/hugr-lab/duckdb-extension-repository/internal/usage"
)

// hostsClient is an egress client narrowed by egress.hosts, as app builds the tenants' one.
func hostsClient(t *testing.T, entries ...string) *egress.Client {
	t.Helper()
	h, err := egress.NewHosts(entries)
	if err != nil {
		t.Fatal(err)
	}
	c, err := egress.New(egress.Config{AllowLoopbackHTTP: true, Hosts: h, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// total is how many requests the repository has answered.
func (r *fakeRepo) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, g := range r.gets {
		n += g
	}
	return n
}

// refusedHost checks err is the refusal of host by egress.hosts.
func refusedHost(t *testing.T, what string, err error, host string) {
	t.Helper()
	var he *egress.HostError
	if !errors.As(err, &he) || he.Host != host || !errors.Is(err, egress.ErrHostNotAllowed) {
		t.Fatalf("%s: %v, want the refusal of %s", what, err, host)
	}
}

const hostMsg = "egress: refused: 127.0.0.1 is not on this server's egress host list"

// Spec 0044: an upstream add is refused by its host before any fetch: a repository's prefix, a
// core or community upstream's built-in host; a bundle-mode upstream contacts nothing.
func TestEgressHostsAdd(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.up.Hosts = hostsClient(t, "other.example")
		_, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"}))
		refusedHost(t, "a repository", err, "127.0.0.1")
		for kind, host := range map[string]string{store.UpstreamCore: "extensions.duckdb.org", store.UpstreamCommunity: "community-extensions.duckdb.org"} {
			_, err := en.up.Add(ctx, admin, "acme", upstreamSpec(kind))
			refusedHost(t, kind, err, host)
		}
		if n := en.repo.total(); n != 0 {
			t.Fatalf("the repository was asked %d times", n)
		}
		if _, err := en.up.Add(ctx, admin, "acme", en.bundleSpec("air", "pg")); err != nil { // src.example.org: unlisted
			t.Fatalf("a bundle-mode upstream on an unlisted host: %v", err)
		}
		// listed, the same repository is added
		en.up.Hosts = hostsClient(t, "127.0.0.1")
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"})); err != nil {
			t.Fatalf("a listed host: %v", err)
		}
	})
}

func upstreamSpec(kind string) (sp upstream.Spec) {
	sp.Name, sp.Kind, sp.Channel, sp.Platforms = kind, kind, "prod", []string{"linux_amd64"}
	sp.Entries = []store.UpstreamEntry{{Name: map[string]string{store.UpstreamCore: "inet", store.UpstreamCommunity: "h3"}[kind]}}
	return sp
}

// Spec 0044: a run whose prefix is off the list fails before its cells: no request, no cell record
// touched, the class egress-host, the host in last_run.error, scheduled as a failed run (the next
// interval, not an interrupted run's pause); listed again, it runs as ever.
func TestEgressHostsRun(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.up.Config.Interval = time.Hour
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		u, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"}))
		if err != nil {
			t.Fatal(err)
		}
		if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
			t.Fatalf("the first run: %+v", r)
		}
		before := en.cell(t, "tresor")
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.1", "v2.0.0"), 2, "tresor_duckdb_cpp_init"))
		asked := en.repo.total()
		en.up.Hosts = hostsClient(t, "other.example")
		start := time.Now()
		r := en.sync(t, false) // the request is taken: the run is not an interrupted one
		if r.Error != "the run failed: "+hostMsg || len(r.Counts) != 0 {
			t.Fatalf("the refused run: %+v", r)
		}
		if n := en.repo.total(); n != asked {
			t.Fatalf("the repository was asked %d times", n-asked)
		}
		if c := en.cell(t, "tresor"); c != before {
			t.Fatalf("the cell changed: %+v, was %+v", c, before)
		}
		if _, runs := en.runEvents(t, u.TenantID, "", "egress-host"); runs != 1 {
			t.Fatalf("upstream.run events of class egress-host: %d", runs)
		}
		cur, err := en.up.Get(ctx, admin, "acme", "acme-repo")
		if err != nil || cur.NextRunAt.Before(start.Add(50*time.Minute)) {
			t.Fatalf("the next run (not an interrupted one's): %v %v", cur.NextRunAt, err)
		}
		en.up.Hosts = hostsClient(t, "127.0.0.1")
		if r := en.sync(t, false); r.Error != "" || r.Counts[store.CellReleased] != 1 {
			t.Fatalf("the run once listed: %+v", r)
		}
	})
}

// Spec 0044: a lazy mirror's first request on a host off the list is a fetch that cannot start:
// the cell recorded failed with the refusal, so the client is answered 404; a pull-through miss
// records nothing. Neither asks the repository.
func TestEgressHostsLazyAndPullThrough(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		u := lazySetup(t, en)
		pt, err := en.up.Add(ctx, admin, "acme", en.pullSpec(store.UpstreamEntry{Name: "tresor"}))
		if err != nil {
			t.Fatal(err)
		}
		en.up.Hosts = hostsClient(t, "other.example")
		asked := en.repo.total()
		miss := release.Miss{TenantID: u.TenantID, UpstreamID: u.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "b",
			Principal: "user:alice", Lazy: true}
		p := en.puller()
		if coming, err := p.Ask(ctx, true, miss); err != nil || !coming {
			t.Fatalf("a first ask: %v %v", coming, err)
		}
		p.Offer(miss)
		p.Drain(ctx)
		if c := en.cell(t, "b"); c.Outcome != store.CellFailed || c.Detail != hostMsg {
			t.Fatalf("the lazy cell: %+v", c)
		}
		if coming, err := p.Ask(ctx, true, miss); err != nil || coming {
			t.Fatalf("an ask after the refusal is answered as any (404): %v %v", coming, err)
		}

		pm := release.Miss{TenantID: pt.TenantID, UpstreamID: pt.ID, DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "tresor",
			Principal: "user:alice"}
		p.Offer(pm)
		p.Drain(ctx)
		cs, err := en.up.Cells(ctx, admin, "acme", "pull", "", [3]string{}, 10)
		if err != nil || len(cs) != 0 {
			t.Fatalf("a pull-through miss recorded: %+v %v", cs, err)
		}
		if n := en.repo.total(); n != asked {
			t.Fatalf("the repository was asked %d times", n-asked)
		}
	})
}

// Spec 0044: a bundle export reads a repository source's .well-known through egress, so a source on
// a host off the list is refused, named.
func TestEgressHostsExport(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			g.put(t, "tresor", "1.0", 1)
			if _, err := g.up.Add(ctx, admin, "acme", g.spec(store.UpstreamEntry{Name: "tresor"})); err != nil {
				t.Fatal(err)
			}
			g.sync(t, false)
			g.air(t, "tresor")
			plan := g.plan(t)
			g.up.Hosts = hostsClient(t, "other.example")
			res, _ := g.export(t, plan, upstream.ExportOptions{})
			if len(res.Sources) != 1 || !strings.Contains(res.Sources[0].Error, hostMsg) || !res.Failed() {
				t.Fatalf("the export's sources: %+v", res.Sources)
			}
		})
	}
}

// Spec 0044: the licence relay and the usage forwarder check the host before a token: no token is
// asked for, the relay's result is the refusal, the forwarder's reason egress-host.
func TestEgressHostsRelayAndUsage(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en, _ := privateEnv(t, e)
			idp := &tokens{list: []string{"T-sub"}}
			t.Setenv("KISTA_TEST_SECRET", "x")
			creds, err := credential.New([]config.Credential{{Name: "cc", Tenants: []string{"acme"}, Prefixes: []string{en.repo.srv.URL + "/"},
				Kind: "client_credentials", TokenURL: "https://idp.example/token", ClientID: "cid", ClientAuth: "secret",
				ClientSecretEnv: "KISTA_TEST_SECRET"}}, credential.Deps{Poster: func(config.Credential) (credential.Poster, error) { return idp, nil }})
			if err != nil {
				t.Fatal(err)
			}
			en.up.Credentials = creds
			sp := en.spec(store.UpstreamEntry{Name: "tresor"})
			sp.Credential = "cc"
			u, err := en.up.Add(ctx, admin, "acme", sp)
			if err != nil {
				t.Fatal(err)
			}
			added := idp.n // the add's kista probe takes one
			refusing := hostsClient(t, "other.example")
			svc := &licenses.Service{Store: en.st, Authz: authz.ServerAdmin{}, Enabled: true, Fetch: en.up.Fetch, Hosts: refusing,
				Credentials: creds, Interval: time.Hour, Log: slog.New(slog.DiscardHandler)}
			res, err := svc.RunUpstream(ctx, u.ID)
			if err != nil || res.Error != hostMsg {
				t.Fatalf("the relay: %+v %v", res, err)
			}
			fw := &usage.Forwarder{Store: en.st, Post: en.up.Fetch.(usage.Poster), Hosts: refusing, Credentials: creds, Holder: "test",
				Log: slog.New(slog.DiscardHandler)}
			fr, err := fw.Forward(ctx, u.ID)
			if err != nil || len(fr.Failed) != usage.ForwardDays || len(fr.Sent) != 0 {
				t.Fatalf("the forwarder: %+v %v", fr, err)
			}
			for _, f := range fr.Failed {
				if f.Reason != "egress-host: 127.0.0.1" {
					t.Fatalf("a day's reason: %+v", f)
				}
			}
			if idp.n != added || len(en.repo.posts) != 0 {
				t.Fatalf("%d token requests, %d posts for a refused host", idp.n-added, len(en.repo.posts))
			}
			// egress refusing on its own (no pre-check): the same reason, not unreachable
			fw.Hosts, fw.Post = nil, refusing
			fr, err = fw.Forward(ctx, u.ID)
			if err != nil || len(fr.Failed) != usage.ForwardDays || fr.Failed[0].Reason != "egress-host: 127.0.0.1" {
				t.Fatalf("the forwarder through a refusing client: %+v %v", fr, err)
			}
		})
	}
}
