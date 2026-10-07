# Spec 0004: Signer backends: key sources for vaults and KMSs

- **Status**: draft
- **Date**: 2026-10-07
- **Author**: vgsml, Claude

## Summary

A channel key's private half lives in a vault or KMS. kista signs through it and never holds the
key. This spec makes the key store pluggable:

- **Named key sources in config.** A signer reference names a source and a key inside it:
  `<source>:<key>`. A reference never carries a host. The administrator configures where each
  source points, and a reference only picks one.
- **Backends behind spec 0002's `Signer` interface:**
  - `file` (exists; development);
  - `vault`: HashiCorp Vault and OpenBao Transit;
  - `azurekv`: Azure Key Vault and Managed HSM;
  - `awskms`: AWS KMS;
  - `gcpkms`: Google Cloud KMS.

  PKCS#11 hardware HSMs come later through the same interface. Kubernetes Secrets are file keys,
  not a backend.
- **The same contract on every backend.**
  - The key is RSA-2048 with `e = 65537`, can only sign, and is neither exportable nor imported.
  - Its version is pinned, and it carries a purpose marker where the backend supports one.
  - It is proven by a probe signature when added, and every signature it makes is checked:
    against the public key and against the key identity the backend reports back.
  - Credentials come only from the platform. Errors carry codes, never bodies.

Delivery is phased, one PR per step:
1. the source registry, the contract, `file`, `vault` (Vault and OpenBao), and `kista admin key
   check`;
2. `azurekv`;
3. `awskms`;
4. `gcpkms`.

Each phase has its own three review passes. The spec becomes `implemented` after phase 4.

The design follows tresor-server's key and material backends (its specs 007, 008 and 012). kista
reuses the design only: tresor-server is under BUSL-1.1, and none of its code is copied.

## Problem

Spec 0003 stores keys by reference, but only `file:` resolves. Production keys must live where each
deployment keeps its keys:
- Azure Key Vault on the Azure Marketplace;
- AWS KMS or Google Cloud KMS on those clouds;
- Vault or OpenBao on Kubernetes, in sovereign clouds and on air-gapped sites.

kista must never depend on one of them.

## Design

### Key sources and references

```yaml
signers:                          # the whole block is file-only (no KISTA_* overrides)
  allow_file: false
  file_dir: /etc/kista/keys
  sources:
    - name: bao                   # [a-z][a-z0-9-]{0,15}; reserved: file, vault, azurekv, awskms,
      kind: vault                 #   gcpkms, pkcs11, http, https, arn, projects
      vault:
        address: https://bao.internal:8200
        namespace: ""             # Vault Enterprise / OpenBao namespaces
        mount: transit
        ca_file: /etc/kista/bao-ca.pem
        auth: { kind: kubernetes, role: kista, token_file: /var/run/secrets/kista/vault-token }
        software_keys: true       # required acknowledgment: Transit keys are software keys
      allow: ["ext-"]             # key-name prefixes; "*" = any; empty = none
      max_concurrency: 4
    - name: prod
      kind: azurekv
      azurekv: { vault: kista-prod, cloud: public }    # or managed_hsm: kista-hsm; cloud: public|china|usgov
      identity: { kind: workload, client_id: "…", tenant_id: "…" }
      allow: ["ext-"]
    - name: aws
      kind: awskms
      awskms: { region: eu-central-1, account: "123456789012", role_arn: "", allow_multi_region: false }
      identity: { kind: pod_identity }                 # pod_identity | irsa | imds | ecs
      allow: ["1234abcd-12ab-34cd-56ef-1234567890ab"]  # key ids; "*" = any (with the purpose tag)
    - name: gcp
      kind: gcpkms
      gcpkms: { key_ring: projects/p/locations/europe-west4/keyRings/kista }
      allow: ["ext-"]
```

**Config validation (static; kista starts even when a source is unreachable):**
- each source has exactly one kind block, matching `kind`;
- names are unique, lowercase and not reserved;
- `allow` is not empty;
- `vault` sources set `software_keys: true` outside dev, which is logged at every start.

**References.** The source name runs up to the first `:`. The rest goes to that kind's parser:

| Kind | Reference | Key name rule | Version rule | Resolves to |
| --- | --- | --- | --- | --- |
| `file` (built in) | `file:<name>` | one path element | the file | `<file_dir>/<name>` (spec 0003) |
| `vault` | `<src>:<key>:v<N>` | `[A-Za-z0-9_-]{1,128}` | `N ≥ 1`, no leading zeros (`v0` would mean "latest") | `POST /v1/<mount>/sign/<key>/sha2-256`, `key_version = N` |
| `azurekv` | `<src>:<name>/<version>` | `[0-9A-Za-z-]{1,127}`, matched lowercased | `[0-9a-f]{32}` (empty would mean "latest") | `https://<vault>.vault.<cloud suffix>/keys/<name>/<version>`, or `https://<hsm>.managedhsm.<cloud suffix>/…` |
| `awskms` | `<src>:<key-id>` | a UUID, or `mrk-<32 hex>` only with `allow_multi_region` | the key id (asymmetric key material never rotates) | `arn:<partition from region>:kms:<region>:<account>:key/<key-id>` |
| `gcpkms` | `<src>:<key>/<version>` | `[A-Za-z0-9_-]{1,63}` | `N ≥ 1` | `<key_ring>/cryptoKeys/<key>/cryptoKeyVersions/<N>` |

- **Where requests go.** Hosts, accounts, regions, key rings, mounts and namespaces come only from
  config. URLs and ARNs are built from parsed parts, never from reference text.
- **What a source may use.** `allow` holds plain string prefixes, matched after the key name passes
  its rule, so `ext-` admits `ext-a` but no name with a separator. AWS key ids carry no meaning, so
  AWS allows listed ids, or `*` together with the purpose tag. Aliases are refused because they can
  be retargeted.
- **A renamed or removed source** leaves its references unresolvable. They fail closed and never
  fall back to another source. `OpenSigner` (spec 0003) also catches a source re-pointed at another
  key: the public key and fingerprint must match the stored row.
- `ID()` returns the reference (`<source>:<key>`) for every backend, never a path or a URL.
- The `file:` references stored under spec 0003 keep working through the built-in `file` source.

### Common contract

A source-independent wrapper (`keysource.Open`) checks every backend:

- **At open.**
  - The public key comes from the backend and is RSA, 2048 bits, `e = 65537`.
  - The backend's metadata shows the key:
    - enabled and inside its validity window;
    - usable for signing only;
    - not exportable, not imported or external;
    - at the pinned version;
    - carrying the purpose marker `kista-purpose = channel-signing`, where the backend has tags or
      labels (Azure tag, AWS tag, GCP label on the CryptoKey).

  Anything that cannot be confirmed fails closed.
- **Re-checked later.** Some properties can change after open: Vault `exportable` can be switched
  on, Azure `key_ops` can be updated, an AWS key can be disabled. The metadata is re-read at least
  every 10 minutes and whenever the channel's cached signer is revalidated (spec 0003's channel
  version). A key that no longer passes stops signing. The deployment's policy grants kista no
  right to change keys, and the documentation lists the audit alerts to set (CloudTrail, Azure
  Activity Log, Vault audit).
- **On every `Sign`:**
  - the 32-byte body hash goes out as a precomputed SHA-256 digest;
  - the result must be 256 bytes and verify against the public key from open;
  - **the backend's answer must name the pinned key**:
    - Azure: `kid`, the host and name matched case-insensitively, the version exactly;
    - AWS: `KeyId` equals the built ARN;
    - GCP: `name` equals the version resource, and the CRC32C fields check;
    - Vault: `key_version` equals N, and the `vault:v<N>:` prefix agrees.
- **Calls.**
  - Every call has a timeout (default 10 s) and at most 3 attempts in total, counting the SDK's
    own retries and the GCP CRC retry. Throttling uses bounded backoff.
  - Each source has a concurrency limit (`max_concurrency`, default 4).
  - Signing happens once per (body hash, key), when a release is made (spec 0008), and the
    signature is stored. No request path triggers KMS calls.
- **Errors** are mapped at the package boundary to the backend's code and request id: HTTP status
  plus Azure error code, the AWS error code, the gRPC or REST status, the Vault status. They never
  include response bodies, principals, tokens or key material, and raw SDK errors are never
  wrapped. Authentication failures become one fixed message per backend. A test scans error
  strings and logs for planted values.
- **Readiness.**
  - Each source registers a health check. It reads an optional `health_key`'s metadata, or reports
    the last open or sign outcome. It never signs, runs at most once per 60 s, and is single-flight.
  - `/readyz` (spec 0006) reports a boolean. Source names, hosts and codes go to admin endpoints and
    metrics.
  - A source that is down makes signing and `key add` fail. Serving and `.well-known` keep working,
    because they use stored public keys.
- **Metrics labels** for later telemetry: `source`, `kind`, `op`, `code`, and latency.
- **Known answer.** Every backend's contract test compares a signature with
  `rsa.SignPKCS1v15(key, crypto.SHA256, digest)` over the same digest. This is what DuckDB checks.

### Protection tiers

| Kind | Default outside dev | Notes |
| --- | --- | --- |
| `azurekv` | `RSA-HSM` only (`require_hsm: true`) | needs a Premium vault or a Managed HSM |
| `gcpkms` | protection level `HSM` or `HSM_SINGLE_TENANT` (`require_hsm: true`) | `SOFTWARE`, `EXTERNAL` and `EXTERNAL_VPC` are refused unless allowed |
| `awskms` | `Origin = AWS_KMS` | all AWS_KMS keys are HSM-backed; `EXTERNAL` (imported) and external key stores are refused |
| `vault` | software keys, with the explicit `software_keys: true` | Transit keys live in Vault's barrier |
| `file` | dev or `allow_file` | spec 0003 |

### Backends

**`vault`: HashiCorp Vault and OpenBao Transit.** A thin HTTP client (`internal/vaultapi`), with no
SDK.

*Key checks.* `GET /v1/<mount>/keys/<key>` must show:
- `type = rsa-2048`, `supports_signing`;
- `exportable = false`, `allow_plaintext_backup = false`, `imported_key = false`;
- `min_encryption_version ≤ N ≤ latest_version`, with N listed under `keys` (so
  `N ≥ min_decryption_version`). Signing is gated by `min_encryption_version`.

The public key is `keys["N"].public_key`.

*Signing only, by policy.* RSA Transit keys always report encryption support, so metadata cannot
prove "signing only". At open, kista calls `POST /v1/sys/capabilities-self`. Its token must have no
capability on:
- `<mount>/encrypt/<key>` and `<mount>/decrypt/<key>`;
- `<mount>/export/*` and `<mount>/backup/<key>`;
- `<mount>/keys/<key>/config`.

It needs only `read` on `<mount>/keys/<key>` and `update` on `<mount>/sign/<key>/sha2-256`.

*Signing.* `POST /v1/<mount>/sign/<key>/sha2-256` with:
- `input`: the base64 digest;
- `prehashed = true`;
- `signature_algorithm = pkcs1v15`;
- `key_version = N`.

Vault and OpenBao then run `rsa.SignPKCS1v15(…, crypto.SHA256, digest)`. The `/sha2-256` path
algorithm must never be `none`, which would drop the DigestInfo.

*Auth.* There is no AppRole and no static token in config, and `VAULT_*` / `BAO_*` environment
variables are ignored.
- `kubernetes`: a projected service-account token with a dedicated audience bound on the Vault role.
  Never the default token, whose audience is the Kubernetes API.
- `jwt`: a projected token file.
- `token_file`: Vault Agent.

Token files must be mode `0600` and are re-read on expiry. A token is renewed at 2/3 of its lease,
and the old one keeps working until it expires if re-login fails. The renewer is a goroutine, so the
registry has `Close()`.

*Transport.* HTTPS with TLS 1.2 or later, `ca_file` for a private CA, and no option to skip
verification. Plain HTTP only on loopback with `profile: dev` (a Vault Agent sidecar). Redirects are
never followed. At most 3 attempts.

**`azurekv`: Azure Key Vault and Managed HSM** (azkeys; Azure dependencies are already in go.mod).

*Key checks.*
- `RSA-HSM` (or `RSA` when `require_hsm: false`), 2048 bits;
- `attributes.enabled`, `nbf`/`exp`;
- `key_ops` is `sign` (and optionally `verify`) only;
- `attributes.exportable = false`, no `release_policy`;
- the purpose tag.

*Signing.* `Sign` with `RS256` over the digest. The service applies PKCS#1 v1.5 with SHA-256
DigestInfo.

*Clouds.*

| `cloud` | Vault suffix | Managed HSM suffix | Authority |
| --- | --- | --- | --- |
| `public` | `.vault.azure.net` | `.managedhsm.azure.net` | public |
| `china` | `.vault.azure.cn` | `.managedhsm.chinacloudapi.cn` | China |
| `usgov` | `.vault.usgovcloudapi.net` | `.managedhsm.usgovcloudapi.net` (to confirm) | US Gov |

The authority host comes from `cloud`, never from `AZURE_AUTHORITY_HOST`. Challenge-resource
verification stays on.

*Identity.* Per source:
- `managed`, with an optional client id;
- `workload`, which needs the federated token file and tenant id;
- `default`, dev only.

`AZURE_CLIENT_SECRET` and certificate variables are refused outside dev. The platform's own
managed-identity endpoint variables (`IDENTITY_ENDPOINT`, used by Container Apps and App Service)
are allowed: the platform sets them.

*Least privilege.*
- **Vault:** the RBAC permission model, and a custom role whose **dataActions** are
  `Microsoft.KeyVault/vaults/keys/read` and `…/keys/sign/action`, assigned per key. The tag is read
  with the key.
- **Managed HSM:** local RBAC, a custom role with `Microsoft.KeyVault/managedHsm/keys/read/action`
  and `…/keys/sign/action`, scoped to `/keys/<name>`.
- Purge protection is a deployment requirement.

**`awskms`: AWS KMS** (aws-sdk-go-v2 `kms`, `config`, `credentials/stscreds`).

*Key checks.* `DescribeKey`:
- `KeySpec = RSA_2048`, `KeyUsage = SIGN_VERIFY`, `KeyState = Enabled`;
- `Origin = AWS_KMS`;
- `MultiRegion` only with `allow_multi_region`;
- `AWSAccountId` and region equal the config.

`ListResourceTags` must show the purpose tag. `GetPublicKey` gives the DER SPKI, and its `KeyUsage`
and `SigningAlgorithms` are checked.

*Signing.* `Sign` with `MessageType = DIGEST` and `SigningAlgorithm = RSASSA_PKCS1_V1_5_SHA_256`.
The response `KeyId` must equal the ARN.

*Credentials.* Built from `identity.kind`, not the default chain:

| `identity.kind` | Provider |
| --- | --- |
| `pod_identity` | the container-credentials endpoint; only the link-local EKS Pod Identity address is accepted |
| `irsa` | web identity token file plus role |
| `imds` | IMDSv2 only |
| `ecs` | the task role |

Optionally, `role_arn` adds STS AssumeRole for cross-account keys. kista refuses:
- static keys, unless `static_credentials: allow` in dev;
- `AWS_PROFILE`, `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`, `AWS_CA_BUNDLE`,
  `AWS_EC2_METADATA_SERVICE_ENDPOINT`, and `AWS_ENDPOINT_URL*`.

Shared config files are loaded as empty lists. `endpoint_url` is allowed only with `profile: dev` or
on loopback. The partition is derived from the region through the SDK resolver, so `aws-us-gov`,
`aws-cn`, `aws-iso*` and new partitions work.

*Least privilege.* `kms:Sign`, `kms:GetPublicKey`, `kms:DescribeKey` and `kms:ListResourceTags` on
the key.

**`gcpkms`: Google Cloud KMS over REST** (`golang.org/x/oauth2/google` for credentials; no gRPC SDK).

*Key checks.*
- `GET …/cryptoKeyVersions/N`: `algorithm = RSA_SIGN_PKCS1_2048_SHA256`, `state = ENABLED`, the
  protection level per the tier, an empty `import_job`, `trusted_wrapping_enabled = false`.
- `GET …/cryptoKeys/<key>`: the purpose label.
- `getPublicKey`: `pem`, checked against `pem_crc32c`.

*Signing.* `POST …:asymmetricSign` with `digest.sha256` and `digestCrc32c`. Checked in the response:
- `verifiedDigestCrc32c`;
- `signatureCrc32c` against the signature;
- `name` equals the version;
- `protectionLevel`.

*Credentials.* Application Default Credentials from the metadata server (GKE workload identity), or
an `external_account` file (federation from AKS or EKS). kista refuses:
- `service_account` key files, unless allowed in dev;
- `authorized_user` (gcloud logins);
- executable credential sources;
- token or impersonation URLs outside `googleapis.com`;
- `GCE_METADATA_HOST`, `GCE_METADATA_IP`, `GCE_METADATA_ROOT`, and a foreign
  `GOOGLE_CLOUD_UNIVERSE_DOMAIN`.

The endpoint is pinned to `cloudkms.googleapis.com`.

*Least privilege.* A custom role with:
- `cloudkms.cryptoKeyVersions.useToSign`;
- `cloudkms.cryptoKeyVersions.viewPublicKey`;
- `cloudkms.cryptoKeyVersions.get`;
- `cloudkms.cryptoKeys.get`.

`roles/cloudkms.signer` alone is not enough.

**Kubernetes Secrets** hold the private key itself. One is mounted into `signers.file_dir` and used
as a `file` key, under the development-or-opt-in rule.

**PKCS#11** is a later backend behind the same interface.

### Package layout

```text
internal/signer               the Signer interface, CheckSignature, errors, the file signer (a leaf package)
internal/keysource            source config, reference parsing, the registry (implements keys.Opener),
                              the common contract wrapper, health checks
internal/keysource/vault      + internal/vaultapi (thin HTTP client, auth, renewal)
internal/keysource/azurekv
internal/keysource/awskms     (build tag kista_no_aws leaves it out)
internal/keysource/gcpkms     (build tag kista_no_gcp leaves it out)
internal/cloud/{azure,aws,gcp}   hardened credential construction
```

`app.Credential` (spec 0003, the Entra database login) moves to `internal/cloud/azure`. The database
login keeps the global `azure.identity`, and key sources have their own `identity`.

### `kista admin key check`

`kista admin key check -signer <ref>` runs open, the contract checks and a probe signature. It
prints the fingerprint and the backend's identity of the key, and stores nothing. It serves
operators preparing keys, and the live tests.

## Security

- **No SSRF and no key squatting.**
  - A reference names a configured source, and every host and account comes from config.
  - `allow` lists and the purpose marker limit which keys a source may use.
  - Only the server administrator sets references (spec 0003). Tenants never bring their own KMS
    in this spec.
  - Tenant key provisioning (spec 0006) uses a separate source and identity with create rights. It
    sets non-exportable keys and the purpose marker, with a per-tenant name prefix.
- **Keys never leave the backend.** kista refuses:
  - exportable, imported and external keys;
  - keys with release policies or trusted wrapping;
  - keys that can encrypt or wrap (Vault: a token that may).

  HSM tiers are the default where the backend has them. Properties are re-checked while in use.
- **Every signature is checked**: against the stored public key, and against the key identity in
  the backend's answer.
- **Credentials come from the platform**, built from explicit config. Steering environment
  variables, static keys, AppRole secrets and gcloud logins are refused outside development.
- **Separation of roles.** A key must be dedicated to kista channel signing:
  - the purpose marker;
  - the signing-only checks, which refuse tresor's KEKs;
  - one fingerprint per server (spec 0003);
  - disjoint prefixes and separate identities for licence keys (spec 0012).

  The documentation states the rule that only kista's identity holds sign rights on these keys.
- **Fail closed.** A source that cannot confirm a property, sign, or name the pinned key produces no
  signature, and nothing is recorded as signed.
- **Cost and denial of service.** KMS calls are bounded per source and happen only when a release is
  made. Health checks never sign.
- **Supply chain.** Per-backend build tags; govulncheck and a licence check in CI. GCP uses REST, so
  no gRPC dependency tree.

## Testing

- **Contract suite** (`internal/keysource/sourcetest`). Every backend runs it, against its fake and,
  where CI has one, against a real server. Capability flags skip what a backend does not have:
  Vault has no expiry, AWS has no versions. Cases:
  - **Signing:**
    - the known-answer signature;
    - sign, then verify;
    - a response naming another key or version;
    - a signature that does not verify.
  - **Keys refused at open:**
    - a wrong version, or a version that would mean "latest";
    - disabled or expired;
    - a wrong type, size or exponent;
    - can encrypt or wrap;
    - exportable, imported or external;
    - with a release policy or trusted wrapping;
    - without the purpose marker.
  - **Changed later:** a property flipped after open stops signing at the next re-check.
  - **Calls and errors:**
    - a timeout;
    - one transient failure, then success, within 3 attempts;
    - an error body or principal that must not appear;
    - an authentication failure.
- **References:** every kind's grammar, `allow` prefixes (`ext` must not admit `extra`), unknown and
  reserved sources, and hosts, ARNs, aliases or URLs inside references, `..`, `v0`, empty
  versions.
- **Real servers in CI:**
  - **Vault and OpenBao** run in dev mode as `docker run` steps (`-dev-tls`, to test `ca_file`).
    The test setup uses the root token to:
    - enable Transit and create an `rsa-2048` key;
    - write kista's policy, with read and sign only;
    - mint a token file;
    - configure a JWT role against a test key.

    kista then authenticates with `token_file` and `jwt`; `kubernetes` auth runs against a fake
    TokenReview. The tests are gated by `KISTA_TEST_VAULT` and `KISTA_TEST_OPENBAO`, and
    `KISTA_TEST_REQUIRE_VAULT=1` on Linux, like the databases.
  - **AWS:** moto (`motoserver/moto`, Apache-2.0) if its KMS signs `RSA_2048` with
    `MessageType = DIGEST`. LocalStack's current image needs an auth token, so it is not used.
    Otherwise AWS is tested with fakes only.
  - **Azure and GCP:** `httptest` TLS servers speaking the REST shapes, with the Azure 401 challenge.
    A test transport is injected into the real clients.
  - **Live tests** (build tag `live`) run with `kista admin key check` in a protected environment
    with federated credentials.
- **e2e.** A channel whose active key is a Vault key, then an OpenBao key: DuckDB installs and loads
  an extension it signed. The e2e workflow's paths include `internal/keysource/**`,
  `internal/vaultapi/**` and `internal/cloud/**`.
- **Compatibility.** A key stored under spec 0003 with `file:<name>` still opens.

## Alternatives considered

- **URLs or ARNs as references.** They let a reference choose a host or an account. Named sources
  make the host a config property.
- **A cloud KMS abstraction library** (gocloud.dev). It covers encryption, not asymmetric signing
  with a digest input, version pinning, or these checks.
- **The Vault SDK.** It is heavy, and there are two near-identical modules. The thin client needs
  five endpoints.
- **Google's gRPC client.** It pulls in grpc, protobuf and genproto, tens of modules and megabytes of
  binary. The REST API carries the same integrity fields.
- **Hand-written SigV4 for AWS.** The modular SDK is moderate in size, and its signing is not worth
  reimplementing.
- **Kubernetes Secrets as a backend.** A Secret holds the private key itself.
- **One PR for all backends.** Too large to review well, so the work is phased under this spec.

## Follow-ups

- **Spec 0005**: blob storage for extension bodies, with the same pattern: filesystem, S3-compatible
  (AWS S3, Cloudflare R2, MinIO), Azure Blob, Google Cloud Storage.
- **Spec 0006**:
  - `/readyz` as a boolean, and source health on admin endpoints;
  - tenant key provisioning with a separate source and identity.
- **Spec 0008**: sign once per (body hash, key) and store the signature.
- **Spec 0012**: licence keys from separate sources.
- **Later**: PKCS#11; tenants bringing their own KMS (an external id per tenant, source config in the
  database, egress allowlists).
