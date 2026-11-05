package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/api"
	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// googleIDP answers like Google's discovery: the issuer on one host, its JWKS on another; and, for
// wrapped.example, an answer egress refused by its host list (the fetch's own refusal, which the
// services wrap in their generic errors).
type googleIDP struct{ idps }

func (g googleIDP) Get(ctx context.Context, url string) ([]byte, http.Header, error) {
	switch url {
	case "https://accounts.example/.well-known/openid-configuration":
		return []byte(`{"issuer":"https://accounts.example","jwks_uri":"https://keys.example/jwks"}`), http.Header{}, nil
	case "https://wrapped.example/.well-known/openid-configuration":
		return nil, nil, &egress.HostError{Host: "wrapped.example"}
	}
	return g.idps.Get(ctx, url)
}

func hostsClient(t *testing.T, entries ...string) *egress.Client {
	t.Helper()
	h, err := egress.NewHosts(entries)
	if err != nil {
		t.Fatal(err)
	}
	c, err := egress.New(egress.Config{AllowLoopbackHTTP: true, Hosts: h})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Spec 0044: an upstream, issuer or feed on a host egress.hosts leaves out is answered 400
// egress-host, naming the host, before the generic answers (the fetch's own refusal, wrapped in
// ErrInvalid or ErrIssuerFetch, too); each answer is an audited refusal with the host. A bundle-mode
// upstream or feed contacts nothing and is added.
func TestEgressHostsAPI(t *testing.T) {
	listed := hostsClient(t, "idp.example", "accounts.example", "wrapped.example", "repo-listed.example")
	m := newMgmt(t, func(o *api.Options) {
		o.Auth.Hosts, o.Upstreams.Hosts, o.Feeds.Hosts = listed, listed, listed
		o.Auth.Fetch = googleIDP{o.Auth.Fetch.(idps)}
		// the upstreams' fetches go through a client that lists nothing: a listed prefix passes the
		// pre-check, and its .well-known read is refused by egress itself
		o.Upstreams.Fetch = hostsClient(t)
	})
	const T = "/api/v1/tenants/acme"
	tom := m.toks["tenant admin"]
	key := `"keys":["sha256:` + strings.Repeat("1", 64) + `"]`
	upstream := func(name, kind, prefix, mode string) string {
		b := `{"name":"` + name + `","kind":"` + kind + `","channel":"prod","platforms":["linux_amd64"],"extensions":[{"name":"x"}]`
		if prefix != "" {
			b += `,"prefix":"` + prefix + `",` + key
		}
		if mode != "" {
			b += `,"mode":"` + mode + `"`
		}
		return b + "}"
	}
	feedKey := `"keys":["sha256:` + strings.Repeat("a", 64) + `"]`
	for name, c := range map[string]struct{ path, body, host string }{
		"a repository prefix":   {"/upstreams", upstream("r", "repository", "https://repo.example/acme/prod", ""), "repo.example"},
		"a core upstream":       {"/upstreams", upstream("c", store.UpstreamCore, "", ""), "extensions.duckdb.org"},
		"a community upstream":  {"/upstreams", upstream("cc", store.UpstreamCommunity, "", ""), "community-extensions.duckdb.org"},
		"the fetch's refusal":   {"/upstreams", upstream("w", "repository", "https://repo-listed.example/a", ""), "repo-listed.example"},
		"an issuer URL":         {"/issuers", `{"name":"x","url":"https://unlisted-idp.example"}`, "unlisted-idp.example"},
		"an explicit jwks_uri":  {"/issuers", `{"name":"y","url":"https://idp.example","jwks_uri":"https://keys.example/jwks"}`, "keys.example"},
		"a discovered jwks_uri": {"/issuers", `{"name":"g","url":"https://accounts.example"}`, "keys.example"},
		"discovery refused":     {"/issuers", `{"name":"z","url":"https://wrapped.example"}`, "wrapped.example"},
		"an online feed": {"/feeds", `{"name":"src","url":"https://source.example/api/v1/tenants/x","credential":"src",` + feedKey + `}`,
			"source.example"},
	} {
		r := m.call(t, "POST", T+c.path, tom, c.body)
		j := r.json(t)
		if r.status != 400 || j["type"] != "urn:kista:problem:egress-host" || j["host"] != c.host || j["title"] != "Host Not Allowed" ||
			j["detail"] != c.host+" is not among the hosts this server may reach (egress.hosts); the server's operator adds it" {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	// bundle mode contacts nothing: added whatever its host
	if r := m.call(t, "POST", T+"/upstreams", tom, upstream("air", "repository", "https://air.example.org/a/b", store.ModeBundle)); r.status != 201 {
		t.Errorf("a bundle-mode upstream: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", T+"/feeds", tom, `{"name":"air","url":"https://air.example.org/api/v1/tenants/x","mode":"bundle",`+feedKey+`}`); r.status != 201 {
		t.Errorf("a bundle-mode feed: %d %s", r.status, r.body)
	}
	// each refusal is recorded with its host (refusals of one caller and reason are counted together,
	// spec 0010: the first written at once, the rest as a count with the last one's host)
	m.events.Close(ctx)
	acme, err := m.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := m.st.ListEvents(ctx, acme.ID, store.EventFilter{Kind: "authz.refused", Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	var hosts []string
	for _, e := range evs {
		var d struct {
			Reason, Host string
			Count        int
		}
		if err := json.Unmarshal([]byte(e.Data), &d); err != nil {
			t.Fatal(err)
		}
		if d.Reason == "egress-host" {
			n += d.Count
			hosts = append(hosts, d.Host)
		}
	}
	if n != 9 || len(hosts) == 0 || slices.Contains(hosts, "") {
		t.Errorf("egress-host refusals: %d with hosts %v, want 9: %+v", n, hosts, evs)
	}
}

// Spec 0044: a feed (spec 0011) on a host egress.hosts leaves out is refused at add (a bundle-mode
// one is added); a feed added before the host left the list fails its run before any token or
// request, with the class egress-host.
func TestEgressHostsFeeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.NotFound(w, nil)
	}))
	t.Cleanup(srv.Close)
	sub := newSubscriber(t, srv, "token")
	feedURL := srv.URL + "/api/v1/tenants/acme"
	fp := "sha256:" + strings.Repeat("a", 64)
	// added while the host was listed (no list), then the list leaves it out (a restart)
	if _, err := sub.feeds.Add(ctx, admin, "acme", attachment.FeedSpec{Name: "src", URL: feedURL, Credential: "src", Keys: []string{fp}}); err != nil {
		t.Fatal(err)
	}
	sub.feeds.Hosts = hostsClient(t, "listed.example")
	var he *egress.HostError
	if _, err := sub.feeds.Add(ctx, admin, "acme", attachment.FeedSpec{Name: "again", URL: feedURL, Credential: "src", Keys: []string{fp}}); !errors.As(err, &he) ||
		he.Host != "127.0.0.1" {
		t.Fatalf("an online feed off the list: %v", err)
	}
	if _, err := sub.feeds.Add(ctx, admin, "acme", attachment.FeedSpec{Name: "air", URL: "https://air.example/api/v1/tenants/x", Mode: store.FeedBundle,
		Keys: []string{fp}}); err != nil {
		t.Fatalf("a bundle-mode feed off the list: %v", err)
	}
	res := sub.run(t, "src")
	if !strings.Contains(res.Error, "127.0.0.1 is not on this server's egress host list") || hits.Load() != 0 {
		t.Fatalf("the run: %+v, %d requests", res, hits.Load())
	}
	if f, _ := sub.feeds.Get(ctx, admin, "acme", "src"); f.Status != store.FeedFailing {
		t.Fatalf("status: %s", f.Status)
	}
	tn, _ := sub.st.GetTenant(ctx, "acme")
	evs, err := sub.st.ListEvents(ctx, tn.ID, store.EventFilter{Kind: "feed.run"})
	if err != nil || len(evs) != 1 || !strings.Contains(evs[0].Data, `"error":"egress-host"`) {
		t.Fatalf("feed.run: %+v %v", evs, err)
	}
}
