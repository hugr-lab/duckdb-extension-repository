package app

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit/sinks"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

func hostsConfig(t *testing.T, hosts []string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Store = config.Store{Kind: "sqlite", SQLite: config.SQLite{Path: "/tmp/x.db"}, Migrate: "auto", Login: config.Login{Kind: "password"}}
	cfg.Egress.HostsEnforce, cfg.Egress.Hosts = true, hosts
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Spec 0044: the tenants' client applies egress.hosts; an empty list refuses every host (it never
// becomes "no list" on its way); without the switch nothing is narrowed. The operator's clients
// (server issuers, the community sync, event sinks) are never narrowed.
func TestEgressHosts(t *testing.T) {
	cfg := hostsConfig(t, []string{"listed.example"})
	eg, err := Egress(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var he *egress.HostError
	if on, n := eg.HostsOn(); !on || n != 1 || eg.HostAllowed("https://listed.example/x") != nil ||
		!errors.As(eg.HostAllowed("https://unlisted.example/x"), &he) || he.Host != "unlisted.example" {
		t.Fatalf("the tenants' client: %v %d", on, n)
	}
	if _, _, err := eg.Get(context.Background(), "https://unlisted.example/x"); !errors.Is(err, egress.ErrHostNotAllowed) {
		t.Fatalf("a fetch off the list: %v", err)
	}
	none, err := Egress(hostsConfig(t, []string{}))
	if err != nil {
		t.Fatal(err)
	}
	if on, n := none.HostsOn(); !on || n != 0 || none.HostAllowed("https://extensions.duckdb.org/x") == nil {
		t.Fatalf("an empty list: %v %d", on, n)
	}
	off := config.Default()
	if eg, err := Egress(off); err != nil || eg.HostAllowed("https://anything.example") != nil {
		t.Fatalf("no switch: %v", err)
	}
	cfg.Events.Sinks = []config.Sink{{Name: "otel", Kind: "otlp", URL: "https://otel.unlisted.example/v1/logs", Tenants: []string{"*"}}}
	server, err := ServerEgress(cfg)
	if err != nil {
		t.Fatal(err)
	}
	community, err := CommunityEgress(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := EventSinks(cfg, "i1")
	if err != nil || len(ss) != 1 {
		t.Fatalf("sinks: %v", err)
	}
	sink, ok := ss[0].Sender.(*sinks.OTLP).Client.(*egress.Client)
	if !ok {
		t.Fatalf("the sink's client: %T", ss[0].Sender.(*sinks.OTLP).Client)
	}
	for name, c := range map[string]*egress.Client{"server issuers": server, "community sync": community, "an event sink": sink} {
		if on, _ := c.HostsOn(); on || c.HostAllowed("https://unlisted.example/x") != nil {
			t.Errorf("%s: narrowed", name)
		}
	}
}

// Spec 0044: a private upstream's token endpoint is the operator's: its client is not narrowed, so
// a token is taken from a host off the list.
func TestEgressHostsCredentials(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","token_type":"Bearer","expires_in":3600}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	ca, secret := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "secret")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	cfg := hostsConfig(t, []string{"listed.example"})
	cfg.Egress.CAFile = ca
	cfg.Upstreams.Credentials = []config.Credential{{Name: "c", Tenants: []string{"acme"}, Prefixes: []string{"https://listed.example/"},
		Kind: "client_credentials", TokenURL: srv.URL + "/token", ClientID: "kista", ClientAuth: "secret", ClientSecretFile: secret,
		Allow: []config.EgressAllow{{CIDR: "127.0.0.1/32", Ports: []uint16{uint16(port)}}}}}
	creds, err := Credentials(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, err := creds.For("c", "acme", "https://listed.example/x")
	if err != nil {
		t.Fatal(err)
	}
	if tok, err := c.Token(context.Background()); err != nil || tok != "tok" {
		t.Fatalf("a token from a host off the list (port %s): %q %v", strconv.Itoa(port), tok, err)
	}
}

// Spec 0044: at start, one warning per stored host off the list (two spellings of a host are one),
// with its records and tenants: issuers' URLs and explicit jwks_uri (serve's one read of them),
// upstreams' prefixes and DuckDB's built-in hosts, feeds' URLs; bundle-mode upstreams and feeds
// contact nothing and are left out.
func TestWarnEgressHosts(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	admin := authz.Actor{Kind: authz.ActorOS, ID: "1:t"}
	ten := &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}
	ids := map[string]string{}
	for _, name := range []string{"acme", "beta"} {
		tn, err := ten.CreateTenant(ctx, admin, name, "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = tn.ID
	}
	ch, err := ten.CreateChannel(ctx, admin, "acme", "prod", store.ChannelSigned, "")
	if err != nil {
		t.Fatal(err)
	}
	fp := "sha256:" + strings.Repeat("a", 64)
	in := func(tenant string, f func(tx *store.Tx) error) {
		t.Helper()
		if err := st.InTenantTx(ctx, ids[tenant], nil, f); err != nil {
			t.Fatal(err)
		}
	}
	issuer := func(tenant, name, url, jwks string) {
		in(tenant, func(tx *store.Tx) error {
			return tx.InsertIssuer(ctx, &store.Issuer{TenantID: ids[tenant], Name: name, URL: url, JWKSURI: jwks, Algorithms: []string{"RS256"},
				CreatedBy: "t"})
		})
	}
	issuer("acme", "a", "https://idp.unlisted", "https://keys.unlisted/jwks")
	issuer("acme", "b", "https://listed.example", "https://keys.unlisted/jwks")
	issuer("beta", "a", "https://idp.unlisted", "")
	upstream := func(name, kind, prefix, mode string) {
		in("acme", func(tx *store.Tx) error {
			return tx.InsertUpstream(ctx, &store.Upstream{TenantID: ids["acme"], Name: name, Kind: kind, Prefix: prefix, ChannelID: ch.ID,
				Mode: mode, Visibility: store.Private, State: store.UpstreamActive, CreatedBy: "t"})
		})
	}
	upstream("core", store.UpstreamCore, "", store.ModeMirror)
	upstream("repo", store.UpstreamRepo, "https://repo.unlisted/x", store.ModePullThrough)
	upstream("listed", store.UpstreamRepo, "https://listed.example/x", store.ModeMirror)
	upstream("air", store.UpstreamRepo, "https://air.unlisted/x", store.ModeBundle)
	upstream("again", store.UpstreamRepo, "https://REPO.Unlisted./y", store.ModeMirror) // repo.unlisted, spelled otherwise
	feed := func(name, url, mode, cred string) {
		in("acme", func(tx *store.Tx) error {
			return tx.InsertFeed(ctx, &store.Feed{TenantID: ids["acme"], Name: name, URL: url, Credential: cred, Keys: []string{fp},
				Offer: store.OfferUpstreams, Mode: mode, CreatedBy: "t"})
		})
	}
	feed("src", "https://feed.unlisted/api/v1/tenants/x", store.FeedOnline, "c")
	feed("air", "https://airfeed.unlisted/api/v1/tenants/x", store.FeedBundle, "")
	// the reads the warning makes
	if rs, err := st.IssuerURLs(ctx); err != nil || len(rs) != 3 || !slices.ContainsFunc(rs, func(r [4]string) bool {
		return r[0] == ids["acme"] && r[1] == "a" && r[2] == "https://idp.unlisted" && r[3] == "https://keys.unlisted/jwks"
	}) || !slices.ContainsFunc(rs, func(r [4]string) bool { return r[0] == ids["beta"] && r[3] == "" }) {
		t.Fatalf("IssuerURLs: %v %v", rs, err)
	}
	if rs, err := st.UpstreamPrefixes(ctx); err != nil || len(rs) != 5 || !slices.Contains(rs, [5]string{ids["acme"], "core", store.UpstreamCore, store.ModeMirror, ""}) ||
		!slices.Contains(rs, [5]string{ids["acme"], "air", store.UpstreamRepo, store.ModeBundle, "https://air.unlisted/x"}) {
		t.Fatalf("UpstreamPrefixes: %v %v", rs, err)
	}
	if rs, err := st.FeedURLs(ctx); err != nil || len(rs) != 2 ||
		!slices.Contains(rs, [4]string{ids["acme"], "src", store.FeedOnline, "https://feed.unlisted/api/v1/tenants/x"}) {
		t.Fatalf("FeedURLs: %v %v", rs, err)
	}

	var buf bytes.Buffer
	issuers, err := st.IssuerURLs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	warnEgressHosts(ctx, st, hostsConfig(t, []string{"listed.example"}).EgressHosts(), issuers, slog.New(slog.NewJSONHandler(&buf, nil)))
	type warning struct {
		Level, Host      string
		Records, Tenants int
	}
	var got []warning
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var w warning
		if err := json.Unmarshal([]byte(line), &w); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		got = append(got, w)
	}
	want := []warning{{"WARN", "extensions.duckdb.org", 1, 1}, {"WARN", "feed.unlisted", 1, 1}, {"WARN", "idp.unlisted", 2, 2},
		{"WARN", "keys.unlisted", 2, 1}, {"WARN", "repo.unlisted", 2, 1}}
	if !slices.Equal(got, want) {
		t.Fatalf("the warnings: %+v\n%s", got, buf.String())
	}
}
