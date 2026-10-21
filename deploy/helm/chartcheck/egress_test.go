package chartcheck

import (
	"bytes"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"go.yaml.in/yaml/v3"
)

type peer struct {
	IPBlock *struct {
		CIDR   string   `yaml:"cidr"`
		Except []string `yaml:"except"`
	} `yaml:"ipBlock"`
	NamespaceSelector map[string]any `yaml:"namespaceSelector"`
	PodSelector       map[string]any `yaml:"podSelector"`
}

type rule struct {
	To    []peer `yaml:"to"`
	Ports []struct {
		Port     int    `yaml:"port"`
		Protocol string `yaml:"protocol"`
	} `yaml:"ports"`
}

type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		PodSelector struct {
			MatchLabels map[string]string `yaml:"matchLabels"`
		} `yaml:"podSelector"`
		PolicyTypes []string `yaml:"policyTypes"`
		Egress      []rule   `yaml:"egress"`
		// a Deployment's pods
		Template struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
		} `yaml:"template"`
		// a CronJob's pods
		JobTemplate struct {
			Spec struct {
				Template struct {
					Metadata struct {
						Labels map[string]string `yaml:"labels"`
					} `yaml:"metadata"`
				} `yaml:"template"`
			} `yaml:"spec"`
		} `yaml:"jobTemplate"`
	} `yaml:"spec"`
}

func helmTemplate(t *testing.T, args ...string) []object {
	t.Helper()
	var objs []object
	dec := yaml.NewDecoder(bytes.NewReader(helmRun(t, args...)))
	for {
		var o object
		if err := dec.Decode(&o); err != nil {
			break
		}
		if o.Kind != "" {
			objs = append(objs, o)
		}
	}
	return objs
}

// what a rule admits, as "peer port/protocol" lines
func admits(r rule) []string {
	var out []string
	for _, p := range r.To {
		var who string
		switch {
		case p.IPBlock != nil:
			who = p.IPBlock.CIDR
		default:
			who = fmt.Sprint(p.NamespaceSelector, p.PodSelector)
		}
		for _, port := range r.Ports {
			out = append(out, fmt.Sprintf("%s %d/%s", who, port.Port, port.Protocol))
		}
	}
	return out
}

// The egress NetworkPolicy (spec 0025): off by default; on, it selects every pod of the release and
// admits DNS, public https outside kista's refused ranges, what the config allows, the identity's
// metadata endpoint and the operator's rules, nothing else.
func TestEgressPolicy(t *testing.T) {
	for _, o := range helmTemplate(t, "-f", "../tests/pass/postgres-s3.yaml") {
		if o.Kind == "NetworkPolicy" && strings.HasSuffix(o.Metadata.Name, "-egress") {
			t.Fatalf("an egress NetworkPolicy by default: %s", o.Metadata.Name)
		}
	}

	objs := helmTemplate(t, "-f", "../tests/pass/egress.yaml")
	var np *object
	var pods []map[string]string
	for i, o := range objs {
		switch o.Kind {
		case "NetworkPolicy":
			if o.Metadata.Name == "t-kista-egress" {
				np = &objs[i]
			} else if !slices.Equal(o.Spec.PolicyTypes, []string{"Ingress"}) {
				t.Errorf("%s: policyTypes %v", o.Metadata.Name, o.Spec.PolicyTypes)
			}
		case "Deployment":
			pods = append(pods, o.Spec.Template.Metadata.Labels)
		case "CronJob":
			pods = append(pods, o.Spec.JobTemplate.Spec.Template.Metadata.Labels)
		}
	}
	if np == nil {
		t.Fatal("no egress NetworkPolicy")
	}
	if !slices.Equal(np.Spec.PolicyTypes, []string{"Egress"}) {
		t.Errorf("policyTypes %v", np.Spec.PolicyTypes)
	}
	if len(pods) != 2 {
		t.Fatalf("want the server's and an admin job's pods, got %v", pods)
	}
	for _, labels := range pods {
		for k, v := range np.Spec.PodSelector.MatchLabels {
			if labels[k] != v {
				t.Errorf("the policy (%v) does not select pods labelled %v", np.Spec.PodSelector.MatchLabels, labels)
			}
		}
	}

	var got []string
	for _, r := range np.Spec.Egress {
		got = append(got, admits(r)...)
	}
	want := []string{
		"map[matchLabels:map[kubernetes.io/metadata.name:kube-system]] map[matchLabels:map[k8s-app:kube-dns]] 53/UDP",
		"map[matchLabels:map[kubernetes.io/metadata.name:kube-system]] map[matchLabels:map[k8s-app:kube-dns]] 53/TCP",
		"map[matchLabels:map[kubernetes.io/metadata.name:kube-system]] map[matchLabels:map[k8s-app:kube-dns]] 8053/UDP",
		"map[matchLabels:map[kubernetes.io/metadata.name:kube-system]] map[matchLabels:map[k8s-app:kube-dns]] 8053/TCP",
		"0.0.0.0/0 443/TCP",
		"::/0 443/TCP",
		"10.30.0.0/24 443/TCP",     // egress.allow, its default port
		"fd00:30::/64 443/TCP",     // egress.allow with ports
		"fd00:30::/64 8443/TCP",    //
		"169.254.170.23/32 80/TCP", // cloudMetadata aws-pod-identity
		"fd00:ec2::23/128 80/TCP",  //
		"10.20.0.5/32 5432/TCP",    // extraRules as given
		"map[matchLabels:map[kubernetes.io/metadata.name:openbao]] map[matchLabels:map[app.kubernetes.io/name:openbao]] 8200/TCP",
		"map[matchLabels:map[kubernetes.io/metadata.name:observability]] map[matchLabels:map[app.kubernetes.io/name:otel-collector]] 4318/TCP",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the policy admits\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// public https leaves out every range kista's own egress refuses (but IPv4-mapped IPv6, which
	// never goes on the wire), each in its own family, and the NAT64 form of each IPv4 one: kista
	// decides on the IPv4 address inside 64:ff9b::/96
	var except []netip.Prefix
	for _, p := range np.Spec.Egress[1].To {
		v6 := strings.Contains(p.IPBlock.CIDR, ":")
		for _, e := range p.IPBlock.Except {
			pf := netip.MustParsePrefix(e)
			if pf.Addr().Is6() != v6 {
				t.Errorf("%s: except %s is of the other family", p.IPBlock.CIDR, e)
			}
			except = append(except, pf)
		}
	}
	covered := func(r netip.Prefix) bool {
		return slices.ContainsFunc(except, func(e netip.Prefix) bool { return e.Bits() <= r.Bits() && e.Contains(r.Addr()) })
	}
	for _, r := range egress.Refused() {
		if r == netip.MustParsePrefix("::ffff:0:0/96") {
			continue
		}
		if !covered(r) {
			t.Errorf("public https admits %s, which kista refuses", r)
		}
		if r.Addr().Is4() {
			b := netip.MustParseAddr("64:ff9b::").As16()
			a := r.Addr().As4()
			copy(b[12:], a[:])
			if nat64 := netip.PrefixFrom(netip.AddrFrom16(b), 96+r.Bits()); !covered(nat64) {
				t.Errorf("public https admits %s, the NAT64 form of %s, which kista refuses", nat64, r)
			}
		}
	}
}

// The networks the configuration allows lose the ranges kista never reaches, even when allowed;
// an allowed network inside one of them is left out. A proxy at an IP address gets its rule, and
// DNS that is no pod its ipBlock.
func TestEgressAllowed(t *testing.T) {
	f := filepath.Join(t.TempDir(), "values.yaml")
	values := `config:
  egress:
    proxy: http://[fd00::5]:3128
    allow:
      - {cidr: 0.0.0.0/0}
      - {cidr: "::/0"}
      - {cidr: 169.254.169.254/32, ports: [80]}
      - {cidr: 168.0.0.0/6}
      - {cidr: "fd00::/8", ports: [8443]}
      - {cidr: "FE80::1/128"}
      - {cidr: 10.30.0.0/24}
networkPolicy:
  egress:
    public: {enabled: false}
    cloudMetadata: [aws-pod-identity]
    dns:
      ipBlocks: [{cidr: 169.254.20.10/32}, {cidr: 10.96.0.10/32, ports: [53]}]
`
	if err := os.WriteFile(f, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	var np *object
	objs := helmTemplate(t, "-f", "../tests/pass/egress-sqlite.yaml", "-f", f)
	for i, o := range objs {
		if o.Kind == "NetworkPolicy" && o.Metadata.Name == "t-kista-egress" {
			np = &objs[i]
		}
	}
	if np == nil {
		t.Fatal("no egress NetworkPolicy")
	}
	var got []string
	for _, r := range np.Spec.Egress[1:] { // after the DNS pods
		for _, line := range admits(r) {
			if p := r.To[0].IPBlock; p != nil && len(p.Except) > 0 {
				line += " except " + strings.Join(p.Except, ",")
			}
			got = append(got, line)
		}
	}
	never := func(v6 bool) string {
		var out []string
		for _, n := range egress.Never() {
			if n.Addr().Is6() == v6 {
				out = append(out, n.String())
			}
		}
		return strings.Join(out, ",")
	}
	want := []string{
		"169.254.20.10/32 53/UDP", "169.254.20.10/32 53/TCP", "169.254.20.10/32 8053/UDP", "169.254.20.10/32 8053/TCP",
		"10.96.0.10/32 53/UDP", "10.96.0.10/32 53/TCP",
		"0.0.0.0/0 443/TCP except " + never(false),
		"::/0 443/TCP except " + never(true),
		// 169.254.169.254/32 and FE80::1/128: inside what kista never reaches, left out
		"168.0.0.0/6 443/TCP except 169.254.0.0/16,168.63.129.16/32",
		"fd00::/8 8443/TCP except fd00:ec2::/112",
		"10.30.0.0/24 443/TCP",
		"fd00::5/128 3128/TCP", // egress.proxy
		"169.254.170.23/32 80/TCP",
		"fd00:ec2::23/128 80/TCP",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the policy admits\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
