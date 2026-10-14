// Package chartcheck checks that the configuration the Helm chart renders (deploy/helm/kista) is one
// kista accepts: each values file in deploy/helm/tests/pass and the kind test's, rendered with helm, loaded by
// config.Load. Skipped without helm on the PATH, unless KISTA_TEST_REQUIRE_HELM is set.
package chartcheck

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"go.yaml.in/yaml/v3"
)

func TestRenderedConfig(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("KISTA_TEST_REQUIRE_HELM") != "" {
			t.Fatal("helm is not on the PATH")
		}
		t.Skip("helm is not on the PATH")
	}
	files, err := filepath.Glob("../tests/pass/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no values files: %v", err)
	}
	files = append(files, "../kind/values.yaml")
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			out, err := exec.Command(helm, "template", "t", "../kista", "-f", f, "--show-only", "templates/configmap.yaml").CombinedOutput()
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, out)
			}
			var cm struct {
				Data map[string]string `yaml:"data"`
			}
			if err := yaml.NewDecoder(bytes.NewReader(out)).Decode(&cm); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "kista.yaml")
			if err := os.WriteFile(path, []byte(cm.Data["kista.yaml"]), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := config.Load(path, nil); err != nil {
				t.Fatalf("kista refuses the rendered config: %v\n%s", err, cm.Data["kista.yaml"])
			}
		})
	}
}
