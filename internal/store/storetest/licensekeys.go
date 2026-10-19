package storetest

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// LicenseKey records pub as a licence key of every build of name in the tenant, brought by the
// source (kind, id), and bumps the tenant's channels: a test's stand-in for a trusted intake of a
// body whose key block declares the key (spec 0021). For a publisher, source is its name, and a
// marked publisher of that name is added when the tenant has none; for an upstream, its id. It
// returns the key's kid.
func LicenseKey(ctx context.Context, st *store.Store, tenantID, name, kind, source string, pub *rsa.PublicKey) (string, error) {
	sourceID := source
	if kind == store.KeySourcePublisher {
		err := st.InTx(ctx, "", func(tx *store.Tx) error {
			p, err := tx.GetPublisher(ctx, tenantID, source)
			if errors.Is(err, store.ErrNotFound) {
				p = store.Publisher{TenantID: tenantID, Name: source, CreatedBy: "os:0:test"}
				err = tx.InsertPublisher(ctx, &p)
			}
			if err != nil {
				return err
			}
			sourceID = p.ID
			_, err = tx.SetPublisherLicenseKeys(ctx, tenantID, source, true)
			return err
		})
		if err != nil {
			return "", err
		}
	}
	pem, err := extfile.MarshalPublicKeyPEM(pub)
	if err != nil {
		return "", err
	}
	kid := extfile.Fingerprint(pub)
	if kid == "" {
		return "", fmt.Errorf("storetest: not an RSA-2048 key with e = 65537")
	}
	if err := store.ExecRaw(ctx, st, `INSERT INTO build_license_keys (tenant_id, build_id, kid, pem, source_kind, source_id, recorded_at)
SELECT tenant_id, id, ?, ?, ?, ?, created_at FROM builds WHERE tenant_id = ? AND name = ?`, kid, pem, kind, sourceID, tenantID, name); err != nil {
		return "", err
	}
	return kid, store.ExecRaw(ctx, st, "UPDATE channels SET release_version = release_version + 1 WHERE tenant_id = ?", tenantID)
}
