package audit

import (
	"strings"
	"testing"
	"time"
)

func TestReduceClient(t *testing.T) {
	for _, c := range []struct{ addr, mode, want string }{
		{"203.0.113.77", ClientTruncated, "203.0.113.0/24"},
		{"203.0.113.77:443", ClientTruncated, "203.0.113.0/24"},
		{"::ffff:203.0.113.77", ClientTruncated, "203.0.113.0/24"},
		{"2001:db8:1:2:3::4", ClientTruncated, "2001:db8:1::/48"},
		{"2002:cb00:714d::1", ClientTruncated, "203.0.113.0/24"},  // 6to4 embeds 203.0.113.77
		{"64:ff9b::cb00:714d", ClientTruncated, "203.0.113.0/24"}, // NAT64
		{"203.0.113.77", ClientFull, "203.0.113.77"},
		{"203.0.113.77", ClientNone, ""},
		{"not an address", ClientTruncated, ""},
	} {
		if got := ReduceClient(c.addr, c.mode); got != c.want {
			t.Errorf("%+v: %q", c, got)
		}
	}
}

func TestData(t *testing.T) {
	if _, err := Data("grant.add", map[string]any{"token": "x"}); err == nil {
		t.Error("a field outside the catalogue")
	}
	if _, err := Data("nope", nil); err == nil {
		t.Error("an unknown kind")
	}
	big := strings.Repeat("x", MaxData)
	d, err := Data("block.add", map[string]any{"body_hash": "h", "reason": big})
	if err != nil || len(d) > MaxData || !strings.Contains(d, `"truncated":true`) || !strings.Contains(d, `"body_hash":"h"`) {
		t.Fatalf("truncated: %d %v", len(d), err)
	}
	for _, k := range Kinds() {
		if k.Subject == "" {
			t.Errorf("%s has no subject form", k.Kind)
		}
	}
}

func TestDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "0s", time.Second: "1s", time.Minute: "1m", 90 * time.Second: "1m30s",
		24 * time.Hour: "24h", 168 * time.Hour: "168h", 90 * time.Minute: "1h30m", 8760 * time.Hour: "8760h"} {
		if got := Duration(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}
