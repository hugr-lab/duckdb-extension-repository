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
		"under public_url":   {base + "serve: { public_url: https://k.example }\n" + iss + "server_audiences: [ 'https://k.example/acme' ] }\n", nil, "canonical"},
		"bad egress cidr":    {base + "auth: { server_egress_allow: [ { cidr: host.example } ] }\n", nil, "not a CIDR"},
		"short max age":      {base + "auth: { admin_token_max_age: 10s }\n", nil, "1m..24h"},
		"bad claim path":     {base + "auth: { server_issuers: [ { name: ops, url: 'https://x.example', roles_claim: 'a..b', required_claims: { a: b } } ] }\n", nil, "claim path"},
	} {
		_, err := Load(write(t, c.file), c.env)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
	// the public URL itself may be a server audience
	if _, err := Load(write(t, base+"serve: { public_url: https://k.example }\n"+iss+"server_audiences: [ 'https://k.example' ] }\n"), nil); err != nil {
		t.Errorf("public_url as the server audience: %v", err)
	}
}
