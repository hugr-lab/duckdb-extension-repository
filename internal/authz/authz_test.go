package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

func TestCovers(t *testing.T) {
	me := auth.Principals{{IssuerID: "i1", Kind: store.PrincipalSubject, Value: "me"}: true, {IssuerID: "i1", Kind: store.PrincipalIssuer}: true}
	g := func(kind, value, channel, ext string, verbs ...string) store.Grant {
		ch := ""
		if channel != "" {
			ch = "id-" + channel
		}
		return store.Grant{IssuerID: "i1", Kind: kind, Value: value, ChannelID: ch, ChannelName: channel, Extension: ext, Verbs: verbs}
	}
	tenant := authz.Resource{Tenant: "acme"}
	prod := authz.Resource{Tenant: "acme", Channel: "prod"}
	tresor := authz.Resource{Tenant: "acme", Channel: "prod", Extension: "tresor"}
	for _, tc := range []struct {
		name  string
		grant store.Grant
		verb  string
		r     authz.Resource
		want  bool
	}{
		{"tenant admin on the tenant", g("subject", "me", "", "", "admin"), "admin", tenant, true},
		{"tenant admin on an extension", g("subject", "me", "", "", "admin"), "admin", tresor, true},
		{"channel admin on the tenant", g("subject", "me", "prod", "", "admin"), "admin", tenant, false},
		{"channel admin on its channel", g("subject", "me", "prod", "", "admin"), "admin", prod, true},
		{"channel admin on another channel", g("subject", "me", "staging", "", "admin"), "admin", prod, false},
		{"extension admin on its channel", g("subject", "me", "", "tresor", "admin"), "admin", prod, false},
		{"extension admin on its extension", g("subject", "me", "", "tresor", "admin"), "admin", tresor, true},
		{"extension admin in another channel", g("subject", "me", "staging", "tresor", "admin"), "admin", tresor, false},
		{"install is not admin", g("subject", "me", "", "", "install"), "admin", tenant, false},
		{"admin implies install", g("subject", "me", "", "", "admin"), "install", tresor, true},
		{"issuer-wide admin is ignored", g("issuer", "", "", "", "admin"), "admin", tenant, false},
		{"issuer-wide admin does not imply install", g("issuer", "", "", "", "admin"), "install", tresor, false},
		{"issuer-wide install", g("issuer", "", "", "", "install"), "install", tresor, true},
		{"another principal", g("subject", "you", "", "", "admin"), "admin", tenant, false},
	} {
		if got := authz.Covers(me, []store.Grant{tc.grant}, tc.verb, tc.r); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	// a server actor is allowed everything, a principal actor nothing outside its tenant, others nothing
	var gz authz.Grants
	ctx := context.Background()
	if err := gz.Allow(ctx, authz.Actor{Kind: authz.ActorServer}, authz.VerbAdmin, authz.Server); err != nil {
		t.Error(err)
	}
	if err := gz.Allow(ctx, authz.Actor{Kind: authz.ActorPrincipal, Tenant: "acme"}, authz.VerbAdmin, authz.Server); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("a principal on the server: %v", err)
	}
	if err := gz.Allow(ctx, authz.Actor{Kind: authz.ActorPrincipal, Tenant: "acme"}, authz.VerbAdmin, authz.Resource{Tenant: "other"}); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("a principal in another tenant: %v", err)
	}
	if err := gz.Allow(ctx, authz.Actor{Kind: authz.ActorSystem}, authz.VerbAdmin, tenant); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("a system actor: %v", err)
	}
}
