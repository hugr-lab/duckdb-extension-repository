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
	"io"
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

// FailureTTL is how long a failed read answers for itself: new connections in that window get the
// same error without a Vault call, so a Vault that does not answer is not asked once per caller.
const FailureTTL = 5 * time.Second

// maxPending bounds the reads remembered for an observer set later (the store's open at start).
const maxPending = 16

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

// flight is one read of Vault, which the callers that arrive meanwhile wait for and share.
type flight struct {
	done chan struct{}
	pw   string
	err  error
	// abandoned: the leader's own context ended it; the waiters read again themselves
	abandoned bool
}

type observation struct {
	outcome string
	d       time.Duration
}

// Login is a store.Login (and store.Rejecter) reading Vault. Its String and Format never show the
// password.
type Login struct {
	api Requester
	o   Options

	mu          sync.Mutex
	pw          string
	until       time.Time
	cur         *flight // the read in progress, if any
	failErr     error   // the last read's failure, answered until failedUntil
	failedUntil time.Time
	pending     []observation // reads before an observer was set

	observe func(outcome string, d time.Duration) // under mu
	counts  sync.Map                              // outcome -> *atomic.Int64
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
	return &Login{api: api, o: o}, nil
}

// String names the login's secret, never the password.
func (l *Login) String() string {
	if l.o.Kind == KV {
		return "vault login (kv " + l.o.Mount + "/" + l.o.Path + "#" + l.o.Field + ")"
	}
	return "vault login (static_role " + l.o.Mount + "/" + l.o.Name + ")"
}

// Format prints String for every verb, %#v and %+v included, so no formatting shows the cache.
func (l *Login) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, l.String()) }

// Observe sets a function told about each Vault read: its outcome and duration (metrics). The
// reads made before it is set (the store's open at start) are told at once.
func (l *Login) Observe(f func(outcome string, d time.Duration)) {
	l.mu.Lock()
	l.observe = f
	pending := l.pending
	l.pending = nil
	l.mu.Unlock()
	for _, p := range pending {
		f(p.outcome, p.d)
	}
}

func (l *Login) observed(outcome string, d time.Duration) {
	l.mu.Lock()
	f := l.observe
	if f == nil && len(l.pending) < maxPending {
		l.pending = append(l.pending, observation{outcome, d})
	}
	l.mu.Unlock()
	if f != nil {
		f(outcome, d)
	}
}

func (l *Login) count(outcome string) {
	c, _ := l.counts.LoadOrStore(outcome, new(atomic.Int64))
	c.(*atomic.Int64).Add(1)
}

// Counts are the requests for a password so far, by outcome. A caller whose own context ended is
// not counted.
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

// Password implements store.Login: the cached password; or the answer of a read in progress; or,
// within FailureTTL of a failed read, its error; or a read of Vault.
func (l *Login) Password(ctx context.Context) (string, error) {
	for {
		l.mu.Lock()
		now := l.o.Now()
		switch {
		case l.pw != "" && now.Before(l.until):
			pw := l.pw
			l.mu.Unlock()
			l.count(OutcomeCached)
			return pw, nil
		case l.failErr != nil && now.Before(l.failedUntil):
			err := l.failErr
			l.mu.Unlock()
			l.count(OutcomeError)
			return "", err
		}
		if f := l.cur; f != nil { // share the read in progress
			l.mu.Unlock()
			select {
			case <-f.done:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			if f.abandoned {
				continue
			}
			if f.err != nil {
				l.count(OutcomeError)
			} else {
				l.count(OutcomeCached)
			}
			return f.pw, f.err
		}
		f := &flight{done: make(chan struct{})}
		l.cur = f
		l.mu.Unlock()
		return l.lead(ctx, f)
	}
}

// lead reads Vault for everyone waiting on f.
func (l *Login) lead(ctx context.Context, f *flight) (string, error) {
	start := l.o.Now()
	pw, ttl, err := l.read(ctx)
	d := l.o.Now().Sub(start)
	l.mu.Lock()
	l.cur = nil
	switch {
	case err == nil:
		if ttl > 0 {
			l.pw, l.until = pw, l.o.Now().Add(ttl)
		} else {
			l.pw, l.until = "", time.Time{}
		}
		l.failErr = nil
	case ctx.Err() != nil: // the caller's own end: no failure of Vault's, nothing to share
		f.abandoned = true
	default: // a failed read ends the cache and answers for itself a moment
		l.pw, l.until = "", time.Time{}
		l.failErr, l.failedUntil = err, l.o.Now().Add(FailureTTL)
	}
	f.pw, f.err = pw, err
	close(f.done)
	l.mu.Unlock()
	switch {
	case err == nil:
		l.count(OutcomeVault)
		l.observed(OutcomeVault, d)
	case !f.abandoned:
		l.count(OutcomeError)
		l.observed(OutcomeError, d)
	}
	return pw, err
}

// Rejected implements store.Rejecter: the database refused a password this login gave, so it is
// not used again. Only the password the cache holds is dropped: a rejection of an older one (a
// connection begun before a rotation) leaves a newer password in place.
func (l *Login) Rejected(password string) {
	l.mu.Lock()
	drop := password != "" && password == l.pw
	if drop {
		l.pw, l.until = "", time.Time{}
	}
	l.mu.Unlock()
	if drop {
		l.count(OutcomeRejected)
	}
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
