package e2e

import (
	"os"
	"strings"
	"testing"
)

// TestRemote runs DuckDB against a kista that is already running, as the chart's kind test
// (deploy/helm/kind/run.sh) deploys it behind an ingress, instead of one this harness starts:
//
//	KISTA_E2E_URL      the public URL, https://host[:port]; the same host serves plain http on port 80
//	KISTA_E2E_CHANNEL  <tenant>/<channel>, a signed channel serving httpfs (public) and
//	                   loadable_extension_demo (private) for this DuckDB
//	KISTA_E2E_CA       the CA bundle of the ingress certificate
//	KISTA_E2E_KEY      a file with the channel key's public key (PEM)
//	KISTA_E2E_TOKEN    a token with install on the channel
//
// It bootstraps httpfs over http, then installs the private extension over https: refused without
// the token, installed and loaded with it.
func TestRemote(t *testing.T) {
	b := needBuild(t)
	base := os.Getenv("KISTA_E2E_URL")
	if base == "" {
		t.Skip("KISTA_E2E_URL is not set (a running kista; see deploy/helm/kind/run.sh)")
	}
	env := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			t.Fatalf("%s is not set", name)
		}
		return v
	}
	channel, ca, token := env("KISTA_E2E_CHANNEL"), env("KISTA_E2E_CA"), env("KISTA_E2E_TOKEN")
	pemKey, err := os.ReadFile(env("KISTA_E2E_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	host, ok := strings.CutPrefix(base, "https://")
	if !ok || strings.ContainsAny(host, "/?#") {
		t.Fatalf("KISTA_E2E_URL: https://host[:port], not %q", base)
	}
	tenant, _, _ := strings.Cut(channel, "/")
	httpURL := "http://" + strings.Split(host, ":")[0] + "/" + channel
	httpsURL := base + "/" + channel

	install := func(withToken bool) []result {
		stmts := []string{
			createRepo("boot", httpURL, string(pemKey)),
			"INSTALL httpfs FROM boot",
			"LOAD httpfs FROM boot",
			"SET ca_cert_file = " + sqlString(ca),
			createRepo("r", httpsURL, string(pemKey)),
		}
		if withToken {
			stmts = append(stmts, "CREATE SECRET s (TYPE http, BEARER_TOKEN "+sqlString(token)+", SCOPE "+sqlString(base+"/"+tenant+"/")+")")
		}
		stmts = append(stmts, "INSTALL loadable_extension_demo FROM r", "LOAD loadable_extension_demo FROM r",
			"SELECT test_alias_hello()")
		return newSession(t, b, nil).exec(stmts...)
	}

	// a failure names the statement, which holds the token: never in a CI log
	allOK := func(res []result) {
		t.Helper()
		for _, r := range res {
			if !r.OK {
				redact := func(s string) string { return strings.ReplaceAll(s, token, "<token>") }
				t.Fatalf("%s\n  failed: %s", redact(r.Stmt), redact(r.Error))
			}
		}
	}
	res := install(false)
	allOK(res[:5])
	if res[5].OK {
		t.Fatal("installed the private extension without a token")
	}
	allOK(install(true))
}
