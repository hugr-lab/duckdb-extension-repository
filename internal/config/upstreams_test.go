package config

import (
	"strings"
	"testing"
	"time"
)

func TestUpstreamsConfig(t *testing.T) {
	cfg, err := Load(write(t, base), nil)
	if err != nil {
		t.Fatal(err)
	}
	if u := cfg.UpstreamLimits(); u.Concurrency != 4 || u.FetchTimeout != 10*time.Minute || u.MinRate != 64<<10 || u.Interval != 6*time.Hour {
		t.Fatalf("defaults: %+v", u)
	}
	cfg, err = Load(write(t, base+"upstreams: { concurrency: 8, fetch_timeout: 30m, min_rate: 1MiB }\n"), nil)
	if err != nil || cfg.UpstreamLimits().Concurrency != 8 || cfg.UpstreamLimits().MinRate != 1<<20 {
		t.Fatalf("set: %+v %v", cfg.Upstreams, err)
	}
	for name, c := range map[string]struct{ file, want string }{
		"concurrency": {base + "upstreams: { concurrency: 64 }\n", "upstreams.concurrency"},
		"timeout":     {base + "upstreams: { fetch_timeout: 5s }\n", "upstreams.fetch_timeout"},
		"rate":        {base + "upstreams: { min_rate: 10 }\n", "upstreams.min_rate"},
		"interval":    {base + "upstreams: { interval: 1m }\n", "upstreams.interval"},
		"negative":    {base + "upstreams: { negative_ttl: 5s }\n", "upstreams.negative_ttl"},
	} {
		if _, err := Load(write(t, c.file), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Load(write(t, base), []string{"KISTA_UPSTREAMS__CONCURRENCY=2"}); err == nil {
		t.Error("upstreams from the environment")
	}
}

// Spec 0009 phase 3: credentials are configuration, validated.
func TestUpstreamCredentials(t *testing.T) {
	ok := `upstreams:
  credentials:
    - { name: enterest-acme, tenants: [acme], prefixes: ["https://enterest.example/acme/"], kind: client_credentials,
        token_url: "https://idp.example/token", client_id: cid, scope: "api://x/.default", client_auth: key_file, key_file: /k.json,
        allow: [{ cidr: 10.0.0.0/8, ports: [443] }] }
    - { name: cluster-b, tenants: ["*"], prefixes: ["https://kista-b.internal/acme/"], kind: token_file, token_file: /t }
`
	cfg, err := Load(write(t, base+ok), nil)
	if err != nil || len(cfg.Upstreams.Credentials) != 2 {
		t.Fatalf("credentials: %+v %v", cfg.Upstreams.Credentials, err)
	}
	cred := func(fields string) string {
		return base + "upstreams: { credentials: [{ name: c, tenants: [acme], " + fields + " }] }\n"
	}
	tf := `prefixes: ["https://u.example/"], kind: token_file, token_file: /t`
	cc := `prefixes: ["https://u.example/"], kind: client_credentials, token_url: "https://idp.example/t", client_id: cid`
	for name, c := range map[string]struct{ file, want string }{
		"name":          {base + "upstreams: { credentials: [{ name: C, tenants: [acme], " + tf + " }] }\n", "credentials[0].name"},
		"no tenants":    {base + "upstreams: { credentials: [{ name: c, " + tf + " }] }\n", "tenants"},
		"bad tenant":    {base + "upstreams: { credentials: [{ name: c, tenants: ['A!'], " + tf + " }] }\n", "tenants"},
		"no prefixes":   {cred("kind: token_file, token_file: /t"), "prefixes"},
		"http prefix":   {cred(`prefixes: ["http://u.example/"], kind: token_file, token_file: /t`), "prefixes[0]"},
		"user prefix":   {cred(`prefixes: ["https://u:p@u.example/"], kind: token_file, token_file: /t`), "prefixes[0]"},
		"kind":          {cred(`prefixes: ["https://u.example/"], kind: basic`), "kind"},
		"relative file": {cred(`prefixes: ["https://u.example/"], kind: token_file, token_file: t`), "token_file"},
		"token url":     {cred(`prefixes: ["https://u.example/"], kind: client_credentials, token_url: "http://idp.example/t", client_id: cid, client_auth: file, assertion_file: /a`), "token_url"},
		"client id":     {cred(`prefixes: ["https://u.example/"], kind: client_credentials, token_url: "https://idp.example/t", client_auth: file, assertion_file: /a`), "client_id"},
		"client auth":   {cred(cc + ", client_auth: password"), "client_auth"},
		"azure":         {cred(cc + ", client_auth: azure"), "azure.identity"},
		"two secrets":   {cred(cc + ", client_auth: secret, client_secret_file: /s, client_secret_env: S"), "client_secret"},
		"no secret":     {cred(cc + ", client_auth: secret"), "client_secret"},
		"allow":         {cred(cc + ", client_auth: file, assertion_file: /a, allow: [{ cidr: x }]"), "allow[0]"},
		"twice": {base + "upstreams: { credentials: [{ name: c, tenants: [acme], " + tf + " }, { name: c, tenants: [acme], " + tf + " }] }\n",
			"credentials[1].name"},
	} {
		if _, err := Load(write(t, c.file), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
