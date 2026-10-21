package chartcheck

import (
	"bytes"
	"fmt"
	"net/netip"
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
	// never goes on the wire), each in its own family
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
	for _, r := range egress.Refused() {
		if r == netip.MustParsePrefix("::ffff:0:0/96") {
			continue
		}
		if !slices.ContainsFunc(except, func(e netip.Prefix) bool { return e.Bits() <= r.Bits() && e.Contains(r.Addr()) }) {
			t.Errorf("public https admits %s, which kista refuses", r)
		}
	}
}
