package egress

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strings"

	"golang.org/x/net/idna"
)

// ErrHostNotAllowed is a host that is not on egress.hosts (spec 0044): refused on its name alone,
// before any resolution or connection. It is a refusal (errors.Is ErrRefused).
var ErrHostNotAllowed = fmt.Errorf("%w: the host is not on this server's egress host list", ErrRefused)

// HostError is a refused host, named (errors.Is ErrHostNotAllowed).
type HostError struct{ Host string }

func (e *HostError) Error() string {
	return fmt.Sprintf("%v: %s is not on this server's egress host list", ErrRefused, e.Host)
}

// Unwrap makes a HostError ErrHostNotAllowed.
func (e *HostError) Unwrap() error { return ErrHostNotAllowed }

// Hosts is egress.hosts as a matcher (spec 0044): exact names, wildcard domains (any name below
// them, never the domain itself) and IP addresses. A nil *Hosts narrows nothing; an empty one
// refuses every host.
type Hosts struct {
	names     map[string]bool
	wildcards map[string]bool // the domain after "*."
	addrs     map[netip.Addr]bool
}

// NewHosts builds the matcher from egress.hosts' entries (CheckHostEntry's rules). It is never nil.
func NewHosts(entries []string) (*Hosts, error) {
	h := &Hosts{names: map[string]bool{}, wildcards: map[string]bool{}, addrs: map[netip.Addr]bool{}}
	for _, e := range entries {
		kind, v, err := parseHostEntry(e)
		if err != nil {
			return nil, fmt.Errorf("egress: host entry %q: %w", e, err)
		}
		switch kind {
		case entryAddr:
			h.addrs[netip.MustParseAddr(v)] = true
		case entryWildcard:
			h.wildcards[v] = true
		default:
			h.names[v] = true
		}
	}
	return h, nil
}

// Len is the number of entries (distinct after normalisation).
func (h *Hosts) Len() int { return len(h.names) + len(h.wildcards) + len(h.addrs) }

// CheckHostEntry checks one egress.hosts entry: a host name, *.<domain>, or an IP address.
func CheckHostEntry(e string) error {
	_, _, err := parseHostEntry(e)
	return err
}

const (
	entryName = iota
	entryWildcard
	entryAddr
)

// parseHostEntry returns an entry's kind and its normalised form.
func parseHostEntry(e string) (int, string, error) {
	switch {
	case e == "":
		return 0, "", errors.New("an empty entry")
	case strings.Contains(e, "://"):
		return 0, "", errors.New("a host, not a URL (no scheme)")
	case strings.HasPrefix(e, "["):
		return 0, "", errors.New("an IP address is written without brackets")
	}
	if _, err := netip.ParsePrefix(e); err == nil {
		return 0, "", errors.New("no CIDR: address ranges are egress.allow's")
	}
	if strings.Contains(e, "/") {
		return 0, "", errors.New("a host, not a URL (no path)")
	}
	if a, err := netip.ParseAddr(e); err == nil {
		if a.Zone() != "" {
			return 0, "", errors.New("an address without a zone")
		}
		return entryAddr, a.Unmap().String(), nil
	}
	if strings.Contains(e, ":") {
		return 0, "", errors.New("no port: ports are egress.allow's")
	}
	kind, name := entryName, e
	if d, ok := strings.CutPrefix(e, "*."); ok {
		kind, name = entryWildcard, d
	}
	if strings.Contains(name, "*") {
		return 0, "", errors.New("* is only the whole leftmost label (*.<domain>)")
	}
	n, ok := normalName(name)
	if !ok {
		return 0, "", errors.New("not a valid host name")
	}
	labels := strings.Split(n, ".")
	if allDigits(labels[len(labels)-1]) {
		return 0, "", errors.New("a name whose last label is all digits is no IP address")
	}
	if kind == entryWildcard && len(labels) < 2 {
		return 0, "", errors.New("a wildcard's domain has at least two labels (*.example.com, not *.com)")
	}
	return kind, n, nil
}

func allDigits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// normalName is a host name as resolution sees it: IDNA-mapped, lowercase, without a trailing dot;
// false for a name that fails the mapping or has an empty label.
func normalName(h string) (string, bool) {
	a, err := idna.Lookup.ToASCII(h)
	if err != nil {
		return "", false
	}
	a = strings.TrimSuffix(strings.ToLower(a), ".")
	if a == "" || slices.Contains(strings.Split(a, "."), "") {
		return "", false
	}
	return a, true
}

// HostKey is a host in the form the list compares it in: an address unmapped (a zone kept, since a
// zoned address is never allowed), a name IDNA-mapped, lowercase and without a trailing dot; a host
// that is neither, lowercased. Two spellings of one host have one key.
func HostKey(host string) string {
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap().String()
	}
	if n, ok := normalName(host); ok {
		return n
	}
	return strings.ToLower(host)
}

// allowed reports whether a host (a URL's, without port or brackets) is on the list: an address
// against the address entries only, a name against the names and the wildcards.
func (h *Hosts) allowed(host string) bool {
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Zone() == "" && h.addrs[a.Unmap()]
	}
	n, ok := normalName(host)
	if !ok {
		return false
	}
	if h.names[n] {
		return true
	}
	for i := strings.IndexByte(n, '.'); i >= 0; i = strings.IndexByte(n, '.') {
		n = n[i+1:]
		if h.wildcards[n] {
			return true
		}
	}
	return false
}

// HostChecker answers whether a URL's host is on egress.hosts (an *egress.Client): nil, or a
// *HostError.
type HostChecker interface {
	HostAllowed(raw string) error
}

// Warner logs a refusal.
type Warner interface {
	Warn(msg string, args ...any)
}

// CheckHost is a caller's pre-check of a URL's host (spec 0044): nil when h is nil or a typed nil
// (no list) or the host is listed; otherwise the *HostError, logged at warn with the caller's
// context (log nil: slog's default).
func CheckHost(h HostChecker, raw string, log Warner, args ...any) error {
	if h == nil {
		return nil
	}
	if v := reflect.ValueOf(h); v.Kind() == reflect.Pointer && v.IsNil() { // a nil *Client in the interface
		return nil
	}
	err := h.HostAllowed(raw)
	var he *HostError
	if errors.As(err, &he) {
		if l, ok := log.(*slog.Logger); log == nil || ok && l == nil {
			log = slog.Default()
		}
		log.Warn("egress: refused: the host is not on egress.hosts", append([]any{"host", he.Host}, args...)...)
	}
	return err
}

// HostAllowed reports whether egress may fetch from a URL's host by egress.hosts: nil when no list
// is configured or the host is on it, a *HostError otherwise. It logs nothing (its callers do).
func (c *Client) HostAllowed(raw string) error { return c.hosts.Check(raw) }

// Check is a URL's host against the list: nil when it is on it (or h is nil), a *HostError
// otherwise. A URL that does not parse has no host: refused, with Host "" (the raw text is never
// taken for a host, nor logged as one).
func (h *Hosts) Check(raw string) error {
	if h == nil {
		return nil
	}
	host := ""
	if u, err := url.Parse(raw); err == nil {
		host = u.Hostname()
	}
	if !h.allowed(host) {
		return &HostError{Host: host}
	}
	return nil
}

// HostsOn reports whether the client narrows by egress.hosts, and the list's entries.
func (c *Client) HostsOn() (bool, int) {
	if c.hosts == nil {
		return false, 0
	}
	return true, c.hosts.Len()
}
