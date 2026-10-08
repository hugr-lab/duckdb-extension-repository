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
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Errors.
var (
	ErrState = errors.New("release: not allowed in this state")
	ErrSlot  = errors.New("release: the slot holds another body; a fix is a new version")
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

// AddOptions are the choices of an add.
type AddOptions struct {
	Name       string
	Private    bool
	NotCurrent bool
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
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, tenant, channel); err != nil {
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
	sp, err := s.Blob.Spool(ctx, sc.Tenant.StorageDomain, r)
	if err != nil {
		return store.Release{}, false, err
	}
	defer sp.Close()
	b, err := BuildFrom(sp.File(), sc.Tenant.ID, o.Name, a.String())
	if err != nil {
		return store.Release{}, false, err
	}
	if b.ABI != store.ABICStruct {
		versions, err := s.Store.ChannelVersions(ctx, ch.ID)
		if err != nil {
			return store.Release{}, false, err
		}
		if !slices.Contains(versions, b.DuckDBVersion) {
			return store.Release{}, false, fmt.Errorf("%w: the channel does not serve DuckDB %s; add the version to it first", store.ErrInvalid, b.DuckDBVersion)
		}
	}
	// check the slot before storing a body that would be refused
	var existing *store.Release
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		cands, err := tx.SlotCandidates(ctx, ch.ID, b.Name, b.ExtVersion, b.Platform)
		if err != nil {
			return err
		}
		existing, err = checkSlot(cands, b, o)
		return err
	}); err != nil {
		return store.Release{}, false, err
	}
	if existing != nil {
		return *existing, true, nil
	}
	if _, err := sp.Commit(ctx); err != nil {
		return store.Release{}, false, err
	}
	// two adds of one body race on the build's insert; the loser finds the winner's build
	for attempt := 0; ; attempt++ {
		bb := b
		err := s.Store.InTx(ctx, "", func(tx *store.Tx) error { return tx.FindOrInsertBuild(ctx, &bb) })
		if err == nil {
			b = bb
			break
		}
		if !errors.Is(err, store.ErrExists) || attempt > 0 {
			return store.Release{}, false, err
		}
	}
	return s.insert(ctx, a, ch.ID, b, o)
}

// checkSlot finds the release in b's slot. The same body with the same choices is the existing
// release; another body, other choices, or mixing c_struct with cpp builds are refused.
func checkSlot(cands []store.Candidate, b store.Build, o AddOptions) (*store.Release, error) {
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
		if c.Visibility != o.visibility() || (c.Seq == 0) != o.NotCurrent || c.State == store.ReleaseYanked {
			if c.State == store.ReleaseYanked {
				return nil, fmt.Errorf("%w: release %s holds this build and is yanked; a fix is a new version", ErrSlot, c.ID)
			}
			return nil, fmt.Errorf("%w: release %s holds this build as %s/%s; change it with release public|private or release current",
				ErrState, c.ID, c.State, c.Visibility)
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

// insert signs outside any transaction and inserts the release under the channel lock, retrying
// while the keys it needs change underneath.
func (s *Service) insert(ctx context.Context, a authz.Actor, channelID string, b store.Build, o AddOptions) (store.Release, bool, error) {
	h, err := parseHash(b.BodyHash)
	if err != nil {
		return store.Release{}, false, err
	}
	sigs := map[string][]byte{}
	keys, err := s.Store.ListKeys(ctx, channelID)
	if err != nil {
		return store.Release{}, false, err
	}
	ch, err := s.channelByID(ctx, channelID)
	if err != nil {
		return store.Release{}, false, err
	}
	need, err := required(keys, ch.ServingKeyID)
	if err != nil {
		return store.Release{}, false, err
	}
	for attempt := 0; attempt < 4; attempt++ {
		for _, k := range need {
			if sigs[k.ID] != nil {
				continue
			}
			sig, err := s.sign(ctx, k, h)
			if err != nil {
				if k.State != store.KeyActive {
					return store.Release{}, false, fmt.Errorf("signing with the serving key %s (the old one until a re-sign moves it: kista admin key resign): %w", k.Fingerprint, err)
				}
				return store.Release{}, false, err
			}
			sigs[k.ID] = sig
		}
		var out store.Release
		var existing bool
		need = nil
		err := s.Store.InTx(ctx, lockKey(channelID), func(tx *store.Tx) error {
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
				if sigs[k.ID] == nil {
					need = append(need, k)
				}
			}
			if len(need) > 0 {
				return errResign
			}
			cands, err := tx.SlotCandidates(ctx, channelID, b.Name, b.ExtVersion, b.Platform)
			if err != nil {
				return err
			}
			if b.ABI != store.ABICStruct {
				ok, err := tx.ChannelServes(ctx, channelID, b.DuckDBVersion)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("%w: the channel does not serve DuckDB %s", store.ErrInvalid, b.DuckDBVersion)
				}
			}
			if ex, err := checkSlot(cands, b, o); err != nil || ex != nil {
				if ex != nil {
					out, existing = *ex, true
				}
				return err
			}
			r := store.Release{TenantID: b.TenantID, ChannelID: channelID, BuildID: b.ID, Name: b.Name,
				ExtVersion: b.ExtVersion, Platform: b.Platform, Slot: b.Slot(), State: store.ReleaseActive,
				Visibility: o.visibility()}
			if !o.NotCurrent {
				if r.Seq, err = tx.NextSeq(ctx, channelID); err != nil {
					return err
				}
			}
			if err := tx.InsertRelease(ctx, &r, a.String()); err != nil {
				return err
			}
			live := map[string]bool{}
			for _, k := range keys {
				live[k.ID] = k.State != store.KeyRetired
			}
			for id, sig := range sigs {
				if live[id] {
					if err := tx.InsertSignature(ctx, r.ID, channelID, id, sig); err != nil {
						return err
					}
				}
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
			out = r
			return tx.BumpReleaseVersion(ctx, channelID)
		})
		if errors.Is(err, errResign) {
			continue
		}
		return out, existing, err
	}
	return store.Release{}, false, errors.New("release: the channel's keys kept changing; try again")
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

// List lists a channel's releases (optionally of one name), newest first.
func (s *Service) List(ctx context.Context, a authz.Actor, tenant, channel, name string) ([]store.Candidate, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbRead, tenant, channel); err != nil {
		return nil, err
	}
	ch, err := s.Store.GetChannel(ctx, tenant, channel)
	if err != nil {
		return nil, err
	}
	return s.Store.ListReleases(ctx, ch.ID, name)
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

// Apply changes one release of a channel.
func (s *Service) Apply(ctx context.Context, a authz.Actor, tenant, channel, id string, c Change) (store.Release, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, tenant, channel); err != nil {
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
