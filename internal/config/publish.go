package config

// Publish configures publication (spec 0008). The whole block is file-only.
type Publish struct {
	MaxPerPrincipal int `yaml:"max_per_principal"` // uploads at once per principal; default 2
	MaxPerTenant    int `yaml:"max_per_tenant"`    // uploads at once per tenant; default 8
	// Providers was trusted publishing's issuers (spec 0008), removed by spec 0023: a configuration
	// that still sets it is refused, so nobody believes it still applies.
	Providers any `yaml:"providers"`
}

// PublishLimits returns the upload limits, or their defaults.
func (c Config) PublishLimits() (perPrincipal, perTenant int) {
	perPrincipal, perTenant = c.Publish.MaxPerPrincipal, c.Publish.MaxPerTenant
	if perPrincipal == 0 {
		perPrincipal = 2
	}
	if perTenant == 0 {
		perTenant = 8
	}
	return perPrincipal, perTenant
}

func validatePublish(bad func(string, ...any), c Config) {
	p := c.Publish
	if p.MaxPerPrincipal < 0 || p.MaxPerPrincipal > 64 {
		bad("publish.max_per_principal must be within 1..64")
	}
	if p.MaxPerTenant < 0 || p.MaxPerTenant > 256 {
		bad("publish.max_per_tenant must be within 1..256")
	}
	// compared as they apply, a value left out at its default
	if perPrincipal, perTenant := c.PublishLimits(); perPrincipal > perTenant {
		bad("publish.max_per_principal (%d) cannot exceed publish.max_per_tenant (%d)", perPrincipal, perTenant)
	}
	if p.Providers != nil {
		bad("publish.providers: trusted publishing was removed (spec 0023); publishers sign their requests with keys " +
			"(kista admin publisher key add)")
	}
}
