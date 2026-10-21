// Package vaultlogin is the database login from HashiCorp Vault or OpenBao (spec 0027): kista reads
// its database password itself, through the vaultapi client the vault key source uses, from a KV v2
// path or a database secrets engine static role, and caches it briefly.
//
// The password lives in memory only: for at most the KV cache, or until the static role's TTL less a
// margin. A failed read ends the cache, and a password the database rejected (store.Rejecter) is
// dropped, so the next connection reads Vault again. Errors name the source and a status, never a
// value, a token or a response body.
package vaultlogin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/vaultapi"
)

// Requester is the narrow Vault API the login uses (vaultapi.Client; fakes in tests).
type Requester interface {
	Do(ctx context.Context, method, path string, body, out any) error
}

// Kinds of secret.
const (
	KV         = "kv"
	StaticRole = "static_role"
)

// Outcomes, as the metrics count them.
const (
	OutcomeVault    = "vault"    // read from Vault
	OutcomeCached   = "cached"   // served from the cache
	OutcomeError    = "error"    // the read failed
	OutcomeRejected = "rejected" // the database rejected the password
)

// StaticMargin is how long before a static role's rotation its password stops being cached.
const StaticMargin = 30 * time.Second

// Options say where the password is.
type Options struct {
	Kind    string // KV or StaticRole
	Mount   string // the KV v2 mount, or the database secrets engine's
	Path    string // KV: the secret's path under the mount
	Field   string // KV: the string field holding the password
	Version int    // KV: a pinned version; 0 reads the latest
	Name    string // StaticRole: the role
	Cache   time.Duration
	// User is the DSN's user: a static role's username must equal it.
	User string
	// Now is the clock (tests).
	Now func() time.Time
}

// Login is a store.Login (and store.Rejecter) reading Vault.
type Login struct {
	api Requester
	o   Options
	sem chan struct{} // one read at a time: a burst of new connections makes one Vault call

	mu    sync.Mutex
	pw    string
	until time.Time

	observe atomic.Pointer[func(outcome string, d time.Duration)]
	counts  sync.Map // outcome -> *atomic.Int64
}

// New returns a login. It contacts nothing: the store's first connection does.
func New(api Requester, o Options) (*Login, error) {
	switch o.Kind {
	case KV:
		if o.Mount == "" || o.Path == "" || o.Field == "" || o.Version < 0 {
			return nil, errors.New("vaultlogin: kv needs a mount, a path and a field")
		}
	case StaticRole:
		if o.Mount == "" || o.Name == "" {
			return nil, errors.New("vaultlogin: static_role needs a mount and a name")
		}
	default:
		return nil, fmt.Errorf("vaultlogin: unknown kind %q", o.Kind)
	}
	o.Mount, o.Path = strings.Trim(o.Mount, "/"), strings.Trim(o.Path, "/")
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Login{api: api, o: o, sem: make(chan struct{}, 1)}, nil
}

// Observe sets a function told about each Vault read: its outcome and duration (metrics).
func (l *Login) Observe(f func(outcome string, d time.Duration)) { l.observe.Store(&f) }

func (l *Login) count(outcome string) {
	c, _ := l.counts.LoadOrStore(outcome, new(atomic.Int64))
	c.(*atomic.Int64).Add(1)
}

// Counts are the requests for a password so far, by outcome.
func (l *Login) Counts() map[string]int64 {
	out := map[string]int64{}
	l.counts.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}

// Expiry is how long the cached password may still be used; 0 when none is cached.
func (l *Login) Expiry() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pw == "" {
		return 0
	}
	return max(0, l.until.Sub(l.o.Now()))
}

func (l *Login) cached() (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pw != "" && l.o.Now().Before(l.until) {
		return l.pw, true
	}
	return "", false
}

func (l *Login) set(pw string, ttl time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ttl <= 0 {
		pw = ""
	}
	l.pw, l.until = pw, l.o.Now().Add(ttl)
}

// Password implements store.Login: the cached password, or a read of Vault.
func (l *Login) Password(ctx context.Context) (string, error) {
	if pw, ok := l.cached(); ok {
		l.count(OutcomeCached)
		return pw, nil
	}
	select {
	case l.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-l.sem }()
	if pw, ok := l.cached(); ok { // read by the caller we waited for
		l.count(OutcomeCached)
		return pw, nil
	}
	start := l.o.Now()
	pw, ttl, err := l.read(ctx)
	outcome := OutcomeVault
	if err != nil {
		outcome, pw, ttl = OutcomeError, "", 0 // a failed read ends the cache
	}
	l.set(pw, ttl)
	l.count(outcome)
	if f := l.observe.Load(); f != nil {
		(*f)(outcome, l.o.Now().Sub(start))
	}
	return pw, err
}

// Rejected implements store.Rejecter: the database refused the password, so it is not used again.
func (l *Login) Rejected() {
	l.set("", 0)
	l.count(OutcomeRejected)
}

// read reads the password and how long it may be cached.
func (l *Login) read(ctx context.Context) (string, time.Duration, error) {
	switch l.o.Kind {
	case KV:
		p := l.o.Mount + "/data/" + l.o.Path
		if l.o.Version > 0 {
			p += "?version=" + strconv.Itoa(l.o.Version)
		}
		var out struct {
			Data *struct {
				Data map[string]json.RawMessage `json:"data"`
			} `json:"data"`
		}
		if err := l.api.Do(ctx, http.MethodGet, p, nil, &out); err != nil {
			return "", 0, describe(err)
		}
		if out.Data == nil || out.Data.Data == nil {
			return "", 0, fmt.Errorf("store: vault login: no KV v2 secret at %s/%s", l.o.Mount, l.o.Path)
		}
		raw, ok := out.Data.Data[l.o.Field]
		if !ok {
			return "", 0, fmt.Errorf("store: vault login: the secret at %s/%s has no field %s", l.o.Mount, l.o.Path, l.o.Field)
		}
		var pw string
		if json.Unmarshal(raw, &pw) != nil {
			return "", 0, fmt.Errorf("store: vault login: the field %s is not a string", l.o.Field)
		}
		if pw == "" {
			return "", 0, fmt.Errorf("store: vault login: the field %s is empty", l.o.Field)
		}
		return pw, l.o.Cache, nil
	case StaticRole:
		var out struct {
			Data *struct {
				Username string `json:"username"`
				Password string `json:"password"`
				TTL      int64  `json:"ttl"`
			} `json:"data"`
		}
		if err := l.api.Do(ctx, http.MethodGet, l.o.Mount+"/static-creds/"+l.o.Name, nil, &out); err != nil {
			return "", 0, describe(err)
		}
		switch {
		case out.Data == nil || out.Data.Password == "":
			return "", 0, fmt.Errorf("store: vault login: static role %s gave no password", l.o.Name)
		case out.Data.Username != l.o.User:
			return "", 0, fmt.Errorf("store: vault login: static role %s is for another user than the DSN's (%s)", l.o.Name, l.o.User)
		}
		return out.Data.Password, time.Duration(out.Data.TTL)*time.Second - StaticMargin, nil
	}
	return "", 0, errors.New("store: vault login: unknown kind")
}

// describe reduces Vault's errors to a status (never Vault's message or a body); a transport
// failure is "did not answer".
func describe(err error) error {
	var ve *vaultapi.Error
	switch {
	case errors.As(err, &ve):
		return fmt.Errorf("store: vault login: status %d", ve.Status)
	case errors.Is(err, vaultapi.ErrUnreachable):
		return errors.New("store: vault login: Vault did not answer")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("store: vault login: %w", err)
	}
	return fmt.Errorf("store: vault login: %w", err) // vaultapi's own: a token file, a refused login
}
