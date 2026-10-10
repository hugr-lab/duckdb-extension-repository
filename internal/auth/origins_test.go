package auth

import (
	"context"
	"net/http"
	"slices"
	"testing"
)

type docFetcher map[string]string

func (f docFetcher) Get(_ context.Context, url string) ([]byte, http.Header, error) {
	if b, ok := f[url]; ok {
		return []byte(b), nil, nil
	}
	return nil, nil, ErrToken
}

// Spec 0015: a discovery document's endpoints give the console's CSP origins; a host that would
// inject into the policy (";", ",", quotes) is dropped.
func TestDiscoverOrigins(t *testing.T) {
	f := docFetcher{"https://idp.example/.well-known/openid-configuration": `{"issuer":"https://idp.example",
		"token_endpoint":"https://tokens.example:8443/t","revocation_endpoint":"https://x;frame-ancestors/r",
		"end_session_endpoint":"https://evil'.example/e","userinfo_endpoint":"javascript:alert(1)"}`}
	got, err := DiscoverOrigins(context.Background(), f, "https://idp.example")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"https://idp.example", "https://tokens.example:8443"}) {
		t.Fatalf("origins: %v", got)
	}
	for in, ok := range map[string]bool{"https://a.example": true, "http://127.0.0.1:8080/x": true, "https://[::1]:8443": true,
		"https://a;b": false, "https://a,b": false, "https://u@a.example": false, "ftp://a.example": false, "": false} {
		if _, got := OriginOf(in); got != ok {
			t.Errorf("OriginOf(%q): %v", in, got)
		}
	}
}
