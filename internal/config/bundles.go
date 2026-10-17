package config

import "time"

// Bundles configures bundles uploaded over the API (spec 0014 phase 2).
type Bundles struct {
	// MaxSize is an upload's largest size (default 20 GiB, at most 1024 GiB).
	MaxSize ByteSize `yaml:"max_size"`
	// ChunkSize is the largest chunk an upload takes (default 16 MiB, 1..64 MiB); a client may ask
	// for less.
	ChunkSize ByteSize `yaml:"chunk_size"`
	// UploadTTL expires an upload that sends no chunk for it, and a failed one kept for retry
	// (default 24h, 1h..168h).
	UploadTTL time.Duration `yaml:"upload_ttl"`
	// UploadMaxAge expires an upload not completed this long after its creation (default 168h, at
	// least upload_ttl, at most 720h).
	UploadMaxAge time.Duration `yaml:"upload_max_age"`
	// MaxUploads is a tenant's uploads not yet imported (default 2, 1..20).
	MaxUploads int `yaml:"max_uploads"`
	// MaxBytes is the declared sizes of the server's uploads not yet imported (default 100 GiB, at
	// least max_size).
	MaxBytes ByteSize `yaml:"max_bytes"`
}

// The bundles block's defaults.
const (
	DefaultBundleMaxSize   = ByteSize(20 << 30)
	DefaultBundleChunkSize = ByteSize(16 << 20)
	DefaultBundleMaxBytes  = ByteSize(100 << 30)
)

// Settings is the block with its defaults filled in.
func (b Bundles) Settings() Bundles {
	if b.MaxSize == 0 {
		b.MaxSize = DefaultBundleMaxSize
	}
	if b.ChunkSize == 0 {
		b.ChunkSize = DefaultBundleChunkSize
	}
	if b.UploadTTL == 0 {
		b.UploadTTL = 24 * time.Hour
	}
	if b.UploadMaxAge == 0 {
		b.UploadMaxAge = 7 * 24 * time.Hour
	}
	if b.MaxUploads == 0 {
		b.MaxUploads = 2
	}
	if b.MaxBytes == 0 {
		b.MaxBytes = max(DefaultBundleMaxBytes, b.MaxSize)
	}
	return b
}

func validateBundles(bad func(string, ...any), c Config) {
	b := c.Bundles
	if b.MaxSize != 0 && (b.MaxSize < 1<<20 || b.MaxSize > 1024<<30) {
		bad("bundles.max_size must be 0 (20 GiB) or within 1 MiB..1024 GiB")
	}
	if b.ChunkSize != 0 && (b.ChunkSize < 1<<20 || b.ChunkSize > 64<<20) {
		bad("bundles.chunk_size must be 0 (16 MiB) or within 1 MiB..64 MiB")
	}
	if b.UploadTTL != 0 && (b.UploadTTL < time.Hour || b.UploadTTL > 7*24*time.Hour) {
		bad("bundles.upload_ttl must be 0 (24h) or within 1h..168h")
	}
	s := b.Settings()
	if b.UploadMaxAge != 0 && (b.UploadMaxAge < s.UploadTTL || b.UploadMaxAge > 30*24*time.Hour) {
		bad("bundles.upload_max_age must be 0 (168h) or at least upload_ttl, at most 720h")
	}
	if b.MaxUploads < 0 || b.MaxUploads > 20 {
		bad("bundles.max_uploads must be 0 (2) to 20")
	}
	if b.MaxBytes != 0 && b.MaxBytes < s.MaxSize {
		bad("bundles.max_bytes must be 0 (100 GiB) or at least max_size")
	}
}
