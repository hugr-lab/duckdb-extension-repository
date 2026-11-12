package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// refusingFetcher is egress refusing every host by its host list.
type refusingFetcher struct{}

func (refusingFetcher) Get(_ context.Context, raw string) ([]byte, http.Header, error) {
	return nil, nil, &egress.HostError{Host: "idp.example"}
}

// Spec 0044: the verifier keeps the last fetch's error (discovery's, the JWKS's), and key() returns
// it as the cause of a missing key.
func TestKeyCause(t *testing.T) {
	ctx := context.Background()
	for name, rec := range map[string]store.Issuer{
		"discovery": {URL: "https://idp.example"},
		"jwks":      {URL: "https://idp.example", JWKSURI: "https://idp.example/jwks"},
	} {
		v := &Verifier{Fetch: refusingFetcher{}}
		_, ok, cause := v.key(ctx, rec, "k1")
		if ok || !errors.Is(cause, egress.ErrHostNotAllowed) {
			t.Errorf("%s: %v %v", name, ok, cause)
		}
	}
	// a fetch that fails otherwise is no host refusal
	v := &Verifier{Fetch: failingFetcher{}}
	if _, ok, cause := v.key(ctx, store.Issuer{URL: "https://idp.example"}, "k1"); ok || cause == nil || errors.Is(cause, egress.ErrHostNotAllowed) {
		t.Errorf("another failure: %v %v", ok, cause)
	}
}

type failingFetcher struct{}

func (failingFetcher) Get(context.Context, string) ([]byte, http.Header, error) {
	return nil, nil, egress.ErrConnect
}
