# The Azure Marketplace package

kista as an Azure Marketplace managed application (spec 0020 phase 4b), built from the Container
Apps recipe in [`../azure-container-apps`](../azure-container-apps):

- `createUiDefinition.json`: the portal's form (region, prefix, database; the server
  administrators' Entra app registrations; the first tenant, which the bootstrap makes; size);
- `build.sh <image@sha256:…> <out.zip>`: `mainTemplate.json` from the recipe, with the image
  pinned by digest, the built-in signer role, and the plan's customer usage attribution
  (`KISTA_MARKETPLACE_PID`), zipped reproducibly with the form.

The customer keeps full access to the managed resource group, and the publisher holds Reader. A
release attaches the package as `kista-azure-app-<version>.zip`, signed and in `SHA256SUMS`. See
[Azure](https://hugr-lab.github.io/duckdb-extension-repository/docs/deployment/azure#the-marketplace-application).
