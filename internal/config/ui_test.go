package config

import (
	"strings"
	"testing"
)

// Spec 0015: ui's defaults and validation.
func TestUIConfig(t *testing.T) {
	cfg, err := Load(write(t, base), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UIEnabled() || len(cfg.UIServerClients()) != 0 {
		t.Fatalf("defaults: %+v", cfg.UI)
	}
	const iss = "auth: { server_issuers: [ { name: ops, url: 'https://login.example', required_claims: { tid: t1 } } ], server_audiences: [api://kista] }\n"
	for name, c := range map[string]struct{ file, want string }{
		"an origin with a path":   {base + "ui: { allowed_origins: ['https://shell.example/x'] }\n", "ui.allowed_origins"},
		"a wildcard":              {base + "ui: { frame_ancestors: ['https://*.example'] }\n", "ui.frame_ancestors"},
		"http off loopback":       {base + "ui: { connect_src: ['http://otel.example'] }\n", "ui.connect_src"},
		"an unknown issuer":       {base + iss + "ui: { server_clients: [ { issuer: nope, client_id: c } ] }\n", "not a server issuer"},
		"two clients":             {base + iss + "ui: { server_clients: [ { issuer: ops, client_id: a }, { issuer: ops, client_id: b } ] }\n", "two clients"},
		"scopes without openid":   {base + iss + "ui: { server_clients: [ { issuer: ops, client_id: a, scopes: [profile] } ] }\n", "openid"},
		"a long environment name": {base + "ui: { environment: '" + strings.Repeat("x", 33) + "' }\n", "ui.environment"},
	} {
		if _, err := Load(write(t, c.file), nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	cfg, err = Load(write(t, base+iss+"ui: { enabled: false, allowed_origins: ['https://shell.example', 'http://127.0.0.1:5173'], "+
		"server_clients: [ { issuer: ops, client_id: kista-console } ] }\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UIEnabled() || len(cfg.UIServerClients()) != 1 || cfg.UI.Environment != "" || len(cfg.UIServerClients()[0].Scopes) != 3 {
		t.Fatalf("set: %+v", cfg.UI)
	}
	if _, err := Load(write(t, base), []string{"KISTA_UI__ENVIRONMENT=x"}); err == nil {
		t.Error("ui from the environment")
	}
}
