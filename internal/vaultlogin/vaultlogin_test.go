package vaultlogin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/vaultapi"
)

var ctx = context.Background()

// planted is a value that must never reach an error.
const planted = "hunter2-planted-secret"

// fake is a Vault serving a KV v2 secret and a static role.
type fake struct {
	mu      sync.Mutex
	calls   atomic.Int64
	query   string
	status  int    // non-zero: every read answers this, with the planted value in the body
	kv      string // the data map's JSON
	creds   string // the static role's data JSON
	token   string
	srv     *httptest.Server
	release chan struct{} // when set, reads wait for it
}

func newFake(t *testing.T) *fake {
	f := &fake{token: "s.test-token", kv: `{"password":"pw-1","user":"kista"}`,
		creds: `{"username":"kista","password":"static-1","ttl":3600,"last_vault_rotation":"2026-10-21T10:00:00Z"}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != f.token {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.calls.Add(1)
		if f.release != nil {
			<-f.release
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.query = r.URL.RawQuery
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"errors":["permission denied ` + planted + `"]}`))
			return
		}
		switch r.URL.Path {
		case "/v1/secret/data/kista/db":
			_, _ = w.Write([]byte(`{"data":{"data":` + f.kv + `,"metadata":{"version":2}}}`))
		case "/v1/kv1/data/kista/db": // a KV v1 mount holding a secret at data/kista/db: no inner data
			_, _ = w.Write([]byte(`{"data":{"password":"pw"}}`))
		case "/v1/database/static-creds/kista":
			_, _ = w.Write([]byte(`{"data":` + f.creds + `}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[]}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) set(fn func(f *fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fake) client(t *testing.T) *vaultapi.Client {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(f.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := vaultapi.New(vaultapi.Config{Address: f.srv.URL, AuthKind: vaultapi.AuthTokenFile, TokenFile: p, AllowHTTP: true,
		Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type clock struct{ now atomic.Int64 }

func (c *clock) Now() time.Time      { return time.Unix(0, c.now.Load()) }
func (c *clock) add(d time.Duration) { c.now.Add(int64(d)) }
func newClock() *clock {
	c := &clock{}
	c.now.Store(time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func kvOpts(c *clock) Options {
	return Options{Kind: KV, Mount: "secret", Path: "kista/db", Field: "password", Cache: time.Minute, User: "kista", Now: c.Now}
}
func staticOpts(c *clock) Options {
	return Options{Kind: StaticRole, Mount: "database", Name: "kista", User: "kista", Now: c.Now}
}
func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func TestKV(t *testing.T) {
	f, c := newFake(t), newClock()
	l := must[*Login](t)(New(f.client(t), kvOpts(c)))
	var observed []string
	l.Observe(func(o string, _ time.Duration) { observed = append(observed, o) })
	if pw := must[string](t)(l.Password(ctx)); pw != "pw-1" {
		t.Fatalf("password %q", pw)
	}
	// cached for a minute: a burst makes one call
	for range 5 {
		must[string](t)(l.Password(ctx))
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("%d Vault calls", n)
	}
	if e := l.Expiry(); e != time.Minute {
		t.Fatalf("expiry %s", e)
	}
	// a rotation in Vault takes effect within the cache
	f.set(func(f *fake) { f.kv = `{"password":"pw-2"}` })
	if pw := must[string](t)(l.Password(ctx)); pw != "pw-1" {
		t.Fatalf("before expiry: %q", pw)
	}
	c.add(time.Minute)
	if pw := must[string](t)(l.Password(ctx)); pw != "pw-2" || f.calls.Load() != 2 {
		t.Fatalf("after expiry: %q, %d calls", pw, f.calls.Load())
	}
	if f.query != "" {
		t.Fatalf("an unpinned read sent %q", f.query)
	}
	got := l.Counts()
	if got[OutcomeVault] != 2 || got[OutcomeCached] != 6 || len(observed) != 2 || observed[0] != OutcomeVault {
		t.Fatalf("counts %v, observed %v", got, observed)
	}
}

func TestKVVersion(t *testing.T) {
	f, c := newFake(t), newClock()
	o := kvOpts(c)
	o.Version = 2
	l := must[*Login](t)(New(f.client(t), o))
	must[string](t)(l.Password(ctx))
	if f.query != "version=2" {
		t.Fatalf("query %q", f.query)
	}
}

func TestKVRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		kv, path, want string
	}{
		"a missing field":    {kv: `{"user":"kista"}`, want: "has no field password"},
		"a number":           {kv: `{"password":42}`, want: "not a string"},
		"an object":          {kv: `{"password":{"v":"` + planted + `"}}`, want: "not a string"},
		"empty":              {kv: `{"password":""}`, want: "empty"},
		"a KV v1 mount":      {path: "kista/db", want: "no KV v2 secret"},
		"another path (404)": {path: "nope", want: "status 404"},
	} {
		t.Run(name, func(t *testing.T) {
			f, c := newFake(t), newClock()
			if tc.kv != "" {
				f.kv = tc.kv
			}
			o := kvOpts(c)
			if tc.path != "" {
				o.Mount, o.Path = "kv1", tc.path
			}
			l := must[*Login](t)(New(f.client(t), o))
			_, err := l.Password(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), planted) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "store: vault login: ") {
				t.Fatalf("the error does not name the source: %v", err)
			}
		})
	}
}

func TestStaticRole(t *testing.T) {
	f, c := newFake(t), newClock()
	l := must[*Login](t)(New(f.client(t), staticOpts(c)))
	if pw := must[string](t)(l.Password(ctx)); pw != "static-1" {
		t.Fatalf("password %q", pw)
	}
	// kept until the TTL less the margin
	if e := l.Expiry(); e != time.Hour-StaticMargin {
		t.Fatalf("expiry %s", e)
	}
	c.add(time.Hour - StaticMargin - time.Second)
	must[string](t)(l.Password(ctx))
	if f.calls.Load() != 1 {
		t.Fatal("read again before the TTL")
	}
	f.set(func(f *fake) { f.creds = `{"username":"kista","password":"static-2","ttl":3600}` })
	c.add(time.Second)
	if pw := must[string](t)(l.Password(ctx)); pw != "static-2" {
		t.Fatalf("after the TTL: %q", pw)
	}
	// a TTL within the margin is not cached
	f.set(func(f *fake) { f.creds = `{"username":"kista","password":"static-3","ttl":20}` })
	c.add(time.Hour)
	must[string](t)(l.Password(ctx))
	must[string](t)(l.Password(ctx))
	if n := f.calls.Load(); n != 4 {
		t.Fatalf("%d calls", n)
	}
}

func TestStaticRoleOtherUser(t *testing.T) {
	f, c := newFake(t), newClock()
	f.creds = `{"username":"admin","password":"` + planted + `","ttl":3600}`
	l := must[*Login](t)(New(f.client(t), staticOpts(c)))
	_, err := l.Password(ctx)
	if err == nil || !strings.Contains(err.Error(), "another user than the DSN's") || strings.Contains(err.Error(), planted) {
		t.Fatalf("got %v", err)
	}
	if l.Expiry() != 0 {
		t.Fatal("a refused password was cached")
	}
}

// A failed read ends the cache and answers a fixed error, for itself during FailureTTL; a Vault
// error's body never shows.
func TestFailures(t *testing.T) {
	f, c := newFake(t), newClock()
	l := must[*Login](t)(New(f.client(t), kvOpts(c)))
	must[string](t)(l.Password(ctx))
	c.add(time.Minute)
	f.set(func(f *fake) { f.status = http.StatusForbidden })
	_, err := l.Password(ctx)
	if err == nil || err.Error() != "store: vault login: status 403" {
		t.Fatalf("got %v", err)
	}
	if l.Expiry() != 0 {
		t.Fatal("the cache outlived a failed read")
	}
	// within FailureTTL the failure answers without a Vault call
	calls := f.calls.Load()
	f.set(func(f *fake) { f.status = 0 })
	if _, err := l.Password(ctx); err == nil || err.Error() != "store: vault login: status 403" || f.calls.Load() != calls {
		t.Fatalf("got %v after %d calls", err, f.calls.Load()-calls)
	}
	c.add(FailureTTL)
	f.set(func(f *fake) { f.status = http.StatusInternalServerError })
	if _, err := l.Password(ctx); err == nil || strings.Contains(err.Error(), planted) || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("got %v", err)
	}
	// Vault gone
	c.add(FailureTTL)
	f.srv.Close()
	if _, err := l.Password(ctx); err == nil || err.Error() != "store: vault login: Vault did not answer" {
		t.Fatalf("got %v", err)
	}
	if got := l.Counts()[OutcomeError]; got != 4 {
		t.Fatalf("%d errors counted", got)
	}
	// Vault back, after FailureTTL
	f2 := newFake(t)
	l2 := must[*Login](t)(New(f2.client(t), kvOpts(c)))
	f2.set(func(f *fake) { f.status = http.StatusForbidden })
	_, _ = l2.Password(ctx)
	f2.set(func(f *fake) { f.status = 0 })
	c.add(FailureTTL)
	if pw := must[string](t)(l2.Password(ctx)); pw != "pw-1" {
		t.Fatalf("after FailureTTL: %q", pw)
	}
}

// A password the database rejected is dropped: the next connection reads Vault again. A rejection
// of another password than the cached one (an older connection's) keeps the cache.
func TestRejected(t *testing.T) {
	f, c := newFake(t), newClock()
	l := must[*Login](t)(New(f.client(t), kvOpts(c)))
	must[string](t)(l.Password(ctx))
	for _, old := range []string{"an-older-password", ""} {
		l.Rejected(old)
		if l.Expiry() == 0 {
			t.Fatalf("a rejection of %q dropped the cached password", old)
		}
	}
	l.Rejected("pw-1")
	if l.Expiry() != 0 {
		t.Fatal("the cache outlived a rejection")
	}
	must[string](t)(l.Password(ctx))
	if n := f.calls.Load(); n != 2 || l.Counts()[OutcomeRejected] != 1 {
		t.Fatalf("%d calls, counts %v", n, l.Counts())
	}
}

// A burst of new connections while Vault answers slowly makes one call, and all share its answer.
func TestBurst(t *testing.T) {
	f, c := newFake(t), newClock()
	f.release = make(chan struct{})
	l := must[*Login](t)(New(f.client(t), kvOpts(c)))
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			if pw, err := l.Password(ctx); err != nil || pw != "pw-1" {
				errs <- errors.Join(err, errors.New(pw))
			}
		})
	}
	time.Sleep(100 * time.Millisecond)
	close(f.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("%d Vault calls for a burst", n)
	}
}

// A Vault that does not answer: the waiters share the leader's error instead of each reading in
// turn, and the next callers get it from the failure's short cache.
func TestBlackHole(t *testing.T) {
	f, c := newFake(t), newClock()
	f.release = make(chan struct{}) // never answers until the test ends
	t.Cleanup(func() { close(f.release) })
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(f.token), 0o600); err != nil {
		t.Fatal(err)
	}
	api, err := vaultapi.New(vaultapi.Config{Address: f.srv.URL, AuthKind: vaultapi.AuthTokenFile, TokenFile: p, AllowHTTP: true,
		Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	l := must[*Login](t)(New(api, kvOpts(c)))
	var reads atomic.Int64
	l.Observe(func(string, time.Duration) { reads.Add(1) })
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if _, err := l.Password(ctx); err == nil || err.Error() != "store: vault login: Vault did not answer" {
				t.Errorf("got %v", err)
			}
		})
	}
	wg.Wait()
	if _, err := l.Password(ctx); err == nil {
		t.Fatal("a black-holed Vault answered")
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("%d reads of a Vault that does not answer", n)
	}
	if got := l.Counts()[OutcomeError]; got != 11 {
		t.Fatalf("%d errors counted", got)
	}
}

// A caller waiting for another's read gives up with its context; a leader's own cancellation is
// neither an error to share nor one to count, and a waiter then reads itself.
func TestContexts(t *testing.T) {
	f, c := newFake(t), newClock()
	f.release = make(chan struct{})
	l := must[*Login](t)(New(f.client(t), kvOpts(c)))
	lctx, lcancel := context.WithCancel(ctx)
	leader := make(chan error, 1)
	go func() { _, err := l.Password(lctx); leader <- err }()
	time.Sleep(50 * time.Millisecond)
	wctx, wcancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer wcancel()
	if _, err := l.Password(wctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter: got %v", err)
	}
	// a waiter that outlives the leader's cancellation reads Vault itself
	waiter := make(chan string, 1)
	go func() { pw, _ := l.Password(ctx); waiter <- pw }()
	time.Sleep(50 * time.Millisecond)
	lcancel()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader: got %v", err)
	}
	close(f.release)
	if pw := <-waiter; pw != "pw-1" {
		t.Fatalf("the waiter got %q", pw)
	}
	if got := l.Counts(); got[OutcomeError] != 0 || got[OutcomeVault] != 1 {
		t.Fatalf("counts %v", got)
	}
	if l.failErr != nil {
		t.Fatal("a cancellation was cached as a failure")
	}
}

// The reads before an observer is set (the store's open at start) reach it when it is set.
func TestObserveLater(t *testing.T) {
	f, c := newFake(t), newClock()
	l := must[*Login](t)(New(f.client(t), kvOpts(c)))
	must[string](t)(l.Password(ctx))
	var got []string
	l.Observe(func(o string, _ time.Duration) { got = append(got, o) })
	c.add(time.Minute)
	must[string](t)(l.Password(ctx))
	if len(got) != 2 || got[0] != OutcomeVault || got[1] != OutcomeVault {
		t.Fatalf("observed %v", got)
	}
}

// No formatting of the login shows its password.
func TestRedacted(t *testing.T) {
	f, c := newFake(t), newClock()
	l := must[*Login](t)(New(f.client(t), kvOpts(c)))
	must[string](t)(l.Password(ctx))
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		if s := fmt.Sprintf(verb, l); strings.Contains(s, "pw-1") || !strings.Contains(s, "secret/kista/db") {
			t.Errorf("%s: %q", verb, s)
		}
	}
	if s := fmt.Sprintf("%v", struct{ L *Login }{l}); strings.Contains(s, "pw-1") {
		t.Errorf("nested: %q", s)
	}
}

func TestNew(t *testing.T) {
	for _, o := range []Options{{Kind: "dynamic_role", Mount: "database", Name: "x"}, {Kind: KV, Mount: "secret", Path: "p"},
		{Kind: KV, Mount: "secret", Field: "f"}, {Kind: StaticRole, Mount: "database"}, {Kind: KV, Mount: "s", Path: "p", Field: "f", Version: -1}} {
		if _, err := New(nil, o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}
