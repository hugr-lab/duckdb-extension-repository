package config

import (
	"strings"
	"testing"
)

// Spec 0020 phase 4a: the bootstrap section, checked at load.
func TestBootstrapConfig(t *testing.T) {
	const ok = `
bootstrap:
  versions: [ { version: v1.4.1, kind: release, c_apis: [v1.2.0] } ]
  tenant: { name: acme, display_name: Acme, audiences: [api://kista-acme] }
  channels: [ { name: prod, kind: signed, versions: [v1.4.1], key: "azure:kista-channel/abc" } ]
  issuer:
    name: entra
    url: https://login.example/tid/v2.0
    required_claims: { tid: tid }
    roles_claim: roles
    console: { client_id: c1, audience: api://kista-acme }
  grants: [ { principal: "role:entra|kista-admin", verbs: [admin] } ]
`
	cfg, err := Load(write(t, base+ok), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Bootstrap.Set() || cfg.Bootstrap.Issuer.Console.ScopeList()[0] != "openid" || cfg.Source == "" {
		t.Fatalf("loaded: %+v", cfg.Bootstrap)
	}
	const server = "auth: { server_issuers: [ { name: ops, url: 'https://login.example/tid/v2.0', required_claims: { tid: t1 } } ], server_audiences: [api://kista] }\n"
	for name, c := range map[string]struct{ file, want string }{
		"no tenant":                        {base + "bootstrap: { channels: [ { name: prod, kind: signed, key: 'file:a.pem' } ] }\n", "needs a tenant"},
		"a reserved tenant name":           {base + "bootstrap: { tenant: { name: api } }\n", "reserved"},
		"an unknown domain":                {base + "bootstrap: { tenant: { name: acme, domain: nope } }\n", "no blob domain"},
		"a signed channel without a key":   {base + "bootstrap: { tenant: { name: acme }, channels: [ { name: prod, kind: signed } ] }\n", "signer reference"},
		"a passthrough channel with a key": {base + "bootstrap: { tenant: { name: acme }, channels: [ { name: core, kind: passthrough, key: 'file:a.pem' } ] }\n", "holds no key"},
		"a bad DuckDB version":             {base + "bootstrap: { tenant: { name: acme }, channels: [ { name: prod, kind: signed, key: 'file:a.pem', versions: [latest] } ] }\n", "DuckDB version"},
		"a principal of another issuer": {base + `bootstrap: { tenant: { name: acme }, issuer: { name: entra, url: 'https://login.example/x' },
  grants: [ { principal: "role:other|x", verbs: [admin] } ] }
`, "principal of bootstrap.issuer"},
		"an unknown verb": {base + `bootstrap: { tenant: { name: acme }, issuer: { name: entra, url: 'https://login.example/x' },
  grants: [ { principal: "role:entra|x", verbs: [own] } ] }
`, "verbs"},
		"admin to a client": {base + `bootstrap: { tenant: { name: acme }, issuer: { name: entra, url: 'https://login.example/x' },
  grants: [ { principal: "client:entra|app", verbs: [admin] } ] }
`, "never an administrator"},
		"a console audience not the tenant's": {base + `bootstrap: { tenant: { name: acme }, issuer: { name: entra, url: 'https://login.example/x', console: { client_id: c, audience: api://other } } }
`, "one of bootstrap.tenant.audiences"},
		"an issuer-wide principal": {base + `bootstrap: { tenant: { name: acme }, issuer: { name: entra, url: 'https://login.example/x' },
  grants: [ { principal: "issuer:entra", verbs: [install] } ] }
`, "issuer-wide"},
		"an extension group grant": {base + `bootstrap: { tenant: { name: acme }, issuer: { name: entra, url: 'https://login.example/x' },
  grants: [ { principal: "role:entra|x", verbs: [admin], extension_group: core } ] }
`, "makes no extension groups"},
		"a grant twice": {base + `bootstrap: { tenant: { name: acme }, issuer: { name: entra, url: 'https://login.example/x' },
  grants: [ { principal: "role:entra|x", verbs: [admin, install] }, { principal: "role:entra|x", verbs: [install, admin] } ] }
`, "listed twice"},
		"a key on two channels":        {base + "bootstrap: { tenant: { name: acme }, channels: [ { name: a, kind: signed, key: 'file:k.pem' }, { name: b, kind: signed, key: 'file:k.pem' } ] }\n", "listed twice"},
		"an audience under public_url": {base + "serve: { public_url: 'https://kista.example' }\nbootstrap: { tenant: { name: acme, audiences: ['https://kista.example/acme'] } }\n", "under serve.public_url"},
		"an http issuer":               {base + "bootstrap: { tenant: { name: acme }, issuer: { name: entra, url: 'http://login.example/x' } }\n", "bootstrap.issuer"},
		"a console at a server issuer": {base + server + `bootstrap: { tenant: { name: acme }, issuer: { name: entra, url: 'https://login.example/tid/v2.0', console: { client_id: c } } }
`, "server issuer ops's URL"},
	} {
		if _, err := Load(write(t, c.file), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// the section is file-only: no variable sets it
	if _, err := Load(write(t, base), []string{"KISTA_BOOTSTRAP__TENANT__NAME=acme"}); err == nil {
		t.Error("KISTA_BOOTSTRAP__TENANT__NAME was taken")
	}
}
