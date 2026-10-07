// Package azure builds Microsoft Entra credentials from explicit config (specs 0003, 0004): managed
// identity, workload identity, or (development only) the developer credential chain. The cloud, and
// so the authority host, comes from config, never from AZURE_AUTHORITY_HOST.
package azure

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// Cloud is an Azure cloud kista knows, with its Key Vault and Managed HSM DNS suffixes.
type Cloud struct {
	Name        string
	Config      cloud.Configuration
	VaultSuffix string
	HSMSuffix   string
}

// Clouds by config name. The China and US Gov Managed HSM suffixes are to be confirmed against a
// live instance (spec 0004); a wrong suffix fails closed.
var Clouds = map[string]Cloud{
	"public": {"public", cloud.AzurePublic, ".vault.azure.net", ".managedhsm.azure.net"},
	"china":  {"china", cloud.AzureChina, ".vault.azure.cn", ".managedhsm.azure.cn"},
	"usgov":  {"usgov", cloud.AzureGovernment, ".vault.usgovcloudapi.net", ".managedhsm.usgovcloudapi.net"},
}

// Host returns the Key Vault or Managed HSM host for a name in a cloud (exactly one of vault and hsm).
func Host(cloudName, vault, hsm string) (string, error) {
	c, ok := Clouds[cloudName]
	switch {
	case !ok:
		return "", fmt.Errorf("azure: unknown cloud %q (public, china, usgov)", cloudName)
	case (vault == "") == (hsm == ""):
		return "", errors.New("azure: exactly one of a vault and a Managed HSM")
	case vault != "":
		return strings.ToLower(vault) + c.VaultSuffix, nil
	}
	return strings.ToLower(hsm) + c.HSMSuffix, nil
}

// Identity is the credential choice.
type Identity struct {
	Kind     string // managed | workload | default
	ClientID string
	TenantID string
}

// Steering are environment variables that would make the SDK use a credential, a token endpoint or
// logging the config did not choose; outside development they are refused. AZURE_AUTHORITY_HOST is
// not among them: AKS workload identity sets it, and the SDK ignores it because the cloud is always
// set from config. The platform's managed-identity endpoint variables (IDENTITY_ENDPOINT,
// IDENTITY_HEADER, MSI_ENDPOINT, IMDS_ENDPOINT, IDENTITY_SERVER_THUMBPRINT) are set by Container
// Apps, App Service, Arc and Service Fabric, and are allowed.
var Steering = []string{"AZURE_CLIENT_SECRET", "AZURE_CLIENT_CERTIFICATE_PATH", "AZURE_CLIENT_CERTIFICATE_PASSWORD",
	"AZURE_USERNAME", "AZURE_PASSWORD", "AZURE_POD_IDENTITY_AUTHORITY_HOST", "AZURE_REGIONAL_AUTHORITY_NAME",
	"AZURE_SDK_GO_LOGGING"}

// Credential builds the credential for a cloud. dev allows the developer chain ("default") and the
// steering variables. The platform's own managed-identity endpoint variables (IDENTITY_ENDPOINT,
// MSI_ENDPOINT) are set by Container Apps / App Service and are allowed.
func Credential(id Identity, cloudName string, dev bool) (azcore.TokenCredential, error) {
	c, ok := Clouds[cloudName]
	if !ok {
		return nil, fmt.Errorf("azure: unknown cloud %q (public, china, usgov)", cloudName)
	}
	if !dev {
		for _, v := range Steering {
			if os.Getenv(v) != "" {
				return nil, fmt.Errorf("azure: %s is set; outside profile dev the credential comes from config only", v)
			}
		}
	}
	co := azcore.ClientOptions{Cloud: c.Config}
	switch id.Kind {
	case "managed":
		opts := &azidentity.ManagedIdentityCredentialOptions{ClientOptions: co}
		if id.ClientID != "" {
			opts.ID = azidentity.ClientID(id.ClientID)
		}
		return azidentity.NewManagedIdentityCredential(opts)
	case "workload":
		if os.Getenv("AZURE_FEDERATED_TOKEN_FILE") == "" {
			return nil, errors.New("azure: workload identity needs AZURE_FEDERATED_TOKEN_FILE")
		}
		if !dev && (id.ClientID == "" || id.TenantID == "") {
			return nil, errors.New("azure: workload identity needs client_id and tenant_id in config")
		}
		return azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
			ClientOptions: co, ClientID: id.ClientID, TenantID: id.TenantID,
		})
	case "default":
		if !dev {
			return nil, errors.New("azure: the developer credential chain (default) is allowed only with profile dev")
		}
		return azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{ClientOptions: co})
	}
	return nil, errors.New("azure: identity kind must be managed, workload or default")
}
