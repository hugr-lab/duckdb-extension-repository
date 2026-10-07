// Package keysource resolves signer references <source>:<key> to signers (spec 0004). Sources are
// configured by the administrator; a reference only picks a key inside one. Every backend is
// wrapped in the same contract: the key is checked at open and re-checked while in use, every
// signature is verified and must come from the pinned key, calls are bounded.
package keysource

import (
	"context"
	"crypto"
	"crypto/rsa"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

// Errors.
var (
	ErrReference = errors.New("keysource: reference not allowed")
	ErrKey       = errors.New("keysource: key refused")
	ErrSign      = errors.New("keysource: signing failed")
)

// Key is a parsed key inside a source: what the backend needs, and how the reference spells it.
type Key struct {
	Name    string // the key name, checked against the source's allow list
	Version string // the pinned version, in the backend's own terms
	Ref     string // the canonical reference <source>:<key>
}

// Backend is one configured source of a kind.
type Backend interface {
	Kind() string
	// ParseKey parses the part of a reference after "<source>:" by the kind's grammar.
	ParseKey(s string) (Key, error)
	// Open fetches the key's public key and checks its properties (Check).
	Open(ctx context.Context, k Key) (Handle, error)
}

// Handle is an opened key.
type Handle interface {
	Public() *rsa.PublicKey
	// Identity is the backend's name for the pinned key version, as its sign answers report it.
	Identity() string
	// Sign signs a 32-byte SHA-256 digest (PKCS#1 v1.5 with DigestInfo) and returns the signature and
	// the identity the backend reported for the key that signed.
	Sign(ctx context.Context, digest []byte) (sig []byte, reported string, err error)
	// Check re-reads the key's properties; an error means the key may no longer sign.
	Check(ctx context.Context) error
}

// Options are the per-source bounds.
type Options struct {
	Allow          []string      // key-name prefixes; "*" = any
	MaxConcurrency int           // default 4
	Timeout        time.Duration // per call; default 10s
	Recheck        time.Duration // property re-check interval; default 10m
}

// Registry maps source names to backends and implements keys.Opener.
type Registry struct {
	files   signer.Resolver
	sources map[string]source
	now     func() time.Time
}

type source struct {
	b    Backend
	opts Options
	sem  chan struct{}
}

// NewRegistry builds a registry with the built-in file source.
func NewRegistry(files signer.Resolver) *Registry {
	return &Registry{files: files, sources: map[string]source{}, now: time.Now}
}

// Add registers a source.
func (r *Registry) Add(name string, b Backend, opts Options) error {
	if name == "file" || r.sources[name].b != nil {
		return fmt.Errorf("keysource: source %q is reserved or defined twice", name)
	}
	if opts.MaxConcurrency <= 0 {
		opts.MaxConcurrency = 4
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.Recheck <= 0 {
		opts.Recheck = 10 * time.Minute
	}
	r.sources[name] = source{b: b, opts: opts, sem: make(chan struct{}, opts.MaxConcurrency)}
	return nil
}

// allowed reports whether a key name passes the allow list.
func allowed(allow []string, name string) bool {
	for _, a := range allow {
		if a == "*" || strings.HasPrefix(name, a) {
			return true
		}
	}
	return false
}

// Parse splits and checks a reference without contacting the backend.
func (r *Registry) Parse(ref string) (string, Key, error) {
	name, rest, ok := strings.Cut(ref, ":")
	if !ok || rest == "" {
		return "", Key{}, fmt.Errorf("%w: %q is not <source>:<key>", ErrReference, ref)
	}
	if name == "file" {
		return name, Key{Name: rest, Ref: ref}, nil
	}
	src, ok := r.sources[name]
	if !ok {
		return "", Key{}, fmt.Errorf("%w: no source named %q", ErrReference, name)
	}
	k, err := src.b.ParseKey(rest)
	if err != nil {
		return "", Key{}, fmt.Errorf("%w: %v", ErrReference, err)
	}
	if !allowed(src.opts.Allow, k.Name) {
		return "", Key{}, fmt.Errorf("%w: key %q is not in source %s's allow list", ErrReference, k.Name, name)
	}
	k.Ref = name + ":" + rest
	return name, k, nil
}

// Open resolves a reference to a signer wrapped in the common contract.
func (r *Registry) Open(ctx context.Context, ref string) (signer.Signer, error) {
	name, k, err := r.Parse(ref)
	if err != nil {
		return nil, err
	}
	if name == "file" {
		return r.files.Open(ctx, ref)
	}
	src := r.sources[name]
	select {
	case src.sem <- struct{}{}: // opens count against the source's concurrency limit too
		defer func() { <-src.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	octx, cancel := context.WithTimeout(ctx, src.opts.Timeout)
	defer cancel()
	h, err := src.b.Open(octx, k)
	if err != nil {
		return nil, err
	}
	if err := extfile.CheckKey(h.Public()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKey, err)
	}
	return &contract{ref: k.Ref, h: h, src: src, now: r.now, checked: r.now()}, nil
}

// contract is the common wrapper around a backend handle.
type contract struct {
	ref string
	h   Handle
	src source
	now func() time.Time

	mu      sync.Mutex // guards checked; held through a re-check, so only one runs at a time
	checked time.Time
}

func (c *contract) Public() *rsa.PublicKey { return c.h.Public() }
func (c *contract) ID() string             { return c.ref }

// Sign implements signer.Signer.
func (c *contract) Sign(ctx context.Context, h extfile.BodyHash) ([]byte, error) {
	select {
	case c.src.sem <- struct{}{}:
		defer func() { <-c.src.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, c.src.opts.Timeout)
	defer cancel()
	c.mu.Lock()
	if c.now().Sub(c.checked) >= c.src.opts.Recheck {
		if err := c.h.Check(ctx); err != nil {
			c.mu.Unlock()
			return nil, fmt.Errorf("%w: the key no longer passes its checks: %v", ErrKey, err)
		}
		c.checked = c.now()
	}
	c.mu.Unlock()
	sig, reported, err := c.h.Sign(ctx, h[:])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSign, err)
	}
	if reported != c.h.Identity() {
		return nil, fmt.Errorf("%w: the backend signed with %q, not the pinned %q", ErrSign, reported, c.h.Identity())
	}
	if len(sig) != extfile.SignatureSize || rsa.VerifyPKCS1v15(c.h.Public(), crypto.SHA256, h[:], sig) != nil {
		return nil, fmt.Errorf("%w: the signature does not verify against the key", ErrSign)
	}
	return sig, nil
}
