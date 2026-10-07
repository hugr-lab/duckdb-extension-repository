// Package keys is the channel-key lifecycle (spec 0003): add, activate, retire, the .well-known
// document, and opening a key's signer checked against its stored fingerprint.
package keys

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Limits.
const (
	// MaxKeys is the most non-retired keys a channel may hold.
	MaxKeys = 16
	// MaxWellKnown is DuckDB's limit on a .well-known document.
	MaxWellKnown = 64 << 10
)

// Errors.
var (
	ErrState    = errors.New("keys: not allowed in the key's state")
	ErrTooSoon  = errors.New("keys: too soon; wait or use --force")
	ErrNoKeys   = errors.New("keys: the channel has no trusted keys")
	ErrMismatch = errors.New("keys: the signer's key is not the registered key")
)

// probe is the body hash signed when a key is added, to prove the key can sign.
var probe = extfile.BodyHash(sha256.Sum256([]byte("kista key probe")))

// Opener opens a signer from a reference.
type Opener interface {
	Open(ctx context.Context, ref string) (signer.Signer, error)
}

// Service manages channel keys.
type Service struct {
	Store      *store.Store
	Signers    Opener
	Authz      authz.Authorizer
	MinTrusted time.Duration
	MinDemoted time.Duration
}

func lockKey(channelID string) string { return "kista/channel/" + channelID }

// channel resolves a channel outside a transaction, to know which lock to take.
func (s *Service) channel(ctx context.Context, a authz.Actor, verb authz.Verb, tenant, channel string) (store.Channel, error) {
	if err := s.Authz.Allow(ctx, a, verb, tenant, channel); err != nil {
		return store.Channel{}, err
	}
	return s.Store.GetChannel(ctx, tenant, channel)
}

// Add registers a key: it opens the signer, checks the key (RSA-2048, e = 65537), proves it can
// sign with a probe signature, and inserts it as trusted (or active, only on a channel that never
// had a key).
func (s *Service) Add(ctx context.Context, a authz.Actor, tenant, channel, ref string, active bool) (store.Key, error) {
	ch, err := s.channel(ctx, a, authz.VerbAdmin, tenant, channel)
	if err != nil {
		return store.Key{}, err
	}
	if ch.Kind != store.ChannelSigned {
		return store.Key{}, fmt.Errorf("%w: a %s channel holds no keys", ErrState, ch.Kind)
	}
	sg, err := s.Signers.Open(ctx, ref)
	if err != nil {
		return store.Key{}, err
	}
	pub := sg.Public()
	if err := extfile.CheckKey(pub); err != nil {
		return store.Key{}, err
	}
	sig, err := sg.Sign(ctx, probe)
	if err != nil {
		return store.Key{}, fmt.Errorf("keys: the probe signature failed: %w", err)
	}
	if _, ok := extfile.Verify(probe, sig, []*rsa.PublicKey{pub}); !ok {
		return store.Key{}, errors.New("keys: the probe signature does not verify")
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return store.Key{}, err
	}
	k := store.Key{TenantID: ch.TenantID, ChannelID: ch.ID, Fingerprint: extfile.Fingerprint(pub),
		SignerRef: ref, PublicKey: der, State: store.KeyTrusted}
	if active {
		k.State = store.KeyActive
	}
	err = s.Store.InTx(ctx, lockKey(ch.ID), func(tx *store.Tx) error {
		c, err := tx.GetChannel(ctx, tenant, channel)
		if err != nil {
			return err
		}
		existing, err := tx.ChannelKeys(ctx, c.ID)
		if err != nil {
			return err
		}
		if active && len(existing) > 0 {
			return fmt.Errorf("%w: only a channel that never had a key may get its first key as active", ErrState)
		}
		live := []store.Key{k}
		for _, e := range existing {
			if e.State != store.KeyRetired {
				live = append(live, e)
			}
		}
		if len(live) > MaxKeys {
			return fmt.Errorf("%w: a channel holds at most %d non-retired keys", ErrState, MaxKeys)
		}
		if _, err := wellKnown(live); err != nil {
			return err
		}
		if err := tx.InsertKey(ctx, &k, a.String(), false); err != nil {
			return err
		}
		return tx.BumpChannel(ctx, &c)
	})
	return k, err
}

// find returns the key of the channel named by id or fingerprint.
func find(keys []store.Key, ref string) (*store.Key, error) {
	for i := range keys {
		if keys[i].ID == ref || keys[i].Fingerprint == ref {
			return &keys[i], nil
		}
	}
	return nil, fmt.Errorf("%w: key %s in this channel", store.ErrNotFound, ref)
}

// Activate makes a trusted key the channel's active key and demotes the current one. The key must
// have been trusted for MinTrusted (from trusted_since, which a demotion never resets), unless force.
func (s *Service) Activate(ctx context.Context, a authz.Actor, tenant, channel, keyRef string, force bool) (store.Key, error) {
	ch, err := s.channel(ctx, a, authz.VerbAdmin, tenant, channel)
	if err != nil {
		return store.Key{}, err
	}
	var out store.Key
	err = s.Store.InTx(ctx, lockKey(ch.ID), func(tx *store.Tx) error {
		c, err := tx.GetChannel(ctx, tenant, channel)
		if err != nil {
			return err
		}
		keys, err := tx.ChannelKeys(ctx, c.ID)
		if err != nil {
			return err
		}
		target, err := find(keys, keyRef)
		if err != nil {
			return err
		}
		if target.State != store.KeyTrusted {
			return fmt.Errorf("%w: key is %s, only a trusted key can be activated", ErrState, target.State)
		}
		forced := false
		if age := tx.Now().Sub(target.TrustedSince); age < s.MinTrusted {
			if !force {
				return fmt.Errorf("%w: trusted for %s, at least %s", ErrTooSoon, age.Round(time.Second), s.MinTrusted)
			}
			forced = true
		}
		// demote first: PostgreSQL and SQLite check the partial unique index row by row
		for i := range keys {
			if keys[i].State == store.KeyActive {
				if err := tx.SetKeyState(ctx, &keys[i], store.KeyTrusted, a.String(), false); err != nil {
					return err
				}
			}
		}
		if err := tx.SetKeyState(ctx, target, store.KeyActive, a.String(), forced); err != nil {
			return err
		}
		out = *target
		return tx.BumpChannel(ctx, &c)
	})
	return out, err
}

// Retire removes a trusted key from .well-known for good. The active key cannot be retired, and a
// key must have been out of the active state for MinDemoted, unless force.
func (s *Service) Retire(ctx context.Context, a authz.Actor, tenant, channel, keyRef string, force bool) (store.Key, error) {
	ch, err := s.channel(ctx, a, authz.VerbAdmin, tenant, channel)
	if err != nil {
		return store.Key{}, err
	}
	var out store.Key
	err = s.Store.InTx(ctx, lockKey(ch.ID), func(tx *store.Tx) error {
		c, err := tx.GetChannel(ctx, tenant, channel)
		if err != nil {
			return err
		}
		keys, err := tx.ChannelKeys(ctx, c.ID)
		if err != nil {
			return err
		}
		target, err := find(keys, keyRef)
		if err != nil {
			return err
		}
		if target.State != store.KeyTrusted {
			return fmt.Errorf("%w: key is %s, only a trusted key can be retired", ErrState, target.State)
		}
		forced := false
		if age := tx.Now().Sub(target.StateChangedAt); age < s.MinDemoted {
			if !force {
				return fmt.Errorf("%w: in its state for %s, at least %s", ErrTooSoon, age.Round(time.Second), s.MinDemoted)
			}
			forced = true
		}
		// From spec 0007 on: refuse while a release of the channel is signed only by this key.
		if err := tx.SetKeyState(ctx, target, store.KeyRetired, a.String(), forced); err != nil {
			return err
		}
		out = *target
		return tx.BumpChannel(ctx, &c)
	})
	return out, err
}

// List returns a channel's keys.
func (s *Service) List(ctx context.Context, a authz.Actor, tenant, channel string) ([]store.Key, error) {
	ch, err := s.channel(ctx, a, authz.VerbRead, tenant, channel)
	if err != nil {
		return nil, err
	}
	return s.Store.ListKeys(ctx, ch.ID)
}

// Events returns the transitions of a channel's keys.
func (s *Service) Events(ctx context.Context, a authz.Actor, tenant, channel string) ([]store.KeyEvent, error) {
	ch, err := s.channel(ctx, a, authz.VerbRead, tenant, channel)
	if err != nil {
		return nil, err
	}
	return s.Store.KeyEvents(ctx, ch.ID)
}

// WellKnown returns the channel's .well-known/duckdb-extension-repo.json and the channel version it
// reflects (for caching).
func (s *Service) WellKnown(ctx context.Context, a authz.Actor, tenant, channel string) ([]byte, int64, error) {
	ch, err := s.channel(ctx, a, authz.VerbRead, tenant, channel)
	if err != nil {
		return nil, 0, err
	}
	if ch.Kind != store.ChannelSigned {
		return nil, 0, fmt.Errorf("%w: a %s channel has no .well-known", ErrNoKeys, ch.Kind)
	}
	keys, err := s.Store.ListKeys(ctx, ch.ID)
	if err != nil {
		return nil, 0, err
	}
	doc, err := wellKnown(keys)
	return doc, ch.Version, err
}

// wellKnown builds the document from a channel's keys: the active key first, then trusted keys by
// trusted_since and fingerprint; retired keys are left out.
func wellKnown(keys []store.Key) ([]byte, error) {
	var live []store.Key
	for _, k := range keys {
		if k.State == store.KeyActive || k.State == store.KeyTrusted {
			live = append(live, k)
		}
	}
	if len(live) == 0 {
		return nil, ErrNoKeys
	}
	sort.SliceStable(live, func(i, j int) bool {
		ai, aj := live[i].State == store.KeyActive, live[j].State == store.KeyActive
		if ai != aj {
			return ai
		}
		if !live[i].TrustedSince.Equal(live[j].TrustedSince) {
			return live[i].TrustedSince.Before(live[j].TrustedSince)
		}
		return live[i].Fingerprint < live[j].Fingerprint
	})
	doc := struct {
		SignatureKeys []string `json:"signature_keys"`
	}{}
	for _, k := range live {
		doc.SignatureKeys = append(doc.SignatureKeys, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: k.PublicKey})))
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if len(out) > MaxWellKnown {
		return nil, fmt.Errorf("%w: .well-known would be %d bytes, DuckDB reads at most %d", ErrState, len(out), MaxWellKnown)
	}
	return out, nil
}

// OpenSigner opens a key's signer and checks that it is the registered key: its public key must be
// the stored one (the key published in .well-known) and match the stored fingerprint. A changed file
// or an edited reference fails closed.
func (s *Service) OpenSigner(ctx context.Context, k store.Key) (signer.Signer, error) {
	if k.Fingerprint == "" || len(k.PublicKey) == 0 {
		return nil, ErrMismatch
	}
	sg, err := s.Signers.Open(ctx, k.SignerRef)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(sg.Public())
	if err != nil || !bytes.Equal(der, k.PublicKey) || extfile.Fingerprint(sg.Public()) != k.Fingerprint {
		return nil, ErrMismatch
	}
	return sg, nil
}
