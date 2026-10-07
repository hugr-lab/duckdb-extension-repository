// Package app wires configuration to the store and services.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Credential builds the configured Azure credential: managed identity, workload identity, or (dev
// only, enforced by config validation) the developer chain.
func Credential(id config.Identity) (azcore.TokenCredential, error) {
	switch id.Kind {
	case "managed":
		opts := &azidentity.ManagedIdentityCredentialOptions{}
		if id.ClientID != "" {
			opts.ID = azidentity.ClientID(id.ClientID)
		}
		return azidentity.NewManagedIdentityCredential(opts)
	case "workload":
		if os.Getenv("AZURE_FEDERATED_TOKEN_FILE") == "" {
			return nil, errors.New("app: workload identity needs AZURE_FEDERATED_TOKEN_FILE")
		}
		return azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
			ClientID: id.ClientID, TenantID: id.TenantID,
		})
	case "default":
		return azidentity.NewDefaultAzureCredential(nil)
	}
	return nil, errors.New("app: azure.identity.kind is not set")
}

// OpenStore opens the configured store and migrates it (store.migrate auto) or checks it (check).
func OpenStore(ctx context.Context, cfg config.Config) (*store.Store, error) {
	var login store.Login
	switch cfg.Store.Login.Kind {
	case "entra":
		cred, err := Credential(cfg.Azure.Identity)
		if err != nil {
			return nil, err
		}
		scope := store.ScopePostgres
		if cfg.Store.Kind == "sqlserver" {
			scope = store.ScopeSQLServer
		}
		login = store.EntraLogin{Credential: cred, Scope: scope}
	default:
		login = store.PasswordLogin{Env: cfg.Store.Login.PasswordEnv, File: cfg.Store.Login.PasswordFile}
	}
	var (
		s   *store.Store
		err error
	)
	switch cfg.Store.Kind {
	case "sqlite":
		s, err = store.OpenSQLite(ctx, cfg.Store.SQLite.Path)
	case "postgres":
		s, err = store.OpenPostgres(ctx, cfg.Store.DSN, login, cfg.Store.MaxOpenConns)
	case "sqlserver":
		s, err = store.OpenSQLServer(ctx, cfg.Store.DSN, login, cfg.Store.MaxOpenConns)
	default:
		return nil, fmt.Errorf("app: unknown store kind %q", cfg.Store.Kind)
	}
	if err != nil {
		return nil, err
	}
	if cfg.Store.Migrate == "auto" {
		err = s.Migrate(ctx)
	} else {
		err = s.Check(ctx)
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Services are the service layer over one store.
type Services struct {
	Store   *store.Store
	Tenants *tenants.Service
	Keys    *keys.Service
}

// NewServices builds the services with an authorizer.
func NewServices(cfg config.Config, s *store.Store, az authz.Authorizer) *Services {
	return &Services{
		Store:   s,
		Tenants: &tenants.Service{Store: s, Authz: az},
		Keys: &keys.Service{
			Store:      s,
			Signers:    signer.Resolver{FileDir: cfg.Signers.FileDir, AllowFile: cfg.FileSignersAllowed()},
			Authz:      az,
			MinTrusted: cfg.Rotation.MinTrusted,
			MinDemoted: cfg.Rotation.MinDemoted,
		},
	}
}
