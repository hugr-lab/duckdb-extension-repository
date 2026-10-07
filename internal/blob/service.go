package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Domain is a named store from config.
type Domain struct {
	Name  string
	Kind  string // fs, s3, azureblob
	Store Store
}

// Options configure a Service.
type Options struct {
	SpoolDir   string // absolute; created with mode 0700
	MaxBody    int64  // the largest body accepted
	MaxIngests int    // concurrent spools
	Log        *slog.Logger
}

// Service stores and serves extension bodies.
type Service struct {
	st      *store.Store
	domains map[string]Domain
	spool   *os.Root
	sem     chan struct{}
	maxBody int64
	log     *slog.Logger

	mu    sync.Mutex
	cache map[cacheKey]store.Blob
	gen   uint64 // bumped by forget: a lookup that raced a forget does not cache its result
}

type cacheKey struct{ domain, body string }

const maxCached = 4096

// StoreID is the pinned identity of a store: a hash of its kind and ID.
func StoreID(kind string, s Store) string {
	h := sha256.Sum256([]byte(kind + "\x00" + s.ID()))
	return hex.EncodeToString(h[:])
}

// overlap reports whether two store ids share objects: one, as a path, contains the other.
func overlap(a, b string) bool {
	a, b = strings.TrimSuffix(a, "/")+"/", strings.TrimSuffix(b, "/")+"/"
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// marker is the content of MarkerKey.
type marker struct {
	Domain     string `json:"domain"`
	StoreID    string `json:"store_id"`
	Deployment string `json:"deployment"`
}

// NewService checks every domain (pin, marker, not public), prepares the spool directory and
// sweeps what a previous run left in it.
func NewService(ctx context.Context, st *store.Store, domains []Domain, o Options) (*Service, error) {
	if o.MaxBody <= 0 || o.MaxIngests <= 0 {
		return nil, errors.New("blob: max_body and max_ingests must be positive")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	s := &Service{st: st, domains: map[string]Domain{}, sem: make(chan struct{}, o.MaxIngests), maxBody: o.MaxBody,
		log: o.Log, cache: map[cacheKey]store.Blob{}}
	for i, d := range domains {
		if err := store.ValidDomain(d.Name); err != nil {
			return nil, err
		}
		if _, dup := s.domains[d.Name]; dup {
			return nil, fmt.Errorf("%w: domain %s is configured twice", ErrDomain, d.Name)
		}
		for _, e := range domains[:i] {
			if overlap(d.Store.ID(), e.Store.ID()) {
				return nil, fmt.Errorf("%w: domains %s and %s share a store", ErrDomain, e.Name, d.Name)
			}
		}
		s.domains[d.Name] = d
	}
	// 1. the deployment id, and every existing pin must still match; nothing is pinned yet
	deployment, err := s.deployment(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.pin(ctx, false); err != nil {
		return nil, err
	}
	// 2. each store must carry our marker (written if absent) and must not be public
	for _, d := range domains {
		if err := s.checkMarker(ctx, d, deployment); err != nil {
			return nil, err
		}
	}
	if err := s.CheckPublic(ctx); err != nil {
		return nil, err
	}
	// 3. only now pin new domains, so a store kista refused is never pinned
	if err := s.pin(ctx, true); err != nil {
		return nil, err
	}
	root, err := openSpool(o.SpoolDir)
	if err != nil {
		return nil, err
	}
	s.spool = root
	s.sweep()
	return s, nil
}

// deployment returns the deployment id. Replicas starting together on a fresh database race to
// create it; the loser re-reads.
func (s *Service) deployment(ctx context.Context) (string, error) {
	var id string
	var err error
	for range 2 {
		err = s.st.InTx(ctx, "kista/blob/domains", func(tx *store.Tx) error {
			var e error
			id, e = tx.DeploymentID(ctx)
			return e
		})
		if !errors.Is(err, store.ErrExists) {
			break
		}
	}
	return id, err
}

// pin compares every configured domain with its pin; with create, it pins those that have none (a
// replica racing on the same pin re-reads and compares).
func (s *Service) pin(ctx context.Context, create bool) error {
	for _, name := range s.Domains() {
		d := s.domains[name]
		id := StoreID(d.Kind, d.Store)
		var err error
		for range 2 {
			err = s.st.InTx(ctx, "kista/blob/domains", func(tx *store.Tx) error {
				pin, err := tx.GetStorageDomain(ctx, d.Name)
				switch {
				case errors.Is(err, store.ErrNotFound):
					if !create {
						return nil
					}
					return tx.PinStorageDomain(ctx, &store.StorageDomain{Name: d.Name, Kind: d.Kind, StoreID: id})
				case err != nil:
					return err
				case pin.StoreID != id || pin.Kind != d.Kind:
					return fmt.Errorf("%w: domain %s now points at another store than the one it was first used with", ErrDomain, d.Name)
				}
				return nil
			})
			if !errors.Is(err, store.ErrExists) {
				break
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Close releases the spool directory and the domains' stores.
func (s *Service) Close() error {
	for _, d := range s.domains {
		if c, ok := d.Store.(io.Closer); ok {
			_ = c.Close()
		}
	}
	return s.spool.Close()
}

func (s *Service) checkMarker(ctx context.Context, d Domain, deployment string) error {
	want := marker{Domain: d.Name, StoreID: StoreID(d.Kind, d.Store), Deployment: deployment}
	got, err := readMarker(ctx, d)
	if errors.Is(err, ErrNotFound) {
		b, _ := json.Marshal(want)
		if err := d.Store.Put(ctx, MarkerKey, bytes.NewReader(b), int64(len(b))); err != nil {
			return fmt.Errorf("blob: domain %s: writing the marker: %w", d.Name, err)
		}
		// another deployment may have claimed the empty store at the same moment: the last writer
		// wins, and the loser sees the other marker here
		got, err = readMarker(ctx, d)
	}
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: domain %s: the store is marked for another deployment, domain or store", ErrDomain, d.Name)
	}
	return nil
}

func readMarker(ctx context.Context, d Domain) (marker, error) {
	r, err := d.Store.Get(ctx, MarkerKey, 0, -1)
	if errors.Is(err, ErrNotFound) {
		return marker{}, err
	}
	if err != nil {
		return marker{}, fmt.Errorf("blob: domain %s: reading the marker: %w", d.Name, err)
	}
	defer r.Close()
	var got marker
	dec := json.NewDecoder(io.LimitReader(r, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil || dec.More() {
		return marker{}, fmt.Errorf("%w: domain %s: the store's marker is not kista's", ErrDomain, d.Name)
	}
	return got, nil
}

// CheckPublic refuses any domain whose marker can be read anonymously. kista runs it at startup and
// periodically.
func (s *Service) CheckPublic(ctx context.Context) error {
	for _, name := range s.Domains() {
		d := s.domains[name]
		readable, ok := d.Store.Anonymous(ctx, MarkerKey)
		if readable {
			return fmt.Errorf("%w: domain %s can be read without credentials; make it private", ErrDomain, d.Name)
		}
		if !ok {
			s.log.Warn("blob: could not check whether a storage domain is public", "domain", d.Name)
		}
	}
	return nil
}

// Domains lists the configured domain names.
func (s *Service) Domains() []string {
	out := make([]string, 0, len(s.domains))
	for n := range s.domains {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// HasDomain reports whether a domain is configured.
func (s *Service) HasDomain(name string) bool { _, ok := s.domains[name]; return ok }

// MissingDomains returns the tenants whose storage domain is not configured. They are served
// nothing (fail closed); kista logs them at startup.
func (s *Service) MissingDomains(ctx context.Context) ([]string, error) {
	ts, err := s.st.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range ts {
		if !s.HasDomain(t.StorageDomain) {
			out = append(out, t.Name)
		}
	}
	return out, nil
}

func (s *Service) domain(name string) (Domain, error) {
	d, ok := s.domains[name]
	if !ok {
		return Domain{}, fmt.Errorf("%w: storage domain %q is not configured", ErrDomain, name)
	}
	return d, nil
}

// Record returns the record of a body in a domain, cached: records change only by a commit or a
// corruption mark, both of which drop the cached entry here.
func (s *Service) Record(ctx context.Context, domain string, body extfile.BodyHash) (store.Blob, error) {
	k := cacheKey{domain, body.String()}
	s.mu.Lock()
	b, ok := s.cache[k]
	gen := s.gen
	s.mu.Unlock()
	if ok {
		return b, nil
	}
	b, err := s.st.GetBlob(ctx, domain, k.body)
	if err != nil {
		return store.Blob{}, err
	}
	s.mu.Lock()
	if s.gen == gen {
		if len(s.cache) >= maxCached {
			clear(s.cache)
		}
		s.cache[k] = b
	}
	s.mu.Unlock()
	return b, nil
}

func (s *Service) forget(domain, body string) {
	s.mu.Lock()
	delete(s.cache, cacheKey{domain, body})
	s.gen++
	s.mu.Unlock()
}
