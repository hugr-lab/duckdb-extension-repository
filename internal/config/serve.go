package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Serve configures kista serve (spec 0006). Everything but public_url is file-only: listeners,
// proxies and limits decide what reaches clients.
type Serve struct {
	PublicURL             string        `yaml:"public_url"`
	Listeners             []Listener    `yaml:"listeners" kista:"fileonly"`
	TrustedProxies        []string      `yaml:"trusted_proxies" kista:"fileonly"`
	Resign                bool          `yaml:"resign" kista:"fileonly"`
	MaxDownloads          int           `yaml:"max_downloads" kista:"fileonly"`
	MaxDownloadsPerClient int           `yaml:"max_downloads_per_client" kista:"fileonly"`
	MinRate               ByteSize      `yaml:"min_rate" kista:"fileonly"`
	WriteIdleTimeout      time.Duration `yaml:"write_idle_timeout" kista:"fileonly"`
	DrainTimeout          time.Duration `yaml:"drain_timeout" kista:"fileonly"`
	ShutdownTimeout       time.Duration `yaml:"shutdown_timeout" kista:"fileonly"`
}

// Listener is one address kista serves on.
type Listener struct {
	Addr        string     `yaml:"addr"`
	Scheme      string     `yaml:"scheme"` // https | http | dual
	TLS         *TLSConfig `yaml:"tls"`
	BehindProxy bool       `yaml:"behind_proxy"`
}

// TLSConfig is a certificate and key, reloaded when the files change.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Serve defaults.
const (
	DefaultMaxDownloads          = 256
	DefaultMaxDownloadsPerClient = 8
	DefaultMinRate               = ByteSize(16 << 10)
	DefaultWriteIdleTimeout      = 30 * time.Second
	DefaultDrainTimeout          = 5 * time.Second
	DefaultShutdownTimeout       = 30 * time.Second
)

// ServeLimits returns the serve limits with defaults applied.
func (c Config) ServeLimits() Serve {
	s := c.Serve
	if s.MaxDownloads == 0 {
		s.MaxDownloads = DefaultMaxDownloads
	}
	if s.MaxDownloadsPerClient == 0 {
		s.MaxDownloadsPerClient = DefaultMaxDownloadsPerClient
	}
	if s.MinRate == 0 {
		s.MinRate = DefaultMinRate
	}
	if s.WriteIdleTimeout == 0 {
		s.WriteIdleTimeout = DefaultWriteIdleTimeout
	}
	if s.DrainTimeout == 0 {
		s.DrainTimeout = DefaultDrainTimeout
	}
	if s.ShutdownTimeout == 0 {
		s.ShutdownTimeout = DefaultShutdownTimeout
	}
	return s
}

// TrustedProxyPrefixes parses trusted_proxies (validated).
func (c Config) TrustedProxyPrefixes() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range c.Serve.TrustedProxies {
		if pf, err := netip.ParsePrefix(p); err == nil {
			out = append(out, pf.Masked())
		}
	}
	return out
}

func validateServe(bad func(string, ...any), c Config) {
	s := c.Serve
	dev := c.Profile == ProfileDev
	if s.PublicURL != "" {
		u, err := url.Parse(s.PublicURL)
		switch {
		case err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil:
			bad("serve.public_url must be scheme://host[:port] without a path or a trailing slash")
		case u.Scheme == "http" && (!dev || !isLoopback(u.Hostname())):
			bad("serve.public_url must be https (http only on loopback with profile dev)")
		case u.Scheme != "https" && u.Scheme != "http":
			bad("serve.public_url must be https")
		}
	}
	seen := map[string]bool{}
	for i, l := range s.Listeners {
		where := fmt.Sprintf("serve.listeners[%d]", i)
		if _, _, err := net.SplitHostPort(l.Addr); err != nil {
			bad("%s.addr must be host:port", where)
		}
		if seen[l.Addr] {
			bad("%s.addr %s is used twice", where, l.Addr)
		}
		seen[l.Addr] = true
		switch l.Scheme {
		case "https":
			if (l.TLS == nil) == !l.BehindProxy {
				bad("%s: an https listener has exactly one of tls and behind_proxy", where)
			}
		case "dual":
			if l.TLS == nil || l.BehindProxy {
				bad("%s: a dual listener needs tls and cannot be behind_proxy", where)
			}
		case "http":
			if l.TLS != nil || l.BehindProxy {
				bad("%s: an http listener has neither tls nor behind_proxy", where)
			}
		default:
			bad("%s.scheme must be https, http or dual", where)
		}
		if l.TLS != nil && (!filepath.IsAbs(l.TLS.CertFile) || !filepath.IsAbs(l.TLS.KeyFile)) {
			bad("%s.tls: cert_file and key_file must be absolute paths", where)
		}
	}
	for _, l := range s.Listeners {
		if l.BehindProxy && len(s.TrustedProxies) == 0 {
			bad("serve.trusted_proxies is required with a behind_proxy listener: without it every client looks like the proxy")
			break
		}
	}
	for _, p := range s.TrustedProxies {
		if _, err := netip.ParsePrefix(p); err != nil {
			bad("serve.trusted_proxies: %q is not a CIDR", p)
		}
	}
	if s.MaxDownloads < 0 || s.MaxDownloadsPerClient < 0 || s.MinRate < 0 || s.WriteIdleTimeout < 0 ||
		s.DrainTimeout < 0 || s.ShutdownTimeout < 0 {
		bad("serve limits cannot be negative")
	}
	if s.WriteIdleTimeout != 0 && s.WriteIdleTimeout < time.Second {
		bad("serve.write_idle_timeout must be at least 1s")
	}
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}
