package config

import (
	"strings"
	"testing"
)

func TestEgressConfig(t *testing.T) {
	ok := base + "egress: { allow: [{ cidr: 10.1.0.0/16, ports: [443, 8443] }, { cidr: 'fd12::/48' }], proxy: 'http://proxy.internal:3128', ca_file: /etc/ca.pem, timeout: 5s }\n"
	cfg, err := Load(write(t, ok), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.EgressAllowPrefixes()) != 2 {
		t.Fatalf("%+v", cfg.Egress)
	}
	for name, c := range map[string]struct {
		file string
		env  []string
		want string
	}{
		"host name":        {base + "egress: { allow: [{ cidr: idp.internal }] }\n", nil, "not a CIDR"},
		"port 0":           {base + "egress: { allow: [{ cidr: 10.0.0.0/8, ports: [0] }] }\n", nil, "not a port"},
		"loopback in prod": {base + "egress: { allow_loopback_http: true }\n", nil, "only with profile dev"},
		"proxy scheme":     {base + "egress: { proxy: 'socks5://p:1080' }\n", nil, "http://host:port"},
		"relative ca":      {base + "egress: { ca_file: ca.pem }\n", nil, "absolute"},
		"timeout":          {base + "egress: { timeout: 5m }\n", nil, "1s..60s"},
		"from env":         {base, []string{"KISTA_EGRESS__PROXY=http://p:1"}, "can only be set in the config file"},
	} {
		if _, err := Load(write(t, c.file), c.env); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

// Spec 0044: egress.hosts, turned on explicitly; a missing or null list with the switch, or a list
// without it, is an error; the entries are checked.
func TestEgressHostsConfig(t *testing.T) {
	on := base + "egress: { hosts_enforce: true, hosts: [extensions.duckdb.org, '*.okta.com', 203.0.113.10, 'Login.Example.'] }\n"
	cfg, err := Load(write(t, on), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := cfg.EgressHosts(); h == nil || h.Len() != 4 {
		t.Fatalf("the list: %+v", h)
	}
	if err := cfg.EgressHosts().Check("https://login.example/x"); err != nil {
		t.Fatalf("a normalised entry: %v", err)
	}
	none, err := Load(write(t, base+"egress: { hosts_enforce: true, hosts: [] }\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := none.EgressHosts(); h == nil || h.Len() != 0 || h.Check("https://extensions.duckdb.org") == nil {
		t.Fatalf("an empty list refuses everything: %+v", h)
	}
	off, err := Load(write(t, base), nil)
	if err != nil {
		t.Fatal(err)
	}
	if off.EgressHosts() != nil {
		t.Fatal("no switch, no list")
	}
	for name, c := range map[string]struct {
		file string
		env  []string
		want string
	}{
		"the switch, no list":      {base + "egress: { hosts_enforce: true }\n", nil, "missing or null"},
		"the switch, a null list":  {base + "egress: { hosts_enforce: true, hosts: null }\n", nil, "missing or null"},
		"a list, no switch":        {base + "egress: { hosts: [a.example] }\n", nil, "without egress.hosts_enforce"},
		"an empty list, no switch": {base + "egress: { hosts: [] }\n", nil, "without egress.hosts_enforce"},
		"a list, the switch off":   {base + "egress: { hosts_enforce: false, hosts: [a.example] }\n", nil, "without egress.hosts_enforce"},
		"a TLD wildcard":           {base + "egress: { hosts_enforce: true, hosts: ['*.com'] }\n", nil, `egress.hosts[0] "*.com"`},
		"a URL":                    {base + "egress: { hosts_enforce: true, hosts: [ok.example, 'https://x.example'] }\n", nil, "egress.hosts[1]"},
		"a port":                   {base + "egress: { hosts_enforce: true, hosts: ['x.example:8443'] }\n", nil, "no port"},
		"a CIDR":                   {base + "egress: { hosts_enforce: true, hosts: [10.0.0.0/8] }\n", nil, "no CIDR"},
		"the switch from env":      {base, []string{"KISTA_EGRESS__HOSTS_ENFORCE=true"}, "can only be set in the config file"},
		"the list from env":        {base, []string{"KISTA_EGRESS__HOSTS=a.example"}, "can only be set in the config file"},
	} {
		if _, err := Load(write(t, c.file), c.env); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

// Spec 0044: with the list on, the bootstrap's issuer and upstreams name hosts on it.
func TestEgressHostsBootstrap(t *testing.T) {
	const fp = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	sec := func(hosts string) string {
		return base + "egress: { hosts_enforce: true, hosts: [" + hosts + "] }\n" + `bootstrap:
  tenant: { name: acme }
  channels: [ { name: prod, kind: signed, versions: [v1.4.1], key: "file:a.pem" } ]
  issuer: { name: entra, url: 'https://login.example/x', jwks_uri: 'https://keys.example/jwks' }
  upstreams:
    - { name: core, kind: duckdb-core, channel: prod, platforms: [linux_amd64], extensions: [{ name: httpfs }] }
    - { name: community, kind: duckdb-community, channel: prod, platforms: [linux_amd64], extensions: [{ name: h3 }] }
    - { name: r, kind: repository, prefix: 'https://Repo.Example/acme', keys: [` + fp + `], channel: prod, platforms: [linux_amd64], extensions: [{ name: tresor }] }
`
	}
	all := []string{"login.example", "keys.example", "extensions.duckdb.org", "community-extensions.duckdb.org", "repo.example"}
	if _, err := Load(write(t, sec(strings.Join(all, ", "))), nil); err != nil {
		t.Fatalf("every host listed: %v", err)
	}
	for i, want := range []string{
		"bootstrap.issuer.url: login.example is not on egress.hosts",
		"bootstrap.issuer.jwks_uri: keys.example is not on egress.hosts",
		"bootstrap.upstreams[0]: extensions.duckdb.org is not on egress.hosts",
		"bootstrap.upstreams[1]: community-extensions.duckdb.org is not on egress.hosts",
		"bootstrap.upstreams[2]: repo.example is not on egress.hosts",
	} {
		hosts := strings.Join(append(append([]string{}, all[:i]...), all[i+1:]...), ", ")
		if _, err := Load(write(t, sec(hosts)), nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("without %s: %v, want %q", all[i], err, want)
		}
	}
}
