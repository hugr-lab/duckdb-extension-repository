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
| `awskms` | AWS KMS | not available yet: config validation refuses it |
| `gcpkms` | Google Cloud KMS | not available yet: config validation refuses it |

PKCS#11 hardware HSMs are not available either. A Kubernetes Secret is not a backend: mounted into
`signers.file_dir`, it is a file key.

## Signer references

A key is named by a **signer reference**, `<source>:<key>`. The source name is everything before
the first `:`; the rest is parsed by that source's kind.

| Kind | Reference | Key name | Version |
| --- | --- | --- | --- |
| `file` | `file:<name>` | one file name inside `signers.file_dir`, no `/` or `..` | the file itself |
| `vault` | `<source>:<key>:v<N>` | `[A-Za-z0-9_-]{1,128}` | `N ≥ 1`, no leading zeros (`v0` would mean "latest" in Vault) |
| `azurekv` | `<source>:<name>/<version>` | `[0-9A-Za-z-]{1,127}`, compared lowercased | 32 lowercase hex characters (an empty version would mean "latest") |

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
```

### Settings of a source

| Setting | Default | Meaning |
| --- | --- | --- |
| `name` | required | `[a-z][a-z0-9-]{0,15}`, unique. Reserved: `file`, `vault`, `azurekv`, `awskms`, `gcpkms`, `pkcs11`, `http`, `https`, `arn`, `projects` |
| `kind` | required | `vault` or `azurekv`; exactly one matching block (`vault:` or `azurekv:`) |
| `allow` | required | key-name prefixes the source may use; `"*"` for any key. An empty list, or an empty prefix, is refused |
| `max_concurrency` | `4` | concurrent opens and signs through this source |
| `timeout` | `10s` | per call, `1s` to `60s` |
| `recheck` | `10m` | how often a key's properties are read again while it is in use; at most `10m` |
| `health_key` | none | accepted by the configuration; this build does not use it |
| `identity` | none | `azurekv` only (required there); refused on `vault` |

**Allow lists** are plain prefixes, matched after the key name passed its kind's rule: `ext-`
admits `ext-prod` but not `extra`. For `azurekv`, names are matched lowercased, so prefixes must be
written in lowercase.

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
  `key_version`, Azure's `kid`).
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
| `vault.software_keys` | must be `true` outside `profile: dev` |

### Authentication

There is no AppRole and no static token in the configuration, and `VAULT_*` and `BAO_*` environment
variables are ignored.

- `kubernetes`: kista logs in at `auth/<mount>/login` with the role and the token file. Use a
  projected service-account token with a dedicated audience bound on the Vault role, never the
  default token (its audience is the Kubernetes API).
- `jwt`: the same login with a JWT read from the token file.
- `token_file`: the file holds a Vault token, kept fresh by Vault Agent.

Token files must be regular files with mode `0600` or narrower (on Kubernetes, set the projected
volume's `defaultMode`); they are read again on expiry. A token is renewed on the first request
after two thirds of its lease; if the new login fails the old token is used until it expires. A
`403` causes one re-login. Requests use TLS 1.2 or later, never follow redirects, and ignore proxy
environment variables.

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

## Registering a key

A key becomes a channel's key in two steps: check it, then add it.

```bash
# open it, run every check, sign a probe; nothing is stored
kista admin -config kista.yaml key check -signer prod:ext-prod/0123456789abcdef0123456789abcdef

# register it in a channel: trusted, or -active for a channel that never had a key
kista admin -config kista.yaml key add acme/prod -signer prod:ext-prod/0123456789abcdef0123456789abcdef
```

`key check` prints the reference and the key's fingerprint. `key add`:

1. opens the reference and checks the key (RSA-2048, `e = 65537`, the contract above);
2. signs a probe (`SHA-256("kista key probe")`) and verifies it;
3. refuses a fingerprint already registered anywhere on the server;
4. refuses more than 16 non-retired keys in the channel, or a `.well-known` document over 64 KiB;
5. stores the public key and the reference, never the private key, and records a `key.add` event
   (with the source's scheme, never the full reference).

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
| `kista admin key list <tenant>/<channel>` | id, fingerprint, state, signer reference, dates |
| `kista admin key activate <tenant>/<channel> <key-id\|fingerprint> [-force]` | make it the signing key |
| `kista admin key retire <tenant>/<channel> <key-id\|fingerprint> [-force]` | remove it from `.well-known` |
| `kista admin key events <tenant>/<channel>` | the key's state transitions |
| `kista admin key resign <tenant>/<channel>` | sign releases with the active key; move the serving key |
| `kista admin attestation-key add <tenant> <signer-ref> [-active]` | register a tenant's attestation key |

`key check` opens a key for the channel purpose, so on Azure it refuses a key tagged
`attestation-signing`.

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
- **The purpose tag**, where the backend has tags. On Azure the `kista-purpose` tag must be
  `channel-signing` for a channel key and `attestation-signing` for an attestation key. A file key
  and a Vault Transit key have no tags and rely on the registry.

Attestation keys sign the statements a tenant's feed serves; they live in the same sources. See
[Attachments](attachments.md).

## Monitoring

kista's policy gives it no right to change keys. Watch the backend's own audit log for changes to
them: the Vault audit device, the Azure Activity Log (key create, import, update, delete). On kista's
side, the events `key.add`, `key.activate`, `key.retire`, `key.resign` and
`attestation_key.add|activate|retire` record every change (see [Observability](observability.md)).
