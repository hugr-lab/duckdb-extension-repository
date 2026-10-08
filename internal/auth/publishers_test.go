package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

func TestRefMatch(t *testing.T) {
	for _, tc := range []struct {
		p, r string
		want bool
	}{
		{"refs/tags/v*", "refs/tags/v1.2.3", true},
		{"refs/tags/v*", "refs/tags/x1", false},
		{"refs/tags/v*", "refs/tags/v1/x", false}, // * stays within a segment
		{"refs/heads/**", "refs/heads/release/1.2", true},
		{"refs/heads/main", "refs/heads/main", true},
		{"refs/heads/main", "refs/heads/main2", false},
		{"refs/**/v*", "refs/tags/v2", true},
		{"refs/*/main", "refs/heads/main", true},
		{"refs/tags/*-rc*", "refs/tags/v1-rc2", true},
	} {
		if got := RefMatch(tc.p, tc.r); got != tc.want {
			t.Errorf("%s ~ %s: %v", tc.p, tc.r, got)
		}
	}
	for _, p := range []string{"heads/main", strings.Repeat("*", 9)} {
		if err := CheckRefPattern(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestProviderVerify(t *testing.T) {
	gh := newIDP(t, "https://token.actions.example")
	now := time.Unix(1_800_000_000, 0)
	p := Provider{Name: "github", URL: gh.url, Verifier: &Verifier{Fetch: gh, Now: func() time.Time { return now }}}
	pub := store.Publisher{ID: "p1", Name: "acl-ci", GitHub: []store.GitHubCredential{{ID: "c1", Provider: "github", OwnerID: "10",
		RepositoryID: "20", Workflow: "hugr-lab/duckdb-acl/.github/workflows/release.yml", Ref: "refs/tags/v*", Environment: "release"}}}
	other := store.Publisher{ID: "p2", Name: "other", GitHub: []store.GitHubCredential{{ID: "c2", Provider: "github", OwnerID: "10",
		RepositoryID: "99", Workflow: "hugr-lab/x/.github/workflows/r.yml"}}}
	// assigned audiences are never a provider token's
	ta := store.TenantAuth{Publishers: []store.Publisher{pub, other}, Audiences: []string{"api://acme"}}
	claims := func(over map[string]any) map[string]any {
		c := map[string]any{"iss": gh.url, "aud": canonical, "sub": "repo:hugr-lab/duckdb-acl:environment:release",
			"iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(), "repository_owner_id": "10", "repository_id": "20",
			"repository": "hugr-lab/duckdb-acl", "workflow_ref": "hugr-lab/duckdb-acl/.github/workflows/release.yml@refs/tags/v1.0.0",
			"ref": "refs/tags/v1.0.0", "environment": "release", "event_name": "push", "sha": "abc", "run_id": "7"}
		for k, v := range over {
			if v == nil {
				delete(c, k)
			} else {
				c[k] = v
			}
		}
		return c
	}
	got, err := p.Verify(ctx, ta, canonical, gh.sign(t, "RS256", "r1", nil, claims(nil)))
	if err != nil || len(got.Publishers) != 1 || got.Publishers[0].Publisher.Name != "acl-ci" || !got.Identity.Principals[PublisherKey("p1")] {
		t.Fatalf("a matching run: %+v %v", got, err)
	}
	if prov := got.Provenance("publisher:acme/acl-ci"); !strings.Contains(prov, `"repository_id":"20"`) || !strings.Contains(prov, `"github:c1"`) {
		t.Fatalf("provenance: %s", prov)
	}
	for name, over := range map[string]map[string]any{
		"another owner":               {"repository_owner_id": "11"},
		"another repository":          {"repository_id": "21"},
		"no repository id":            {"repository_id": nil},
		"a numeric id":                {"repository_id": 20},
		"an @ in the file":            {"workflow_ref": "hugr-lab/duckdb-acl/.github/workflows/release.yml@x.yml@refs/tags/v1.0.0"},
		"another ref in workflow_ref": {"workflow_ref": "hugr-lab/duckdb-acl/.github/workflows/release.yml@refs/heads/main"},
		"merge_group":                 {"event_name": "merge_group"},
		"another workflow":            {"workflow_ref": "hugr-lab/duckdb-acl/.github/workflows/other.yml@refs/tags/v1.0.0"},
		"a branch":                    {"ref": "refs/heads/main"},
		"no environment":              {"environment": nil},
		"pull_request":                {"event_name": "pull_request"},
		"pull_request_target":         {"event_name": "pull_request_target"},
		"workflow_run":                {"event_name": "workflow_run"},
		"an assigned audience":        {"aud": "api://acme"},
		"a long lifetime":             {"exp": now.Add(2 * time.Hour).Unix()},
		"another issuer":              {"iss": "https://idp.example"},
	} {
		if _, err := p.Verify(ctx, ta, canonical, gh.sign(t, "RS256", "r1", nil, claims(over))); !errors.Is(err, ErrToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a credential on another provider does not match
	ta.Publishers[0].GitHub[0].Provider = "ghes"
	if _, err := p.Verify(ctx, ta, canonical, gh.sign(t, "RS256", "r1", nil, claims(nil))); !errors.Is(err, ErrToken) {
		t.Errorf("another provider's credential: %v", err)
	}
	if (Providers{p}).For(gh.url) == nil || (Providers{p}).For(gh.url+"/") != nil {
		t.Error("For matches the issuer exactly")
	}
}

func TestAPIKeys(t *testing.T) {
	key, prefix, hash, err := NewAPIKey()
	if err != nil || !strings.HasPrefix(key, "kista_"+prefix+"_") || len(hash) != 64 || hash != APIKeyHash(key) {
		t.Fatalf("a key: %q %q %q %v", key, prefix, hash, err)
	}
	if !WellFormedAPIKey(key) || !IsAPIKey(key) {
		t.Fatalf("not well formed: %s", key)
	}
	other, _, _, _ := NewAPIKey()
	if other == key {
		t.Fatal("two keys are the same")
	}
	for _, bad := range []string{"kista_", "kista_zzzzzzzz_" + key[15:], strings.ToUpper(key), key + "a", key[:len(key)-1], "kista_" + prefix + "-" + key[15:], key[:len(key)-1] + "1"} {
		if WellFormedAPIKey(bad) {
			t.Errorf("%q is well formed", bad)
		}
	}
}
