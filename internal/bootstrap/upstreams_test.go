package bootstrap_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bootstrap"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

const sourcePrefix = "https://up.example/vendor/prod"

// source is another repository's .well-known: its key, down for a while, or gone.
type source struct {
	mu         sync.Mutex
	pem        string
	down, gone int   // answers left that fail: no connection; 404
	busy       []int // statuses to answer first, in order (503, 429)
	reads      int
}

func newSource(t *testing.T) (*source, string) {
	t.Helper()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return &source{pem: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}, extfile.Fingerprint(&k.PublicKey)
}

func (s *source) Get(_ context.Context, url string) ([]byte, http.Header, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if url != sourcePrefix+"/.well-known/duckdb-extension-repo.json" {
		return nil, nil, &egress.StatusError{Status: 404, Host: "up.example"}
	}
	switch {
	case len(s.busy) > 0:
		code := s.busy[0]
		s.busy = s.busy[1:]
		return nil, nil, &egress.StatusError{Status: code, Host: "up.example"}
	case s.down > 0:
		s.down--
		return nil, nil, egress.ErrConnect
	case s.gone > 0:
		s.gone--
		return nil, nil, &egress.StatusError{Status: 404, Host: "up.example"}
	}
	b, _ := json.Marshal(map[string]any{"signature_keys": []string{s.pem}})
	return b, http.Header{}, nil
}

func (s *source) Download(context.Context, string, egress.DownloadOptions, io.Writer) (egress.Download, error) {
	return egress.Download{}, errors.New("no downloads in the bootstrap")
}

func (s *source) PortAllowed(uint16) bool { return true }

// withUpstreams gives the env an upstream service reading the source, and the section two upstreams
// of its channel: DuckDB core, and the source.
func (en *env) withUpstreams(t *testing.T, src *source, fp string) config.Bootstrap {
	t.Helper()
	en.d.Upstreams = &upstream.Service{Store: en.st, Fetch: src, Authz: authz.ServerAdmin{}, Log: slog.New(slog.DiscardHandler)}
	b := section()
	b.Upstreams = []config.BootstrapUpstream{
		{Name: "core", Kind: store.UpstreamCore, Channel: "prod", Platforms: []string{"linux_amd64"},
			Extensions: []config.BootstrapUpstreamEntry{{Name: "httpfs"}}},
		{Name: "vendor", Kind: store.UpstreamRepo, Prefix: sourcePrefix + "/", Keys: []string{fp}, Channel: "prod",
			Platforms: []string{"linux_amd64", "linux_arm64"}, Extensions: []config.BootstrapUpstreamEntry{{Name: "acl", Versions: []string{"1.0.0"}}}},
	}
	return b
}

// Spec 0024: the section's upstreams are made after the grants, through upstream add, each with its
// event as system:bootstrap, due for a first run; the next start finds them the same.
func TestBootstrapUpstreams(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			src, fp := newSource(t)
			b := en.withUpstreams(t, src, fp)

			// dry run first: the plan names them, nothing is written
			res, err := bootstrap.Run(ctx, en.d, b, bootstrap.Options{Actor: authz.OSActor(), DryRun: true})
			if err != nil {
				t.Fatalf("dry run: %v", err)
			}
			if a := planned(res.Plan, "upstream", "vendor"); a != bootstrap.Create {
				t.Fatalf("dry run, vendor: %q (%v)", a, res.Plan.Items)
			}
			if _, err := en.st.GetTenant(ctx, "acme"); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("a dry run wrote")
			}

			res, err = bootstrap.Run(ctx, en.d, b, serveOpts)
			if err != nil || !res.Applied {
				t.Fatalf("apply: %+v %v", res, err)
			}
			tn, _ := en.st.GetTenant(ctx, "acme")
			var ups []store.Upstream
			_ = en.st.InTx(ctx, "", func(tx *store.Tx) error {
				var err error
				ups, err = tx.ListUpstreams(ctx, tn.ID)
				return err
			})
			if len(ups) != 2 || ups[1].Name != "vendor" || ups[1].Prefix != sourcePrefix || ups[1].Visibility != store.Private ||
				!ups[1].NextRunAt.IsZero() {
				t.Fatalf("the upstreams: %+v", ups)
			}
			evs := en.events(t, "upstream.add")
			if len(evs) != 2 || evs[0].Actor != "system:bootstrap" {
				t.Fatalf("upstream.add: %+v", evs)
			}
			// due for a first run: a mirror without a next run
			due, err := en.st.DueUpstreams(ctx, time.Now(), true)
			if err != nil || len(due) != 2 {
				t.Fatalf("due: %d %v", len(due), err)
			}

			// the record holds: the next start applies nothing; a re-armed one finds them the same
			if res, err := bootstrap.Run(ctx, en.d, b, serveOpts); err != nil || !res.Already {
				t.Fatalf("again: %+v %v", res, err)
			}
			if err := bootstrap.Rearm(ctx, en.d, authz.OSActor()); err != nil {
				t.Fatal(err)
			}
			res, err = bootstrap.Run(ctx, en.d, b, bootstrap.Options{Actor: authz.OSActor(), Holder: "cli", Adopt: true, Retry: 1})
			if err != nil || planned(res.Plan, "upstream", "vendor") != bootstrap.Same || planned(res.Plan, "upstream", "core") != bootstrap.Same {
				t.Fatalf("re-armed: %+v %v", res.Plan.Items, err)
			}
		})
	}
}

// What upstream add would refuse is found before any write: a source without the pinned key, an
// allowlist that differs from an upstream that exists, a name another upstream of the channel lists.
func TestBootstrapUpstreamsRefused(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			src, _ := newSource(t)
			_, other := newSource(t)
			b := en.withUpstreams(t, src, other) // pins a key the source does not have
			_, err := bootstrap.Run(ctx, en.d, b, serveOpts)
			if !errors.Is(err, bootstrap.ErrRefused) || !strings.Contains(err.Error(), "vendor") {
				t.Fatalf("an unpinned key: %v", err)
			}
			if _, err := en.st.GetTenant(ctx, "acme"); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("written before the refusal")
			}

			// a source that is gone (404) is a refusal, not a wait
			src2, fp2 := newSource(t)
			src2.gone = 100
			b = en.withUpstreams(t, src2, fp2)
			if _, err := bootstrap.Run(ctx, en.d, b, serveOpts); !errors.Is(err, bootstrap.ErrRefused) {
				t.Fatalf("a 404: %v", err)
			}
		})
	}
}

// A source not reachable for now is tried again: the bootstrap converges.
func TestBootstrapUpstreamsWait(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			src, fp := newSource(t)
			src.down = 2
			b := en.withUpstreams(t, src, fp)
			o := serveOpts
			o.Retry = 30 * time.Second // the first tries wait 1s, 2s
			res, err := bootstrap.Run(ctx, en.d, b, o)
			if err != nil || !res.Applied {
				t.Fatalf("converged: %+v %v", res, err)
			}
			if src.reads < 3 {
				t.Fatalf("read %d times", src.reads)
			}
		})
	}
}

func planned(p bootstrap.Plan, kind, name string) bootstrap.Action {
	for _, i := range p.Items {
		if i.Kind == kind && i.Name == name {
			return i.Action
		}
	}
	return ""
}

// In a tenant that exists, the section's upstreams are checked against what it holds, all of them
// together: a different allowlist, a collision with an upstream there, two of the section's that
// collide with each other: refused before any write. Without -adopt, none is made at all.
func TestBootstrapUpstreamsExisting(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			src, fp := newSource(t)
			b := en.withUpstreams(t, src, fp)
			if _, err := bootstrap.Run(ctx, en.d, b, serveOpts); err != nil {
				t.Fatal(err)
			}
			tn, _ := en.st.GetTenant(ctx, "acme")
			count := func() int {
				var n int
				_ = en.st.InTx(ctx, "", func(tx *store.Tx) error {
					ups, err := tx.ListUpstreams(ctx, tn.ID)
					n = len(ups)
					return err
				})
				return n
			}
			if err := bootstrap.Rearm(ctx, en.d, authz.OSActor()); err != nil {
				t.Fatal(err)
			}
			adopt := bootstrap.Options{Actor: authz.OSActor(), Holder: "cli", Adopt: true, Retry: 1}
			refused := func(b config.Bootstrap, name, want string) {
				t.Helper()
				res, err := bootstrap.Run(ctx, en.d, b, adopt)
				if !errors.Is(err, bootstrap.ErrRefused) || planned(res.Plan, "upstream", name) != bootstrap.Refused || !strings.Contains(err.Error(), want) {
					t.Fatalf("%s: %v %v", name, err, res.Plan.Items)
				}
				if count() != 2 {
					t.Fatal("written before the refusal")
				}
			}
			// another allowlist for an upstream that exists
			changed := en.withUpstreams(t, src, fp)
			changed.Upstreams[1].Extensions[0].Versions = []string{"2.0.0"}
			refused(changed, "vendor", "allowlist entry acl")
			// a name another upstream of the channel lists
			taken := en.withUpstreams(t, src, fp)
			taken.Upstreams = append(taken.Upstreams, config.BootstrapUpstream{Name: "core2", Kind: store.UpstreamCore, Channel: "prod",
				Platforms: []string{"linux_arm64"}, Extensions: []config.BootstrapUpstreamEntry{{Name: "httpfs"}}})
			refused(taken, "core2", "httpfs")
			// two of the section's that collide with each other: the second is refused, neither made
			pair := en.withUpstreams(t, src, fp)
			pair.Upstreams = append(pair.Upstreams,
				config.BootstrapUpstream{Name: "json-a", Kind: store.UpstreamCore, Channel: "prod", Platforms: []string{"linux_amd64"},
					Extensions: []config.BootstrapUpstreamEntry{{Name: "json"}}},
				config.BootstrapUpstream{Name: "json-b", Kind: store.UpstreamCore, Channel: "prod", Platforms: []string{"linux_amd64"},
					Extensions: []config.BootstrapUpstreamEntry{{Name: "json"}}})
			refused(pair, "json-b", "json")
			// without -adopt: a new upstream in a tenant the bootstrap did not make (it was re-armed)
			more := en.withUpstreams(t, src, fp)
			more.Upstreams = append(more.Upstreams, config.BootstrapUpstream{Name: "json", Kind: store.UpstreamCore, Channel: "prod",
				Platforms: []string{"linux_amd64"}, Extensions: []config.BootstrapUpstreamEntry{{Name: "json"}}})
			if _, err := bootstrap.Run(ctx, en.d, more, serveOpts); !errors.Is(err, bootstrap.ErrRefused) || !strings.Contains(err.Error(), "-adopt") {
				t.Fatalf("without -adopt: %v", err)
			}
			if count() != 2 {
				t.Fatal("made without -adopt")
			}
		})
	}
}

// 503 and 429 from a source are answers that may pass: tried again.
func TestBootstrapUpstreamsBusy(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			en := newEnv(t, e)
			src, fp := newSource(t)
			src.busy = []int{503, 429}
			b := en.withUpstreams(t, src, fp)
			o := serveOpts
			o.Retry = 30 * time.Second
			if res, err := bootstrap.Run(ctx, en.d, b, o); err != nil || !res.Applied {
				t.Fatalf("converged: %+v %v", res, err)
			}
		})
	}
}
