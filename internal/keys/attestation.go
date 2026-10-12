package keys

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Attestation keys (spec 0011 phase 1b): a tenant's keys for signing the statements its feed
// serves. They live in the same key sources as channel keys, opened for their own purpose (a key
// source with metadata requires the marker attestation-signing), with registry rows of their own:
// one key is never a channel key and an attestation key. Their lifecycle is a channel key's: a
// server administrator registers one, a tenant administrator activates and retires it, one is
// active per tenant.

// PurposeOpener opens a signer for a role (keysource.Registry); an Opener without it (the file
// resolver, in tests) opens every key alike.
type PurposeOpener interface {
	OpenPurpose(ctx context.Context, ref, purpose string) (signer.Signer, error)
}

func (s *Service) openAttestation(ctx context.Context, ref string) (signer.Signer, error) {
	if po, ok := s.Signers.(PurposeOpener); ok {
		return po.OpenPurpose(ctx, ref, keysource.PurposeAttestation)
	}
	return s.Signers.Open(ctx, ref)
}

func attestationSubject(k store.Key) string { return "attestation-key:" + k.ID }

// tenantAdmin checks a tenant administrator and reads the tenant.
func (s *Service) tenantAdmin(ctx context.Context, a authz.Actor, tenant string) (store.Tenant, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant}); err != nil {
		return store.Tenant{}, err
	}
	return s.Store.GetTenant(ctx, tenant)
}

// AddAttestation registers a tenant's attestation key (server-wide, like a channel key's signer
// reference): checked and probed as a channel key is, inserted as trusted, or active only for a
// tenant that never had one.
func (s *Service) AddAttestation(ctx context.Context, a authz.Actor, tenant, ref string, active bool) (store.Key, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
		return store.Key{}, err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return store.Key{}, err
	}
	sg, err := s.openAttestation(ctx, ref)
	if err != nil {
		return store.Key{}, fmt.Errorf("%w: %w", ErrSigner, err)
	}
	pub := sg.Public()
	if err := extfile.CheckKey(pub); err != nil {
		return store.Key{}, fmt.Errorf("%w: %w", ErrSigner, err)
	}
	if err := Probe(ctx, sg); err != nil {
		return store.Key{}, fmt.Errorf("%w: %w", ErrSigner, err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return store.Key{}, fmt.Errorf("%w: %w", ErrSigner, err)
	}
	k := store.Key{TenantID: t.ID, Fingerprint: extfile.Fingerprint(pub), SignerRef: ref, PublicKey: der, State: store.KeyTrusted}
	if active {
		k.State = store.KeyActive
	}
	err = s.Store.InTx(ctx, store.AttestationLock(t.ID), func(tx *store.Tx) error {
		existing, err := tx.AttestationKeys(ctx, t.ID)
		if err != nil {
			return err
		}
		if active && len(existing) > 0 {
			return fmt.Errorf("%w: only a tenant that never had an attestation key may get its first as active", ErrState)
		}
		live := 1
		for _, e := range existing {
			if e.State != store.KeyRetired {
				live++
			}
		}
		if live > MaxKeys {
			return fmt.Errorf("%w: a tenant holds at most %d non-retired attestation keys", ErrState, MaxKeys)
		}
		if err := tx.InsertAttestationKey(ctx, &k, a.String()); err != nil {
			return err
		}
		scheme, _, _ := strings.Cut(k.SignerRef, ":")
		return tx.Event(ctx, t.ID, a.String(), "attestation_key.add", attestationSubject(k),
			map[string]any{"key": k.ID, "fingerprint": k.Fingerprint, "scheme": scheme, "state": k.State})
	})
	return k, err
}

// changeAttestation moves a tenant's attestation key: activate (a trusted key, trusted for
// MinTrusted unless force; the active one is demoted) or retire (a trusted key, out of the active
// state for MinDemoted unless force).
func (s *Service) changeAttestation(ctx context.Context, a authz.Actor, tenant, keyRef string, expected int64, force, activate bool) (store.Key, error) {
	if force {
		if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Server); err != nil {
			return store.Key{}, err
		}
	}
	t, err := s.tenantAdmin(ctx, a, tenant)
	if err != nil {
		return store.Key{}, err
	}
	var out store.Key
	err = s.Store.InTx(ctx, store.AttestationLock(t.ID), func(tx *store.Tx) error {
		keys, err := tx.AttestationKeys(ctx, t.ID)
		if err != nil {
			return err
		}
		target, err := find(keys, keyRef)
		if err != nil {
			return err
		}
		if expected != 0 && target.Version != expected {
			return store.ErrConflict
		}
		if target.State != store.KeyTrusted {
			return fmt.Errorf("%w: key is %s, only a trusted key can be activated or retired", ErrState, target.State)
		}
		since, least, verb := target.TrustedSince, s.MinTrusted, "activate"
		if !activate {
			since, least, verb = target.StateChangedAt, s.MinDemoted, "retire"
		}
		forced := false
		if age := tx.Now().Sub(since); age < least {
			if !force {
				return fmt.Errorf("%w: %s for %s, at least %s", ErrTooSoon, map[bool]string{true: "trusted", false: "in its state"}[activate],
					age.Round(time.Second), least)
			}
			forced = true
		}
		to := store.KeyRetired
		if activate {
			to = store.KeyActive
			for i := range keys {
				if keys[i].State == store.KeyActive {
					if err := tx.SetAttestationKeyState(ctx, &keys[i], store.KeyTrusted, a.String()); err != nil {
						return err
					}
				}
			}
		}
		if err := tx.SetAttestationKeyState(ctx, target, to, a.String()); err != nil {
			return err
		}
		out = *target
		return tx.Event(ctx, t.ID, a.String(), audit.Kind("attestation_key."+verb), attestationSubject(*target),
			map[string]any{"key": target.ID, "fingerprint": target.Fingerprint, "from": store.KeyTrusted, "forced": forced})
	})
	return out, err
}

// ActivateAttestation makes a trusted attestation key the tenant's active one.
func (s *Service) ActivateAttestation(ctx context.Context, a authz.Actor, tenant, keyRef string, expected int64, force bool) (store.Key, error) {
	return s.changeAttestation(ctx, a, tenant, keyRef, expected, force, true)
}

// RetireAttestation retires a trusted attestation key: it is no longer published.
func (s *Service) RetireAttestation(ctx context.Context, a authz.Actor, tenant, keyRef string, expected int64, force bool) (store.Key, error) {
	return s.changeAttestation(ctx, a, tenant, keyRef, expected, force, false)
}

// ListAttestation lists a tenant's attestation keys (its administrators).
func (s *Service) ListAttestation(ctx context.Context, a authz.Actor, tenant string) ([]store.Key, error) {
	t, err := s.tenantAdmin(ctx, a, tenant)
	if err != nil {
		return nil, err
	}
	return s.Store.AttestationKeys(ctx, t.ID)
}

// PublicAttestation lists a tenant's attestation keys with their states (the active one first, the
// retired last): public by design, like .well-known, so subscribers can pin them. A retired key is
// listed too: the statements it signed are still served, and a subscriber that pinned it verifies
// them; it signs nothing more. An unknown tenant has none.
func (s *Service) PublicAttestation(ctx context.Context, tenant string) ([]store.Key, error) {
	t, err := s.Store.GetTenant(ctx, tenant)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	all, err := s.Store.AttestationKeys(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	rank := map[string]int{store.KeyActive: 0, store.KeyTrusted: 1, store.KeyRetired: 2}
	sort.SliceStable(all, func(i, j int) bool { return rank[all[i].State] < rank[all[j].State] })
	return all, nil
}

// OpenAttestationSigner opens an attestation key's signer and checks it is the registered key.
func (s *Service) OpenAttestationSigner(ctx context.Context, k store.Key) (signer.Signer, error) {
	if k.Fingerprint == "" || len(k.PublicKey) == 0 {
		return nil, ErrMismatch
	}
	sg, err := s.openAttestation(ctx, k.SignerRef)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(sg.Public())
	if err != nil || !bytes.Equal(der, k.PublicKey) || extfile.Fingerprint(sg.Public()) != k.Fingerprint {
		return nil, ErrMismatch
	}
	return sg, nil
}
