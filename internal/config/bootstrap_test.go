package config

import (
	"encoding/json"
	"fmt"
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
		"audit of one type": {base + `bootstrap: { tenant: { name: acme }, issuer: { name: entra, url: 'https://login.example/x' },
  grants: [ { principal: "role:entra|x", verbs: [audit], package_type: duckdb_extension } ] }
`, "audit is granted on the tenant of every package type"},
		"a channel's grant twice, by its type": {base + `bootstrap: { tenant: { name: acme }, issuer: { name: entra, url: 'https://login.example/x' },
  channels: [ { name: prod, kind: signed, key: 'file:a.pem' } ],
  grants: [ { principal: "role:entra|x", verbs: [install], channel: prod }, { principal: "role:entra|x", verbs: [install], channel: prod, package_type: duckdb_extension } ] }
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

// Spec 0024: the section's upstreams, checked at load with upstream add's own checks.
func TestBootstrapUpstreamsConfig(t *testing.T) {
	const fp = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	creds := `upstreams:
  credentials:
    - { name: hosted, tenants: [acme], prefixes: ["https://repo.example/acme/"], kind: token_file, token_file: /t }
    - { name: public-ok, tenants: ["*"], prefixes: ["https://repo.example/"], allow_public: true, kind: token_file, token_file: /t }
`
	sec := func(ups string) string {
		return base + creds + `bootstrap:
  tenant: { name: acme }
  channels:
    - { name: prod, kind: signed, versions: [v1.4.1], key: "file:a.pem" }
    - { name: core, kind: passthrough, versions: [v1.4.1] }
  upstreams:
` + ups
	}
	ok := sec(`    - { name: core, kind: duckdb-core, channel: prod, platforms: [linux_amd64, linux_amd64], extensions: [{ name: httpfs }] }
    - { name: community, kind: duckdb-community, channel: prod, fetch: lazy, platforms: [linux_amd64], extensions: [{ name: h3, eager: true }] }
    - { name: hosted, kind: repository, prefix: "HTTPS://Repo.Example:443/acme/prod/", keys: [` + fp + `], credential: hosted,
        channel: prod, platforms: [linux_amd64], extensions: [{ name: tresor, versions: [1.0.0, 1.0.0] }] }
    - { name: passthrough, kind: duckdb-core, channel: core, platforms: [linux_amd64], extensions: [{ name: json }] }
    - { name: pull, kind: repository, prefix: "https://repo.example/vendor", keys: [` + fp + `], channel: prod, mode: pull-through,
        platforms: [linux_amd64], extensions: [{ name: "*" }] }
`)
	cfg, err := Load(write(t, ok), nil)
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Bootstrap.Defaulted().Upstreams
	if d[2].Prefix != "https://repo.example/acme/prod" || d[2].Visibility != "private" || d[2].Mode != "mirror" ||
		len(d[0].Platforms) != 1 || len(d[2].Extensions[0].Versions) != 1 || d[3].Visibility != "public" ||
		d[0].Fetch != "" || d[1].Fetch != "lazy" || !d[1].StoreEntries()[0].Eager || d[4].Fetch != "" {
		t.Fatalf("defaulted: %+v", d)
	}
	core := "    - { name: core, kind: duckdb-core, channel: prod, platforms: [linux_amd64], extensions: [{ name: httpfs }] }\n"
	repo := func(extra string) string {
		return "    - { name: r, kind: repository, prefix: 'https://repo.example/acme/x', keys: [" + fp + "], channel: prod, platforms: [linux_amd64], extensions: [{ name: tresor }]" + extra + " }\n"
	}
	for name, c := range map[string]struct{ file, want string }{
		"no tenant":                    {base + "bootstrap: { upstreams: [ { name: c, kind: duckdb-core } ] }\n", "needs a tenant"},
		"a name twice":                 {sec(core + core), `upstream "core" is listed twice`},
		"an unknown channel":           {sec("    - { name: c, kind: duckdb-core, channel: nope, platforms: [linux_amd64], extensions: [{ name: httpfs }] }\n"), "none of bootstrap.channels"},
		"bundle mode":                  {sec(repo(", mode: bundle")), "bundle mode is an operator's"},
		"a core with a prefix":         {sec("    - { name: c, kind: duckdb-core, prefix: 'https://x.example', channel: prod, platforms: [linux_amd64], extensions: [{ name: httpfs }] }\n"), "takes no prefix or keys"},
		"a repository, no keys":        {sec("    - { name: r, kind: repository, prefix: 'https://repo.example/x', channel: prod, platforms: [linux_amd64], extensions: [{ name: tresor }] }\n"), "pins at least one key"},
		"an http prefix":               {sec("    - { name: r, kind: repository, prefix: 'http://repo.example/x', keys: [" + fp + "], channel: prod, platforms: [linux_amd64], extensions: [{ name: tresor }] }\n"), "prefix"},
		"a port egress denies":         {sec("    - { name: r, kind: repository, prefix: 'https://repo.example:8443/x', keys: [" + fp + "], channel: prod, platforms: [linux_amd64], extensions: [{ name: tresor }] }\n"), "egress does not reach"},
		"a bad fingerprint":            {sec("    - { name: r, kind: repository, prefix: 'https://repo.example/x', keys: [md5:x], channel: prod, platforms: [linux_amd64], extensions: [{ name: tresor }] }\n"), "fingerprint"},
		"no platforms":                 {sec("    - { name: c, kind: duckdb-core, channel: prod, extensions: [{ name: httpfs }] }\n"), "platforms: at least one"},
		"a bad platform":               {sec("    - { name: c, kind: duckdb-core, channel: prod, platforms: [wasm_eh], extensions: [{ name: httpfs }] }\n"), "platform"},
		"a mirror of nothing":          {sec("    - { name: c, kind: duckdb-core, channel: prod, platforms: [linux_amd64] }\n"), "names at least one"},
		"a core name elsewhere":        {sec("    - { name: c, kind: duckdb-community, channel: prod, platforms: [linux_amd64], extensions: [{ name: httpfs }] }\n"), "allow_reserved"},
		"an alias":                     {sec("    - { name: c, kind: duckdb-core, channel: prod, platforms: [linux_amd64], extensions: [{ name: postgres }] }\n"), "alias"},
		"a star in a community mirror": {sec("    - { name: s, kind: duckdb-community, channel: prod, platforms: [linux_amd64], extensions: [{ name: '*' }] }\n"), "a repository mirror's of a kista channel"},
		"a name on two upstreams": {sec(core + "    - { name: c2, kind: duckdb-core, channel: prod, platforms: [linux_arm64], extensions: [{ name: httpfs }] }\n"),
			`extension of channel prod "httpfs" is listed twice`},
		"a passthrough repository": {sec("    - { name: r, kind: repository, prefix: 'https://repo.example/x', keys: [" + fp + "], channel: core, platforms: [linux_amd64], extensions: [{ name: tresor }] }\n"), "duckdb-core upstreams only"},
		"a private passthrough":    {sec("    - { name: c, kind: duckdb-core, channel: core, visibility: private, platforms: [linux_amd64], extensions: [{ name: httpfs }] }\n"), "releases are public"},
		"a credential on core":     {sec("    - { name: c, kind: duckdb-core, credential: hosted, channel: prod, platforms: [linux_amd64], extensions: [{ name: httpfs }] }\n"), "only a repository upstream"},
		"an unknown credential":    {sec(repo(", credential: nope")), "none of upstreams.credentials"},
		"another prefix":           {sec("    - { name: r, kind: repository, prefix: 'https://repo.example/other/x', keys: [" + fp + "], credential: hosted, channel: prod, platforms: [linux_amd64], extensions: [{ name: tresor }] }\n"), "its prefixes"},
		"public without allow":     {sec(repo(", credential: hosted, visibility: public")), "allow_public"},
		"a bad fetch":              {sec(repo(", fetch: sometimes")), "fetch must be eager or lazy"},
		"a lazy pull-through":      {sec(repo(", mode: pull-through, fetch: lazy")), "only a mirror takes one"},
		"a lazy passthrough":       {sec("    - { name: c, kind: duckdb-core, channel: core, fetch: lazy, platforms: [linux_amd64], extensions: [{ name: httpfs }] }\n"), "no lazy mirror"},
		"an eager pull-through":    {sec("    - { name: r, kind: repository, prefix: 'https://repo.example/acme/x', keys: [" + fp + "], channel: prod, mode: pull-through, platforms: [linux_amd64], extensions: [{ name: tresor, eager: true }] }\n"), "only a mirror's entry is eager"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, c.file), nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
	// a credential for another tenant
	other := strings.Replace(sec(repo(", credential: hosted")), "tenants: [acme]", "tenants: [beta]", 1)
	if _, err := Load(write(t, other), nil); err == nil || !strings.Contains(err.Error(), "not for tenant acme") {
		t.Fatalf("another tenant's credential: %v", err)
	}
	// public with a credential that allows it
	if _, err := Load(write(t, sec("    - { name: r, kind: repository, prefix: 'https://repo.example/x', keys: ["+fp+"], credential: public-ok, visibility: public, channel: prod, platforms: [linux_amd64], extensions: [{ name: tresor }] }\n")), nil); err != nil {
		t.Fatalf("an allowed public credential: %v", err)
	}
}

// The rest of the upstreams' refusals at load: kinds, modes, duplicates, "*", and the limits.
func TestBootstrapUpstreamsLimits(t *testing.T) {
	const fp = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	sec := func(versions, ups string) string {
		return base + `bootstrap:
  tenant: { name: acme }
  channels:
    - { name: prod, kind: signed, versions: [` + versions + `], key: "file:a.pem" }
    - { name: core, kind: passthrough, versions: [v1.4.1] }
  upstreams:
` + ups
	}
	pull := func(name string) string {
		return "    - { name: " + name + ", kind: repository, prefix: 'https://repo.example/" + name + "', keys: [" + fp + "], channel: prod, mode: pull-through, platforms: [linux_amd64], extensions: [{ name: '*' }] }\n"
	}
	var ks []string
	for i := range 11 {
		ks = append(ks, fmt.Sprintf(`"sha256:%064x"`, i))
	}
	keys := strings.Join(ks, ", ")
	plats := []string{"linux_amd64", "linux_arm64", "osx_arm64", "osx_amd64", "windows_amd64", "windows_arm64",
		"linux_amd64_musl", "linux_arm64_musl", "freebsd_amd64", "linux_amd64_gcc4"}
	var vs, names []string
	for i := range 20 {
		vs = append(vs, fmt.Sprintf("v1.%d.0", i))
	}
	for i := range 101 {
		names = append(names, fmt.Sprintf("{ name: ext%d }", i))
	}
	many := "    - { name: big, kind: repository, prefix: 'https://repo.example/x', keys: [" + fp + "], channel: prod, platforms: [" +
		strings.Join(plats, ", ") + "], extensions: [" + strings.Join(names, ", ") + "] }\n"
	for name, c := range map[string]struct{ file, want string }{
		"a bad visibility": {sec("v1.4.1", "    - { name: c, kind: duckdb-core, visibility: open, channel: prod, platforms: [linux_amd64], extensions: [{ name: httpfs }] }\n"), "visibility"},
		"a bad kind":       {sec("v1.4.1", "    - { name: c, kind: duckdb, channel: prod, platforms: [linux_amd64], extensions: [{ name: httpfs }] }\n"), ".kind must be"},
		"a name twice in one upstream": {sec("v1.4.1", "    - { name: c, kind: duckdb-core, channel: prod, platforms: [linux_amd64], extensions: [{ name: httpfs }, { name: httpfs }] }\n"),
			`"httpfs" is listed twice`},
		"pull-through on passthrough": {sec("v1.4.1", "    - { name: c, kind: duckdb-core, mode: pull-through, channel: core, platforms: [linux_amd64], extensions: [{ name: httpfs }] }\n"), "mirrors only"},
		"too many keys":               {sec("v1.4.1", "    - { name: r, kind: repository, prefix: 'https://repo.example/x', keys: ["+keys+"], channel: prod, platforms: [linux_amd64], extensions: [{ name: tresor }] }\n"), "keys: at most"},
		"two stars in a channel":      {sec("v1.4.1", pull("a")+pull("b")), `"*" is listed twice`},
		"a star with versions":        {sec("v1.4.1", "    - { name: r, kind: repository, prefix: 'https://repo.example/x', keys: ["+fp+"], channel: prod, mode: pull-through, platforms: [linux_amd64], extensions: [{ name: '*', versions: [1.0.0] }] }\n"), "pull-through upstream's entry"},
		"a star on core":              {sec("v1.4.1", "    - { name: c, kind: duckdb-core, mode: pull-through, channel: prod, platforms: [linux_amd64], extensions: [{ name: '*' }] }\n"), "takes no core name"},
		"too many cells":              {sec(strings.Join(vs, ", "), many), "cells"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, c.file), nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
	// a section without upstreams keeps the digest it had: the field is left out of the JSON
	b := Bootstrap{Tenant: BootstrapTenant{Name: "acme"}}
	j1, _ := json.Marshal(b.Defaulted())
	b.Upstreams = []BootstrapUpstream{}
	j2, _ := json.Marshal(b.Defaulted())
	if string(j1) != string(j2) || strings.Contains(string(j1), "upstreams") {
		t.Fatalf("the section's JSON changed: %s / %s", j1, j2)
	}
}
