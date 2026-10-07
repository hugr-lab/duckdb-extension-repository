package azure

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clean empties every steering variable for the test (t.Setenv restores them afterwards).
func clean(t *testing.T) {
	for _, v := range append(Steering, "AZURE_FEDERATED_TOKEN_FILE", "AZURE_CLIENT_ID", "AZURE_TENANT_ID") {
		t.Setenv(v, "")
	}
}

func TestCredential(t *testing.T) {
	clean(t)
	if _, err := Credential(Identity{Kind: "managed"}, "mars", false); err == nil {
		t.Fatal("unknown cloud")
	}
	if _, err := Credential(Identity{Kind: "default"}, "public", false); err == nil || !strings.Contains(err.Error(), "dev") {
		t.Fatalf("developer chain outside dev: %v", err)
	}
	if _, err := Credential(Identity{Kind: "default"}, "public", true); err != nil {
		t.Fatalf("developer chain in dev: %v", err)
	}
	if _, err := Credential(Identity{Kind: "approle"}, "public", true); err == nil {
		t.Fatal("unknown kind")
	}
	for _, v := range Steering {
		t.Setenv(v, "x")
		_, err := Credential(Identity{Kind: "managed"}, "public", false)
		if err == nil || !strings.Contains(err.Error(), v) {
			t.Errorf("%s set: %v", v, err)
		}
		if _, err := Credential(Identity{Kind: "managed"}, "public", true); err != nil {
			t.Errorf("%s in dev: %v", v, err)
		}
		t.Setenv(v, "")
	}
	// AKS workload identity sets AZURE_AUTHORITY_HOST; it is ignored because the cloud is explicit
	t.Setenv("AZURE_AUTHORITY_HOST", "https://login.microsoftonline.com/")
	for _, c := range []string{"public", "china", "usgov"} {
		if _, err := Credential(Identity{Kind: "managed", ClientID: "c"}, c, false); err != nil {
			t.Errorf("%s: %v", c, err)
		}
	}
	if _, err := Credential(Identity{Kind: "workload", ClientID: "c", TenantID: "t"}, "public", false); err == nil {
		t.Fatal("workload identity without a token file")
	}
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("x"), 0o600)
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", tok)
	if _, err := Credential(Identity{Kind: "workload"}, "public", false); err == nil {
		t.Fatal("workload identity from environment ids")
	}
	if _, err := Credential(Identity{Kind: "workload", ClientID: "c", TenantID: "t"}, "public", false); err != nil {
		t.Fatal(err)
	}
}

func TestHost(t *testing.T) {
	for _, c := range []struct{ cloud, vault, hsm, want string }{
		{"public", "Kista", "", "kista.vault.azure.net"},
		{"china", "kista", "", "kista.vault.azure.cn"},
		{"usgov", "", "kista-hsm", "kista-hsm.managedhsm.usgovcloudapi.net"},
		{"public", "", "kista-hsm", "kista-hsm.managedhsm.azure.net"},
	} {
		if got, err := Host(c.cloud, c.vault, c.hsm); err != nil || got != c.want {
			t.Errorf("%+v: %q %v", c, got, err)
		}
	}
	for _, c := range [][3]string{{"mars", "k", ""}, {"public", "", ""}, {"public", "k", "h"}} {
		if _, err := Host(c[0], c[1], c[2]); err == nil {
			t.Errorf("%v accepted", c)
		}
	}
}
