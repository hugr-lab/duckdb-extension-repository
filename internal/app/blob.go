package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	blobfs "github.com/hugr-lab/duckdb-extension-repository/internal/blob/fs"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/s3"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// BlobStores opens the configured storage domains' stores (spec 0005). Nothing is contacted.
func BlobStores(cfg config.Config) ([]blob.Domain, error) {
	var out []blob.Domain
	fail := func(err error) ([]blob.Domain, error) {
		closeStores(out)
		return nil, err
	}
	for _, d := range cfg.BlobDomains() {
		timeout := d.Timeout
		if timeout == 0 {
			timeout = config.DefaultBlobTimeout
		}
		var st blob.Store
		switch d.Kind {
		case "fs":
			s, err := blobfs.Open(d.FS.Root)
			if err != nil {
				return fail(fmt.Errorf("app: storage domain %s: %w", d.Name, err))
			}
			st = s
		case "s3":
			u, err := url.Parse(d.S3.Endpoint)
			if err != nil {
				return fail(fmt.Errorf("app: storage domain %s: invalid endpoint", d.Name))
			}
			s, err := s3.New(s3.Config{
				Endpoint: u.Host, Secure: u.Scheme == "https", Bucket: d.S3.Bucket, Prefix: d.S3.Prefix,
				Region: d.S3.Region, PathStyle: d.S3.Lookup == "path", SSE: d.S3.SSE, KMSKeyID: d.S3.KMSKeyID,
				AccessKeyFile: d.S3.AccessKeyFile, SecretKeyFile: d.S3.SecretKeyFile, CAFile: d.S3.CAFile, Timeout: timeout,
			})
			if err != nil {
				return fail(fmt.Errorf("app: storage domain %s: %w", d.Name, err))
			}
			st = s
		default:
			return fail(fmt.Errorf("app: storage domain %s: kind %s is not available in this build", d.Name, d.Kind))
		}
		out = append(out, blob.Domain{Name: d.Name, Kind: d.Kind, Store: st})
	}
	return out, nil
}

// BlobService opens the stores and starts the blob service: domains are pinned and checked, the
// spool is prepared, and tenants whose domain is missing are logged (they are served nothing).
func BlobService(ctx context.Context, cfg config.Config, st *store.Store, log *slog.Logger) (*blob.Service, error) {
	if log == nil {
		log = slog.Default()
	}
	domains, err := BlobStores(cfg)
	if err != nil {
		return nil, err
	}
	maxBody, maxIngests := cfg.BlobLimits()
	svc, err := blob.NewService(ctx, st, domains, blob.Options{
		SpoolDir: cfg.SpoolDir(), MaxBody: maxBody, MaxIngests: maxIngests, Log: log,
	})
	if err != nil {
		closeStores(domains)
		return nil, err
	}
	missing, err := svc.MissingDomains(ctx)
	if err != nil {
		_ = svc.Close()
		return nil, err
	}
	for _, t := range missing {
		log.Error("blob: a tenant's storage domain is not configured; it is served nothing", "tenant", t)
	}
	return svc, nil
}

func closeStores(ds []blob.Domain) {
	for _, d := range ds {
		if c, ok := d.Store.(io.Closer); ok {
			_ = c.Close()
		}
	}
}
