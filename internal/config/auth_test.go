package config

import (
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

func TestAuthConfig(t *testing.T) {
	ok := base + `
serve: { public_url: https://kista.example }
auth:
  server_issuers:
    - name: ops
      url: https://login.example/tid/v2.0
      required_claims: { tid: t1 }
      roles_claim: roles
      client_claim: azp
  server_audiences: [ "api://kista" ]
  server_egress_allow: [ { cidr: 10.1.0.0/16 } ]
  server_admins: [ "role:ops|kista.admin", "subject:ops|oid-1" ]
  # a principal in both lists, and one twice: accepted
  server_license_admins: [ "client:ops|reconciler", "role:ops|kista.admin", "client:ops|reconciler" ]
  server_distribution_admins: [ "client:ops|enterest" ]
  admin_token_max_age: 30m
`
	cfg, err := Load(write(t, ok), nil)
	if err != nil {
		t.Fatal(err)
	}
	is := cfg.ServerIssuers()
	if len(is) != 1 || is[0].ID != auth.ServerIssuerID("ops") || is[0].RolesClaim[0] != "roles" || len(is[0].Algorithms) == 0 {
		t.Fatalf("issuers: %+v", is)
	}
	admins := cfg.ServerAdmins()
	if len(admins) != 2 || !admins[auth.Key{IssuerID: "server:ops", Kind: store.PrincipalRole, Value: "kista.admin"}] {
		t.Fatalf("admins: %v", admins)
	}
	// spec 0030: the distribution admins
	if da := cfg.ServerDistributionAdmins(); len(da) != 1 || !da[auth.Key{IssuerID: "server:ops", Kind: store.PrincipalClient, Value: "enterest"}] {
		t.Fatalf("distribution admins: %v", da)
	}
	// spec 0029: the licence managers, apart from the administrators
	lm := cfg.ServerLicenseAdmins()
	if len(lm) != 2 || !lm[auth.Key{IssuerID: "server:ops", Kind: store.PrincipalClient, Value: "reconciler"}] ||
		admins[auth.Key{IssuerID: "server:ops", Kind: store.PrincipalClient, Value: "reconciler"}] {
		t.Fatalf("licence managers: %v", lm)
	}
	if cfg.AdminTokenMaxAge() != 30*time.Minute || cfg.ServerAudiences()[0] != "api://kista" {
		t.Fatalf("%v %v", cfg.AdminTokenMaxAge(), cfg.ServerAudiences())
	}
	// defaults: the audience is the public URL, an hour for management tokens
	def, err := Load(write(t, base+"serve: { public_url: https://kista.example }\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if def.AdminTokenMaxAge() != time.Hour || def.ServerAudiences()[0] != "https://kista.example" || def.ServerIssuers() != nil {
		t.Fatalf("defaults: %v %v", def.AdminTokenMaxAge(), def.ServerAudiences())
	}

	iss := "auth: { server_issuers: [ { name: ops, url: 'https://login.example', required_claims: { tid: t1 } } ], "
	for name, c := range map[string]struct {
		file string
		env  []string
		want string
	}{
		"from env":           {base, []string{"KISTA_AUTH__ADMIN_TOKEN_MAX_AGE=5m"}, "can only be set in the config file"},
		"no required claims": {base + "auth: { server_issuers: [ { name: ops, url: 'https://login.example' } ] }\n", nil, "required_claims"},
		"http issuer":        {base + "auth: { server_issuers: [ { name: ops, url: 'http://login.example', required_claims: { a: b } } ] }\n", nil, "must be https"},
		"bad name":           {base + "auth: { server_issuers: [ { name: Ops, url: 'https://x.example', required_claims: { a: b } } ] }\n", nil, "issuer name"},
		"bad algorithm":      {base + "auth: { server_issuers: [ { name: ops, url: 'https://x.example', algorithms: [HS256], required_claims: { a: b } } ] }\n", nil, "algorithm"},
		"twice":              {base + "auth: { server_issuers: [ { name: ops, url: 'https://x.example', required_claims: { a: b } }, { name: ops, url: 'https://y.example', required_claims: { a: b } } ] }\n", nil, "used twice"},
		"no audience":        {base + iss + "}\n", nil, "server_audiences (or serve.public_url) is required"},
		"issuer admin":       {base + iss + "server_audiences: [a], server_admins: [ 'issuer:ops' ] }\n", nil, "issuer: is refused"},
		"unknown issuer":     {base + iss + "server_audiences: [a], server_admins: [ 'role:corp|x' ] }\n", nil, "not a server issuer"},
		"bad admin":          {base + iss + "server_audiences: [a], server_admins: [ 'whatever' ] }\n", nil, "kind:issuer|value"},
		// spec 0029: licence managers are checked as administrators are, and file-only
		"issuer licence manager": {base + iss + "server_audiences: [a], server_license_admins: [ 'issuer:ops' ] }\n", nil, "server_license_admins[0]: issuer: is refused"},
		"unknown licence issuer": {base + iss + "server_audiences: [a], server_license_admins: [ 'role:corp|x' ] }\n", nil, "server_license_admins[0]: corp is not a server issuer"},
		"bad licence manager":    {base + iss + "server_audiences: [a], server_license_admins: [ 'whatever' ] }\n", nil, "server_license_admins[0]: \"whatever\" is kind:issuer|value"},
		"publisher licence mgr":  {base + iss + "server_audiences: [a], server_license_admins: [ 'publisher:ci' ] }\n", nil, "server_license_admins[0]: publisher: is refused"},
		"issuer distribution admin": {base + iss + "server_audiences: [a], server_distribution_admins: [ 'issuer:ops' ] }\n", nil,
			"server_distribution_admins[0]: issuer: is refused"},
		"two narrow roles": {base + iss + "server_audiences: [a], server_license_admins: [ 'role:ops|x' ], server_distribution_admins: [ 'role:ops|x' ] }\n", nil,
			"server_distribution_admins[0]: role:ops|x is also a licence manager"},
		"publisher admin": {base + iss + "server_audiences: [a], server_admins: [ 'publisher:ci' ] }\n", nil, "server_admins[0]: publisher: is refused"},
		// a role, group or client principal of an issuer without the claim never matches
		"role without roles_claim":    {base + iss + "server_audiences: [a], server_license_admins: [ 'role:ops|x' ] }\n", nil, "server_license_admins[0]: role:ops|x never matches: the server issuer ops has no roles_claim"},
		"group without groups_claim":  {base + iss + "server_audiences: [a], server_admins: [ 'group:ops|x' ] }\n", nil, "server_admins[0]: group:ops|x never matches: the server issuer ops has no groups_claim"},
		"client without client_claim": {base + iss + "server_audiences: [a], server_admins: [ 'client:ops|x' ] }\n", nil, "server_admins[0]: client:ops|x never matches: the server issuer ops has no client_claim"},
		"licence managers by env":     {base, []string{"KISTA_AUTH__SERVER_LICENSE_ADMINS=role:ops|x"}, "can only be set in the config file"},
		"under public_url":            {base + "serve: { public_url: https://k.example }\n" + iss + "server_audiences: [ 'https://k.example/acme' ] }\n", nil, "canonical"},
		"bad egress cidr":             {base + "auth: { server_egress_allow: [ { cidr: host.example } ] }\n", nil, "not a CIDR"},
		"short max age":               {base + "auth: { admin_token_max_age: 10s }\n", nil, "auth.admin_token_max_age must be at least 1m"},
		"long max age":                {base + "auth: { admin_token_max_age: 25h }\n", nil, "auth.admin_token_max_age is at most limits.ceilings.admin_token_max_age"},
		"default over a lowered ceiling": {base + "limits: { ceilings: { admin_token_max_age: 30m } }\n", nil,
			"auth.admin_token_max_age is at most limits.ceilings.admin_token_max_age"},
		"max age ceiling 0": {base + "limits: { ceilings: { admin_token_max_age: 0s } }\n", nil, "limits.ceilings.admin_token_max_age is at least 1s"},
		"bad claim path":    {base + "auth: { server_issuers: [ { name: ops, url: 'https://x.example', roles_claim: 'a..b', required_claims: { a: b } } ] }\n", nil, "claim path"},
	} {
		_, err := Load(write(t, c.file), c.env)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
	if c, err := Load(write(t, base+"auth: { admin_token_max_age: 48h }\nlimits: { ceilings: { admin_token_max_age: 48h } }\n"), nil); err != nil || c.AdminTokenMaxAge() != 48*time.Hour {
		t.Errorf("a 48h max age under a raised ceiling: %v", err)
	}
	// the public URL itself may be a server audience
	if _, err := Load(write(t, base+"serve: { public_url: https://k.example }\n"+iss+"server_audiences: [ 'https://k.example' ] }\n"), nil); err != nil {
		t.Errorf("public_url as the server audience: %v", err)
	}
}

func TestPublishConfig(t *testing.T) {
	cfg, err := Load(write(t, base), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := cfg.PublishLimits(); a != 2 || b != 8 {
		t.Fatalf("default limits: %d %d", a, b)
	}
	for name, c := range map[string]struct {
		file string
		env  []string
		want string
	}{
		"from env":                      {base, []string{"KISTA_PUBLISH__MAX_PER_TENANT=4"}, "can only be set in the config file"},
		"providers (spec 0023)":         {base + "publish: { providers: [ { name: gh, url: 'https://x.example' } ] }\n", nil, "removed (spec 0023)"},
		"providers off (spec 0023)":     {base + "publish: { providers: [] }\n", nil, "removed (spec 0023)"},
		"per principal":                 {base + "publish: { max_per_principal: 9, max_per_tenant: 4 }\n", nil, "cannot exceed"},
		"over the default tenant limit": {base + "publish: { max_per_principal: 16 }\n", nil, "cannot exceed"},
		"tenant under the default":      {base + "publish: { max_per_tenant: 1 }\n", nil, "cannot exceed"},
		"server issuer at the provider": {base + "serve: { public_url: https://k.example }\nauth: { server_issuers: [ { name: ops, url: 'https://token.actions.githubusercontent.com', required_claims: { a: b } } ] }\n", nil, "CI token issuer"},
	} {
		_, err := Load(write(t, c.file), c.env)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}
