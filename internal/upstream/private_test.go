package upstream_test

import (
	"context"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// privateEnv is an env whose repository is served over TLS and takes a Bearer token, with a
// token_file credential "sub" for acme at its prefix.
func privateEnv(t *testing.T, e storetest.Engine) (*env, string) {
	t.Helper()
	en := newEnv(t, e)
	repo := newRepoTLS(t, true)
	repo.keys = en.repo.keys
	en.repo = repo
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: repo.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(repo.srv.URL)
	port, _ := strconv.Atoi(u.Port())
	eg, err := egress.New(egress.Config{CAFile: ca, Allow: []egress.Allow{{Prefix: netip.MustParsePrefix("127.0.0.1/32"), Ports: []uint16{uint16(port)}}}})
	if err != nil {
		t.Fatal(err)
	}
	en.up.Fetch = eg
	tokenFile := filepath.Join(dir, "token")
	creds, err := credential.New([]config.Credential{
		{Name: "sub", Tenants: []string{"acme"}, Prefixes: []string{repo.srv.URL + "/"}, Kind: "token_file", TokenFile: tokenFile},
		{Name: "beta-only", Tenants: []string{"beta"}, Prefixes: []string{repo.srv.URL + "/"}, Kind: "token_file", TokenFile: tokenFile},
		{Name: "elsewhere", Tenants: []string{"acme"}, Prefixes: []string{"https://elsewhere.example/"}, Kind: "token_file", TokenFile: tokenFile},
		{Name: "open", Tenants: []string{"acme"}, Prefixes: []string{repo.srv.URL + "/"}, Kind: "token_file", TokenFile: tokenFile, AllowPublic: true},
	}, credential.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	en.up.Credentials = creds
	return en, tokenFile
}

// Spec 0009 phase 3: a private upstream's credential, who may use it, what it sends, what it
// brings.
func TestPrivateUpstream(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en, tokenFile := privateEnv(t, e)
			en.repo.token = "T-sub"
			if err := os.WriteFile(tokenFile, []byte("T-sub\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
			sp := en.spec(store.UpstreamEntry{Name: "tresor"})
			// who may use a credential: refused alike, unknown or another's
			for name, mutate := range map[string]func(s *struct{ cred, kind, vis string }){
				"another tenant's":  func(s *struct{ cred, kind, vis string }) { s.cred = "beta-only" },
				"another prefix's":  func(s *struct{ cred, kind, vis string }) { s.cred = "elsewhere" },
				"unknown":           func(s *struct{ cred, kind, vis string }) { s.cred = "nope" },
				"public, not open":  func(s *struct{ cred, kind, vis string }) { s.cred, s.vis = "sub", store.Public },
				"on duckdb's kinds": func(s *struct{ cred, kind, vis string }) { s.cred, s.kind = "sub", store.UpstreamCommunity },
			} {
				c := struct{ cred, kind, vis string }{kind: store.UpstreamRepo}
				mutate(&c)
				bad := sp
				bad.Credential, bad.Kind, bad.Visibility = c.cred, c.kind, c.vis
				if c.kind != store.UpstreamRepo {
					bad.Prefix, bad.Keys = "", nil
				}
				if _, err := en.up.Add(ctx, admin, "acme", bad); err == nil {
					t.Errorf("%s: added", name)
				} else if c.cred != "sub" && !strings.Contains(err.Error(), "is not one this upstream may use") {
					t.Errorf("%s: %v", name, err)
				}
			}
			sp.Credential = "sub"
			u, err := en.up.Add(ctx, admin, "acme", sp)
			if err != nil {
				t.Fatal(err)
			}
			if u.Credential != "sub" || u.Visibility != store.Private {
				t.Fatalf("a credentialed upstream is private: %+v", u)
			}
			if _, err := en.up.Set(ctx, admin, "acme", "acme-repo", store.Public, "", 0); err == nil {
				t.Fatal("made public without allow_public")
			}
			// the token goes with the files, and the release is private
			if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
				t.Fatalf("a run: %+v", r)
			}
			rs := en.releases(t, "tresor")
			if len(rs) != 1 || rs[0].Visibility != store.Private {
				t.Fatalf("releases: %+v", rs)
			}
			if len(en.repo.authorizations()) == 0 || en.repo.authorizations()[len(en.repo.authorizations())-1] != "Bearer T-sub" {
				t.Fatalf("Authorization: %q", en.repo.authorizations())
			}
			// a token the upstream refuses: upstream.auth, once (403); a 401 is tried again once
			en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.1", "v2.0.0"), 2, "tresor_duckdb_cpp_init"))
			for tok, tries := range map[string]int{"other": 1, "stale-1": 2} {
				_ = os.WriteFile(tokenFile, []byte(tok), 0o600)
				before := len(en.repo.authorizations())
				if r := en.sync(t, false); r.Counts[store.CellFailed] != 1 {
					t.Fatalf("%s: %+v", tok, r)
				}
				if c := en.cell(t, "tresor"); !strings.HasPrefix(c.Detail, "upstream.auth:") || strings.Contains(c.Detail, tok) {
					t.Fatalf("%s: the cell: %+v", tok, c)
				}
				if n := len(en.repo.authorizations()) - before; n != tries {
					t.Fatalf("%s: %d requests", tok, n)
				}
			}
			// the credential cleared: no token sent, the upstream answers 401
			if _, err := en.up.SetCredential(ctx, admin, "acme", "acme-repo", "", 0); err != nil {
				t.Fatal(err)
			}
			before := len(en.repo.authorizations())
			en.sync(t, false)
			if a := en.repo.authorizations()[before]; a != "" {
				t.Fatalf("a cleared credential sent %q", a)
			}
			// allow_public: a credential that allows public releases lets the upstream be public
			if _, err := en.up.SetCredential(ctx, admin, "acme", "acme-repo", "open", 0); err != nil {
				t.Fatal(err)
			}
			if _, err := en.up.Set(ctx, admin, "acme", "acme-repo", store.Public, "", 0); err != nil {
				t.Fatalf("public with allow_public: %v", err)
			}
			if _, err := en.up.SetCredential(ctx, admin, "acme", "acme-repo", "sub", 0); err == nil {
				t.Fatal("a public upstream took a credential that does not allow public releases")
			}
			evs, _ := en.st.ListEvents(ctx, u.TenantID, store.EventFilter{Kind: "upstream.change", Limit: 100})
			n := 0
			for _, e := range evs {
				if strings.Contains(e.Data, `"change":"credential"`) {
					n++
				}
			}
			if n != 2 {
				t.Fatalf("credential changes recorded: %d", n)
			}
		})
	}
}

// tokens is a fake token endpoint giving out a list of tokens in turn, or an OAuth error.
type tokens struct {
	mu   sync.Mutex
	list []string
	err  string
	n    int
}

func (e *tokens) PostForm(context.Context, string, url.Values) (int, []byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.n++
	if e.err != "" {
		return 400, []byte(`{"error":"` + e.err + `"}`), nil
	}
	tok := e.list[min(e.n-1, len(e.list)-1)]
	return 200, []byte(`{"access_token":"` + tok + `","expires_in":3600}`), nil
}

// errLog records the errors a run logs.
type errLog struct {
	mu   sync.Mutex
	errs []string
}

func (l *errLog) Info(string, ...any) {}
func (l *errLog) Warn(string, ...any) {}
func (l *errLog) Error(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, msg)
}

// Spec 0009 phase 3, review: what reaches the API, intake's visibility, a release's credential,
// client credentials against the upstream, the operator's misconfiguration, redirects.
func TestPrivateUpstreamRules(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en, tokenFile := privateEnv(t, e)
			en.repo.token = "T-sub"
			_ = os.WriteFile(tokenFile, []byte("T-sub"), 0o600)
			if en.up.WithAuthz(authz.ServerAdmin{}).Credentials == nil {
				t.Fatal("the API's service lost the credentials")
			}
			en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
			sp := en.spec(store.UpstreamEntry{Name: "tresor"})
			sp.Credential, sp.Visibility = "open", store.Public
			if _, err := en.up.Add(ctx, admin, "acme", sp); err != nil {
				t.Fatal(err)
			}
			// the operator withdraws allow_public: intake brings private releases, whatever the
			// upstream's visibility says
			reg := en.up.Credentials
			restricted, _ := credential.New([]config.Credential{{Name: "open", Tenants: []string{"acme"}, Prefixes: []string{en.repo.srv.URL + "/"},
				Kind: "token_file", TokenFile: tokenFile}}, credential.Deps{})
			en.up.Credentials = restricted
			if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
				t.Fatalf("a run: %+v", r)
			}
			rel := en.releases(t, "tresor")[0].Release
			if rel.Visibility != store.Private || release.CredentialOf(rel) != "open" {
				t.Fatalf("intake: %+v", rel)
			}
			// the release cannot be made public while its credential does not allow it
			if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "tresor", rel.ID, release.SetPublic, 0); err == nil {
				t.Fatal("a subscription's release made public")
			}
			en.rel.MayPublish = func(c string) bool { return c == "open" }
			if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "tresor", rel.ID, release.SetPrivate, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := en.rel.Apply(ctx, admin, "acme", "prod", "tresor", rel.ID, release.SetPublic, 0); err != nil {
				t.Fatalf("allowed: %v", err)
			}
			en.rel.MayPublish = nil
			// promotion carries the credential
			// (the release is public now, allowed then; the credential no longer allows it: the
			// promotion is private)
			made, _, err := en.rel.Promote(ctx, admin, "acme", "other", "tresor", release.PromoteOptions{From: "prod", Release: rel.ID})
			if err != nil || len(made) != 1 || release.CredentialOf(made[0]) != "open" || made[0].Visibility != store.Private {
				t.Fatalf("promoted: %+v %v", made, err)
			}
			if _, err := en.rel.Apply(ctx, admin, "acme", "other", "tresor", made[0].ID, release.SetPublic, 0); err == nil {
				t.Fatal("a promoted subscription's release made public")
			}
			// a credential gone from the configuration: the cells say so
			en.up.Credentials = credential.Registry{}
			en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.1", "v2.0.0"), 2, "tresor_duckdb_cpp_init"))
			en.sync(t, false)
			if c := en.cell(t, "tresor"); c.Detail != "upstream.auth: the credential is not configured for this upstream" {
				t.Fatalf("a removed credential: %+v", c)
			}
			// client credentials: a cached token the upstream refuses (401) is fetched again once; a
			// token younger than 30 seconds is kept (one token request, not one a cell)
			idp := &tokens{list: []string{"stale-a", "T-sub"}}
			clock := time.Now()
			var cmu sync.Mutex
			cc, err := credential.New([]config.Credential{{Name: "open", Tenants: []string{"acme"}, Prefixes: []string{en.repo.srv.URL + "/"},
				Kind: "client_credentials", TokenURL: "https://idp.example/token", ClientID: "cid", ClientAuth: "secret", ClientSecretEnv: "KISTA_TEST_SECRET"}},
				credential.Deps{Poster: func(config.Credential) (credential.Poster, error) { return idp, nil },
					Now: func() time.Time { cmu.Lock(); defer cmu.Unlock(); return clock }})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("KISTA_TEST_SECRET", "x")
			if tok, err := cc["open"].Token(ctx); err != nil || tok != "stale-a" {
				t.Fatalf("the first token: %q %v", tok, err)
			}
			cc["open"].Invalidate("stale-a") // young: kept
			if tok, _ := cc["open"].Token(ctx); tok != "stale-a" || idp.n != 1 {
				t.Fatalf("a young token forgotten: %q (%d tokens)", tok, idp.n)
			}
			cmu.Lock()
			clock = clock.Add(31 * time.Second)
			cmu.Unlock()
			en.up.Credentials = cc
			if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 || idp.n != 2 {
				t.Fatalf("a refused token, then a new one: %+v (%d tokens)", r, idp.n)
			}
			// the identity provider refusing kista's own client: the run fails, logged, no cell recorded
			lg := &errLog{}
			en.up.Log = lg
			idp.err = "invalid_client"
			cmu.Lock()
			clock = clock.Add(31 * time.Second)
			cmu.Unlock()
			cc["open"].Invalidate("T-sub")
			en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.2", "v2.0.0"), 3, "tresor_duckdb_cpp_init"))
			before := en.cell(t, "tresor")
			if _, err := en.up.Sync(ctx, admin, "acme", "acme-repo", false); err != nil {
				t.Fatal(err)
			}
			en.run.Once(ctx)
			u, _ := en.up.Get(ctx, admin, "acme", "acme-repo")
			if !strings.Contains(u.LastRun, "the identity provider refuses kista's own client") {
				t.Fatalf("the run: %s", u.LastRun)
			}
			if c := en.cell(t, "tresor"); c.Outcome != before.Outcome || !c.FetchedAt.Equal(before.FetchedAt) {
				t.Fatalf("a cell was recorded: %+v", c)
			}
			if len(lg.errs) == 0 {
				t.Fatal("not logged as an error")
			}
			// a redirect is not followed: the token never reaches another host; .well-known never
			// gets one
			other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				t.Errorf("the redirect was followed, Authorization %q", q.Header.Get("Authorization"))
			}))
			defer other.Close()
			_ = os.WriteFile(tokenFile, []byte("T-sub"), 0o600)
			en.up.Credentials = reg
			en.up.Log = slog.New(slog.DiscardHandler)
			en.repo.mu.Lock()
			en.repo.redirects = map[string]string{"/v2.0.0/linux_amd64/tresor.duckdb_extension.gz": other.URL + "/x"}
			en.repo.mu.Unlock()
			en.sync(t, false)
			if c := en.cell(t, "tresor"); c.Outcome != store.CellFailed {
				t.Fatalf("a redirect: %+v", c)
			}
			en.repo.mu.Lock()
			defer en.repo.mu.Unlock()
			for _, a := range en.repo.wellAuth {
				if a != "" {
					t.Fatalf(".well-known got %q", a)
				}
			}
		})
	}
}
