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

// The spool emptyDir's limit and the pod's ephemeral-storage request: the server's ingests, or
// spool.sizeLimit, plus spool.importHeadroom (spec 0014).
func TestSpoolSize(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("KISTA_TEST_REQUIRE_HELM") != "" {
			t.Fatal("helm is not on the PATH")
		}
		t.Skip("helm is not on the PATH")
	}
	for _, c := range []struct {
		set    []string
		values string // a values file: its numbers are float64 to Helm
		want   string
	}{
		{nil, "", "34816Mi"}, // (2 × 8 + 1) × 2GiB
		{[]string{"spool.importHeadroom=2Gi"}, "", "36864Mi"},
		{[]string{"spool.importHeadroom=0"}, "", "34816Mi"},
		{[]string{"spool.importHeadroom=512Mi", "config.blob.max_body=256MiB", "config.blob.max_ingests=1"}, "", "1280Mi"},
		{[]string{"spool.sizeLimit=10G"}, "", "10G"},
		{[]string{"spool.sizeLimit=10Gi", "spool.importHeadroom=2Gi"}, "", "12288Mi"},
		{[]string{"spool.sizeLimit=1Ti", "spool.importHeadroom=1048576"}, "", "1048577Mi"},
		{nil, "spool: {importHeadroom: 1073741824}", "35840Mi"},
		{nil, "spool: {sizeLimit: 10Gi, importHeadroom: 0}", "10Gi"},
	} {
		args := []string{"template", "t", "../kista", "-f", "../tests/pass/postgres-s3.yaml", "--show-only", "templates/deployment.yaml"}
		if c.values != "" {
			f := filepath.Join(t.TempDir(), "values.yaml")
			if err := os.WriteFile(f, []byte(c.values), 0o600); err != nil {
				t.Fatal(err)
			}
			args = append(args, "-f", f)
		}
		for _, s := range c.set {
			args = append(args, "--set", s)
		}
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v %s: helm template: %v\n%s", c.set, c.values, err, out)
		}
		var d struct {
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Resources struct {
								Requests map[string]string `yaml:"requests"`
							} `yaml:"resources"`
						} `yaml:"containers"`
						Volumes []struct {
							Name     string `yaml:"name"`
							EmptyDir *struct {
								SizeLimit string `yaml:"sizeLimit"`
							} `yaml:"emptyDir"`
						} `yaml:"volumes"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.NewDecoder(bytes.NewReader(out)).Decode(&d); err != nil {
			t.Fatal(err)
		}
		limit := ""
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.Name == "tmp" && v.EmptyDir != nil {
				limit = v.EmptyDir.SizeLimit
			}
		}
		if limit != c.want || len(d.Spec.Template.Spec.Containers) == 0 ||
			d.Spec.Template.Spec.Containers[0].Resources.Requests["ephemeral-storage"] != c.want {
			t.Errorf("%v %s: limit %q, requests %v; want %s", c.set, c.values, limit, d.Spec.Template.Spec.Containers, c.want)
		}
	}
}
