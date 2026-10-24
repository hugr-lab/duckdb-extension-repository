package release

import (
	"context"
	"errors"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// ErrLicenseKeys is a marked publisher's publication whose body has a malformed key block (spec
// 0021): refused, so a source of licence keys never publishes a build whose keys kista cannot read.
var ErrLicenseKeys = errors.New("release: the file's licence key block is malformed (license_keys)")

func licenseKeys(f *extfile.File) []store.LicenseKey {
	out := make([]store.LicenseKey, 0, len(f.LicenseKeys.Keys))
	for _, k := range f.LicenseKeys.Keys {
		out = append(out, store.LicenseKey{Kid: k.Kid, PEM: k.PEM})
	}
	return out
}

// publisherKeys makes a publication record the build's licence keys for each marked publisher that
// matched the caller and holds publish on the name itself (spec 0021).
func (s *Service) publisherKeys(ctx context.Context, a authz.Actor, res authz.Resource, sources []string, f *extfile.File, it *item) error {
	var trusted []string
	for _, id := range sources {
		pa := authz.Actor{Kind: authz.ActorPublisher, ID: a.ID, Tenant: a.Tenant, Principals: auth.Principals{auth.PublisherKey(id): true}}
		if s.Authz.Allow(ctx, pa, authz.VerbPublish, res) == nil {
			trusted = append(trusted, id)
		}
	}
	if len(trusted) == 0 {
		return nil
	}
	if f.LicenseKeys.Err != nil {
		return ErrLicenseKeys
	}
	if len(f.LicenseKeys.Keys) > 0 {
		it.keys, it.keyKind, it.keySources = licenseKeys(f), store.KeySourcePublisher, trusted
	}
	return nil
}

// distributorKeys is publisherKeys for a distributor's publisher's publication into the catalogue
// (spec 0033 phase B): it publishes the name without a grant (distributorMay checked it), so each
// marked publisher that matched the caller records the build's keys.
func (s *Service) distributorKeys(_ context.Context, _ authz.Actor, _ authz.Resource, sources []string, f *extfile.File, it *item) error {
	if len(sources) == 0 {
		return nil
	}
	if f.LicenseKeys.Err != nil {
		return ErrLicenseKeys
	}
	if len(f.LicenseKeys.Keys) > 0 {
		it.keys, it.keyKind, it.keySources = licenseKeys(f), store.KeySourcePublisher, sources
	}
	return nil
}

// recordKeys records an item's licence keys on its build for each of its sources.
func recordKeys(ctx context.Context, tx *store.Tx, b store.Build, it item) error {
	for _, src := range it.keySources {
		if _, err := tx.RecordLicenseKeys(ctx, b.TenantID, b.ID, it.keyKind, src, it.keys); err != nil {
			return err
		}
	}
	return nil
}
