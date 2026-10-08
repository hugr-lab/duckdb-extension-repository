package release

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Errors.
var (
	ErrState = errors.New("release: not allowed in this state")
	ErrSlot  = errors.New("release: the slot holds another body; a fix is a new version")
	// ErrBlocked is a body banned in the tenant (spec 0008).
	ErrBlocked = errors.New("release: the body is blocked in this tenant")
)

// SignerOpener opens a key's signer, checked against the stored key (keys.Service.OpenSigner).
type SignerOpener interface {
	OpenSigner(ctx context.Context, k store.Key) (signer.Signer, error)
}

// Service manages builds and releases.
type Service struct {
	Store   *store.Store
	Blob    *blob.Service
	Signers SignerOpener
	Authz   authz.Authorizer
}

func lockKey(channelID string) string { return "kista/channel/" + channelID }

// AddOptions are the choices of an add or a publication.
type AddOptions struct {
	Name       string
	Private    bool
	NotCurrent bool
	// Unchecked skips spec 0008's file checks (prefix, format, entry point): the CLI's operator
	// vouches for the body. Never set over the API.
	Unchecked bool
	// Publish, when set, makes this a publication (spec 0008): the caller needs publish on the name,
	// the declared version and platform must be the footer's, and the checks always run.
	Publish *Publication
	// NoWait fails with blob.ErrBusy instead of waiting for an ingest slot.
	NoWait bool
}

// Publication is what a publisher declares about an upload.
type Publication struct {
	Version, Platform string
	Provenance        string // JSON, recorded on the release
}

func (o AddOptions) visibility() string {
	if o.Private {
		return store.Private
	}
	return store.Public
}

// Add puts a built extension into a signed channel: spool, check, commit, sign, release. It returns
// the release and whether it already existed (the same body in the same slot with the same choices).
func (s *Service) Add(ctx context.Context, a authz.Actor, tenant, channel string, r io.Reader, o AddOptions) (store.Release, bool, error) {
	res := authz.Resource{Tenant: tenant, Channel: channel}
	verb := authz.VerbAdmin
	if o.Publish != nil {
		o.Unchecked = false
		verb = authz.VerbPublish
		res = authz.Resource{Tenant: tenant, Channel: channel, Extension: o.Name, Reserved: reserved.Kind(o.Name) != ""}
	}
	if err := s.Authz.Allow(ctx, a, verb, res); err != nil {
		return store.Release{}, false, err
	}
	sc, err := s.Store.GetServeChannel(ctx, tenant, channel)
	if err != nil {
		return store.Release{}, false, err
	}
	ch := sc.Channel
	if ch.Kind != store.ChannelSigned {
		return store.Release{}, false, fmt.Errorf("%w: a %s channel is fed by upstreams (spec 0009)", ErrState, ch.Kind)
	}
	if _, err := activeKey(ctx, s.Store, ch.ID); err != nil {
		return store.Release{}, false, err
	}
	spool := s.Blob.Spool
	if o.NoWait {
		spool = s.Blob.SpoolNoWait
	}
	sp, err := spool(ctx, sc.Tenant.StorageDomain, r)
	if err != nil {
		return store.Release{}, false, err
	}
	defer sp.Close()
	b, err := BuildFrom(sp.File(), sc.Tenant.ID, o.Name, a.String())
	if err != nil {
		return store.Release{}, false, err
	}
	it := item{b: b, visibility: o.visibility(), notCurrent: o.NotCurrent, origin: store.OriginAdmin}
	if p := o.Publish; p != nil {
		if p.Version != b.ExtVersion || p.Platform != b.Platform {
			return store.Release{}, false, fmt.Errorf("%w: the file's footer says version %q, platform %q; the request says %q, %q",
				store.ErrInvalid, b.ExtVersion, b.Platform, p.Version, p.Platform)
		}
		it.origin, it.provenance = store.OriginPublication, p.Provenance
		it.b.Origin = store.OriginPublication
	}
	if !o.Unchecked {
		if err := CheckFile(sp.File(), o.Name); err != nil {
			return store.Release{}, false, err
		}
		it.b.Checked = true
	}
	if blocked, err := s.Store.Blocked(ctx, sc.Tenant.ID, b.BodyHash); err != nil || blocked {
		if err == nil {
			err = ErrBlocked
		}
		return store.Release{}, false, err
	}
	// check the slot before storing a body that would be refused
	var existing *store.Release
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		cands, err := tx.SlotCandidates(ctx, ch.ID, b.Name, b.ExtVersion, b.Platform)
		if err != nil {
			return err
		}
		existing, err = checkSlot(cands, it)
		return err
	}); err != nil {
		return store.Release{}, false, err
	}
	if existing != nil {
		if it.b.Checked { // publishing the same body with the checks marks its Build checked
			bb := it.b
			if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &bb) }); err != nil {
				return store.Release{}, false, err
			}
		}
		return *existing, true, nil
	}
	// the channel must serve the build before its body is stored (re-checked under the lock)
	capis, err := s.Store.ChannelCAPIs(ctx, ch.ID)
	if err != nil {
		return store.Release{}, false, err
	}
	if err := servable(capis, b); err != nil {
		return store.Release{}, false, err
	}
	if _, err := sp.Commit(ctx); err != nil {
		return store.Release{}, false, err
	}
	// two adds of one body race on the build's insert; the loser finds the winner's build
	for attempt := 0; ; attempt++ {
		bb := it.b
		err := s.Store.InTx(ctx, "", func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &bb) })
		if err == nil {
			it.b = bb
			break
		}
		if !errors.Is(err, store.ErrExists) || attempt > 0 {
			return store.Release{}, false, err
		}
	}
	rs, existed, err := s.insert(ctx, a, ch.ID, []item{it})
	if err != nil {
		return store.Release{}, false, err
	}
	return rs[0], existed, nil
}

// item is one release to insert: a Build and the release's choices.
type item struct {
	b                  store.Build
	visibility         string
	notCurrent         bool
	origin, provenance string
}

// checkSlot finds the release in its slot. The same body with the same choices is the existing
// release; another body, other choices, or mixing c_struct with cpp builds are refused.
func checkSlot(cands []store.Candidate, it item) (*store.Release, error) {
	b := it.b
	for _, c := range cands {
		if (c.ABI == store.ABICStruct) != (b.ABI == store.ABICStruct) {
			return nil, fmt.Errorf("%w: %s %s on %s already has %s releases; c_struct and exact-version builds do not mix",
				ErrSlot, b.Name, b.ExtVersion, b.Platform, c.ABI)
		}
		if c.Slot != b.Slot() {
			continue
		}
		if c.BodyHash != b.BodyHash {
			return nil, fmt.Errorf("%w (%s %s %s %s)", ErrSlot, b.Name, b.ExtVersion, b.Platform, c.Slot)
		}
		if c.State == store.ReleaseYanked {
			return nil, fmt.Errorf("%w: %s %s %s %s holds this build and is yanked; a fix is a new version",
				ErrSlot, b.Name, b.ExtVersion, b.Platform, c.Slot)
		}
		if c.Visibility != it.visibility || (c.Seq == 0) != it.notCurrent {
			return nil, fmt.Errorf("%w: %s %s %s %s holds this build as %s/%s; change it with release public|private or release current",
				ErrState, b.Name, b.ExtVersion, b.Platform, c.Slot, c.State, c.Visibility)
		}
		r := c.Release
		return &r, nil
	}
	return nil, nil
}

func activeKey(ctx context.Context, st *store.Store, channelID string) (store.Key, error) {
	keys, err := st.ListKeys(ctx, channelID)
	if err != nil {
		return store.Key{}, err
	}
	for _, k := range keys {
		if k.State == store.KeyActive {
			return k, nil
		}
	}
	return store.Key{}, fmt.Errorf("%w: the channel has no active key", ErrState)
}

// sign signs a body hash with a key and verifies the result against the key's stored public key.
func (s *Service) sign(ctx context.Context, k store.Key, h extfile.BodyHash) ([]byte, error) {
	sg, err := s.Signers.OpenSigner(ctx, k)
	if err != nil {
		return nil, err
	}
	sig, err := sg.Sign(ctx, h)
	if err != nil {
		return nil, err
	}
	pub, err := x509.ParsePKIXPublicKey(k.PublicKey)
	rsaPub, ok := pub.(*rsa.PublicKey)
	if err != nil || !ok {
		return nil, errors.New("release: the stored key is not an RSA key")
	}
	if _, ok := extfile.Verify(h, sig, []*rsa.PublicKey{rsaPub}); !ok {
		return nil, errors.New("release: the signer's signature does not verify against the stored key")
	}
	return sig, nil
}

func parseHash(hexHash string) (extfile.BodyHash, error) {
	var h extfile.BodyHash
	if len(hexHash) != 64 {
		return h, errors.New("release: bad body hash")
	}
	for i := range h {
		var v byte
		for _, c := range hexHash[2*i : 2*i+2] {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= byte(c - '0')
			case c >= 'a' && c <= 'f':
				v |= byte(c-'a') + 10
			default:
				return h, errors.New("release: bad body hash")
			}
		}
		h[i] = v
	}
	return h, nil
}

// required are the keys a new release must be signed by: the active key and the serving key (the
// active key when the channel has none yet).
func required(keys []store.Key, serving string) ([]store.Key, error) {
	var out []store.Key
	for _, k := range keys {
		if k.State == store.KeyActive || k.ID == serving {
			out = append(out, k)
		}
	}
	if !slices.ContainsFunc(out, func(k store.Key) bool { return k.State == store.KeyActive }) {
		return nil, fmt.Errorf("%w: the channel has no active key", ErrState)
	}
	return out, nil
}

// insert signs outside any transaction and inserts the releases under the channel lock, all or
// none, retrying while the keys it needs change underneath. It returns the releases (existing ones
// for items whose slot already holds them with the same choices) and whether all of them existed.
func (s *Service) insert(ctx context.Context, a authz.Actor, channelID string, items []item) ([]store.Release, bool, error) {
	hashes := make([]extfile.BodyHash, len(items))
	for i, it := range items {
		h, err := parseHash(it.b.BodyHash)
		if err != nil {
			return nil, false, err
		}
		hashes[i] = h
	}
	sigs := make([]map[string][]byte, len(items))
	for i := range sigs {
		sigs[i] = map[string][]byte{}
	}
	keys, err := s.Store.ListKeys(ctx, channelID)
	if err != nil {
		return nil, false, err
	}
	ch, err := s.channelByID(ctx, channelID)
	if err != nil {
		return nil, false, err
	}
	need, err := required(keys, ch.ServingKeyID)
	if err != nil {
		return nil, false, err
	}
	for attempt := 0; attempt < 4; attempt++ {
		for i := range items {
			for _, k := range need {
				if sigs[i][k.ID] != nil {
					continue
				}
				sig, err := s.sign(ctx, k, hashes[i])
				if err != nil {
					if k.State != store.KeyActive {
						return nil, false, fmt.Errorf("signing with the serving key %s (the old one until a re-sign moves it: kista admin key resign): %w", k.Fingerprint, err)
					}
					return nil, false, err
				}
				sigs[i][k.ID] = sig
			}
		}
		var out []store.Release
		existing := 0
		need = nil
		err := s.Store.InTx(ctx, lockKey(channelID), func(tx *store.Tx) error {
			out, existing, need = nil, 0, nil
			c, err := tx.ChannelByID(ctx, channelID)
			if err != nil {
				return err
			}
			keys, err := tx.ChannelKeys(ctx, channelID)
			if err != nil {
				return err
			}
			req, err := required(keys, c.ServingKeyID)
			if err != nil {
				return err
			}
			for _, k := range req {
				for i := range items {
					if sigs[i][k.ID] == nil {
						need = append(need, k)
						break
					}
				}
			}
			if len(need) > 0 {
				return errResign
			}
			live := map[string]bool{}
			for _, k := range keys {
				live[k.ID] = k.State != store.KeyRetired
			}
			capis, err := tx.ChannelCAPIs(ctx, channelID)
			if err != nil {
				return err
			}
			for i, it := range items {
				b := it.b
				// spec 0008: a body blocked after the caller's first check is never released
				if blocked, err := tx.Blocked(ctx, b.TenantID, b.BodyHash); err != nil || blocked {
					if err == nil {
						err = ErrBlocked
					}
					return err
				}
				cands, err := tx.SlotCandidates(ctx, channelID, b.Name, b.ExtVersion, b.Platform)
				if err != nil {
					return err
				}
				ex, err := checkSlot(cands, it)
				if err != nil {
					return err
				}
				if ex != nil {
					out = append(out, *ex)
					existing++
					continue
				}
				if err := servable(capis, b); err != nil {
					return err
				}
				r := store.Release{TenantID: b.TenantID, ChannelID: channelID, BuildID: b.ID, Name: b.Name,
					ExtVersion: b.ExtVersion, Platform: b.Platform, Slot: b.Slot(), State: store.ReleaseActive,
					Visibility: it.visibility, Origin: it.origin, Provenance: it.provenance}
				if !it.notCurrent {
					if r.Seq, err = tx.NextSeq(ctx, channelID); err != nil {
						return err
					}
				}
				if err := tx.InsertRelease(ctx, &r, a.String()); err != nil {
					return err
				}
				for id, sig := range sigs[i] {
					if live[id] {
						if err := tx.InsertSignature(ctx, r.ID, channelID, id, sig); err != nil {
							return err
						}
					}
				}
				out = append(out, r)
			}
			if existing == len(items) {
				return nil
			}
			if c.ServingKeyID == "" {
				for _, k := range req {
					if k.State == store.KeyActive {
						if err := tx.SetServingKey(ctx, channelID, "", k.ID); err != nil {
							return err
						}
					}
				}
			}
			return tx.BumpReleaseVersion(ctx, channelID)
		})
		if errors.Is(err, errResign) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		return out, existing == len(items), nil
	}
	return nil, false, errors.New("release: the channel's keys kept changing; try again")
}

// servable checks that a channel serves a build: its exact DuckDB version (cpp, c_struct_unstable),
// or a DuckDB version whose C API maximum accepts it (c_struct).
func servable(capis map[string][]store.CAPI, b store.Build) error {
	if b.ABI == store.ABICStruct {
		if b.CAPI == nil || !acceptsAny(capis, *b.CAPI) {
			return fmt.Errorf("%w: no DuckDB version the channel serves accepts C API %s (%s %s %s)",
				store.ErrInvalid, b.CAPI, b.Name, b.ExtVersion, b.Platform)
		}
		return nil
	}
	if _, ok := capis[b.DuckDBVersion]; !ok {
		return fmt.Errorf("%w: the channel does not serve DuckDB %s (%s %s %s); add the version to it first",
			store.ErrInvalid, b.DuckDBVersion, b.Name, b.ExtVersion, b.Platform)
	}
	return nil
}

// acceptsAny reports whether a DuckDB version of the channel accepts a c_struct build's C API.
func acceptsAny(capis map[string][]store.CAPI, c store.CAPI) bool {
	for _, maxima := range capis {
		for _, m := range maxima {
			if m.Accepts(c) {
				return true
			}
		}
	}
	return false
}

var errResign = errors.New("release: keys changed")

func (s *Service) channelByID(ctx context.Context, id string) (store.Channel, error) {
	var c store.Channel
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		c, err = tx.ChannelByID(ctx, id)
		return err
	})
	return c, err
}

// List lists a channel's releases (optionally of one name: an administrator of that extension may,
// and its publishers, spec 0008), newest first.
func (s *Service) List(ctx context.Context, a authz.Actor, tenant, channel, name string) ([]store.Candidate, error) {
	if err := s.mayRead(ctx, a, tenant, channel, name); err != nil {
		return nil, err
	}
	ch, err := s.Store.GetChannel(ctx, tenant, channel)
	if err != nil {
		return nil, err
	}
	return s.Store.ListReleases(ctx, ch.ID, name)
}

// mayRead allows an extension's administrators and, for one name, its publishers.
func (s *Service) mayRead(ctx context.Context, a authz.Actor, tenant, channel, name string) error {
	res := authz.Resource{Tenant: tenant, Channel: channel, Extension: name}
	err := s.Authz.Allow(ctx, a, authz.VerbAdmin, res)
	if name == "" || !errors.Is(err, authz.ErrDenied) {
		return err
	}
	res.Reserved = reserved.Kind(name) != ""
	for _, v := range []authz.Verb{authz.VerbPublish, authz.VerbPromote} {
		if err = s.Authz.Allow(ctx, a, v, res); !errors.Is(err, authz.ErrDenied) {
			return err
		}
	}
	return err
}

// Change is a change to one release.
type Change string

// Changes.
const (
	Yank        Change = "yank"
	Deprecate   Change = "deprecate"
	Activate    Change = "activate"
	MakeCurrent Change = "current"
	SetPublic   Change = "public"
	SetPrivate  Change = "private"
)

// Apply changes one release of a channel. With a name, the release must be of that extension (an
// administrator of the extension may change it); a non-zero expected version must be the release's.
func (s *Service) Apply(ctx context.Context, a authz.Actor, tenant, channel, name, id string, c Change, expected int64) (store.Release, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, Channel: channel, Extension: name}); err != nil {
		return store.Release{}, err
	}
	ch, err := s.Store.GetChannel(ctx, tenant, channel)
	if err != nil {
		return store.Release{}, err
	}
	var out store.Release
	err = s.Store.InTx(ctx, lockKey(ch.ID), func(tx *store.Tx) error {
		r, err := tx.GetRelease(ctx, ch.ID, id)
		if err != nil {
			return err
		}
		if name != "" && r.Name != name {
			return fmt.Errorf("%w: release %s", store.ErrNotFound, id) // another extension's id
		}
		if expected != 0 && r.Version != expected {
			return store.ErrConflict
		}
		refuse := func() error { return fmt.Errorf("%w: %q is not allowed on a %s release", ErrState, c, r.State) }
		actor := a.String()
		switch c {
		case Yank:
			if r.State == store.ReleaseYanked {
				return refuse()
			}
			err = tx.SetReleaseState(ctx, &r, store.ReleaseYanked, actor)
		case Deprecate:
			if r.State != store.ReleaseActive {
				return refuse()
			}
			err = tx.SetReleaseState(ctx, &r, store.ReleaseDeprecated, actor)
		case Activate:
			if r.State != store.ReleaseDeprecated {
				return refuse()
			}
			err = tx.SetReleaseState(ctx, &r, store.ReleaseActive, actor)
		case MakeCurrent:
			if r.State != store.ReleaseActive {
				return refuse()
			}
			var seq int64
			if seq, err = tx.NextSeq(ctx, ch.ID); err == nil {
				err = tx.SetReleaseSeq(ctx, &r, seq, actor)
			}
		case SetPublic, SetPrivate:
			if r.State == store.ReleaseYanked {
				return refuse()
			}
			err = tx.SetReleaseVisibility(ctx, &r, string(c), actor)
		default:
			return fmt.Errorf("%w: unknown change %q", store.ErrInvalid, c)
		}
		if err != nil {
			return err
		}
		out = r
		return tx.BumpReleaseVersion(ctx, ch.ID)
	})
	return out, err
}
