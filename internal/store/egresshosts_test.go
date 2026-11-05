package store_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
)

// Spec 0044: the reads behind serve's start-time warning, across tenants: issuers with their
// explicit jwks_uri (or ""), upstreams with kind, mode and prefix (or ""), feeds with mode and URL.
func TestEgressHostReads(t *testing.T) {
	each(t, func(t *testing.T, e storetest.Engine) {
		s := e.Open(t)
		tn, ch := fixture(t, s)
		inTx(t, s, func(tx *store.Tx) error {
			for _, is := range []store.Issuer{{Name: "a", URL: "https://idp.example", JWKSURI: "https://keys.example/jwks"}, {Name: "b", URL: "https://b.example"}} {
				is.TenantID, is.Algorithms, is.CreatedBy = tn.ID, []string{"RS256"}, "os:1:t"
				if err := tx.InsertIssuer(ctx, &is); err != nil {
					return err
				}
			}
			for _, u := range []store.Upstream{{Name: "core", Kind: store.UpstreamCore, Mode: store.ModeMirror},
				{Name: "air", Kind: store.UpstreamRepo, Prefix: "https://air.example/x", Mode: store.ModeBundle}} {
				u.TenantID, u.ChannelID, u.Visibility, u.State, u.CreatedBy = tn.ID, ch.ID, store.Private, store.UpstreamActive, "os:1:t"
				if err := tx.InsertUpstream(ctx, &u); err != nil {
					return err
				}
			}
			return tx.InsertFeed(ctx, &store.Feed{TenantID: tn.ID, Name: "src", URL: "https://src.example/api/v1/tenants/x", Credential: "c",
				Keys: []string{"sha256:" + strings.Repeat("a", 64)}, Offer: store.OfferUpstreams, CreatedBy: "os:1:t"})
		})
		is, err := s.IssuerURLs(ctx)
		if err != nil || !slices.Equal(is, [][4]string{{tn.ID, "a", "https://idp.example", "https://keys.example/jwks"}, {tn.ID, "b", "https://b.example", ""}}) {
			t.Fatalf("IssuerURLs: %v %v", is, err)
		}
		us, err := s.UpstreamPrefixes(ctx)
		if err != nil || !slices.Equal(us, [][5]string{{tn.ID, "air", store.UpstreamRepo, store.ModeBundle, "https://air.example/x"},
			{tn.ID, "core", store.UpstreamCore, store.ModeMirror, ""}}) {
			t.Fatalf("UpstreamPrefixes: %v %v", us, err)
		}
		fs, err := s.FeedURLs(ctx)
		if err != nil || !slices.Equal(fs, [][4]string{{tn.ID, "src", store.FeedOnline, "https://src.example/api/v1/tenants/x"}}) {
			t.Fatalf("FeedURLs: %v %v", fs, err)
		}
	})
}
