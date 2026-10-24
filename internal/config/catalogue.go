package config

// Catalogue bounds what one source holds in a tenant's catalogue (spec 0033): an intake beyond
// either is refused until older entries are purged.
type Catalogue struct {
	MaxEntries int      `yaml:"max_entries"` // builds placed nowhere per source; default 20000
	MaxBytes   ByteSize `yaml:"max_bytes"`   // their bodies per source; default 100 GiB
}

// Catalogue defaults.
const (
	DefaultCatalogueEntries = 20000
	DefaultCatalogueBytes   = ByteSize(100 << 30)
)

// CatalogueSettings returns catalogue: with its defaults.
func (c Config) CatalogueSettings() Catalogue {
	g := c.Catalogue
	if g.MaxEntries == 0 {
		g.MaxEntries = DefaultCatalogueEntries
	}
	if g.MaxBytes == 0 {
		g.MaxBytes = DefaultCatalogueBytes
	}
	return g
}

func validateCatalogue(bad func(string, ...any), c Config) {
	if n := c.Catalogue.MaxEntries; n != 0 && (n < 100 || n > 10_000_000) {
		bad("catalogue.max_entries must be 0 (20000) or within 100..10000000")
	}
	if n := c.Catalogue.MaxBytes; n != 0 && (n < 1<<30 || n > 1<<50) {
		bad("catalogue.max_bytes must be 0 (100 GiB) or within 1 GiB..1 PiB")
	}
}
