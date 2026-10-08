package egress

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestCheck(t *testing.T) {
	c := &Client{allow: []Allow{{Prefix: netip.MustParsePrefix("10.1.0.0/16"), Ports: []uint16{8443}},
		{Prefix: netip.MustParsePrefix("169.254.169.254/32")}, {Prefix: netip.MustParsePrefix("192.168.5.0/24")}}}
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "0.1.2.3": false, "10.0.0.1": false, "172.16.0.1": false, "192.168.1.1": false,
		"100.64.0.1": false, "169.254.1.1": false, "198.18.0.1": false, "192.0.0.1": false, "240.0.0.1": false,
		"224.0.0.1": false, "255.255.255.255": false, "::1": false, "::": false, "fe80::1": false, "fc00::1": false,
		"fec0::1": false, "ff02::1": false, "2001:db8::1": false,
		"::ffff:127.0.0.1": false, "::127.0.0.1": false, "64:ff9b::7f00:1": false, "64:ff9b::808:808": true,
		"64:ff9b:1::1": false, "2002:7f00:1::": false, "2002:808:808::": false, "2001:0:4136:e378:8000:63bf:3fff:fdd2": false,
		"168.63.129.16": false, "169.254.169.254": false, "100.100.100.200": false, "fd00:ec2::254": false,
		"192.168.5.7":     true, // allowlisted
		"fd00:ec2::254%1": false, "fc00::1%1": false, "2606:4700::1111%1": false, "fe80::1%eth0": false,
		"::ffff:0:7f00:1": false, "169.254.169.253": false, "fd00:ec2::253": false, "64:ff9b:1::a9fe:a9fe": false,
	} {
		if got := c.check(netip.MustParseAddr(addr), 443); got != want {
			t.Errorf("%s:443: %v, want %v", addr, got, want)
		}
	}
	for _, tc := range []struct {
		addr string
		port uint16
		want bool
	}{{"10.1.2.3", 8443, true}, {"10.1.2.3", 443, false}, {"8.8.8.8", 8443, false}, {"192.168.5.7", 8443, false}} {
		if got := c.check(netip.MustParseAddr(tc.addr), tc.port); got != tc.want {
			t.Errorf("%s:%d: %v, want %v", tc.addr, tc.port, got, tc.want)
		}
	}
}

func newTLS(t *testing.T, h http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv, srv.Listener.Addr().(*net.TCPAddr).AddrPort().String()
}

func client(t *testing.T, srv *httptest.Server, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.hc.Transport.(*http.Transport).TLSClientConfig.RootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	return c
}

func loopbackAllow(port uint16) []Allow {
	return []Allow{{Prefix: netip.MustParsePrefix("127.0.0.1/32"), Ports: []uint16{port}}}
}

func TestGet(t *testing.T) {
	ctx := context.Background()
	srv, hp := newTLS(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "/big":
			_, _ = io.WriteString(w, strings.Repeat("x", 2048))
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			http.Error(w, "secret body", http.StatusTeapot)
		}
	})
	port := netip.MustParseAddrPort(hp).Port()
	// the IdP's host name resolves to the loopback test server
	resolve := func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "idp.example":
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		case "mixed.example":
			return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}, nil
		}
		return nil, errors.New("no such host")
	}
	c := client(t, srv, Config{Allow: loopbackAllow(port), MaxBytes: 1024, Resolve: resolve, OwnHost: "kista.example"})

	// the test certificate is for 127.0.0.1/example.com; dial by address with the host checked
	if b, _, err := c.Get(ctx, "https://127.0.0.1:"+itoa(port)+"/ok"); err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("ok: %q %v", b, err)
	}
	for name, tc := range map[string]struct {
		url  string
		want error
	}{
		"too large":        {"https://127.0.0.1:" + itoa(port) + "/big", ErrTooLarge},
		"redirect":         {"https://127.0.0.1:" + itoa(port) + "/redirect", ErrStatus},
		"status":           {"https://127.0.0.1:" + itoa(port) + "/teapot", ErrStatus},
		"http":             {"http://127.0.0.1:" + itoa(port) + "/ok", ErrRefused},
		"userinfo":         {"https://u:p@127.0.0.1:" + itoa(port) + "/ok", ErrRefused},
		"own host":         {"https://kista.example/x", ErrRefused},
		"not allowlisted":  {"https://127.0.0.2:" + itoa(port) + "/ok", ErrRefused},
		"mixed resolution": {"https://mixed.example/x", ErrRefused},
		"unresolvable":     {"https://nowhere.example/x", ErrRefused},
		"zoned":            {"https://[fd00:ec2::254%251]/latest", ErrRefused},
		"own host, port":   {"https://kista.example:443/x", ErrRefused},
		"own host, dot":    {"https://kista.example./x", ErrRefused},
		"own host, idna":   {"https://ｋｉｓｔａ.example/x", ErrRefused},
		"file":             {"file:///etc/passwd", ErrRefused},
	} {
		_, _, err := c.Get(ctx, tc.url)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: the error carries the body: %v", name, err)
		}
	}
	// a client without the allowlist refuses the loopback server
	strict := client(t, srv, Config{Resolve: resolve})
	if _, _, err := strict.Get(ctx, "https://127.0.0.1:"+itoa(port)+"/ok"); !errors.Is(err, ErrRefused) {
		t.Fatalf("loopback without an allowlist: %v", err)
	}
	// http on loopback only with the development switch
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{}") }))
	defer plain.Close()
	dev, _ := New(Config{AllowLoopbackHTTP: true})
	if _, _, err := dev.Get(ctx, plain.URL+"/x"); err != nil {
		t.Fatalf("http on loopback in dev: %v", err)
	}
}

// Through an explicit proxy, kista asks for CONNECT to the address it checked, not to a name the
// proxy would resolve.
func TestProxyConnectsToCheckedAddress(t *testing.T) {
	ctx := context.Background()
	srv, hp := newTLS(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{}") })
	port := netip.MustParseAddrPort(hp).Port()
	var mu sync.Mutex
	var targets []string
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	go func() {
		for {
			c, err := proxy.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				mu.Lock()
				targets = append(targets, req.Host)
				mu.Unlock()
				up, err := net.Dial("tcp", req.Host)
				if err != nil {
					return
				}
				defer up.Close()
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\n")
				go func() { _, _ = io.Copy(up, c) }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
	resolve := func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	pu, _ := url.Parse("http://" + proxy.Addr().String())
	c := client(t, srv, Config{Allow: loopbackAllow(port), Proxy: pu, Resolve: resolve})
	c.hc.Transport.(*http.Transport).TLSClientConfig.ServerName = "example.com"
	if _, _, err := c.Get(ctx, "https://idp.example:"+itoa(port)+"/x"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(targets) != 1 || targets[0] != "127.0.0.1:"+itoa(port) {
		t.Fatalf("CONNECT targets: %v", targets)
	}
}

func itoa(p uint16) string { return strconv.Itoa(int(p)) }
