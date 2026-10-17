---
title: Azure
---

# Azure

kista on Azure Container Apps, with the Bicep recipe in
[`deploy/azure-container-apps`](https://github.com/hugr-lab/duckdb-extension-repository/tree/main/deploy/azure-container-apps):
the service, its database, its key, its storage and its jobs in one deployment, with no static
credential of the service's own. On AKS, use the [Helm chart](kubernetes.md) with workload
identity; the settings below are the same.

The design is in
[spec 0020](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0020-cloud-recipes/spec.md).

## What the recipe makes

| | |
| --- | --- |
| **The identity** | a user-assigned managed identity, the only credential: no key, no password |
| **Key Vault** | Premium, RBAC authorization, purge protection (a parameter, on by default); a channel key `<prefix>-channel`, `RSA-HSM` 2048, `sign` and `verify` only, not exportable, tagged `kista-purpose=channel-signing`; with `attestationKey`, an attestation key `<prefix>-attestation` tagged `kista-purpose=attestation-signing`; the signer's role assigned to the identity on each key, not the vault (`signerRole`: a custom role that may only read keys and sign, or the built-in Key Vault Crypto User) |
| **The database** | PostgreSQL Flexible Server in a delegated subnet with a private DNS zone, or Azure SQL behind a private endpoint (`database: 'sqlserver'`); Entra authentication only, the identity its Entra administrator |
| **Blob Storage** | shared keys disabled, no public access, TLS 1.2, a private container; `Storage Blob Data Contributor` on that container only |
| **Container Apps** | an environment in the recipe's VNet (Consumption); the app with external ingress serving http and https without a redirect, probes on `:8081`, the spool on an `EmptyDir` |
| **Jobs** | one manual job per command: `migrate`, `blob check`, `blob verify`, `blob gc -apply`, and with a `bootstrap`, `bootstrap -dry-run` (its plan against the store) |
| **Log Analytics** | the environment's logs |

kista's configuration is one document in the variable `KISTA_CONFIG`, read with
`-config env:KISTA_CONFIG`. The PostgreSQL server is verified (`verify-full`) against the roots
Azure Database for PostgreSQL chains to, in `store.ca_pem`
(`deploy/azure-container-apps/ca/azure-postgres.pem`, with their fingerprints).

## Deploy

The Azure CLI with Bicep, and an account that is **Owner** (or User Access Administrator and
Contributor) of the resource group: with `signerRole: custom` the recipe creates a role definition,
and either way role assignments (Role Based Access Control Administrator and Contributor suffice for
`builtin`).

```sh
az group create -n kista -l northeurope
cp deploy/azure-container-apps/examples/trial.bicepparam my.bicepparam   # image, issuers, admins
az deployment group create -g kista -f deploy/azure-container-apps/main.bicep -p my.bicepparam
az deployment group show -g kista -n main --query properties.outputs.address.value -o tsv
```

- **`image`** is a version or a digest: `ghcr.io/hugr-lab/kista:vX.Y.Z` or `…@sha256:…`. Pin a
  moving tag by its digest.
- **`domain`** is a host of yours bound to the app as a custom domain (add it to the app with a
  managed certificate after the first deployment); empty, kista's URL is the app's own name
  under `azurecontainerapps.io`.
- **Regions.** A subscription may be refused a region outright (westeurope often refuses new
  customers), and a service in some regions: PostgreSQL Flexible Server (check with
  `az postgres flexible-server list-skus -l <region>`; a `reason` means restricted) and Azure SQL
  ("not accepting creation of new … SQL Database servers"). With Azure SQL, `databaseLocation`
  puts the server in another region; its private endpoint stays in the VNet's.
- **`signerRole`**: `custom` (the default) makes a role that may only read keys and sign, which
  needs Owner or User Access Administrator. `builtin` assigns Key Vault Crypto User instead, which
  makes no role definition, but also allows `update`, `backup`, `encrypt`, `decrypt`, `wrap` and
  `unwrap` on the key. The keys allow only `sign` and `verify`, and kista refuses a key that allows
  more; an identity that holds `update` could still change that, or the key's `kista-purpose` tag
  (kista then refuses the key: a denial of service, not a misuse).
- **Upgrading a deployment of the earlier recipe**, which assigned the signer's role on the vault:
  the redeployment assigns it on each key, and leaves the vault-wide assignment, which then still
  reaches every key in the vault. Delete it:
  `az role assignment delete --scope <vault id> --assignee <the identity's principal id>`.
- **`bootstrap`** makes the first tenant at the first start (see
  [bootstrap](../configuration.md#bootstrap)), as
  `{ versions, tenant, channel: { name, versions }, issuer, grants }`. The template registers its
  channel key for the channel and, with `attestationKey`, its attestation key for the tenant, so no
  key version is ever typed.
- **`serverConsoleClients`** sets the server console's clients (`ui.server_clients`), as
  `{ issuer, client_id, scopes }`. With Entra, the scopes must include `<the API app id>/.default`, or
  Entra issues the console a Microsoft Graph token.
- **Switching the store** (PostgreSQL to Azure SQL, or a new server) needs an empty container:
  kista marks the blob domain for its store and refuses a domain marked for another.

## Administration with Entra ID

Entra ID issues tokens for an app registration; its v2.0 tokens carry the app's client id as
`aud`, `https://login.microsoftonline.com/<tenant>/v2.0` as `iss`, and, for an application, the
caller's client id as `azp`. The server audience is therefore the app's client id, not kista's URL:

```bicep
param serverIssuers = [
  {
    name: 'entra'
    url: 'https://login.microsoftonline.com/<tenant>/v2.0'
    required_claims: { azp: '<the client app id>' }
  }
]
param serverAudiences = ['<the server app id>']
param serverAdmins = ['subject:entra|<the caller principal object id>']
```

- Register an app for the server's audience, and one per tenant; set its
  `api.requestedAccessTokenVersion` to 2. A tenant's default policy refuses an `api://` identifier
  URI without a verified domain: use `api://<app id>`.
- A tenant's audience is its app's client id, assigned by a server administrator
  (`POST /api/v1/tenants/<t>/audiences`), and its issuer the same Entra URL
  ([Access](../access.md#audiences)).
- `required_claims` keeps other callers of the tenant out: `azp` for an application (client
  credentials), or `tid` and roles or groups for people.

## Run a command

```sh
az containerapp job start -g kista -n kista-blob-check
az containerapp job execution list -g kista -n kista-blob-check -o table
```

`<prefix>-migrate`, `-blob-check`, `-blob-verify`, `-blob-gc` (and, with a `bootstrap`,
`-bootstrap`, its plan) run `kista admin` with the app's
configuration and identity. `gcInterval` (24 hours by default) sets `gc.interval`: kista then
collects on schedule, and `-blob-gc` may apply a pass at once; with it empty, `-apply` needs
`-force` ([Commands](../administration/commands.md)). Each command is a job of its own: `az containerapp job start --args`
replaces the whole template, the configuration included.

## The network

- **Trusted proxies.** The environment's Envoy reaches the replicas from the platform's reserved
  ranges (`100.100.0.0/17`, `100.100.128.0/19`, `100.100.160.0/19`, `100.100.192.0/19`; measured
  live), which no subnet of yours may use. Those are `serve.trusted_proxies`; the audit records
  the caller's address. **The environment is the trust boundary:** keep it kista's alone. Another
  app in it can reach kista's replicas from those ranges and claim any client address (the audit,
  the per-client limits) or `X-Forwarded-Proto: https`; Container Apps has no network policy
  between an environment's apps.
- http and https both serve, without a redirect: DuckDB's bootstrap needs plain http, and kista
  ignores a token sent over plain http.
- The database has no public endpoint. Key Vault and Storage are reached on their public
  endpoints, by identity only; for private endpoints, add them in your network.

## Limits

- **240 seconds.** Container Apps cuts any request after 240 seconds, upload or download. A large
  extension on a slow link does not fit: publish from a runner in the same region, and keep
  `max_body` in proportion.
- **The spool.** A replica has 2, 4 or 8 GiB of ephemeral storage (0.5, 1, more vCPUs), the
  `EmptyDir` included: (2 × `maxIngests` + 1) × `maxBodyMib` must leave 1 GiB, or the deployment
  fails.

## For production

- **The database administrator.** kista's identity is the server's Entra administrator: on
  PostgreSQL a member of `azure_pg_admin`, on Azure SQL `dbo`. The server is kista's own, so the
  reach is that server. The narrower way: an Entra group as the administrator, and the identity
  created as an ordinary database role (`pgaadauth_create_principal`, or `CREATE USER … FROM
  EXTERNAL PROVIDER`) that owns kista's database.
- Keep `purgeProtection` on: a deleted vault and its keys stay recoverable, and every signature the
  channel served depends on the key.
- **Who may act as kista.** Anyone allowed to assign the identity to a resource
  (`Microsoft.ManagedIdentity/userAssignedIdentities/*/assign/action`), to add a federated
  credential to it (`…/federatedIdentityCredentials/write`, which Contributor on the group holds:
  tokens as kista from outside Azure), or to change the app or a job, acts as kista. Keep all three
  to the platform's operators.
- Blob soft delete keeps deleted bodies 7 days; lengthen it to guard against deletion by a
  compromised identity (garbage collection deletes bodies, so it may).
- Pin the image by digest; more replicas and a larger database SKU.

## The Marketplace application

kista is also offered as an Azure Marketplace **managed application** built from this recipe
(`deploy/azure-marketplace`, the release asset `kista-azure-app-<version>.zip`). Its resources sit in
a managed resource group in your subscription.

- **You keep full access.** The application has no deny assignment, so you rotate keys, start the
  jobs, bind your domain and upgrade, as with the recipe. That holds for principals with rights on
  the subscription. A role on the application's own resource group alone does not reach the managed
  one.
- **The publisher can only read.** The publisher holds Reader on the managed resource group, with no
  write access and no just-in-time elevation. Reader shows:
  - the app's configuration (`KISTA_CONFIG`, which holds no secret);
  - the resources' metadata;
  - the Log Analytics workspace, with kista's request and audit logs.

  It shows no database row, blob or key material. The logs hold your users' identities and
  addresses. To keep them from the publisher, set the workspace's access mode to require
  workspace permissions, or delete the publisher's role assignment (an update of the application
  may restore it).
- **Before you buy**, prepare in Entra ID:
  - an API app registration for the server, with v2.0 tokens
    (`api.requestedAccessTokenVersion: 2`) and an app role (`kista-admin`) assigned to the
    administrators;
  - a single-page app for the server console, with a delegated permission on the server API app,
    consented;
  - an API app for the first tenant, also with v2.0 tokens and its own role (`kista-tenant-admin`);
  - optionally, in another directory, a single-page app for the tenant's console, with a delegated
    permission on the tenant API app.

  The UI asks for their IDs; it creates no app registration. Use app roles rather than groups:
  groups need group claims, and past about 200 groups a token carries none. When the tenant signs
  in at the same directory as the server administrators, it gets no console client of its own:
  its administrators use the API, and server administrators open it in the server console.
- **After deployment**, add `https://<kista's host>/ui/server/callback` as the server console app's
  redirect URI, and `https://<kista's host>/ui/t/<tenant>/callback` as the tenant console app's. A
  custom domain is bound as with the recipe.
- **What it pins.** The package pins kista's image by digest and uses the built-in signer role
  (`signerRole: builtin`). The template makes the keys. At the first start, the bootstrap makes the
  tenant and its channel, registers the keys, and adds the issuer and the administrators' grant.
  The UI does not check that the subscription's resource providers are registered for you; it warns,
  and the deployment fails until they are.
- **Upgrades.** Change the app's and the jobs' image to a new release's digest, or create a new
  application from the new plan version.
- **Deleting the application** deletes the managed resource group. The vault stays soft-deleted
  for its retention.
- **Verify the package** against the release: `kista-azure-app-<version>.zip` is signed like the
  binaries (`cosign verify-blob` with its `.sigstore.json`), and its SHA-256 is in `SHA256SUMS`.

## Destroy

With `signerRole: custom`, delete the role's assignments (one per key) and the custom role first:
a role definition outlives the group, and goes only without assignments. Then delete the resource
group; with `builtin`, that is all.

```sh
scope=$(az group show -n kista --query id -o tsv)
role=$(az role definition list --custom-role-only true --scope "$scope" --query '[0].name' -o tsv)
if [ -n "$role" ]; then   # never an empty --role: that lists every assignment
  az role assignment list --all --role "$role" --query '[].id' -o tsv | xargs -n1 az role assignment delete --ids
  az role definition delete --name "$role" --scope "$scope"
fi
az group delete -n kista
```

The vault stays in soft delete for its retention. Purge it (`az keyvault purge`) unless purge
protection is on; then it waits out the retention. A new deployment of the same group and prefix
before that takes it back with `vaultCreateMode: 'recover'`, the channel key with it, which is
usually what you want: every signature the channel served depends on that key.
