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
