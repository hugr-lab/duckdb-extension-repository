---
title: Signing keys
---

# Signing keys

A signed channel serves every extension as `body ‖ channel signature`. The signature is RSA-2048,
PKCS#1 v1.5 with a SHA-256 DigestInfo over DuckDB's composite body hash: the only form DuckDB
accepts. kista never holds the private key of a production channel. It signs through a **key
source**: a vault or KMS that the server administrator configures, and that kista calls with a
32-byte digest.

This page covers where keys live, how kista checks them, and how a key is registered. Rotation
(trusted, active, retired) is on [Key rotation](administration/key-rotation.md). The design is in
specs [0003](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0003-store-tenants-keys/spec.md)
and [0004](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0004-signer-backends/spec.md).

## Key sources

| Kind | Backend | Status |
| --- | --- | --- |
| `file` (built in) | an unencrypted PEM key file in `signers.file_dir` | development, or explicit opt-in |
| `vault` | HashiCorp Vault or OpenBao Transit | available |
| `azurekv` | Azure Key Vault or Azure Key Vault Managed HSM | available |
| `awskms` | AWS KMS | available |
| `gcpkms` | Google Cloud KMS | available |
| `kubesecret` | a Kubernetes Secret volume | available, as an explicit opt-in: the weakest source |

PKCS#11 hardware HSMs are not available either. A channel key in a Kubernetes Secret is readable
by anyone with `get secrets` in the namespace, sits in etcd, and often in GitOps sources. Keep
channel keys in a KMS, an HSM or Vault Transit. Where none exists, a `kubesecret` source accepts a
Secret volume, with an acknowledgment and the checks of [Kubernetes Secrets](#kubernetes-secrets);
a Secret mounted into `signers.file_dir` is still refused (its files are links, root-owned; see
[File keys](#file-keys)).

Each source kind has a **protection tier**, which `key check`, `key list`, the management API and
`key.add` events show: `file` (a key file), `kubesecret` (a Kubernetes Secret), `software` (Vault
Transit: software keys in Vault's barrier) and `kms` (Azure Key Vault or Managed HSM, AWS KMS,
Google Cloud KMS).

## Signer references

A key is named by a **signer reference**, `<source>:<key>`. The source name is everything before
the first `:`; the rest is parsed by that source's kind.

| Kind | Reference | Key name | Version |
| --- | --- | --- | --- |
| `file` | `file:<name>` | one file name inside `signers.file_dir`, no `/` or `..` | the file itself |
| `vault` | `<source>:<key>:v<N>` | `[A-Za-z0-9_-]{1,128}` | `N ≥ 1`, no leading zeros (`v0` would mean "latest" in Vault) |
| `azurekv` | `<source>:<name>/<version>` | `[0-9A-Za-z-]{1,127}`, compared lowercased | 32 lowercase hex characters (an empty version would mean "latest") |
| `awskms` | `<source>:<key-id>` | a key id: a UUID in lowercase hex, or `mrk-` and 32 lowercase hex characters (a multi-Region key, with `allow_multi_region`). Aliases and ARNs are refused | the key id itself: an asymmetric KMS key is never rotated |
| `gcpkms` | `<source>:<key>/<version>` | a CryptoKey id in the source's key ring, `[A-Za-z0-9_-]{1,63}` | the CryptoKeyVersion number, `[1-9][0-9]{0,9}` (required) |
| `kubesecret` | `<source>:<name>` | a Secret data key without dots, `[A-Za-z0-9_-]{1,63}` | the name itself: a key is never replaced under its name |

- A reference never carries a host, URL, account or path. Hosts, mounts and namespaces come only
  from the configuration, so a reference can only pick a key inside a source the administrator
  configured.
- The version is always pinned. A new key version in the vault is a new key in kista.
- Only the server administrator sets references: `kista admin key add`, or the API route that only
  a server administrator may call. A tenant administrator can activate and retire keys, never
  point kista at one.
- A renamed or removed source leaves its references unresolvable. They fail closed and never fall
  back to another source.

## Configuration

The whole `signers` block is file-only: no `KISTA_*` variable can set any part of it.

```yaml title="kista.yaml"
signers:
  allow_file: false              # file: references outside profile dev
  file_dir: /etc/kista/keys      # the only directory file: references resolve in
  sources:
    - name: bao
      kind: vault
      vault:
        address: https://bao.internal:8200
        namespace: ""            # Vault Enterprise / OpenBao namespace
        mount: transit
        ca_file: /etc/kista/bao-ca.pem
        auth:
          kind: kubernetes       # kubernetes | jwt | token_file
          role: kista
          token_file: /var/run/secrets/kista/vault-token
        software_keys: true      # required outside profile dev
      allow: ["ext-"]
    - name: prod
      kind: azurekv
      azurekv:
        vault: kista-prod        # or managed_hsm: kista-hsm
        cloud: public            # public | china | usgov
      identity:
        kind: workload
        client_id: "00000000-0000-0000-0000-000000000000"
        tenant_id: "00000000-0000-0000-0000-000000000000"
      allow: ["ext-"]
    - name: aws
      kind: awskms
      awskms:
        region: eu-central-1
        account: "123456789012"
      identity:
        kind: pod_identity       # pod_identity | irsa | imds | ecs
      allow: ["1234abcd-12ab-34cd-56ef-1234567890ab"]   # key ids, or "*"
    - name: gcp
      kind: gcpkms
      gcpkms:
        key_ring: projects/acme-kista/locations/europe-west4/keyRings/kista
        principal: kista-signer@acme-kista.iam.gserviceaccount.com
      allow: ["ext-"]
    - name: k8s
      kind: kubesecret
      kubesecret:
        dir: /etc/kista/signing/prod   # a Secret volume's mountPath
        etcd_encryption_at_rest: true  # required outside profile dev
      allow: ["ext-"]
```

### Settings of a source

| Setting | Default | Meaning |
| --- | --- | --- |
| `name` | required | `[a-z][a-z0-9-]{0,15}`, unique. Reserved: `file`, `vault`, `azurekv`, `awskms`, `gcpkms`, `kubesecret`, `pkcs11`, `http`, `https`, `arn`, `projects` |
| `kind` | required | `vault`, `azurekv`, `awskms`, `gcpkms` or `kubesecret`; exactly one matching block (`vault:`, `azurekv:`, `awskms:`, `gcpkms:` or `kubesecret:`) |
| `allow` | required | key-name prefixes the source may use; `"*"` for any key. An empty list, or an empty prefix, is refused. For `awskms`, whole key ids or `"*"` |
| `max_concurrency` | `4` | concurrent opens and signs through this source |
| `timeout` | `10s` | per call, `1s` to `60s` |
| `recheck` | `10m` | how often a key's properties are read again while it is in use; at most `10m` |
| `health_key` | none | accepted by the configuration; this build does not use it |
| `identity` | none | required for `azurekv` and `awskms`, with that cloud's kinds; refused on `vault`, `gcpkms` and `kubesecret` (which use `vault.auth`, `principal` and `credentials_file`, and a mounted Secret) |

**Allow lists** are plain prefixes, matched after the key name passed its kind's rule: `ext-`
admits `ext-prod` but not `extra`. For `azurekv`, names are matched lowercased, so prefixes must be
written in lowercase. For `awskms`, an entry must be a whole key id (or `"*"`, which the purpose
tag then guards): AWS key ids are random, so a prefix of one means nothing. A `mrk-` id is accepted
only with `allow_multi_region`.

kista does not contact a source at startup. It starts even when a source is unreachable; signing
and `key add` through it then fail, while serving and `.well-known` keep working, since they use
the stored public keys. `/readyz` does not depend on key sources.

## The contract every key meets

Whatever the backend, a key is checked when it is opened, re-checked while in use, and every
signature it makes is verified.

- **At open**: the public key comes from the backend and is RSA, 2048 bits, `e = 65537`. The
  backend's metadata must show the key enabled, usable for signing only, not exportable, not
  imported, at the pinned version, and (where the backend has tags) marked with its purpose.
  Anything that cannot be confirmed fails closed.
- **While in use**: before a signature, the metadata is read again when the last check is older
  than `recheck`. A key that no longer passes stops signing.
- **Every signature**: the digest goes out as a precomputed SHA-256 digest; the answer must be
  256 bytes, verify against the public key, and name the pinned key version (Vault's
  `key_version`, Azure's `kid`, AWS's `KeyId`, which must be the key's ARN, Google's `name`, which
  must be the version's resource name).
- **Every open of a registered key** compares the signer's public key with the one stored when the
  key was registered (the one published in `.well-known`) and its fingerprint. A file that changed,
  or a source re-pointed at another key, never signs.
- **Errors** carry a status, an error code and a request id, never response bodies, tokens or key
  material.

Signing happens when a release is made or re-signed, never per download. The signature is stored.

## File keys

A `file:` reference resolves to `<signers.file_dir>/<name>`. File keys are allowed only when
`signers.file_dir` is set **and** either `profile: dev` or `signers.allow_file: true`.
`signers.file_dir` must be an absolute path.

The file must be:

- a regular file, not a symlink (opened with `O_NOFOLLOW` on Unix);
- on Unix, owned by the user kista runs as, with mode `0600` or narrower;
- one unencrypted PEM block, `PRIVATE KEY` (PKCS#8) or `RSA PRIVATE KEY` (PKCS#1), RSA-2048 with
  `e = 65537`, at most 64 KiB.

```bash
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out /etc/kista/keys/dev-channel.pem
chmod 0600 /etc/kista/keys/dev-channel.pem
kista admin -config kista.yaml key check -signer file:dev-channel.pem
```

This rule is stricter than the one for other files kista reads
([Files kista reads](security.md#files-kista-reads)): a key file cannot be a link and must be
owned by the user kista runs as; a Secret volume's root-owned, group-readable files are refused,
so a Kubernetes Secret mounted into `signers.file_dir` does not work. A `kubesecret` source
accepts exactly a Secret volume, with its own checks and warnings (see
[Kubernetes Secrets](#kubernetes-secrets)).

A file key has no tags, so its role is held by the fingerprint registry alone (see
[Purposes](#purposes)). Keep file keys for development and tests; a production key belongs in a
vault or KMS.

## HashiCorp Vault and OpenBao Transit

A `vault` source signs with a Transit key through a thin HTTP client (no Vault SDK). Transit keys
are software keys in Vault's barrier, so outside `profile: dev` the source must say
`software_keys: true`.

### Settings

| Setting | Meaning |
| --- | --- |
| `vault.address` | `https://host[:port]`, no path. Plain `http` only on a loopback host with `profile: dev` (a Vault Agent sidecar) |
| `vault.namespace` | a Vault Enterprise or OpenBao namespace: letters, digits, `_`, `-` and `/` |
| `vault.mount` | the Transit mount path, relative (`transit`) |
| `vault.ca_file` | trust only this CA bundle (a private CA); empty: the system roots |
| `vault.auth.kind` | `kubernetes`, `jwt` or `token_file` |
| `vault.auth.mount` | the auth mount path; default the kind's name (`kubernetes` or `jwt`) |
| `vault.auth.role` | the Vault role; required for `kubernetes` and `jwt` |
| `vault.auth.token_file` | absolute path: the projected service-account token (`kubernetes`), the JWT (`jwt`), or the token Vault Agent writes (`token_file`) |
| `vault.software_keys` | must be `true` outside `profile: dev`; `kista serve` logs a warning for each `vault` source at every start |

### Authentication

There is no AppRole and no static token in the configuration, and `VAULT_*` and `BAO_*` environment
variables are ignored.

- `kubernetes`: kista logs in at `auth/<mount>/login` with the role and the token file. Use a
  projected service-account token with a dedicated audience bound on the Vault role, never the
  default token (its audience is the Kubernetes API).
- `jwt`: the same login with a JWT read from the token file.
- `token_file`: the file holds a Vault token, kept fresh by Vault Agent.

The token file follows [Files kista reads](security.md#files-kista-reads). With `kubernetes` and
`jwt` it is a projected token: owned by root or kista's user, any read bits; nobody but the
owner may write it. With `token_file` it holds a Vault token, a secret: owned by kista's user
with mode `0600` or narrower, or root-owned with kista's group and mode `0440` or narrower.
`vault.ca_file` is public material, owned by root or kista's user. The token file is read again
on expiry. A token is renewed on the first request after two thirds of its lease; if the new
login fails the old token is used until it expires. A `403` causes one re-login. Requests use
TLS 1.2 or later, never follow redirects, and ignore proxy environment variables.

### What the key and the policy must look like

kista reads `GET /v1/<mount>/keys/<key>` and refuses the key unless:

- `type` is `rsa-2048` and `supports_signing` is true;
- `exportable` and `allow_plaintext_backup` are false;
- `imported_key` is reported and false (a server that does not report it is refused);
- OpenBao's `soft_deleted` is false;
- the pinned version lies between `min_encryption_version` and `latest_version`, and its public key
  is listed.

RSA Transit keys always report encryption support, so kista proves "sign only" from the token's
own capabilities (`POST /v1/sys/capabilities-self`) at open and at every re-check. The key path
must answer exactly `["read"]`, and every other operation on the key exactly `["deny"]`: `encrypt`,
`decrypt`, `rewrap`, `datakey/plaintext`, `datakey/wrapped`, `backup`, `restore`,
`keys/<key>/config`, `keys/<key>/rotate`, `keys/<key>/trim`, and the `export/*` paths. A broader
policy (a `transit/*` wildcard, for example) makes the key refused.

```bash
vault secrets enable transit
vault write transit/keys/ext-prod type=rsa-2048

vault policy write kista-ext-prod - <<'EOF'
path "transit/keys/ext-prod" {
  capabilities = ["read"]
}
path "transit/sign/ext-prod/sha2-256" {
  capabilities = ["update"]
}
EOF
```

`sys/capabilities-self` is granted by Vault's `default` policy. Bind the policy to kista's role
(Kubernetes or JWT auth), then check the key:

```bash
kista admin -config kista.yaml key check -signer bao:ext-prod:v1
```

Signing is `POST /v1/<mount>/sign/<key>/sha2-256` with `prehashed=true`,
`signature_algorithm=pkcs1v15` and `key_version=N`.

## Azure Key Vault and Managed HSM

An `azurekv` source signs with a key in an Azure Key Vault or a Managed HSM, through the Azure SDK,
`RS256` over the digest.

### Settings

| Setting | Meaning |
| --- | --- |
| `azurekv.vault` | the vault name (3 to 24 letters, digits and dashes) |
| `azurekv.managed_hsm` | or a Managed HSM name; exactly one of `vault` and `managed_hsm` |
| `azurekv.cloud` | `public` (default), `china` or `usgov`: decides the host and the Entra authority |
| `azurekv.require_hsm` | default `true`: only `RSA-HSM` keys. `false` (plain `RSA`) is allowed only with `profile: dev` |
| `identity.kind` | `managed`, `workload`, or `default` (the developer credential chain, `profile: dev` only) |
| `identity.client_id` | a user-assigned managed identity's client id (optional for `managed`); required for `workload` |
| `identity.tenant_id` | required for `workload` |

| `cloud` | Key Vault host | Managed HSM host |
| --- | --- | --- |
| `public` | `<vault>.vault.azure.net` | `<hsm>.managedhsm.azure.net` |
| `china` | `<vault>.vault.azure.cn` | `<hsm>.managedhsm.azure.cn` |
| `usgov` | `<vault>.vault.usgovcloudapi.net` | `<hsm>.managedhsm.usgovcloudapi.net` |

The China and US Government Managed HSM suffixes have not been confirmed against a live instance;
a wrong suffix fails closed.

### Identity

- `managed`: the platform's managed identity (Container Apps, App Service, AKS node identity),
  optionally a user-assigned one by `client_id`.
- `workload`: AKS workload identity. It needs `AZURE_FEDERATED_TOKEN_FILE` in the environment (the
  AKS webhook sets it) and `client_id` and `tenant_id` in the configuration, never from
  `AZURE_CLIENT_ID` or `AZURE_TENANT_ID`.
- Outside `profile: dev`, kista refuses to build a credential while any of these variables is set:
  `AZURE_CLIENT_SECRET`, `AZURE_CLIENT_CERTIFICATE_PATH`, `AZURE_CLIENT_CERTIFICATE_PASSWORD`,
  `AZURE_USERNAME`, `AZURE_PASSWORD`, `AZURE_POD_IDENTITY_AUTHORITY_HOST`,
  `AZURE_REGIONAL_AUTHORITY_NAME`, `AZURE_SDK_GO_LOGGING`. The platform's managed-identity endpoint
  variables (`IDENTITY_ENDPOINT`, `IDENTITY_HEADER`, `MSI_ENDPOINT`, `IMDS_ENDPOINT`,
  `IDENTITY_SERVER_THUMBPRINT`) are allowed.
- Token requests and Key Vault calls ignore proxy environment variables.

### What the key must look like

kista reads the key at the pinned version and refuses it unless:

- its type is `RSA-HSM` (or `RSA` with `require_hsm: false`), 2048 bits;
- it is enabled and inside its `nbf` / `exp` window;
- `key_ops` holds `sign`, optionally `verify`, and nothing else;
- it is not exportable, has no release policy and does not back a certificate;
- it carries the tag `kista-purpose` with its role's value: `channel-signing` for a channel key,
  `attestation-signing` for an [attestation key](attachments.md).

Key Vault does not report imported (BYOK) material, so kista cannot detect it. Keep import rights
out of kista's role, let whoever creates keys set the tag, and alert on key creation and import in
the Activity Log.

```bash
az keyvault key create --vault-name kista-prod --name ext-prod \
  --kty RSA-HSM --size 2048 --ops sign verify \
  --tags kista-purpose=channel-signing
```

The reference is `prod:ext-prod/<version>`, with the 32-character version from the key's `kid`.

### Permissions

kista needs to read the key and sign with it, nothing more.

- **Key Vault**: the Azure RBAC permission model, and a custom role whose `dataActions` are
  `Microsoft.KeyVault/vaults/keys/read` and `Microsoft.KeyVault/vaults/keys/sign/action`, assigned
  to kista's identity per key. A Premium vault is needed for `RSA-HSM`.
- **Managed HSM**: local RBAC, a custom role with
  `Microsoft.KeyVault/managedHsm/keys/read/action` and
  `Microsoft.KeyVault/managedHsm/keys/sign/action`, scoped to `/keys/<name>`.
- Turn on purge protection.
- Only kista's identity should hold sign rights on these keys.

## AWS KMS

An `awskms` source signs with an asymmetric KMS key in one AWS account and region, through the AWS
SDK for Go v2, `RSASSA_PKCS1_V1_5_SHA_256` over the digest.

```yaml title="kista.yaml"
signers:
  sources:
    - name: aws
      kind: awskms
      awskms:
        region: eu-central-1
        account: "123456789012"      # the account that holds the keys
        role_arn: ""                 # optional: a role to assume, for keys in another account
        allow_multi_region: false
        fips: false
      identity:
        kind: irsa
        role_arn: arn:aws:iam::123456789012:role/kista
      allow: ["1234abcd-12ab-34cd-56ef-1234567890ab"]
```

### Settings

| Setting | Meaning |
| --- | --- |
| `awskms.region` | required; a region such as `eu-central-1` (`^[a-z]{2,4}(-[a-z]+)+-\d{1,2}$`). KMS and STS are called in this region |
| `awskms.account` | required; the 12-digit account that holds the keys |
| `awskms.role_arn` | optional; an IAM role ARN in the region's partition, assumed with STS `AssumeRole` (session name `kista`) after the identity |
| `awskms.allow_multi_region` | default `false`; `true` accepts multi-Region keys (`mrk-` ids) |
| `awskms.fips` | default `false`; `true` uses the FIPS endpoints of KMS, and of STS in the `us-` regions (the only ones where STS has them). Refused in a partition without FIPS endpoints (`aws` and `aws-us-gov` have them) and with `endpoint_url` |
| `awskms.endpoint_url` | the moto emulator only: `profile: dev`, an `http` or `https` URL on a loopback host, and `identity.kind: static`, so no platform token goes to it. It replaces both the KMS and the STS endpoint |
| `identity.kind` | `pod_identity`, `irsa`, `imds`, `ecs`, or `static` (`profile: dev` only) |
| `identity.role_arn` | `irsa` only, and required there: the IAM role the ServiceAccount is bound to, in the region's partition |
| `identity.access_key_file`, `identity.secret_key_file` | `static` only, and required there: absolute paths to the access key id and the secret |

`client_id` and `tenant_id` are Azure's and are refused here; the AWS fields are refused on an
`azurekv` source.

### Identity

kista builds the credentials from `identity.kind`. It never calls the SDK's default credential
chain and never reads `~/.aws/config` or `~/.aws/credentials`. For the kind chosen it reads the
variables the platform sets (EKS, ECS, EC2) and checks them at startup; a value outside the Accepted
column stops kista.

| `identity.kind` | Platform | Read | Accepted |
| --- | --- | --- | --- |
| `pod_identity` | EKS Pod Identity | `AWS_CONTAINER_CREDENTIALS_FULL_URI`, `AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE` | the URI is `http://169.254.170.23/v1/credentials` or `http://[fd00:ec2::23]/v1/credentials`; the token file is read again for every retrieval |
| `irsa` | EKS IAM roles for service accounts | `AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE` | `AWS_ROLE_ARN` equals `identity.role_arn`; STS `AssumeRoleWithWebIdentity` in the region, session name `kista` |
| `ecs` | ECS task role (EC2, Fargate, ECS Anywhere) | `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` | a path starting with `/`, without `//`, `@`, `\`, `?`, `#`, `%` or `..`, on `http://169.254.170.2` |
| `imds` | EC2 instance profile | nothing | IMDSv2 at `http://169.254.169.254`, no fallback to IMDSv1. IPv6-only instances are not supported yet |

- `irsa` takes the role from the configuration. Whoever edits the ServiceAccount's annotation can
  change `AWS_ROLE_ARN`, but not kista's identity: kista then refuses to start.
- The token files of `pod_identity` and `irsa` are Kubernetes projected tokens: kista follows
  a relative link inside the token's directory, wants the file owned by root or kista's user
  (any read bits; nobody but the owner may write it), reads it again for every retrieval and
  refuses an empty one ([Files kista reads](security.md#files-kista-reads)).
- `imds` in a container needs the instance's metadata hop limit raised to `2`. With `imds`,
  `AWS_EC2_METADATA_DISABLED=true` stops kista outside `profile: dev`.
- `static` reads `identity.access_key_file` and `identity.secret_key_file`, each a secret file
  ([Files kista reads](security.md#files-kista-reads)): owned by kista's user with mode `0600` or
  narrower, or root-owned with kista's group and mode `0440` or narrower. A relative link inside
  the file's directory is followed. It is for development against the emulator.
- Credentials are cached, and refreshed five minutes (jittered) before they expire. Nothing is
  fetched at startup.
- STS, KMS and the platform's credential endpoints are reached through a client that ignores proxy
  environment variables, requires TLS 1.2 or later and never follows redirects.

Outside `profile: dev`, kista refuses to start while any of these steering variables is set. kista
would not use them, but whoever set one expects it to apply:

- `AWS_ACCESS_KEY_ID`, `AWS_ACCESS_KEY`, `AWS_SECRET_ACCESS_KEY`, `AWS_SECRET_KEY`,
  `AWS_SESSION_TOKEN`, `AWS_CONTAINER_AUTHORIZATION_TOKEN`;
- `AWS_PROFILE`, `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`, `AWS_CA_BUNDLE`;
- `AWS_EC2_METADATA_SERVICE_ENDPOINT`, `AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE`;
- `AWS_USE_FIPS_ENDPOINT`, `AWS_USE_DUALSTACK_ENDPOINT` (use `fips` in the configuration);
- `AWS_ENDPOINT_URL` and every `AWS_ENDPOINT_URL_*`.

`AWS_REGION`, `AWS_DEFAULT_REGION`, `AWS_STS_REGIONAL_ENDPOINTS` and `AWS_ROLE_SESSION_NAME` are
ignored: the region and the session name come from the configuration.

### Partitions

The partition follows from the region's prefix. It is used to build the key's ARN and to check
every `role_arn`.

| Region prefix | Partition |
| --- | --- |
| `cn-` | `aws-cn` |
| `us-gov-` | `aws-us-gov` |
| `us-iso-` | `aws-iso` |
| `us-isob-` | `aws-iso-b` |
| `eu-isoe-` | `aws-iso-e` |
| `us-isof-` | `aws-iso-f` |
| `eusc-` | `aws-eusc` |
| any other | `aws` |

The ARN `DescribeKey` returns must equal the one kista built, so a wrong partition fails closed.

### What the key must look like

kista names the key by its ARN, `arn:<partition>:kms:<region>:<account>:key/<key-id>`, built from
the configuration and the reference only. It reads `DescribeKey`, `ListResourceTags` and
`GetPublicKey`, and refuses the key unless:

- the described `Arn` equals that ARN, and `AWSAccountId` equals `account`;
- `KeySpec` is `RSA_2048` and `KeyUsage` is `SIGN_VERIFY`;
- `KeyState` is `Enabled`;
- `Origin` is `AWS_KMS`: no imported material (`EXTERNAL`), no CloudHSM or external key store;
- it is not in a custom key store;
- it is not a multi-Region key, unless `allow_multi_region` is set;
- it has no `ValidTo` (an expiry, which only imported material has);
- it carries the tag `kista-purpose` with its role's value: `channel-signing` for a channel key,
  `attestation-signing` for an [attestation key](attachments.md);
- `GetPublicKey` reports `SIGN_VERIFY`, lists `RSASSA_PKCS1_V1_5_SHA_256`, and returns an RSA
  public key, the same one at every re-check.

**Another account.** `ListResourceTags` does not work across accounts, so the tag of a key in
another account cannot be read. Such a key is reachable only through `awskms.role_arn`, a role in
the key's account, with `awskms.account` set to that account. Without it the key is refused.

Signing is `Sign` with `MessageType = DIGEST` and `SigningAlgorithm = RSASSA_PKCS1_V1_5_SHA_256`.
The answer must name the same algorithm, and its `KeyId` must be the key's ARN. Each call makes at
most 3 attempts. An error keeps the AWS error code and the request id only; a failure without an
answer (no network, no credentials) is one fixed message.

```bash
aws kms create-key --region eu-central-1 \
  --key-spec RSA_2048 --key-usage SIGN_VERIFY \
  --description "kista channel key acme/prod" \
  --tags TagKey=kista-purpose,TagValue=channel-signing
```

The reference is `aws:<key-id>`, with the `KeyId` from the answer's `KeyMetadata`:

```bash
kista admin -config kista.yaml key check -signer aws:1234abcd-12ab-34cd-56ef-1234567890ab
```

### Permissions

kista needs to read the key and sign with it, nothing more: `kms:DescribeKey`,
`kms:ListResourceTags`, `kms:GetPublicKey` and `kms:Sign`, on the key's ARN.

```json title="kista-kms-policy.json"
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "kms:DescribeKey",
        "kms:ListResourceTags",
        "kms:GetPublicKey",
        "kms:Sign"
      ],
      "Resource": "arn:aws:kms:eu-central-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"
    }
  ]
}
```

- Attach it to kista's role (the IRSA or Pod Identity role, the task role, or the instance
  profile's role). The key policy must let IAM policies grant access (the default key policy does)
  or grant the same actions itself.
- With `awskms.role_arn`, attach the policy to that role instead. Its trust policy must allow
  kista's identity, which needs `sts:AssumeRole` on it.
- Keep `kms:TagResource`, `kms:UntagResource`, `kms:CreateKey`, `kms:ImportKeyMaterial` and
  `kms:PutKeyPolicy` away from kista's role: whoever sets the tag decides which keys kista accepts.

## Google Cloud KMS

A `gcpkms` source signs with an asymmetric key in one Cloud KMS key ring, through Cloud KMS's REST
API (no Google SDK), `RSA_SIGN_PKCS1_2048_SHA256` over the digest. Requests go to
`https://cloudkms.googleapis.com/v1/` only; regional and REP endpoints are not supported yet.

```yaml title="kista.yaml"
signers:
  sources:
    - name: gcp
      kind: gcpkms
      gcpkms:
        key_ring: projects/acme-kista/locations/europe-west4/keyRings/kista
        principal: kista-signer@acme-kista.iam.gserviceaccount.com
        require_hsm: true
        credentials_file: ""         # empty: the metadata server; or an external_account file
      allow: ["ext-"]
```

### Settings

| Setting | Meaning |
| --- | --- |
| `gcpkms.key_ring` | required; `projects/<project id>/locations/<location>/keyRings/<ring>`. The project is named by its id (`[a-z][a-z0-9-]{4,28}[a-z0-9]`), not its number: names are compared exactly, and KMS echoes the id. The location is `[a-z0-9-]{1,63}`, the ring `[A-Za-z0-9_-]{1,63}` |
| `gcpkms.principal` | the service account kista acts as, `<name>@<project>.iam.gserviceaccount.com`; required outside `profile: dev`. Google-managed accounts (such as the Compute Engine default account) are refused |
| `gcpkms.require_hsm` | default `true`: protection level `HSM` or `HSM_SINGLE_TENANT`. `false` also accepts `SOFTWARE`, and is allowed only with `profile: dev`. `EXTERNAL` and `EXTERNAL_VPC` are always refused |
| `gcpkms.credentials_file` | an absolute path. Absent: the metadata server. Otherwise an `external_account` file, or, with `profile: dev` only, a `service_account` key file |
| `gcpkms.endpoint_url` | the tests' fake only: `profile: dev`, an `http` or `https` URL on a loopback host, and no `credentials_file`. No credential is loaded; requests carry a fixed test token, so no real token reaches the fake |

`allow` lists key-id prefixes, as for Vault. `identity` is refused: a `gcpkms` source is reached
through `principal` and `credentials_file`.

### Credentials

kista builds the credentials from the configuration. It never looks up Application Default
Credentials (no `~/.config/gcloud`, no well-known file).

- **The metadata server** (`credentials_file` absent): GKE Workload Identity, or a Compute Engine
  VM's service account. kista asks the server itself, at its fixed address
  `http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/`, with
  `Metadata-Flavor: Google`; the answer must carry the same header and is read up to 64 KiB. At
  every token refresh, kista first reads the account's `email` and refuses to sign or store unless it equals
  `principal`: a changed annotation could otherwise hand out another account. On GKE without Workload Identity, or in a `hostNetwork` pod, the server would
  otherwise hand out the node's account. IPv6-only Compute Engine is not supported yet.
- **An `external_account` file**: workload identity federation, for kista outside Google Cloud
  (AKS, EKS, any cluster with an OIDC issuer). kista parses the file strictly and builds the token
  source from the parsed parts only: the Google library opens no file and calls nothing but STS and
  IAM Credentials. The rules are below.
- **A `service_account` key file**: only with `profile: dev`. Its `token_uri` must be
  `https://oauth2.googleapis.com/token`, its `universe_domain` absent or `googleapis.com`, and, when
  `principal` is set, its `client_email` must equal it.
- **Every other type is refused**: `authorized_user` (a gcloud login),
  `external_account_authorized_user`, `impersonated_service_account`, `gdch_service_account`.

The credentials file is public material ([Files kista reads](security.md#files-kista-reads)): a
regular file owned by root or by kista's user, that nobody but its owner may write (no group or
other write bit), at most 64 KiB. Its links are followed, as in a Kubernetes ConfigMap's
projection. An `external_account` file holds no secret, but whoever may edit it chooses kista's
identity. With `profile: dev` it may be a `service_account` key, which holds a private key, yet it
is checked as public: a gap accepted in dev only.

**What an `external_account` file must hold.** An unknown field is refused.

| Field | Accepted |
| --- | --- |
| `type` | `external_account` |
| `audience` | a workload identity pool provider: `//iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>`, pool and provider `[a-z0-9-]{4,32}` |
| `subject_token_type` | `urn:ietf:params:oauth:token-type:jwt` |
| `token_url` | `https://sts.googleapis.com/v1/token` |
| `token_info_url` | absent, or `https://sts.googleapis.com/v1/introspect` |
| `service_account_impersonation_url` | exactly `https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/<principal>:generateAccessToken`; required outside `profile: dev` |
| `service_account_impersonation.token_lifetime_seconds` | absent, or `600` to `3600` |
| `universe_domain` | absent, or `googleapis.com` |
| `quota_project_id` | any |
| `credential_source` | `file`, an absolute path, with `format` absent, `text`, or `json` with `subject_token_field_name` |
| `workforce_pool_user_project`, `client_id`, `client_secret` | refused: workforce pools and client credentials are for people, not kista |

- **Only `file` credential sources.** The file is a Kubernetes projected ServiceAccount token whose
  audience is the provider. kista reads it at every exchange, through a relative link inside the
  token's directory; it must be owned by root or kista's user (any read bits; nobody but the
  owner may write it). An empty file, or one holding more than one token, is refused.
- **`aws1` and `url` sources are refused.** On EKS and AKS they answer with the node's identity
  (EC2's instance role, the Azure VM's identity), which every pod on the node shares.
- **`executable` and `certificate` sources are refused.** An executable source runs a program the
  file names; certificate (X.509) federation is not supported.

**Steering variables.** Outside `profile: dev`, kista refuses to start while any of these is set:

- `GOOGLE_APPLICATION_CREDENTIALS`, `GCE_METADATA_HOST`, `GCE_METADATA_IP`, `GCE_METADATA_ROOT`;
- `STORAGE_EMULATOR_HOST`, `GOOGLE_API_USE_CLIENT_CERTIFICATE`, `GOOGLE_API_USE_MTLS_ENDPOINT`;
- `GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES`, `CLOUDSDK_CONFIG`;
- `GOOGLE_CLOUD_UNIVERSE_DOMAIN`, unless it is `googleapis.com`.

`GOOGLE_CLOUD_PROJECT` is ignored.

- **Scope.** Tokens are asked for with `https://www.googleapis.com/auth/cloud-platform`: IAM is the
  limit, not scopes. A Compute Engine VM needs that access scope.
- **Refresh.** Tokens are cached and refreshed 225 seconds before they expire, since the metadata
  server itself hands out a cached token until about five minutes are left. Nothing is fetched at
  startup, and a slow metadata server or STS never holds a call past its deadline.
- **The client** ignores proxy environment variables, never follows redirects, and requires TLS 1.2
  or later. The link-local metadata address is the only plain-http request; no Google token is ever
  sent over http.

### What the key must look like

kista reads the key version, the key and the public key, and refuses the key unless:

- the version's `name` is its own resource name, and its `state` is `ENABLED`;
- its `algorithm` is `RSA_SIGN_PKCS1_2048_SHA256`;
- its `protectionLevel` is `HSM` or `HSM_SINGLE_TENANT` (also `SOFTWARE` with `require_hsm: false`);
- it has no `importJob` (imported material is refused) and no `externalProtectionLevelOptions`;
- the key's `purpose` is `ASYMMETRIC_SIGN`, and `importOnly` is false;
- the key carries the label `kista-purpose` with its role's value: `channel-signing` for a channel
  key, `attestation-signing` for an [attestation key](attachments.md);
- `getPublicKey` names the same version, with the version's `algorithm` and `protectionLevel`; its
  `pemCrc32c` matches the PEM (CRC32C); the PEM is an RSA public key, the same one at every
  re-check.

Signing is `POST <version>:asymmetricSign` with `digest.sha256` and `digestCrc32c`. The answer must
have `verifiedDigestCrc32c: true`, a `signatureCrc32c` that matches the signature, a `name` equal to
the version, and an accepted protection level. A CRC mismatch is retried once, as Google advises,
then refused. Each call makes at most 3 attempts, on `429`, `5xx` and connection failures. An error
keeps the HTTP status and Google's status word (`PERMISSION_DENIED`), nothing else.

```bash
gcloud kms keyrings create kista --project acme-kista --location europe-west4
gcloud kms keys create ext-prod --project acme-kista --keyring kista --location europe-west4 \
  --purpose asymmetric-signing --default-algorithm rsa-sign-pkcs1-2048-sha256 \
  --protection-level hsm --labels kista-purpose=channel-signing
```

The reference is `gcp:ext-prod/1`, with the version number:

```bash
kista admin -config kista.yaml key check -signer gcp:ext-prod/1
```

### Permissions

kista needs to read the key and sign with it, nothing more. Make a custom role with exactly four
permissions and grant it to kista's service account on each key. `roles/cloudkms.signer` alone is
not enough: it cannot read the key or its public key.

```bash
gcloud iam roles create kistaSigner --project acme-kista --title "kista signer" --permissions \
  cloudkms.cryptoKeyVersions.useToSign,cloudkms.cryptoKeyVersions.viewPublicKey,cloudkms.cryptoKeyVersions.get,cloudkms.cryptoKeys.get

gcloud iam service-accounts create kista-signer --project acme-kista
gcloud kms keys add-iam-policy-binding ext-prod --project acme-kista --keyring kista --location europe-west4 \
  --member serviceAccount:kista-signer@acme-kista.iam.gserviceaccount.com \
  --role projects/acme-kista/roles/kistaSigner
```

Keep `cloudkms.cryptoKeys.update` (it changes labels), `cloudkms.cryptoKeys.create`,
`cloudkms.cryptoKeyVersions.create`, the import-job permissions and `setIamPolicy` away from kista's
account: whoever sets the label decides which keys kista accepts.

### GKE Workload Identity

Bind kista's Kubernetes ServiceAccount to the service account, and leave `credentials_file` empty:

```bash
gcloud iam service-accounts add-iam-policy-binding kista-signer@acme-kista.iam.gserviceaccount.com \
  --role roles/iam.workloadIdentityUser \
  --member "serviceAccount:acme-kista.svc.id.goog[kista/kista]"
kubectl annotate serviceaccount kista --namespace kista \
  iam.gke.io/gcp-service-account=kista-signer@acme-kista.iam.gserviceaccount.com
```

The metadata server then reports that account, and kista compares it with `principal`. kista needs
a service account to compare: granting roles to the Kubernetes ServiceAccount directly, without the
annotation, does not give it one.

### Outside Google Cloud: AKS and EKS

Federate the cluster's OIDC issuer with a workload identity pool, let the Kubernetes
ServiceAccount impersonate kista's service account, and produce an `external_account` file whose
credential source is a projected token:

```bash
ISSUER=$(az aks show -g kista-rg -n kista-aks --query oidcIssuerProfile.issuerUrl -o tsv)
# EKS: aws eks describe-cluster --name kista-eks --query cluster.identity.oidc.issuer --output text
NUMBER=$(gcloud projects describe acme-kista --format 'value(projectNumber)')

gcloud iam workload-identity-pools create kista --project acme-kista --location global
gcloud iam workload-identity-pools providers create-oidc kista-aks --project acme-kista --location global \
  --workload-identity-pool kista --issuer-uri "$ISSUER" \
  --attribute-mapping google.subject=assertion.sub
gcloud iam service-accounts add-iam-policy-binding kista-signer@acme-kista.iam.gserviceaccount.com \
  --role roles/iam.workloadIdentityUser \
  --member "principal://iam.googleapis.com/projects/$NUMBER/locations/global/workloadIdentityPools/kista/subject/system:serviceaccount:kista:kista"

gcloud iam workload-identity-pools create-cred-config \
  projects/$NUMBER/locations/global/workloadIdentityPools/kista/providers/kista-aks \
  --service-account kista-signer@acme-kista.iam.gserviceaccount.com \
  --credential-source-file /var/run/secrets/gcp/token \
  --output-file gcp.json
kubectl create configmap kista-gcp --namespace kista --from-file gcp.json
```

Mount the file and a projected token whose audience is the provider (the provider's default
allowed audience is its full name with `https:` in front):

```yaml
volumes:
  - name: gcp-token
    projected:
      sources:
        - serviceAccountToken:
            path: token
            audience: https://iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/kista/providers/kista-aks
            expirationSeconds: 3600
  - name: gcp-config
    configMap: { name: kista-gcp }
# kista's container mounts gcp-token at /var/run/secrets/gcp and gcp-config at /etc/kista/gcp
```

Then set `credentials_file: /etc/kista/gcp/gcp.json`. Do not use `--aws` or `--azure` with
`create-cred-config`: they make `aws1` and `url` sources, which kista refuses.

`deploy/gcp/up.sh` builds all of this but the cluster side in a disposable project: a key ring with
a channel key and an attestation key, the service account and its custom role, a private bucket,
and optionally a pool for GitHub Actions. `deploy/gcp/down.sh` deletes the project. See
[Development](development.md#a-disposable-gcp-stack).

## Kubernetes Secrets

:::danger The weakest source, chosen explicitly

A `kubesecret` source signs with RSA keys held in a Kubernetes Secret. Its protection is the
cluster's: whoever reads Secrets in kista's namespace, or etcd's disks unencrypted, has the keys,
and with them can sign extensions that every DuckDB client of the channel accepts. There is no
HSM, no non-exportability and no usage log. Prefer a [KMS](#key-sources) or
[Vault Transit](#hashicorp-vault-and-openbao-transit); use this source only where neither exists
(OTC, Hetzner, IONOS, STACKIT without a Vault or OpenBao), and read the whole section first.

:::

It is opt-in three times over: the source is configured, it carries a required acknowledgment,
and kista checks at every open that the files really come from a Secret volume. The `file` signer
does not change: a Secret mounted into `signers.file_dir` is still refused.

```yaml title="kista.yaml"
signers:
  sources:
    - name: k8s
      kind: kubesecret
      allow: ["ext-"]                       # key-name prefixes, as for every source
      kubesecret:
        dir: /etc/kista/signing/prod        # the Secret volume's mountPath
        etcd_encryption_at_rest: true       # required outside profile dev
```

| Setting | Meaning |
| --- | --- |
| `kubesecret.dir` | the Secret volume's `mountPath`: absolute and clean. It must not overlap `signers.file_dir` or another `kubesecret` source's directory, and must not hold another secret file the configuration names (the database password, a Vault token). One Secret volume per source |
| `kubesecret.etcd_encryption_at_rest` | must be `true` outside `profile: dev`: your statement that the cluster encrypts Secrets at rest. kista cannot check it; it logs it at every start |

A reference is `<source>:<name>` and opens `<dir>/<name>`. The name is a Secret data key without
dots, `[A-Za-z0-9_-]{1,63}` (never `..data` or a timestamped directory), and passes `allow`. The
name is the version: a key is never replaced under the same name.

### What kista checks

At every open, and again every `recheck` (default and at most `10m`) while the key is in use:

1. **The mount** (Linux only; on other platforms the kind is refused with an error saying so):
   `/proc/self/mountinfo` shows `dir` as a mount point, `tmpfs`, mounted read-only. The kubelet
   mounts Secret volumes (and projected volumes) so; a ConfigMap volume (on the node's disk), a
   `hostPath`, a PVC or a writable `emptyDir` with `medium: Memory` are refused.
2. **The layout** of the kubelet's atomic writer: `<dir>/..data` is a relative link to a
   `..<timestamp>` directory beside it, and `<dir>/<name>` is the relative link `..data/<name>`.
   The file is opened inside `dir`, so no link may leave it, and an absolute link is refused.
3. **The file**: a regular file with one hard link, owned by root with kista's group (not 0), mode
   `0440` or narrower: a Secret volume's file under `fsGroup`. A file owned by kista's own user is
   refused here: it is not a Secret volume's.
4. **The key**: one unencrypted PKCS#8 or PKCS#1 RSA key, 2048 bits, `e = 65537`, at most 64 KiB,
   as for [file keys](#file-keys); then [the contract every key meets](#the-contract-every-key-meets),
   including the public key and fingerprint against the registered key.

A different key under the same name, a file that is gone, or a mount that no longer fits stops
the key from signing. Checks 1 and 2 catch a misconfiguration; they are no defence against whoever
controls the pod spec. The parsed key stays in kista's memory, as a file key does; it is never
logged or written anywhere.

A projected volume passes the same checks. Use one only with Secret sources: a ConfigMap source in
it would put the key in a ConfigMap. The chart mounts plain Secret volumes (`secretVolumes`).

A `kubesecret` source holds channel keys only: an attestation key through it is refused.

### Making it visible

- `kista serve` logs a warning at every start for each `kubesecret` source, with its `dir` and the
  acknowledgment.
- Each start records a `signer.source.weak` event per source (subject `source:<name>`), so an
  auditor finds it without reading the configuration.
- `key add` records `key.add` with `protection: kubesecret`; `key check`, `key list` and the
  management API show the protection tier (see [Registering a key](#registering-a-key)).
- The `.well-known` document does not show it: clients trust the channel's key; how it is stored
  is the operator's business.

### What you take on

- **Etcd encryption at rest is required.** On a cluster whose control plane you run, configure an
  `EncryptionConfiguration`, ideally with a KMS provider; on k3s, start the servers with
  `--secrets-encryption`. On a managed cluster, check what the provider does: it is often not
  stated, and kista's documentation verifies no provider's default for you.
- **RBAC on Secrets in kista's namespace is access to the key.** That covers `get`, `list` and
  `watch` on Secrets; the right to create pods, Jobs or CronJobs there (a pod can mount any Secret
  of its namespace); `exec` into kista's pods; and the nodes, since a kubelet holds the Secrets of
  its pods.
- **Copies.** Backups (Velero, etcd snapshots), GitOps sources (Sealed Secrets, SOPS) and
  `kubectl get -o yaml` output are copies of the key. An API server audit policy that logs `get`
  and `list` on Secrets in the namespace is the only record of a read.
- **No HSM, no non-exportability, no usage log** of signatures beyond kista's own events.
- **A compromise** means anyone holding the key can sign extensions the channel's clients accept.
  Rotate (below) and retire the old key at once; the minimums of
  [Key rotation](administration/key-rotation.md#emergency-revocation) apply outside dev.

### Rotation

kista's rotation (trusted, then active, then retired) is unchanged:

1. Add the new key under a **new** data key name, in the source's Secret (one Secret volume per
   source), for example `ext-prod-2027`.
2. `kista admin key add acme/prod -signer k8s:ext-prod-2027`, and activate it after
   `rotation.min_trusted`.
3. Remove the old data key only once its key is retired: a reference that no longer resolves fails
   closed, and a retired key signs nothing.

Never rewrite a key under the same name: the next check refuses it by its fingerprint, and the
channel stops signing.

```bash
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out ext-prod-2027.pem
kubectl -n kista create secret generic kista-signing-prod \
  --from-file=ext-prod=ext-prod.pem --from-file=ext-prod-2027=ext-prod-2027.pem \
  --dry-run=client -o yaml | kubectl apply -f -
shred -u ext-prod-2027.pem
```

The kubelet updates a mounted Secret within a minute or so; `key check` shows when the new name
resolves. The [Kubernetes page](deployment/kubernetes.md#signing-keys-from-a-secret) shows the
chart's values.

## Registering a key

A key becomes a channel's key in two steps: check it, then add it.

```bash
# open it, run every check, sign a probe; nothing is stored
kista admin -config kista.yaml key check -signer prod:ext-prod/0123456789abcdef0123456789abcdef

# register it in a channel: trusted, or -active for a channel that never had a key
kista admin -config kista.yaml key add acme/prod -signer prod:ext-prod/0123456789abcdef0123456789abcdef
```

`key check` prints the reference, the key's fingerprint and its protection tier
(`file:dev.pem sha256:… protection=file`). `key add`:

1. opens the reference and checks the key (RSA-2048, `e = 65537`, the contract above);
2. signs a probe (`SHA-256("kista key probe")`) and verifies it;
3. refuses a fingerprint already registered anywhere on the server;
4. refuses more than 16 non-retired keys in the channel, or a `.well-known` document over 64 KiB;
5. stores the public key and the reference, never the private key, and records a `key.add` event
   (with the source's scheme and the protection tier, never the full reference).

The new key starts `trusted`. Only a channel that never had a key may take its first key as
`active` directly; afterwards, activation waits `rotation.min_trusted` (see
[Key rotation](administration/key-rotation.md)). A passthrough channel holds no keys.

The same is available over the API to a server administrator only:
`POST /api/v1/tenants/{t}/channels/{c}/keys` with `{"signer_ref": "...", "active": false}`. A
reference that cannot be opened, or whose key is refused, answers `400`; the reason is in the
server log. See [HTTP API](api.md).

| Command | Purpose |
| --- | --- |
| `kista admin key check -signer <ref>` | open, check, probe; stores nothing |
| `kista admin key add <tenant>/<channel> -signer <ref> [-active]` | register a channel key |
| `kista admin key list <tenant>/<channel>` | id, fingerprint, state, signer reference, protection tier, dates |
| `kista admin key activate <tenant>/<channel> <key-id\|fingerprint> [-force]` | make it the signing key |
| `kista admin key retire <tenant>/<channel> <key-id\|fingerprint> [-force]` | remove it from `.well-known` |
| `kista admin key events <tenant>/<channel>` | the key's state transitions |
| `kista admin key resign <tenant>/<channel>` | sign releases with the active key; move the serving key |
| `kista admin attestation-key add <tenant> <signer-ref> [-active]` | register a tenant's attestation key |

`key check` opens a key for the channel purpose, so on Azure, AWS and Google Cloud it refuses a key
tagged or labelled `attestation-signing`. The API's key objects carry `protection` next to
`signer_ref`, for server administrators only.

## Fingerprints

A key's fingerprint is `sha256:` followed by the lowercase hex SHA-256 of its public key in SPKI
DER form. It is what `key list` and the API show, what events record, what `activate` and `retire`
accept in place of a key id, and what upstreams and feeds pin.

```bash
openssl pkey -in key.pem -pubout -outform DER | sha256sum
```

## Purposes

One key holds one role. Two mechanisms keep the roles apart:

- **The fingerprint registry.** A fingerprint exists once on the server, across channel keys and
  attestation keys. The same key can never serve two channels, two tenants, or a channel and a
  feed.
- **The purpose tag**, where the backend has tags. On Azure and AWS the `kista-purpose` tag, and on
  Google Cloud the `kista-purpose` label, must be `channel-signing` for a channel key and
  `attestation-signing` for an attestation key. A file key
  and a Vault Transit key have no tags and rely on the registry. A `kubesecret` source opens
  channel keys only.

Attestation keys sign the statements a tenant's feed serves; they live in the same sources. See
[Attachments](attachments.md).

## Monitoring

kista's policy gives it no right to change keys. Watch the backend's own audit log for changes to
them: the Vault audit device, the Azure Activity Log (key create, import, update, delete), AWS
CloudTrail (`CreateKey`, `ImportKeyMaterial`, `TagResource`, `UntagResource`, `PutKeyPolicy`,
`DisableKey`, `ScheduleKeyDeletion`), Google Cloud Audit Logs for Cloud KMS (`CreateCryptoKey`,
`CreateCryptoKeyVersion`, `ImportCryptoKeyVersion`, `UpdateCryptoKey`, `DestroyCryptoKeyVersion`,
`SetIamPolicy`; Admin Activity logs are always on). On kista's
side, the events `key.add`, `key.activate`, `key.retire`, `key.resign` and
`attestation_key.add|activate|retire` record every change (see [Observability](observability.md)).
A `kubesecret` source has no backend log: an API server audit policy logging reads of Secrets in
kista's namespace is the only record, and `signer.source.weak` marks every start with such a
source.
