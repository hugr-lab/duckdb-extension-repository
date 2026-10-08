package config

import (
	"strings"
	"testing"
	"time"
)

func TestServeConfig(t *testing.T) {
	ok := base + `
serve:
  public_url: https://enterest.hugr-lab.com
  listeners:
    - { addr: ":443", scheme: https, tls: { cert_file: /run/tls/tls.crt, key_file: /run/tls/tls.key } }
    - { addr: ":80", scheme: http }
    - { addr: ":8443", scheme: https, behind_proxy: true }
    - { addr: ":9443", scheme: dual, tls: { cert_file: /a, key_file: /b } }
  trusted_proxies: [10.0.0.0/8, "fd00::/8"]
  resign: true
  min_rate: 32KiB
`
	cfg, err := Load(write(t, ok), []string{"KISTA_SERVE__PUBLIC_URL=https://ext.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Serve.PublicURL != "https://ext.example.com" || len(cfg.Serve.Listeners) != 4 || len(cfg.TrustedProxyPrefixes()) != 2 {
		t.Fatalf("%+v", cfg.Serve)
	}
	lim := cfg.ServeLimits()
	if lim.MinRate != 32<<10 || lim.MaxDownloads != DefaultMaxDownloads || lim.WriteIdleTimeout != 30*time.Second {
		t.Fatalf("limits %+v", lim)
	}
	for name, c := range map[string]struct {
		file string
		env  []string
		want string
	}{
		"resign from env":     {base, []string{"KISTA_SERVE__RESIGN=true"}, "can only be set in the config file"},
		"public_url path":     {base + "serve: { public_url: 'https://x.example/kista' }\n", nil, "without a path"},
		"public_url http":     {base + "serve: { public_url: 'http://x.example' }\n", nil, "must be https"},
		"https without tls":   {base + "serve: { listeners: [{ addr: ':1', scheme: https }] }\n", nil, "exactly one of tls and behind_proxy"},
		"https with both":     {base + "serve: { listeners: [{ addr: ':1', scheme: https, behind_proxy: true, tls: { cert_file: /a, key_file: /b } }] }\n", nil, "exactly one"},
		"http with tls":       {base + "serve: { listeners: [{ addr: ':1', scheme: http, tls: { cert_file: /a, key_file: /b } }] }\n", nil, "neither"},
		"dual without tls":    {base + "serve: { listeners: [{ addr: ':1', scheme: dual }] }\n", nil, "dual listener needs tls"},
		"bad scheme":          {base + "serve: { listeners: [{ addr: ':1', scheme: h3 }] }\n", nil, "scheme must be"},
		"twice":               {base + "serve: { listeners: [{ addr: ':1', scheme: http }, { addr: ':1', scheme: http }] }\n", nil, "used twice"},
		"relative cert":       {base + "serve: { listeners: [{ addr: ':1', scheme: https, tls: { cert_file: a, key_file: /b } }] }\n", nil, "absolute"},
		"proxy without trust": {base + "serve: { listeners: [{ addr: ':1', scheme: https, behind_proxy: true }] }\n", nil, "trusted_proxies is required"},
		"bad proxy":           {base + "serve: { trusted_proxies: [10.0.0.1] }\n", nil, "not a CIDR"},
		"tiny write timeout":  {base + "serve: { write_idle_timeout: 10ms }\n", nil, "at least 1s"},
		"negative downloads":  {base + "serve: { max_downloads: -1 }\n", nil, "negative"},
	} {
		_, err := Load(write(t, c.file), c.env)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
	dev := "profile: dev\nrotation: { min_trusted: 1h, min_demoted: 1h }\n" + base + "serve: { public_url: 'http://127.0.0.1:8080' }\n"
	if _, err := Load(write(t, dev), nil); err != nil {
		t.Fatalf("http public_url on loopback in dev: %v", err)
	}
}
