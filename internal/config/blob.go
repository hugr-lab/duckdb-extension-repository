package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Blob configures where extension bodies are stored (spec 0005). The whole block is file-only.
type Blob struct {
	Domains    []BlobDomain `yaml:"domains"`
	SpoolDir   string       `yaml:"spool_dir"`
	MaxBody    ByteSize     `yaml:"max_body"`
	MaxIngests int          `yaml:"max_ingests"`
}

// BlobDomain is a named store.
type BlobDomain struct {
	Name      string        `yaml:"name"`
	Kind      string        `yaml:"kind"` // fs | s3 | azureblob (phase 2)
	FS        *FSDomain     `yaml:"fs"`
	S3        *S3Domain     `yaml:"s3"`
	AzureBlob *yaml.Node    `yaml:"azureblob"` // phase 2
	Timeout   time.Duration `yaml:"timeout"`   // s3: connect, headers and idle reads/writes; default 30s, 1s..5m
}

// FSDomain is a local directory (or a mounted volume).
type FSDomain struct {
	Root string `yaml:"root"`
}

// S3Domain is an S3-compatible bucket and prefix.
type S3Domain struct {
	Endpoint      string `yaml:"endpoint"` // https://host[:port]; http only on loopback in dev
	Bucket        string `yaml:"bucket"`
	Prefix        string `yaml:"prefix"`
	Region        string `yaml:"region"`
	Lookup        string `yaml:"lookup"` // path | dns
	SSE           string `yaml:"sse"`    // none | s3 | kms
	KMSKeyID      string `yaml:"kms_key_id"`
	AccessKeyFile string `yaml:"access_key_file"`
	SecretKeyFile string `yaml:"secret_key_file"`
	CAFile        string `yaml:"ca_file"` // trust only this CA bundle (an internal CA); empty: system roots
}

// ByteSize is a size written as a number of bytes or with a KiB, MiB or GiB suffix.
type ByteSize int64

// UnmarshalYAML implements yaml.Unmarshaler.
func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	v, err := parseSize(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*b = v
	return nil
}

func parseSize(s string) (ByteSize, error) {
	mult := int64(1)
	for suffix, m := range map[string]int64{"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30} {
		if v, ok := strings.CutSuffix(s, suffix); ok {
			s, mult = strings.TrimSpace(v), m
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("not a size (bytes, or a number with KiB, MiB or GiB)")
	}
	return ByteSize(n * mult), nil
}

// Blob defaults.
const (
	DefaultBlobRoot    = "/var/lib/kista/blobs"
	DefaultSpoolDir    = "/var/lib/kista/spool"
	DefaultMaxBody     = ByteSize(1 << 30)
	DefaultMaxIngests  = 4
	DefaultBlobTimeout = 30 * time.Second
	maxMaxBody         = ByteSize(4 << 30) // a gzip trailer records the size modulo 4 GiB
)

// BlobDomains returns the configured domains, or the default fs domain: /var/lib/kista/blobs, or
// in dev with SQLite, a directory beside the database file.
func (c Config) BlobDomains() []BlobDomain {
	if len(c.Blob.Domains) > 0 {
		return c.Blob.Domains
	}
	return []BlobDomain{{Name: store.DefaultDomain, Kind: "fs", FS: &FSDomain{Root: c.besideDB("blobs", DefaultBlobRoot)}}}
}

// SpoolDir is blob.spool_dir or its default.
func (c Config) SpoolDir() string {
	if c.Blob.SpoolDir != "" {
		return c.Blob.SpoolDir
	}
	return c.besideDB("spool", DefaultSpoolDir)
}

func (c Config) besideDB(name, def string) string {
	if c.Profile == ProfileDev && c.Store.Kind == "sqlite" && filepath.IsAbs(c.Store.SQLite.Path) {
		return filepath.Join(filepath.Dir(c.Store.SQLite.Path), name)
	}
	return def
}

// BlobLimits returns max_body and max_ingests, or their defaults.
func (c Config) BlobLimits() (maxBody int64, maxIngests int) {
	maxBody, maxIngests = int64(c.Blob.MaxBody), c.Blob.MaxIngests
	if maxBody == 0 {
		maxBody = int64(DefaultMaxBody)
	}
	if maxIngests == 0 {
		maxIngests = DefaultMaxIngests
	}
	return maxBody, maxIngests
}

// HasBlobDomain reports whether a storage domain is configured.
func (c Config) HasBlobDomain(name string) bool {
	for _, d := range c.BlobDomains() {
		if d.Name == name {
			return true
		}
	}
	return false
}

var (
	s3Bucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	ipLike   = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)
	s3Region = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
)

func validateBlob(bad func(string, ...any), c Config) {
	dev := c.Profile == ProfileDev
	b := c.Blob
	if b.SpoolDir != "" && !filepath.IsAbs(b.SpoolDir) {
		bad("blob.spool_dir must be an absolute path")
	}
	if b.MaxBody < 0 || b.MaxBody > maxMaxBody {
		bad("blob.max_body must be within 1..4GiB")
	}
	if b.MaxIngests < 0 || b.MaxIngests > 256 {
		bad("blob.max_ingests must be within 1..256")
	}
	seen := map[string]bool{}
	if len(b.Domains) > 0 {
		hasDefault := false
		for _, d := range b.Domains {
			hasDefault = hasDefault || d.Name == store.DefaultDomain
		}
		if !hasDefault {
			bad("blob.domains must include %q: tenants created without a domain, and every tenant created before storage domains, live there", store.DefaultDomain)
		}
	}
	for i, d := range b.Domains {
		where := fmt.Sprintf("blob.domains[%d]", i)
		if store.ValidDomain(d.Name) != nil {
			bad("%s.name must match [a-z][a-z0-9-]{0,15}", where)
		}
		if seen[d.Name] {
			bad("%s: domain %s is defined twice", where, d.Name)
		}
		seen[d.Name] = true
		if d.Timeout != 0 && (d.Timeout < time.Second || d.Timeout > 5*time.Minute) {
			bad("%s.timeout must be within 1s..5m", where)
		}
		blocks := 0
		for _, set := range []bool{d.FS != nil, d.S3 != nil, d.AzureBlob != nil} {
			if set {
				blocks++
			}
		}
		switch d.Kind {
		case "fs":
			if d.FS == nil || blocks != 1 {
				bad("%s: kind fs needs exactly one block, fs", where)
				continue
			}
			if !filepath.IsAbs(d.FS.Root) || filepath.Clean(d.FS.Root) != d.FS.Root {
				bad("%s.fs.root must be a clean absolute path", where)
			}
			if d.Timeout != 0 {
				bad("%s.timeout applies to remote stores only", where)
			}
		case "s3":
			if d.S3 == nil || blocks != 1 {
				bad("%s: kind s3 needs exactly one block, s3", where)
				continue
			}
			validateS3(bad, where, *d.S3, dev)
		case "azureblob":
			bad("%s: kind azureblob comes in phase 2 of spec 0005", where)
		default:
			bad("%s.kind must be fs or s3", where)
		}
	}
	// two domains on one store would share objects (and one's garbage collection would delete the
	// other's); the service refuses overlap by store identity too
	for i, a := range b.Domains {
		for _, o := range b.Domains[:i] {
			if a.Kind == o.Kind && a.Kind == "fs" && a.FS != nil && o.FS != nil && pathsOverlap(a.FS.Root, o.FS.Root) ||
				a.Kind == o.Kind && a.Kind == "s3" && a.S3 != nil && o.S3 != nil && strings.EqualFold(a.S3.Endpoint, o.S3.Endpoint) &&
					a.S3.Bucket == o.S3.Bucket && pathsOverlap("/"+a.S3.Prefix, "/"+o.S3.Prefix) {
				bad("blob.domains: %s and %s share a store", o.Name, a.Name)
			}
		}
	}
}

func pathsOverlap(a, b string) bool {
	a, b = strings.TrimSuffix(a, "/")+"/", strings.TrimSuffix(b, "/")+"/"
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

func validateS3(bad func(string, ...any), where string, s S3Domain, dev bool) {
	u, err := url.Parse(s.Endpoint)
	switch {
	case err != nil || u.Host == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.User != nil || u.Fragment != "":
		bad("%s.s3.endpoint must be a scheme://host[:port] URL", where)
	case u.Scheme == "http":
		if !dev || !store.IsLoopbackHost(u.Hostname()) {
			bad("%s.s3.endpoint: plain http only on loopback with profile dev", where)
		}
	case u.Scheme != "https":
		bad("%s.s3.endpoint must be https", where)
	}
	if !s3Bucket.MatchString(s.Bucket) || strings.Contains(s.Bucket, "..") || strings.Contains(s.Bucket, ".-") ||
		strings.Contains(s.Bucket, "-.") || ipLike.MatchString(s.Bucket) {
		bad("%s.s3.bucket is not a valid bucket name", where)
	}
	if s.Lookup == "dns" && strings.Contains(s.Bucket, ".") && strings.HasPrefix(s.Endpoint, "https://") {
		bad("%s.s3: a bucket name with dots cannot be reached with lookup dns over https (the certificate does not match); use lookup path", where)
	}
	if blob.ValidPrefix(s.Prefix) != nil {
		bad("%s.s3.prefix must be empty or lowercase segments each ending with /", where)
	}
	if !s3Region.MatchString(s.Region) {
		bad("%s.s3.region is required (R2: auto)", where)
	}
	if s.Lookup != "path" && s.Lookup != "dns" {
		bad("%s.s3.lookup must be path or dns", where)
	}
	switch s.SSE {
	case "none", "s3":
		if s.KMSKeyID != "" {
			bad("%s.s3.kms_key_id is only for sse kms", where)
		}
	case "kms":
		if s.KMSKeyID == "" {
			bad("%s.s3.kms_key_id is required for sse kms", where)
		}
	default:
		bad("%s.s3.sse is required: none, s3 or kms", where)
	}
	if s.CAFile != "" && !filepath.IsAbs(s.CAFile) {
		bad("%s.s3.ca_file must be an absolute path", where)
	}
	if !filepath.IsAbs(s.AccessKeyFile) || !filepath.IsAbs(s.SecretKeyFile) {
		bad("%s.s3: access_key_file and secret_key_file must be absolute paths", where)
	}
}
