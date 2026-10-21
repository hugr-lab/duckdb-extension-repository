package config

import (
	"strings"
	"testing"
)

func TestKubeSecretSource(t *testing.T) {
	ok := base + `
signers:
  file_dir: /etc/kista/keys
  sources:
    - name: k8s
      kind: kubesecret
      allow: ["ext-"]
      kubesecret: { dir: /etc/kista/signing/prod, etcd_encryption_at_rest: true }
`
	cfg, err := Load(write(t, ok), nil)
	if err != nil {
		t.Fatal(err)
	}
	if k := cfg.Signers.Sources[0].KubeSecret; k == nil || k.Dir != "/etc/kista/signing/prod" || !k.EtcdEncryptionAtRest {
		t.Fatalf("%+v", cfg.Signers.Sources[0])
	}
	bad := map[string]string{
		"no acknowledgment":    strings.Replace(ok, "etcd_encryption_at_rest: true", "etcd_encryption_at_rest: false", 1),
		"acknowledgment unset": strings.Replace(ok, ", etcd_encryption_at_rest: true", "", 1),
		"no block":             strings.Replace(ok, "      kubesecret: { dir: /etc/kista/signing/prod, etcd_encryption_at_rest: true }\n", "", 1),
		"two blocks": strings.Replace(ok, "      allow:", `      vault: { address: "https://b:8200", mount: t, software_keys: true, auth: { kind: token_file, token_file: /t } }
      allow:`, 1),
		"identity":            strings.Replace(ok, "      allow:", "      identity: { kind: managed }\n      allow:", 1),
		"reserved name":       strings.Replace(ok, "name: k8s", "name: kubesecret", 1),
		"relative dir":        strings.Replace(ok, "dir: /etc/kista/signing/prod", "dir: signing/prod", 1),
		"unclean dir":         strings.Replace(ok, "dir: /etc/kista/signing/prod", "dir: /etc/kista/signing/../prod", 1),
		"trailing slash":      strings.Replace(ok, "dir: /etc/kista/signing/prod", "dir: /etc/kista/signing/prod/", 1),
		"root dir":            strings.Replace(ok, "dir: /etc/kista/signing/prod", "dir: /", 1),
		"empty dir":           strings.Replace(ok, "dir: /etc/kista/signing/prod", `dir: ""`, 1),
		"the file_dir":        strings.Replace(ok, "dir: /etc/kista/signing/prod", "dir: /etc/kista/keys", 1),
		"under file_dir":      strings.Replace(ok, "dir: /etc/kista/signing/prod", "dir: /etc/kista/keys/k8s", 1),
		"over file_dir":       strings.Replace(ok, "dir: /etc/kista/signing/prod", "dir: /etc/kista", 1),
		"the password inside": strings.Replace(ok, "kind: sqlite", "kind: sqlite\n  login: { password_file: /etc/kista/signing/prod/password }", 1),
		"a token inside": ok + `    - name: bao
      kind: vault
      allow: ["*"]
      vault: { address: "https://b:8200", mount: t, software_keys: true, auth: { kind: token_file, token_file: /etc/kista/signing/prod/token } }
`,
		"two sources on one volume": ok + `    - name: k8s2
      kind: kubesecret
      allow: ["*"]
      kubesecret: { dir: /etc/kista/signing/prod, etcd_encryption_at_rest: true }
`,
		"nested sources": ok + `    - name: k8s2
      kind: kubesecret
      allow: ["*"]
      kubesecret: { dir: /etc/kista/signing, etcd_encryption_at_rest: true }
`,
	}
	for name, file := range bad {
		if _, err := Load(write(t, file), nil); err == nil {
			t.Errorf("%s: accepted", name)
		} else if testing.Verbose() {
			t.Logf("%s: %v", name, err)
		}
	}
	// a sibling directory with a shared prefix is apart
	if _, err := Load(write(t, ok+`    - name: k8s2
      kind: kubesecret
      allow: ["*"]
      kubesecret: { dir: /etc/kista/signing/prod2, etcd_encryption_at_rest: true }
`), nil); err != nil {
		t.Fatalf("sibling: %v", err)
	}
	// the acknowledgment is the configuration's, never the environment's
	if _, err := Load(write(t, base), []string{"KISTA_SIGNERS__SOURCES=x"}); err == nil {
		t.Fatal("sources from the environment")
	}
	// dev: the acknowledgment is not required (spec 0026, open question 5)
	dev := "profile: dev\n" + strings.Replace(ok, ", etcd_encryption_at_rest: true", "", 1)
	if _, err := Load(write(t, dev), nil); err != nil {
		t.Fatalf("dev: %v", err)
	}
	// the error names the acknowledgment
	_, err = Load(write(t, bad["no acknowledgment"]), nil)
	if err == nil || !strings.Contains(err.Error(), "etcd_encryption_at_rest must be true") {
		t.Fatalf("got %v", err)
	}
}
