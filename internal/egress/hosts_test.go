package egress

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mustHosts(t *testing.T, entries ...string) *Hosts {
	t.Helper()
	h, err := NewHosts(entries)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Spec 0044: what an entry may be, in the form it is compared in.
func TestHostEntries(t *testing.T) {
	for e, want := range map[string]struct {
		kind int
		norm string
	}{
		"extensions.duckdb.org":      {entryName, "extensions.duckdb.org"},
		"Login.MicrosoftOnline.com.": {entryName, "login.microsoftonline.com"},
		"localhost":                  {entryName, "localhost"},
		"*.okta.com":                 {entryWildcard, "okta.com"},
		"*.Example.ORG.":             {entryWildcard, "example.org"},
		"bücher.example":             {entryName, "xn--bcher-kva.example"},
		"ｋｉｓｔａ.example":              {entryName, "kista.example"},
		"203.0.113.10":               {entryAddr, "203.0.113.10"},
		"2001:db8::1":                {entryAddr, "2001:db8::1"},
		"::ffff:203.0.113.10":        {entryAddr, "203.0.113.10"},
	} {
		kind, norm, err := parseHostEntry(e)
		if err != nil || kind != want.kind || norm != want.norm {
			t.Errorf("%q: %d %q %v, want %d %q", e, kind, norm, err, want.kind, want.norm)
		}
	}
	for e, want := range map[string]string{
		"":                    "empty",
		"*":                   "leftmost label",
		"*.com":               "two labels",
		"*.0.10":              "all digits",
		"a.*.b":               "leftmost label",
		"*x.com":              "leftmost label",
		"**.example.com":      "leftmost label",
		"https://example.com": "no scheme",
		"example.com/path":    "no path",
		"example.com:443":     "no port",
		"10.0.0.0/8":          "no CIDR",
		"[2001:db8::1]":       "without brackets",
		"1.2.3":               "all digits",
		"example.123":         "all digits",
		"fe80::1%eth0":        "zone",
		"bad_name.example":    "valid host name",
		"a..example":          "valid host name",
		".":                   "valid host name",
	} {
		if err := CheckHostEntry(e); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", e, err, want)
		}
	}
	if _, err := NewHosts([]string{"ok.example", "*"}); err == nil || !strings.Contains(err.Error(), `"*"`) {
		t.Errorf("NewHosts names the entry: %v", err)
	}
}

// Spec 0044: names as resolution sees them, wildcards below their domain only, addresses as
// addresses.
func TestHostMatching(t *testing.T) {
	h := mustHosts(t, "extensions.duckdb.org", "*.okta.com", "bücher.example", "203.0.113.10", "2001:db8::1")
	for host, want := range map[string]bool{
		"extensions.duckdb.org":   true,
		"EXTENSIONS.duckdb.org":   true,
		"extensions.duckdb.org.":  true,
		"ｅｘｔｅｎｓｉｏｎｓ.duckdb.org":   true,
		"bücher.example":          true,
		"xn--bcher-kva.example":   true,
		"duckdb.org":              false,
		"x.extensions.duckdb.org": false,
		"acme.okta.com":           true,
		"a.b.acme.okta.com":       true,
		"okta.com":                false,
		"evilokta.com":            false,
		"okta.com.evil.example":   false,
		"bad_name.okta.com":       false, // fails the IDNA mapping: refused
		"203.0.113.10":            true,
		"::ffff:203.0.113.10":     true,
		"::ffff:cb00:710a":        true,
		"203.0.113.11":            false,
		"2001:db8::1":             true,
		"2001:0db8:0:0::1":        true,
		"":                        false,
	} {
		if got := h.allowed(host); got != want {
			t.Errorf("%q: %v, want %v", host, got, want)
		}
	}
	// an address host matches address entries only, a name host name entries only
	byName := mustHosts(t, "203.0.113.10.example", "*.addr.example")
	if byName.allowed("203.0.113.10") {
		t.Error("an address matched a name entry")
	}
	if mustHosts(t, "127.0.0.1").allowed("localhost") {
		t.Error("a name matched an address entry")
	}
	// nil narrows nothing; an empty list refuses everything
	if err := (&Client{}).HostAllowed("https://anything.example/x"); err != nil {
		t.Errorf("no list: %v", err)
	}
	empty := &Client{hosts: mustHosts(t)}
	var he *HostError
	if err := empty.HostAllowed("https://extensions.duckdb.org/x"); !errors.As(err, &he) || he.Host != "extensions.duckdb.org" ||
		!errors.Is(err, ErrHostNotAllowed) || !errors.Is(err, ErrRefused) {
		t.Errorf("an empty list: %v", err)
	}
	if on, n := empty.HostsOn(); !on || n != 0 {
		t.Errorf("an empty list is on: %v %d", on, n)
	}
	if got := (&HostError{Host: "x.example"}).Error(); got != "egress: refused: x.example is not on this server's egress host list" {
		t.Errorf("the message: %q", got)
	}
}

// Spec 0044: a URL that does not parse is refused with no host (never its raw text as one, even when
// that text is a listed name); a typed-nil checker is no list, as a nil one is.
func TestHostCheckEdges(t *testing.T) {
	h := mustHosts(t, "listed.example")
	var he *HostError
	for _, raw := range []string{"https://listed.example/%zz", "listed.example\x7f://", "https://[::1"} {
		if err := h.Check(raw); !errors.As(err, &he) || he.Host != "" {
			t.Errorf("%q: %v", raw, err)
		}
	}
	if err := h.Check("listed.example"); !errors.As(err, &he) || he.Host != "" {
		t.Errorf("a bare name is a URL with no host: %v", err)
	}
	var none *Client
	for name, c := range map[string]HostChecker{"nil": nil, "a typed nil": none} {
		if err := CheckHost(c, "https://anything.example/x", nil); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := CheckHost(&Client{hosts: h}, "https://other.example/x", nil); !errors.As(err, &he) || he.Host != "other.example" {
		t.Errorf("a list: %v", err)
	}
}

// Spec 0044: a host off the list is refused before anything happens (no resolution, no
// connection, no CONNECT), by checkURL and, when it is bypassed, by dial; a listed host still meets
// every address rule.
func TestHostsBeforeAddresses(t *testing.T) {
	ctx := context.Background()
	srv, hp := newTLS(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{}") })
	port := netip.MustParseAddrPort(hp).Port()
	var resolved atomic.Int32
	addrs := map[string]string{"idp.example": "127.0.0.1", "public.example": "8.8.8.8", "meta.example": "169.254.169.254",
		"inside.example": "10.1.2.3", "unlisted.example": "127.0.0.1"}
	resolve := func(_ context.Context, host string) ([]netip.Addr, error) {
		resolved.Add(1)
		if a, ok := addrs[host]; ok {
			return []netip.Addr{netip.MustParseAddr(a)}, nil
		}
		return nil, errors.New("no such host")
	}
	hosts := mustHosts(t, "idp.example", "public.example", "meta.example", "inside.example")
	c := client(t, srv, Config{Allow: loopbackAllow(port), Resolve: resolve, Hosts: hosts})
	c.hc.Transport.(*http.Transport).TLSClientConfig.ServerName = "example.com"
	base := "https://idp.example:" + itoa(port)
	if _, _, err := c.Get(ctx, base+"/x"); err != nil {
		t.Fatalf("a listed host on an allowed address: %v", err)
	}
	// refused on the name: never resolved, the same *HostError on every method, as HostAllowed says
	resolved.Store(0)
	refused := "https://unlisted.example:" + itoa(port) + "/x"
	check := func(name string, err error) {
		t.Helper()
		var he *HostError
		if !errors.As(err, &he) || he.Host != "unlisted.example" {
			t.Errorf("%s: %v", name, err)
		}
	}
	check("HostAllowed", c.HostAllowed(refused))
	_, _, err := c.Get(ctx, refused)
	check("Get", err)
	_, err = c.Post(ctx, refused, "application/json", nil, nil)
	check("Post", err)
	_, _, err = c.PostForm(ctx, refused, url.Values{})
	check("PostForm", err)
	_, err = c.PostJSON(ctx, refused, nil, nil, 0)
	check("PostJSON", err)
	_, err = c.Download(ctx, refused, DownloadOptions{MaxBytes: 10, Timeout: time.Second}, &bytes.Buffer{})
	check("Download", err)
	// dial on its own, as the transport calls it (checkURL bypassed)
	_, err = c.hc.Get(refused)
	check("the transport", err)
	if _, err := c.dial(ctx, "tcp", net.JoinHostPort("unlisted.example", itoa(port))); err == nil {
		t.Error("dial connected to an unlisted host")
	}
	if n := resolved.Load(); n != 0 {
		t.Errorf("an unlisted host was resolved %d times", n)
	}
	// a listed host still meets the address rules: metadata and private addresses, other ports
	for name, u := range map[string]string{
		"metadata":             "https://meta.example/x",
		"private, not allowed": "https://inside.example/x",
		"a public port 8443":   "https://public.example:8443/x",
	} {
		_, _, err := c.Get(ctx, u)
		var he *HostError
		if !errors.Is(err, ErrRefused) || errors.As(err, &he) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a private host needs both its name on the list and its network in egress.allow
	allowInside := []Allow{{Prefix: netip.MustParsePrefix("10.1.0.0/16")}}
	for name, tc := range map[string]struct {
		hosts *Hosts
		allow []Allow
		pass  bool
	}{
		"both":          {hosts, allowInside, true},
		"the name only": {hosts, nil, false},
		"the CIDR only": {mustHosts(t, "idp.example"), allowInside, false},
	} {
		cc, _ := New(Config{Allow: tc.allow, Resolve: resolve, Hosts: tc.hosts, Timeout: 200 * time.Millisecond})
		_, err := cc.dial(ctx, "tcp", "inside.example:443")
		var he *HostError
		switch {
		case tc.pass && errors.Is(err, ErrRefused):
			t.Errorf("%s: refused: %v", name, err)
		case !tc.pass && !errors.Is(err, ErrRefused):
			t.Errorf("%s: not refused: %v", name, err)
		case name == "the CIDR only" && !errors.As(err, &he):
			t.Errorf("%s: not refused by name: %v", name, err)
		}
	}
}

// Spec 0044: through an explicit proxy, a refused host is never asked for.
func TestHostsNoConnect(t *testing.T) {
	var connects atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		connects.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	resolve := func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	c, err := New(Config{Proxy: pu, Resolve: resolve, Hosts: mustHosts(t, "listed.example")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.hc.Get("https://unlisted.example/x"); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("through the proxy: %v", err)
	}
	if n := connects.Load(); n != 0 {
		t.Fatalf("the proxy was asked %d times for an unlisted host", n)
	}
	// a listed host is asked for (the proxy refuses it here)
	if _, _, err := c.Get(context.Background(), "https://listed.example/x"); !errors.Is(err, ErrConnect) || connects.Load() != 1 {
		t.Fatalf("a listed host through the proxy: %v (%d)", err, connects.Load())
	}
}
