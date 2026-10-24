// Package app wires configuration to the store and services.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	rdsauth "github.com/aws/aws-sdk-go-v2/feature/rds/auth"
	"golang.org/x/oauth2"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/audit/sinks"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bootstrap"
	awscloud "github.com/hugr-lab/duckdb-extension-repository/internal/cloud/aws"
	"github.com/hugr-lab/duckdb-extension-repository/internal/cloud/azure"
	gcpcloud "github.com/hugr-lab/duckdb-extension-repository/internal/cloud/gcp"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/awskms"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/azurekv"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/gcpkms"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/kubesecret"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/vault"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
	"github.com/hugr-lab/duckdb-extension-repository/internal/vaultapi"
	"github.com/hugr-lab/duckdb-extension-repository/internal/vaultlogin"
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
	case "aws":
		l, err := rdsLogin(cfg)
		if err != nil {
			return nil, err
		}
		login = l
	case "gcp":
		l, err := cloudSQLLogin(cfg)
		if err != nil {
			return nil, err
		}
		login = l
	case "vault":
		l, err := VaultLogin(cfg)
		if err != nil {
			return nil, err
		}
		login = l
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
		s, err = store.OpenPostgres(ctx, cfg.Store.DSN, login, cfg.Store.MaxOpenConns, []byte(cfg.Store.CAPEM))
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
	s.EventClients = cfg.EventSettings().ClientAddresses
	// events written here carry the configured sinks' bits (spec 0010)
	if len(cfg.Events.Sinks) > 0 {
		// a sink without a free bit gets none until a serve replica frees a removed sink's
		if _, err := sinks.Attach(ctx, s, cfg.Events.Sinks); err != nil && !errors.Is(err, store.ErrNoSinkBit) {
			s.Close()
			return nil, err
		}
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
	// Attachments holds documents about bodies (spec 0011); it needs a blob service (WithBlob).
	Attachments *attachment.Service
	// Feeds are the tenants' subscriptions to other repositories' feeds (spec 0011 phase 1b).
	Feeds *attachment.Feeds
	// Licenses are the tenants' licences (spec 0012).
	Licenses *licenses.Service
}

// WithBlob gives the release, upstream and attachment services a blob service (adding a release
// stores its body, an attachment its content).
func (s *Services) WithBlob(b *blob.Service) {
	s.Releases.Blob, s.Upstreams.Blob, s.Attachments.Blob = b, b, b
}

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
		case "awskms":
			a := src.AWSKMS
			dev := cfg.Profile == config.ProfileDev
			creds, err := awscloud.Credentials(AWSIdentity(src.Identity, a.RoleARN), a.Region,
				awscloud.Options{Dev: dev, FIPS: a.FIPS, STSEndpoint: a.EndpointURL})
			if err != nil {
				return nil, fmt.Errorf("app: source %s: %w", src.Name, err)
			}
			b, err := awskms.New(a.Region, a.Account, a.AllowMultiRegion, creds, awskms.Options{Endpoint: a.EndpointURL, FIPS: a.FIPS})
			if err != nil {
				return nil, fmt.Errorf("app: source %s: %w", src.Name, err)
			}
			if err := reg.Add(src.Name, b, opts); err != nil {
				return nil, err
			}
		case "gcpkms":
			g := src.GCPKMS
			ts, hc, err := GCPTokens(cfg, g.GCPCreds)
			if err != nil {
				return nil, fmt.Errorf("app: source %s: %w", src.Name, err)
			}
			requireHSM := g.RequireHSM == nil || *g.RequireHSM
			if err := reg.Add(src.Name, gcpkms.New(g.KeyRing, ts, requireHSM, hc, gcpkms.Options{Endpoint: g.EndpointURL}), opts); err != nil {
				return nil, err
			}
		case "kubesecret":
			// spec 0026: the Secret volume is checked at every open, not here
			b, err := kubesecret.New(src.KubeSecret.Dir, kubesecret.Options{})
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

// rdsLogin is RDS IAM authentication (spec 0020): a token signed locally with the service's AWS
// identity for each new connection, bound to its host, port and user, good for 15 minutes.
func rdsLogin(cfg config.Config) (store.Login, error) {
	creds, err := awscloud.Credentials(AWSIdentity(cfg.AWS.Identity, ""), cfg.AWS.Region,
		awscloud.Options{Dev: cfg.Profile == config.ProfileDev})
	if err != nil {
		return nil, fmt.Errorf("app: store.login aws: %w", err)
	}
	region := cfg.AWS.Region
	return store.TokenLogin{For: func(ctx context.Context, t store.Target) (string, error) {
		tok, err := rdsauth.BuildAuthToken(ctx, net.JoinHostPort(t.Host, strconv.Itoa(int(t.Port))), region, t.User, creds)
		if err != nil {
			return "", fmt.Errorf("store: the service's AWS identity did not give a database token: %w", err)
		}
		return tok, nil
	}}, nil
}

// VaultLogin is the database password from Vault or OpenBao (spec 0027): a client of its own (never
// a key source's, whose role signs), a KV v2 field or a static role's password, the DSN's user.
func VaultLogin(cfg config.Config) (*vaultlogin.Login, error) {
	v := cfg.Store.Login.Vault
	api, err := vaultapi.New(vaultapi.Config{
		Address: v.Address, Namespace: v.Namespace, CAFile: v.CAFile,
		AuthKind: v.Auth.Kind, AuthMount: v.Auth.Mount, Role: v.Auth.Role, TokenFile: v.Auth.TokenFile,
		AllowHTTP: cfg.Profile == config.ProfileDev,
	})
	if err != nil {
		return nil, fmt.Errorf("app: store.login vault: %w", err)
	}
	user, err := store.DSNUser(cfg.Store.Kind, cfg.Store.DSN)
	if err != nil {
		return nil, fmt.Errorf("app: store.login vault: %w", err)
	}
	o := vaultlogin.Options{User: user}
	switch {
	case v.KV != nil:
		o.Kind, o.Mount, o.Path, o.Field, o.Version, o.Cache = vaultlogin.KV, v.KV.Mount, v.KV.Path, v.KV.Field, v.KV.Version,
			v.VaultLoginCacheOrDefault()
	case v.StaticRole != nil:
		o.Kind, o.Mount, o.Name = vaultlogin.StaticRole, v.StaticRole.Mount, v.StaticRole.Name
	}
	l, err := vaultlogin.New(api, o)
	if err != nil {
		return nil, fmt.Errorf("app: store.login vault: %w", err)
	}
	return l, nil
}

// metadataURL replaces the metadata server for the gcp database login in tests.
var metadataURL string

// cloudSQLLogin is Cloud SQL IAM database authentication (spec 0020): an access token of the
// service's principal, scoped to the database login only.
func cloudSQLLogin(cfg config.Config) (store.Login, error) {
	c := cfg.GCP.GCPCreds
	var ts oauth2.TokenSource
	if c.EndpointURL != "" {
		ts = gcpcloud.StaticToken()
	} else {
		var err error
		ts, err = gcpcloud.TokenSource(c.CredentialsFile, c.Principal, gcpcloud.Options{Dev: cfg.Profile == config.ProfileDev,
			HTTPClient: gcpcloud.HTTPClient(), Scopes: []string{gcpcloud.SQLLoginScope}, MetadataURL: metadataURL})
		if err != nil {
			return nil, fmt.Errorf("app: store.login gcp: %w", err)
		}
	}
	return store.TokenLogin{For: func(ctx context.Context, _ store.Target) (string, error) {
		tok, err := gcpcloud.Token(ctx, ts)
		if err != nil {
			return "", fmt.Errorf("store: the service's Google identity did not give a database token: %w", err)
		}
		return tok.AccessToken, nil
	}}, nil
}

// GCPTokens are the Google credentials of a source or a domain (spec 0004 phase 4), with the HTTP
// client their calls use. A development fake (endpoint_url) gets a fixed token and no credential.
func GCPTokens(cfg config.Config, c config.GCPCreds) (oauth2.TokenSource, *http.Client, error) {
	hc := gcpcloud.HTTPClient()
	if c.EndpointURL != "" {
		return gcpcloud.StaticToken(), hc, nil
	}
	ts, err := gcpcloud.TokenSource(c.CredentialsFile, c.Principal, gcpcloud.Options{Dev: cfg.Profile == config.ProfileDev, HTTPClient: hc})
	return ts, hc, err
}

// AWSIdentity is a configured AWS identity, with the role to assume after it ("" for none).
func AWSIdentity(id *config.Identity, assume string) awscloud.Identity {
	out := awscloud.Identity{AssumeRole: assume}
	if id != nil {
		out.Kind, out.PlatformRole, out.AccessKeyFile, out.SecretKeyFile = id.Kind, id.RoleARN, id.AccessKeyFile, id.SecretKeyFile
	}
	return out
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
	cat := cfg.CatalogueSettings()
	rel := &release.Service{Store: s, Signers: ks, Authz: az, MaxEntries: cat.MaxEntries, MaxBytes: cat.MaxBytes}
	maxBody, maxIngests := cfg.BlobLimits()
	ul := cfg.UpstreamLimits()
	creds, err := Credentials(cfg)
	if err != nil {
		return nil, err
	}
	bundles, err := credential.NewBundleSources(cfg.Upstreams.BundleSources)
	if err != nil {
		return nil, err
	}
	// what a private upstream brought becomes public only as its credential allows (spec 0009 phase
	// 3), and what an authenticated bundle source brought as upstreams.bundle_sources allows (0014)
	rel.MayPublish = func(tenant, name string) bool {
		if prefix, ok := credential.BundlePrefix(name); ok {
			return bundles.AllowPublic(tenant, prefix)
		}
		c, ok := creds[name]
		return ok && c.AllowPublic
	}
	// a purge before every replica keeps purged slots could see its slot refilled (spec 0016)
	rel.PurgeOff = cfg.GCSettings().Interval == 0
	at := &attachment.Service{Store: s, Authz: az, MaxSize: cfg.Attachments.MaxSize, Keys: ks, PublicURL: cfg.Serve.PublicURL}
	feeds := &attachment.Feeds{Store: s, Authz: az, Attachments: at, Fetch: eg, Credentials: creds,
		Interval: cfg.Attachments.FeedIntervalOrDefault(), Log: slog.Default(), TempDir: filepath.Join(cfg.SpoolDir(), "feeds")}
	lic := &licenses.Service{Store: s, Authz: az, Enabled: cfg.Licenses.On(), Refuse: cfg.Licenses.Refused(), Fetch: eg, Credentials: creds,
		Interval: cfg.Licenses.RefreshOrDefault(), Log: slog.Default()}
	return &Services{
		Store:    s,
		Sources:  reg,
		Tenants:  &tenants.Service{Store: s, Authz: az, HasDomain: cfg.HasBlobDomain},
		Keys:     ks,
		Releases: rel,
		Auth: &tenants.AuthAdmin{Store: s, Authz: az, Fetch: eg, PublicURL: cfg.Serve.PublicURL,
			AllowHTTP: cfg.Egress.AllowLoopbackHTTP, ServerAudiences: cfg.ServerAudiences(),
			ServerIssuerURLs: serverIssuerURLs(cfg)},
		Upstreams: &upstream.Service{Store: s, Releases: rel, Fetch: eg, Authz: az, MaxBody: maxBody, MaxIngests: maxIngests,
			TempDir: filepath.Join(cfg.SpoolDir(), "upstream"), Log: slog.Default(),
			Config: upstream.Config{Concurrency: ul.Concurrency, FetchTimeout: ul.FetchTimeout, MinRate: int64(ul.MinRate),
				Interval: ul.Interval}, Credentials: creds, Feeds: feeds, Uploads: uploadSettings(cfg), Licenses: lic},
		Attachments: at,
		Feeds:       feeds,
		Licenses:    lic,
	}, nil
}

// Credentials builds the private upstreams' credentials (spec 0009 phase 3): each token endpoint
// through egress with egress.allow and the credential's own allowlist; the platform's identity for
// client_auth azure. Nothing is contacted here.
func Credentials(cfg config.Config) (credential.Registry, error) {
	d := credential.Deps{Poster: func(c config.Credential) (credential.Poster, error) { return egressWith(cfg, c.Allow, 0) }}
	if cfg.Azure.Identity.Kind != "" {
		id := cfg.Azure.Identity
		cloudName := cfg.Azure.Cloud
		if cloudName == "" {
			cloudName = "public"
		}
		// Entra's federated credential audience per cloud
		d.AzureExchange = map[string]string{"public": "api://AzureADTokenExchange", "china": "api://AzureADTokenExchangeChina",
			"usgov": "api://AzureADTokenExchangeUSGov"}[cloudName]
		// one credential, made at first use: its token cache is reused
		cred := sync.OnceValues(func() (azcore.TokenCredential, error) {
			return azure.Credential(azure.Identity{Kind: id.Kind, ClientID: id.ClientID, TenantID: id.TenantID}, cloudName,
				cfg.Profile == config.ProfileDev)
		})
		d.Azure = func(ctx context.Context, scope string) (string, error) {
			c, err := cred()
			if err != nil {
				return "", err
			}
			tok, err := c.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{scope}})
			return tok.Token, err
		}
	}
	return credential.New(cfg.Upstreams.Credentials, d)
}

// WithAuthz returns the services with another authorizer (the API's), sharing everything else.
func (s *Services) WithAuthz(az authz.Authorizer) *Services {
	ten, ks, rel, au, up, at, fd := *s.Tenants, *s.Keys, *s.Releases, *s.Auth, s.Upstreams.WithAuthz(az), *s.Attachments, *s.Feeds
	ten.Authz, ks.Authz, rel.Authz, au.Authz, at.Authz, fd.Authz = az, az, az, az, az, az
	rel.Signers = &ks
	up.Releases = &rel
	at.Keys = &ks
	fd.Attachments = &at
	up.Feeds = &fd // a bundle's feeds, imported under the copy's authorizer too
	return &Services{Store: s.Store, Sources: s.Sources, Tenants: &ten, Keys: &ks, Releases: &rel, Auth: &au, Upstreams: up, Attachments: &at,
		Feeds: &fd, Licenses: s.Licenses.WithAuthz(az)}
}

func serverIssuerURLs(cfg config.Config) []string {
	var out []string
	for _, s := range cfg.Auth.ServerIssuers {
		out = append(out, s.URL)
	}
	return out
}

// BootstrapDeps are the services the configuration's bootstrap acts through (spec 0020 phase 4a).
func BootstrapDeps(svc *Services, log *slog.Logger) bootstrap.Deps {
	return bootstrap.Deps{Store: svc.Store, Tenants: svc.Tenants, Auth: svc.Auth, Keys: svc.Keys, Upstreams: svc.Upstreams, Log: log}
}

// uploadSettings are the bundles block's limits on uploads (spec 0014 phase 2).
func uploadSettings(cfg config.Config) upstream.UploadSettings {
	b := cfg.Bundles.Settings()
	return upstream.UploadSettings{MaxSize: int64(b.MaxSize), ChunkSize: int64(b.ChunkSize), MaxBytes: int64(b.MaxBytes), MaxUploads: b.MaxUploads,
		TTL: b.UploadTTL, MaxAge: b.UploadMaxAge}
}
