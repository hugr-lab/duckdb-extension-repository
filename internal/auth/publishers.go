package auth

import (
	"encoding/json"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Publication is a publisher's verified signed request (spec 0023): the publisher whose key
// signed it, as its principal.
type Publication struct {
	Identity   Identity
	Publishers []MatchedPublisher
	Provider   string // "key"
}

// MatchedPublisher is a publisher and the credential that matched.
type MatchedPublisher struct {
	Publisher  store.Publisher
	Credential string // key:<fingerprint>
}

// PublisherKey is a publisher's principal.
func PublisherKey(publisherID string) Key {
	return Key{Kind: store.PrincipalPublisher, Value: publisherID}
}

// Provenance is a publication's record (spec 0008, 0023): the actor, the provider and the
// credentials. The statement and the signature are kept beside it, on the release.
func (p Publication) Provenance(actor string) string {
	creds := []string{}
	for _, x := range p.Publishers {
		creds = append(creds, x.Credential)
	}
	b, _ := json.Marshal(map[string]any{"actor": actor, "provider": p.Provider, "credentials": creds})
	return string(b)
}

// GitHubActionsIssuer is GitHub Actions' token issuer. Any GitHub workflow can request a token for
// any audience from it, so no issuer record, tenant or server, may have its URL: trusting it for
// installs or administration would let any repository in (spec 0008, kept by spec 0023).
const GitHubActionsIssuer = "https://token.actions.githubusercontent.com"

// RefusedIssuer reports whether an issuer URL may not be an issuer record's.
func RefusedIssuer(url string) bool {
	return strings.TrimSuffix(url, "/") == GitHubActionsIssuer
}

// IsRetiredAPIKey reports whether a Bearer has the shape of a retired API key (spec 0008's
// kista_…): the DuckDB routes take one as no token, so a stale configuration does not break public
// installs (spec 0023).
func IsRetiredAPIKey(tok string) bool { return strings.HasPrefix(tok, "kista_") }
