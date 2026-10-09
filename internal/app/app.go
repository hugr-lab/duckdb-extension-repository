// Package app wires configuration to the store and services.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/cloud/azure"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/azurekv"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/vault"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
	"github.com/hugr-lab/duckdb-extension-repository/internal/vaultapi"
)

// OpenStore opens the configured store and migrates it (store.migrate auto) or checks it (check).
func OpenStore(ctx context.Context, cfg config.Config) (*store.Store, error) {
	var login store.Login
	switch cfg.Store.Login.Kind {
	case "entra":
		id := cfg.Azure.Identity
		cloudName := cfg.Azure.Cloud
		if cloudName == "" {
			cloudName = "public"
		}
		cred, err := azure.Credential(azure.Identity{Kind: id.Kind, ClientID: id.ClientID, TenantID: id.TenantID}, cloudName,
			cfg.Profile == config.ProfileDev)
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
	Store    *store.Store
	Sources  *keysource.Registry
	Tenants  *tenants.Service
	Keys     *keys.Service
	Releases *release.Service // Blob is set by callers that store bodies (WithBlob)
	Auth     *tenants.AuthAdmin
	// Upstreams manages upstreams (spec 0009); runs need a blob service (WithBlob).
	Upstreams *upstream.Service
}

// WithBlob gives the release and upstream services a blob service (adding a release stores its body).
func (s *Services) WithBlob(b *blob.Service) { s.Releases.Blob, s.Upstreams.Blob = b, b }

// KeySources builds the registry of key sources from config (spec 0004): the built-in file source
// and every configured vault or KMS. Sources are not contacted here: kista starts even when one is
// unreachable, and signing through it fails closed.
func KeySources(cfg config.Config) (*keysource.Registry, error) {
	reg := keysource.NewRegistry(signer.Resolver{FileDir: cfg.Signers.FileDir, AllowFile: cfg.FileSignersAllowed()})
	for _, src := range cfg.Signers.Sources {
		opts := keysource.Options{Allow: src.Allow, MaxConcurrency: src.MaxConcurrency, Recheck: src.Recheck, Timeout: src.Timeout}
		switch src.Kind {
		case "vault":
			v := src.Vault
			api, err := vaultapi.New(vaultapi.Config{
				Address: v.Address, Namespace: v.Namespace, CAFile: v.CAFile,
				AuthKind: v.Auth.Kind, AuthMount: v.Auth.Mount, Role: v.Auth.Role, TokenFile: v.Auth.TokenFile,
				AllowHTTP: cfg.Profile == config.ProfileDev, Timeout: src.Timeout,
			})
			if err != nil {
				return nil, fmt.Errorf("app: source %s: %w", src.Name, err)
			}
			if err := reg.Add(src.Name, vault.New(api, v.Mount), opts); err != nil {
				return nil, err
			}
		case "azurekv":
			a := src.AzureKV
			cloudName := a.Cloud
			if cloudName == "" {
				cloudName = "public"
			}
			host, err := azure.Host(cloudName, a.Vault, a.ManagedHSM)
			if err != nil {
				return nil, fmt.Errorf("app: source %s: %w", src.Name, err)
			}
			cred, err := azure.Credential(azure.Identity{Kind: src.Identity.Kind, ClientID: src.Identity.ClientID,
				TenantID: src.Identity.TenantID}, cloudName, cfg.Profile == config.ProfileDev)
			if err != nil {
				return nil, fmt.Errorf("app: source %s: %w", src.Name, err)
			}
			requireHSM := a.RequireHSM == nil || *a.RequireHSM
			b, err := azurekv.New(host, cred, requireHSM, azurekv.Options{Timeout: src.Timeout, Transport: azure.HTTPClient()})
			if err != nil {
				return nil, fmt.Errorf("app: source %s: %w", src.Name, err)
			}
			if err := reg.Add(src.Name, b, opts); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("app: source %s: kind %s is not available in this build", src.Name, src.Kind)
		}
	}
	return reg, nil
}

// NewServices builds the services with an authorizer.
func NewServices(cfg config.Config, s *store.Store, az authz.Authorizer) (*Services, error) {
	reg, err := KeySources(cfg)
	if err != nil {
		return nil, err
	}
	eg, err := Egress(cfg)
	if err != nil {
		return nil, err
	}
	ks := &keys.Service{
		Store:      s,
		Signers:    reg,
		Authz:      az,
		MinTrusted: cfg.Rotation.MinTrusted,
		MinDemoted: cfg.Rotation.MinDemoted,
	}
	rel := &release.Service{Store: s, Signers: ks, Authz: az}
	maxBody, maxIngests := cfg.BlobLimits()
	ul := cfg.UpstreamLimits()
	return &Services{
		Store:    s,
		Sources:  reg,
		Tenants:  &tenants.Service{Store: s, Authz: az, HasDomain: cfg.HasBlobDomain},
		Keys:     ks,
		Releases: rel,
		Auth: &tenants.AuthAdmin{Store: s, Authz: az, Fetch: eg, PublicURL: cfg.Serve.PublicURL,
			AllowHTTP: cfg.Egress.AllowLoopbackHTTP, ServerAudiences: cfg.ServerAudiences(), Providers: Providers(cfg, eg)},
		Upstreams: &upstream.Service{Store: s, Releases: rel, Fetch: eg, Authz: az, MaxBody: maxBody, MaxIngests: maxIngests,
			TempDir: filepath.Join(cfg.SpoolDir(), "upstream"), Log: slog.Default(),
			Config: upstream.Config{Concurrency: ul.Concurrency, FetchTimeout: ul.FetchTimeout, MinRate: int64(ul.MinRate),
				Interval: ul.Interval}},
	}, nil
}

// Providers builds the trusted-publishing providers (spec 0008), each with a verifier of its own.
func Providers(cfg config.Config, f auth.Fetcher) auth.Providers {
	var out auth.Providers
	for _, p := range cfg.PublishProviders() {
		out = append(out, auth.Provider{Name: p.Name, URL: p.URL, Verifier: &auth.Verifier{Fetch: f}})
	}
	return out
}

// WithAuthz returns the services with another authorizer (the API's), sharing everything else.
func (s *Services) WithAuthz(az authz.Authorizer) *Services {
	ten, ks, rel, au, up := *s.Tenants, *s.Keys, *s.Releases, *s.Auth, s.Upstreams.WithAuthz(az)
	ten.Authz, ks.Authz, rel.Authz, au.Authz = az, az, az, az
	rel.Signers = &ks
	up.Releases = &rel
	return &Services{Store: s.Store, Sources: s.Sources, Tenants: &ten, Keys: &ks, Releases: &rel, Auth: &au, Upstreams: up}
}
