// Package gcp builds Google Cloud credentials from explicit config (specs 0004 phase 4, 0005 phase
// 3b): the metadata server (GKE Workload Identity, a GCE service account), asked at its fixed
// address and checked against the configured principal; or an external_account file (workload
// identity federation from a projected Kubernetes token), parsed strictly and impersonating the
// principal; service_account key files only in development. Application Default Credentials are
// never looked up, and the steering variables are refused.
package gcp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/google/externalaccount"
)

// Scope is the scope kista asks for: IAM is the limit, not scopes.
const Scope = "https://www.googleapis.com/auth/cloud-platform"

// Steering are variables that would make a Google client use credentials, a metadata server or an
// endpoint the config did not choose; outside development they are refused.
var Steering = []string{"GOOGLE_APPLICATION_CREDENTIALS", "GCE_METADATA_HOST", "GCE_METADATA_IP", "GCE_METADATA_ROOT",
	"STORAGE_EMULATOR_HOST", "GOOGLE_API_USE_CLIENT_CERTIFICATE", "GOOGLE_API_USE_MTLS_ENDPOINT",
	"GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES", "CLOUDSDK_CONFIG"}

var (
	keyRingRule   = regexp.MustCompile(`^projects/[a-z][a-z0-9-]{4,28}[a-z0-9]/locations/[a-z0-9-]{1,63}/keyRings/[A-Za-z0-9_-]{1,63}$`)
	bucketRule    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,61}[a-z0-9]$`)
	principalRule = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]@[a-z][a-z0-9-]{4,28}[a-z0-9]\.iam\.gserviceaccount\.com$`)
)

// ValidKeyRing reports whether s is a key ring's resource name (the project by its id).
func ValidKeyRing(s string) bool { return keyRingRule.MatchString(s) }

// ValidBucket reports whether s is a bucket name kista accepts (no dots: a dotted name is a domain).
func ValidBucket(s string) bool { return bucketRule.MatchString(s) && !strings.Contains(s, "goog") }

// ValidPrincipal reports whether s is a user-managed service account's email.
func ValidPrincipal(s string) bool { return principalRule.MatchString(s) }

// HTTPClient is the client for Google calls and the metadata server: no proxy from the environment,
// TLS 1.2 or later for https, no redirects.
func HTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Env is what TokenSource reads from the process (tests replace it).
type Env struct {
	Environ  func() []string
	ReadFile func(string) ([]byte, error) // the credentials file
	// ReadToken reads a projected token file: a symlink into a timestamped directory, any mode
	ReadToken func(string) ([]byte, error)
}

func (e Env) defaults() Env {
	if e.Environ == nil {
		e.Environ = os.Environ
	}
	if e.ReadFile == nil {
		e.ReadFile = readCredentialsFile
	}
	if e.ReadToken == nil {
		e.ReadToken = readTokenFile
	}
	return e
}

// Options configure TokenSource.
type Options struct {
	Dev        bool
	HTTPClient *http.Client // default HTTPClient()
	Env        Env
	// MetadataURL replaces the metadata server's base URL (tests); empty in production.
	MetadataURL string
}

// metadataBase is the metadata server's default service account, at its fixed address.
const metadataBase = "http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/"

// refreshEarly is how long before expiry a token is refreshed: the metadata server itself hands out
// a cached token until about 5 minutes are left.
const refreshEarly = 225 * time.Second

// TokenSource builds the token source for a credentials file ("": the metadata server) acting as
// principal (required outside dev). Nothing is fetched here.
func TokenSource(credentialsFile, principal string, o Options) (oauth2.TokenSource, error) {
	env := o.Env.defaults()
	if !o.Dev {
		if v := steering(env.Environ()); v != "" {
			return nil, fmt.Errorf("gcp: %s is set; outside profile dev the credentials come from config only", v)
		}
		if !ValidPrincipal(principal) {
			return nil, errors.New("gcp: principal, the service account kista acts as, is required outside profile dev")
		}
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = HTTPClient()
	}
	var ts oauth2.TokenSource
	if credentialsFile == "" {
		base := o.MetadataURL
		if base == "" {
			base = metadataBase
		}
		ts = &metadataSource{base: base, principal: principal, hc: hc}
	} else {
		b, err := env.ReadFile(credentialsFile)
		if err != nil {
			return nil, fmt.Errorf("gcp: credentials_file could not be read: %w", err)
		}
		if ts, err = fromFile(b, principal, hc, o.Dev, env); err != nil {
			return nil, err
		}
	}
	return oauth2.ReuseTokenSourceWithExpiry(nil, ts, refreshEarly), nil
}

func steering(environ []string) string {
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		if v == "" {
			continue
		}
		for _, s := range Steering {
			if k == s {
				return k
			}
		}
		if k == "GOOGLE_CLOUD_UNIVERSE_DOMAIN" && v != "googleapis.com" {
			return k
		}
	}
	return ""
}

// fromFile builds a token source from a credentials file of an accepted type.
func fromFile(b []byte, principal string, hc *http.Client, dev bool, env Env) (oauth2.TokenSource, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return nil, errors.New("gcp: credentials_file is not JSON")
	}
	switch head.Type {
	case "external_account":
		return externalAccount(b, principal, hc, dev, env)
	case "service_account":
		if !dev {
			return nil, errors.New("gcp: a service_account key file is allowed only with profile dev; use the metadata server or workload identity federation")
		}
		var sa struct {
			TokenURI       string `json:"token_uri"`
			ClientEmail    string `json:"client_email"`
			UniverseDomain string `json:"universe_domain"`
		}
		_ = json.Unmarshal(b, &sa)
		switch {
		case sa.TokenURI != "https://oauth2.googleapis.com/token":
			return nil, errors.New("gcp: the service account's token_uri must be https://oauth2.googleapis.com/token")
		case sa.UniverseDomain != "" && sa.UniverseDomain != "googleapis.com":
			return nil, errors.New("gcp: the service account's universe_domain must be googleapis.com")
		case principal != "" && sa.ClientEmail != principal:
			return nil, errors.New("gcp: the service account key is not the principal's")
		}
		ctx := context.WithValue(context.Background(), oauth2.HTTPClient, hc)
		c, err := google.CredentialsFromJSONWithTypeAndParams(ctx, b, google.ServiceAccount, google.CredentialsParams{Scopes: []string{Scope}})
		if err != nil {
			return nil, errors.New("gcp: the service account key file could not be used")
		}
		return c.TokenSource, nil
	}
	return nil, fmt.Errorf("gcp: credentials of type %q are not accepted (external_account; service_account in dev)", head.Type)
}

// externalFile is an external_account file, as far as kista accepts one.
type externalFile struct {
	Type                           string `json:"type"`
	Audience                       string `json:"audience"`
	SubjectTokenType               string `json:"subject_token_type"`
	TokenURL                       string `json:"token_url"`
	TokenInfoURL                   string `json:"token_info_url"`
	ServiceAccountImpersonationURL string `json:"service_account_impersonation_url"`
	ServiceAccountImpersonation    struct {
		TokenLifetimeSeconds int `json:"token_lifetime_seconds"`
	} `json:"service_account_impersonation"`
	QuotaProjectID           string `json:"quota_project_id"`
	UniverseDomain           string `json:"universe_domain"`
	WorkforcePoolUserProject string `json:"workforce_pool_user_project"`
	ClientID                 string `json:"client_id"`
	ClientSecret             string `json:"client_secret"`
	CredentialSource         struct {
		File                        string                 `json:"file"`
		URL                         string                 `json:"url"`
		Headers                     map[string]string      `json:"headers"`
		EnvironmentID               string                 `json:"environment_id"`
		RegionURL                   string                 `json:"region_url"`
		RegionalCredVerificationURL string                 `json:"regional_cred_verification_url"`
		IMDSv2SessionTokenURL       string                 `json:"imdsv2_session_token_url"`
		Format                      externalaccount.Format `json:"format"`
		Executable                  json.RawMessage        `json:"executable"`
		Certificate                 json.RawMessage        `json:"certificate"`
	} `json:"credential_source"`
}

var audienceRule = regexp.MustCompile(`^//iam\.googleapis\.com/projects/\d{1,20}/locations/global/workloadIdentityPools/[a-z0-9-]{4,32}/providers/[a-z0-9-]{4,32}$`)

// externalAccount parses and checks an external_account file, and builds its token source from the
// parsed parts only; the projected token is read by kista (a SubjectTokenSupplier), so the library
// opens no file and calls nothing but STS and IAM Credentials.
func externalAccount(b []byte, principal string, hc *http.Client, dev bool, env Env) (oauth2.TokenSource, error) {
	var f externalFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, errors.New("gcp: the external_account file has a field or value kista does not accept")
	}
	refuse := func(why string) (oauth2.TokenSource, error) {
		return nil, errors.New("gcp: external_account: " + why)
	}
	cs := f.CredentialSource
	impersonate := ""
	if principal != "" {
		impersonate = "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" + principal + ":generateAccessToken"
	}
	lifetime := f.ServiceAccountImpersonation.TokenLifetimeSeconds
	switch {
	case !audienceRule.MatchString(f.Audience):
		return refuse("audience must be a workload identity pool provider (//iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/…)")
	case f.SubjectTokenType != "urn:ietf:params:oauth:token-type:jwt":
		return refuse("subject_token_type must be urn:ietf:params:oauth:token-type:jwt (a projected token)")
	case f.TokenURL != "https://sts.googleapis.com/v1/token":
		return refuse("token_url must be https://sts.googleapis.com/v1/token")
	case f.TokenInfoURL != "" && f.TokenInfoURL != "https://sts.googleapis.com/v1/introspect":
		return refuse("token_info_url must be https://sts.googleapis.com/v1/introspect")
	case f.ServiceAccountImpersonationURL != impersonate && (!dev || principal != "" || f.ServiceAccountImpersonationURL != ""):
		return refuse("service_account_impersonation_url must impersonate the principal (iamcredentials.googleapis.com generateAccessToken)")
	case lifetime != 0 && (lifetime < 600 || lifetime > 3600):
		return refuse("service_account_impersonation.token_lifetime_seconds must be within 600..3600")
	case f.UniverseDomain != "" && f.UniverseDomain != "googleapis.com":
		return refuse("universe_domain must be googleapis.com")
	case f.QuotaProjectID != "":
		return refuse("quota_project_id is not used by kista; remove it")
	case f.WorkforcePoolUserProject != "" || f.ClientID != "" || f.ClientSecret != "":
		return refuse("workforce pools and client credentials are for people, not kista")
	case cs.URL != "" || cs.EnvironmentID != "" || cs.RegionURL != "" || cs.RegionalCredVerificationURL != "" ||
		cs.IMDSv2SessionTokenURL != "" || len(cs.Headers) > 0:
		return refuse("credential_source must be a file (a projected token): url and aws1 answer with the node's identity")
	case len(cs.Executable) > 0 && string(cs.Executable) != "null", len(cs.Certificate) > 0 && string(cs.Certificate) != "null":
		return refuse("executable and certificate credential sources are not accepted")
	case !strings.HasPrefix(cs.File, "/"):
		return refuse("credential_source.file must be an absolute path")
	}
	switch {
	case cs.Format.Type == "" || cs.Format.Type == "text":
	case cs.Format.Type == "json" && cs.Format.SubjectTokenFieldName != "":
	default:
		return refuse("credential_source.format must be text, or json with subject_token_field_name")
	}
	conf := externalaccount.Config{
		Audience: f.Audience, SubjectTokenType: f.SubjectTokenType, TokenURL: f.TokenURL,
		ServiceAccountImpersonationURL:             f.ServiceAccountImpersonationURL,
		ServiceAccountImpersonationLifetimeSeconds: lifetime,
		SubjectTokenSupplier:                       projectedToken{path: cs.File, format: cs.Format, read: env.ReadToken},
		QuotaProjectID:                             f.QuotaProjectID, Scopes: []string{Scope}, UniverseDomain: "googleapis.com",
	}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, hc) // never cancelled: refreshes use it
	ts, err := externalaccount.NewTokenSource(ctx, conf)
	if err != nil {
		return refuse("the configuration could not be used")
	}
	return ts, nil
}

// projectedToken reads the projected token at every exchange.
type projectedToken struct {
	path   string
	format externalaccount.Format
	read   func(string) ([]byte, error)
}

func (p projectedToken) SubjectToken(context.Context, externalaccount.SupplierOptions) (string, error) {
	b, err := p.read(p.path)
	if err != nil {
		return "", errors.New("gcp: the projected token could not be read")
	}
	tok := strings.TrimSpace(string(b))
	if p.format.Type == "json" {
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			return "", errors.New("gcp: the projected token file is not JSON")
		}
		s, _ := m[p.format.SubjectTokenFieldName].(string)
		tok = s
	}
	if tok == "" || strings.ContainsAny(tok, " \t\r\n") {
		return "", errors.New("gcp: the projected token file holds no token")
	}
	return tok, nil
}

// metadataSource asks the metadata server for the default service account's token, after checking
// once that the account is the principal.
type metadataSource struct {
	base      string
	principal string
	hc        *http.Client
	mu        sync.Mutex
}

func (m *metadataSource) get(path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := m.hc.Do(req)
	if err != nil {
		return nil, errors.New("gcp: the metadata server could not be reached")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Metadata-Flavor") != "Google" {
		return nil, fmt.Errorf("gcp: the metadata server answered %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, errors.New("gcp: the metadata server's answer could not be read")
	}
	return b, nil
}

func (m *metadataSource) Token() (*oauth2.Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.principal != "" { // at every refresh: an annotation change could hand out another account
		b, err := m.get("email")
		if err != nil {
			return nil, err
		}
		if got := strings.TrimSpace(string(b)); got != m.principal {
			return nil, ErrPrincipal
		}
	}
	b, err := m.get("token?scopes=" + url.QueryEscape(Scope))
	if err != nil {
		return nil, err
	}
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &t); err != nil || t.AccessToken == "" || t.ExpiresIn <= 0 {
		return nil, errors.New("gcp: the metadata server gave no token")
	}
	return &oauth2.Token{AccessToken: t.AccessToken, TokenType: "Bearer", Expiry: time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)}, nil
}

// readCredentialsFile reads a credentials file: a regular file nobody but its owner may write. An
// external_account file holds no secret, but who may edit it chooses kista's identity. It may be a
// Kubernetes ConfigMap's projection, a symlink into a timestamped directory, which is followed.
func readCredentialsFile(path string) ([]byte, error) {
	return readRegular(path, true)
}

// readTokenFile reads a projected token: a regular file, any mode (the volume's), through its link.
func readTokenFile(path string) ([]byte, error) {
	return readRegular(path, false)
}

func readRegular(path string, ownerWrite bool) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || ownerWrite && (st.Mode().Perm()&0o022 != 0 || !ownedByUs(st)) {
		return nil, fmt.Errorf("gcp: %s must be a regular file nobody else may write, owned by root or kista's user", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil || len(b) > 64<<10 {
		return nil, errors.New("gcp: the file could not be read whole")
	}
	return b, nil
}

// ErrPrincipal is the metadata server's account differing from the configured principal.
var ErrPrincipal = errors.New("gcp: the metadata server's service account is not the principal (no Workload Identity for this pod?)")

// Token gets a token from ts, bounded by ctx: a token source takes no context, and a slow metadata
// server or STS must not hold a call past its deadline (the source keeps the late answer).
func Token(ctx context.Context, ts oauth2.TokenSource) (*oauth2.Token, error) {
	type result struct {
		t   *oauth2.Token
		err error
	}
	ch := make(chan result, 1)
	go func() {
		t, err := ts.Token()
		ch <- result{t, err}
	}()
	select {
	case r := <-ch:
		return r.t, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// StaticToken is the fixed token requests to a development fake carry (endpoint_url): no credential
// is loaded, so no real token can reach it.
func StaticToken() oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "kista-dev-fake", TokenType: "Bearer"})
}
