package chartcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// helmRun runs helm template on the chart with args; skipped without helm, unless
// KISTA_TEST_REQUIRE_HELM is set.
func helmRun(t *testing.T, args ...string) []byte {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("KISTA_TEST_REQUIRE_HELM") != "" {
			t.Fatal("helm is not on the PATH")
		}
		t.Skip("helm is not on the PATH")
	}
	out, err := exec.Command(helm, append([]string{"template", "t", "../kista"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", args, err, out)
	}
	return out
}

// configTemplate (spec 0025): off, config is taken as written; on, each string holding "{{" is
// rendered by tpl, within that string only.
func TestConfigTemplate(t *testing.T) {
	render := func(values string, set ...string) map[string]any {
		t.Helper()
		args := []string{"-f", "../tests/pass/config-template.yaml", "--show-only", "templates/configmap.yaml"}
		if values != "" {
			f := filepath.Join(t.TempDir(), "values.yaml")
			if err := os.WriteFile(f, []byte(values), 0o600); err != nil {
				t.Fatal(err)
			}
			args = append(args, "-f", f)
		}
		for _, s := range set {
			args = append(args, "--set", s)
		}
		var cm struct {
			Data map[string]string `yaml:"data"`
		}
		if err := yaml.Unmarshal(helmRun(t, args...), &cm); err != nil {
			t.Fatal(err)
		}
		var c map[string]any
		if err := yaml.Unmarshal([]byte(cm.Data["kista.yaml"]), &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	dsn := func(c map[string]any) string { return c["store"].(map[string]any)["dsn"].(string) }
	bucket := func(c map[string]any) string {
		d := c["blob"].(map[string]any)["domains"].([]any)[0].(map[string]any)
		return d["s3"].(map[string]any)["bucket"].(string)
	}

	if got := dsn(render("")); got != "postgres://kista@db.platform.svc:5432/kista?sslmode=verify-full" {
		t.Errorf("on: dsn %q", got)
	}
	off := render("", "configTemplate=false")
	if got := dsn(off); got != "postgres://kista@{{ .Values.global.database.host }}:5432/{{ .Values.global.database.name }}?sslmode=verify-full" {
		t.Errorf("off: dsn %q", got)
	}
	if got := bucket(off); got != "{{ .Release.Name }}-bodies" {
		t.Errorf("off: bucket %q", got)
	}

	// a value cannot add keys: it stays inside its string
	inj := render("global: {database: {host: \"db\\nprofile: dev\\negress: {allow: [{cidr: 0.0.0.0/0}]}\"}}\n")
	for _, k := range []string{"profile", "egress"} {
		if _, ok := inj[k]; ok {
			t.Errorf("a global value added %s: %v", k, inj)
		}
	}
	if got := dsn(inj); !strings.Contains(got, "db\nprofile: dev") {
		t.Errorf("dsn %q", got)
	}

	// "{{" written as a template stays literal
	esc := render(`config: {blob: {domains: [{name: default, kind: s3, s3: {endpoint: https://s3.example.com, bucket: '{{ "{{" }}x', region: r, lookup: path, sse: none}}]}}`)
	if got := bucket(esc); got != "{{x" {
		t.Errorf("escaped: bucket %q", got)
	}
}
