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
| **Key Vault** | Premium, RBAC authorization, purge protection (a parameter, on by default); a channel key `<prefix>-channel`, `RSA-HSM` 2048, `sign` and `verify` only, not exportable, tagged `kista-purpose=channel-signing`; a custom role that may only read keys and sign, assigned to the identity on the vault |
| **The database** | PostgreSQL Flexible Server in a delegated subnet with a private DNS zone, or Azure SQL behind a private endpoint (`database: 'sqlserver'`); Entra authentication only, the identity its Entra administrator |
| **Blob Storage** | shared keys disabled, no public access, TLS 1.2, a private container; `Storage Blob Data Contributor` on that container only |
| **Container Apps** | an environment in the recipe's VNet (Consumption); the app with external ingress serving http and https without a redirect, probes on `:8081`, the spool on an `EmptyDir` |
| **Jobs** | one manual job per command: `migrate`, `blob check`, `blob verify`, `blob gc -apply` |
| **Log Analytics** | the environment's logs |

kista's configuration is one document in the variable `KISTA_CONFIG`, read with
`-config env:KISTA_CONFIG`. The PostgreSQL server is verified (`verify-full`) against the roots
Azure Database for PostgreSQL chains to, in `store.ca_pem`
(`deploy/azure-container-apps/ca/azure-postgres.pem`, with their fingerprints).

## Deploy

The Azure CLI with Bicep, and an account that is **Owner** (or User Access Administrator and
Contributor) of the resource group: the recipe creates a role definition and role assignments.

```sh
az group create -n kista -l northeurope
cp deploy/azure-container-apps/examples/trial.bicepparam my.bicepparam   # image, issuers, admins
az deployment group create -g kista -f deploy/azure-container-apps/main.bicep -p my.bicepparam
az deployment group show -g kista -n main --query properties.outputs.url.value -o tsv
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

`<prefix>-migrate`, `-blob-check`, `-blob-verify` and `-blob-gc` run `kista admin` with the app's
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

## Destroy

Delete the role's assignment and the custom role first (a role definition outlives the group, and
goes only without assignments), then the resource group:

```sh
role=$(az role definition list --custom-role-only true --scope "$(az group show -n kista --query id -o tsv)" --query '[0].name' -o tsv)
az role assignment list --all --role "$role" --query '[].id' -o tsv | xargs -n1 az role assignment delete --ids
az role definition delete --name "$role" --scope "$(az group show -n kista --query id -o tsv)"
az group delete -n kista
```

The vault stays in soft delete for its retention. Purge it (`az keyvault purge`) unless purge
protection is on; then it waits out the retention. A new deployment of the same group and prefix
before that takes it back with `vaultCreateMode: 'recover'`, the channel key with it, which is
usually what you want: every signature the channel served depends on that key.
