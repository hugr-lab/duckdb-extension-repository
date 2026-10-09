// Package credential obtains the tokens private upstreams take (spec 0009 phase 3). Credentials
// are named in configuration, limited to tenants and upstream prefixes; nothing of them is in the
// database or the API. Tokens come from a file read at each use, or from an OAuth 2.0 token
// endpoint (client credentials) that kista logs in to with the platform's identity, a projected
// token, a private_key_jwt from a key file, or, the last resort, a client secret.
package credential

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
)

// Errors, one per class; none carries a token, a secret or an identity provider's answer.
var (
	// ErrUnknown is a credential not configured (removed or renamed).
	ErrUnknown = errors.New("credential: not configured")
	// ErrNotAllowed is a credential an upstream may not use: another tenant's, or another prefix's.
	ErrNotAllowed = errors.New("credential: not for this tenant or prefix")
	// ErrClient is the identity provider refusing kista's own client (invalid_client,
	// unauthorized_client): the operator's configuration, not the upstream's.
	ErrClient = errors.New("credential: the identity provider refuses kista's own client (its configuration)")
	// ErrToken is no token: the token endpoint or the token file failed.
	ErrToken = errors.New("credential: no token")
)

// Poster posts a token request (egress.Client.PostForm).
type Poster interface {
	PostForm(ctx context.Context, raw string, form url.Values) (int, []byte, error)
}

// Deps are what credentials need from outside: a poster per credential (egress with its allowlist)
// and the platform's identity's token for a scope.
type Deps struct {
	Poster func(config.Credential) (Poster, error)
	Azure  func(ctx context.Context, scope string) (string, error)
	// AzureExchange is the audience of Entra's federated credentials: api://AzureADTokenExchange
	// (default), …China, …USGov per cloud.
	AzureExchange string
	Now           func() time.Time
}

// Credential is a named credential.
type Credential struct {
	Name        string
	AllowPublic bool
	info        Info
	all         bool
	tenants     map[string]bool
	prefixes    []*url.URL
	src         source
}

type source interface {
	token(ctx context.Context) (string, error)
	invalidate(tok string)
}

// Registry is the configured credentials by name.
type Registry map[string]*Credential

// New builds the registry; nothing is contacted.
func New(cfgs []config.Credential, d Deps) (Registry, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	r := Registry{}
	for _, c := range cfgs {
		cr := &Credential{Name: c.Name, AllowPublic: c.AllowPublic, tenants: map[string]bool{},
			info: Info{Name: c.Name, Kind: c.Kind, Tenants: slices.Clone(c.Tenants), Prefixes: slices.Clone(c.Prefixes), AllowPublic: c.AllowPublic}}
		for _, t := range c.Tenants {
			if t == "*" {
				cr.all = true
			}
			cr.tenants[t] = true
		}
		for _, p := range c.Prefixes {
			u, err := url.Parse(p)
			if err != nil {
				return nil, fmt.Errorf("credential %s: a prefix: %w", c.Name, err)
			}
			cr.prefixes = append(cr.prefixes, u)
		}
		switch c.Kind {
		case "token_file":
			cr.src = &tokenFile{path: c.TokenFile}
		case "client_credentials":
			src, err := newClientCredentials(c, d)
			if err != nil {
				return nil, fmt.Errorf("credential %s: %w", c.Name, err)
			}
			cr.src = src
		default:
			return nil, fmt.Errorf("credential %s: kind %q", c.Name, c.Kind)
		}
		r[c.Name] = cr
	}
	return r, nil
}

// For returns a credential an upstream of a tenant, at a prefix, may use.
func (r Registry) For(name, tenant, prefix string) (*Credential, error) {
	c, ok := r[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknown, name)
	}
	if !c.all && !c.tenants[tenant] || !c.covers(prefix) {
		return nil, fmt.Errorf("%w: %s", ErrNotAllowed, name)
	}
	return c, nil
}

// covers reports whether a prefix is under one of the credential's: the same scheme, host and port,
// and a path at or below the credential's on a segment boundary.
func (c *Credential) covers(prefix string) bool {
	u, err := url.Parse(prefix)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		slices.Contains(strings.Split(u.Path, "/"), "..") {
		return false
	}
	for _, p := range c.prefixes {
		if !strings.EqualFold(u.Scheme, p.Scheme) || !strings.EqualFold(u.Hostname(), p.Hostname()) || port(u) != port(p) {
			continue
		}
		base := strings.TrimSuffix(p.EscapedPath(), "/")
		path := u.EscapedPath()
		if base == "" || path == base || strings.HasPrefix(path, base+"/") {
			return true
		}
	}
	return false
}

func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	return "443"
}

// Token returns a token to send as Bearer.
func (c *Credential) Token(ctx context.Context) (string, error) { return c.src.token(ctx) }

// Invalidate forgets a cached token the upstream refused (only that one: another may have been
// fetched since), unless it is younger than 30 seconds: an upstream refusing every token costs one
// token request in that while.
func (c *Credential) Invalidate(tok string) { c.src.invalidate(tok) }

// tokenFile is a token read from a file at each use (a projected ServiceAccount token, rotated by
// the kubelet).
type tokenFile struct{ path string }

func (t *tokenFile) token(context.Context) (string, error) {
	b, err := os.ReadFile(t.path)
	if err != nil {
		return "", fmt.Errorf("%w: the token file cannot be read", ErrToken)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" || strings.ContainsAny(tok, " \r\n\t") {
		return "", fmt.Errorf("%w: the token file holds no token", ErrToken)
	}
	return tok, nil
}

func (t *tokenFile) invalidate(string) {}

// clientCredentials is an OAuth 2.0 client credentials grant, its token cached until a minute
// before it ends.
type clientCredentials struct {
	cfg    config.Credential
	post   Poster
	assert func(ctx context.Context) (string, error) // a client assertion; nil with a secret
	secret func() (string, error)
	now    func() time.Time

	mu       sync.Mutex
	tok      string
	until    time.Time
	fetched  time.Time // when tok was fetched: a token newer than refreshAfter401 is not forgotten
	failed   error     // the last fetch's failure, given back until failedTo
	failedTo time.Time // a short negative cache: a failing endpoint is asked again in a while
	inflight chan struct{}
}

// failFor is how long a failed token fetch is given back before the endpoint is asked again.
const failFor = 30 * time.Second

const jwtBearer = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

func newClientCredentials(c config.Credential, d Deps) (*clientCredentials, error) {
	if d.Poster == nil {
		return nil, errors.New("no egress for the token endpoint")
	}
	p, err := d.Poster(c)
	if err != nil {
		return nil, err
	}
	cc := &clientCredentials{cfg: c, post: p, now: d.Now}
	switch c.ClientAuth {
	case "azure":
		if d.Azure == nil {
			return nil, errors.New("client_auth azure needs azure.identity")
		}
		aud := d.AzureExchange
		if aud == "" {
			aud = "api://AzureADTokenExchange"
		}
		cc.assert = func(ctx context.Context) (string, error) { return d.Azure(ctx, aud+"/.default") }
	case "file":
		cc.assert = func(context.Context) (string, error) {
			b, err := os.ReadFile(c.AssertionFile)
			if err != nil || strings.TrimSpace(string(b)) == "" {
				return "", fmt.Errorf("%w: the assertion file cannot be read", ErrToken)
			}
			return strings.TrimSpace(string(b)), nil
		}
	case "key_file":
		aud := c.AssertionAudience
		if aud == "" {
			aud = c.TokenURL
		}
		kf := &keyFile{cfg: c}
		cc.assert = func(context.Context) (string, error) {
			k, err := kf.get()
			if err != nil {
				return "", fmt.Errorf("%w: %s", ErrToken, err)
			}
			return k.sign(c.ClientID, aud, d.Now())
		}
	case "secret":
		cc.secret = func() (string, error) {
			v := os.Getenv(c.ClientSecretEnv)
			if c.ClientSecretFile != "" {
				b, err := os.ReadFile(c.ClientSecretFile)
				if err != nil {
					return "", fmt.Errorf("%w: the client secret file cannot be read", ErrToken)
				}
				v = string(b)
			}
			if v = strings.TrimSpace(v); v == "" {
				return "", fmt.Errorf("%w: no client secret", ErrToken)
			}
			return v, nil
		}
	default:
		return nil, fmt.Errorf("client_auth %q", c.ClientAuth)
	}
	return cc, nil
}

// refreshAfter401 is how young a token is kept although an upstream refused it: the upstream
// refusing every token (a wrong audience) costs one token request in this while, not one a cell.
const refreshAfter401 = 30 * time.Second

func (c *clientCredentials) invalidate(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok == tok && c.now().Sub(c.fetched) >= refreshAfter401 {
		c.tok = ""
	}
}

// token gives the cached token, or fetches one: one fetch at a time, the others waiting for it (or
// for their context); a failure is given back for failFor.
func (c *clientCredentials) token(ctx context.Context) (string, error) {
	for {
		c.mu.Lock()
		now := c.now()
		switch {
		case c.tok != "" && now.Before(c.until):
			tok := c.tok
			c.mu.Unlock()
			return tok, nil
		case c.failed != nil && now.Before(c.failedTo):
			err := c.failed
			c.mu.Unlock()
			return "", err
		case c.inflight != nil:
			ch := c.inflight
			c.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		ch := make(chan struct{})
		c.inflight = ch
		c.mu.Unlock()
		tok, life, err := c.fetch(ctx)
		c.mu.Lock()
		c.inflight = nil
		close(ch)
		now = c.now()
		if err != nil {
			if ctx.Err() == nil { // a caller giving up is no failure of the endpoint
				c.failed, c.failedTo = err, now.Add(failFor)
			}
			c.mu.Unlock()
			return "", err
		}
		// used until a minute before it ends, or half its life when it is short
		c.tok, c.until, c.fetched, c.failed = tok, now.Add(life-min(time.Minute, life/2)), now, nil
		c.mu.Unlock()
		return tok, nil
	}
}

// fetch asks the token endpoint.
func (c *clientCredentials) fetch(ctx context.Context) (string, time.Duration, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.cfg.ClientID}}
	if c.cfg.Scope != "" {
		form.Set("scope", c.cfg.Scope)
	}
	if c.cfg.Audience != "" {
		form.Set("audience", c.cfg.Audience)
	}
	if c.secret != nil {
		sec, err := c.secret()
		if err != nil {
			return "", 0, err
		}
		form.Set("client_secret", sec)
	} else {
		a, err := c.assert(ctx)
		if err != nil {
			if errors.Is(err, ErrToken) {
				return "", 0, err
			}
			return "", 0, fmt.Errorf("%w: the platform's identity gave no client assertion", ErrToken)
		}
		form.Set("client_assertion_type", jwtBearer)
		form.Set("client_assertion", a)
	}
	status, body, err := c.post.PostForm(ctx, c.cfg.TokenURL, form)
	if err != nil {
		return "", 0, fmt.Errorf("%w: the token endpoint: %w", ErrToken, err)
	}
	var ans struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(body, &ans)
	switch {
	case ans.Error == "invalid_client" || ans.Error == "unauthorized_client":
		return "", 0, ErrClient
	case status != 200 || ans.AccessToken == "":
		return "", 0, fmt.Errorf("%w: the token endpoint answered %d %s", ErrToken, status, errCode(ans.Error))
	}
	life := time.Duration(ans.ExpiresIn) * time.Second
	if life <= 0 {
		life = 5 * time.Minute
	}
	return ans.AccessToken, life, nil
}

// errCode is an OAuth error code, kept only when it is one (a code is safe to log; a description
// is not).
func errCode(s string) string {
	for _, r := range s {
		if r != '_' && (r < 'a' || r > 'z') {
			return ""
		}
	}
	return s
}

// signer is a private_key_jwt key.
type signer struct {
	key *rsa.PrivateKey
	kid string
}

// keyFile is a private_key_jwt key read from its file, again when the file changes (a rotation),
// the last good key kept when a read fails.
type keyFile struct {
	cfg  config.Credential
	mu   sync.Mutex
	key  *signer
	mod  time.Time
	size int64
}

func (k *keyFile) get() (*signer, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	st, err := os.Stat(k.cfg.KeyFile)
	if err != nil {
		if k.key != nil {
			return k.key, nil
		}
		return nil, errors.New("the key file cannot be read")
	}
	if k.key != nil && st.ModTime().Equal(k.mod) && st.Size() == k.size {
		return k.key, nil
	}
	key, err := readKey(k.cfg)
	if err != nil {
		if k.key != nil {
			return k.key, nil
		}
		return nil, err
	}
	k.key, k.mod, k.size = key, st.ModTime(), st.Size()
	return key, nil
}

// readKey reads a key file: ZITADEL's JSON ({keyId, key, clientId}), or a PEM RSA key with key_id.
func readKey(c config.Credential) (*signer, error) {
	b, err := os.ReadFile(c.KeyFile)
	if err != nil {
		return nil, errors.New("the key file cannot be read")
	}
	kid, raw := c.KeyID, b
	var z struct {
		KeyID string `json:"keyId"`
		Key   string `json:"key"`
	}
	if json.Unmarshal(b, &z) == nil && z.Key != "" {
		raw = []byte(z.Key)
		if z.KeyID != "" {
			kid = z.KeyID
		}
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return nil, errors.New("the key file holds no PEM key")
	}
	var key any
	if key, err = x509.ParsePKCS8PrivateKey(blk.Bytes); err != nil {
		key, err = x509.ParsePKCS1PrivateKey(blk.Bytes)
	}
	rk, ok := key.(*rsa.PrivateKey)
	if err != nil || !ok {
		return nil, errors.New("the key file holds no RSA private key")
	}
	if kid == "" {
		return nil, errors.New("the key has no id: key_id, or the key file's keyId")
	}
	return &signer{key: rk, kid: kid}, nil
}

// sign makes a client assertion: iss and sub the client id, aud the token endpoint (or the IdP's
// issuer), a minute's life, a fresh jti.
func (s *signer) sign(clientID, aud string, now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": s.kid})
	cl, _ := json.Marshal(map[string]any{"iss": clientID, "sub": clientID, "aud": aud, "iat": now.Unix(),
		"exp": now.Add(time.Minute).Unix(), "jti": hex.EncodeToString(jti)})
	in := enc.EncodeToString(h) + "." + enc.EncodeToString(cl)
	d := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, d[:])
	if err != nil {
		return "", err
	}
	return in + "." + enc.EncodeToString(sig), nil
}

// Info is what a server administrator sees of a credential: never its settings.
type Info struct {
	Name, Kind  string
	Tenants     []string
	Prefixes    []string
	AllowPublic bool
}

// Infos lists the credentials by name.
func (r Registry) Infos() []Info {
	out := make([]Info, 0, len(r))
	for _, c := range r {
		out = append(out, c.info)
	}
	slices.SortFunc(out, func(a, b Info) int { return strings.Compare(a.Name, b.Name) })
	return out
}
