---
title: Publishing and promotion
---

# Publishing and promotion

Your own builds reach a channel in one of three ways:

- **over the API**: a person with a tenant token, or a **publisher** (CI of any forge, signing its
  requests with its key), uploads the file into a signed channel;
- **with the CLI** on the server: `kista admin release add`, as the server administrator;
- **by promotion**: a release already in one channel (usually `staging`) is released in another
  (`prod`) with that channel's signature. Nothing is copied.

Every route below is under `/api/v1/tenants/{tenant}`. The design reasoning is in
[spec 0008](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0008-publication/spec.md).

## What is checked

kista checks the uploaded file before anything is committed. A file that fails any check is refused
with `400`, naming the check.

1. **The footer.** The `version` and `platform` you declare must equal what the file's footer says.
   A CI job that picked the wrong artifact fails instead of publishing it.
2. **The metadata prefix.** The 22-byte prefix DuckDB's build writes before the metadata must be
   there. kista never rewrites a body it signs.
3. **The binary format** matches the platform: `linux_*` is an ELF shared object, `osx_*` a thin
   Mach-O dylib or bundle, `windows_*` a PE DLL, on the platform's machine (`amd64`, `arm64`).
   `wasm_*` and unknown platforms are refused.
4. **The entry point.** The binary must export the symbol DuckDB looks up to load the name, and no
   other extension's entry point:

   | ABI in the footer | Symbol |
   | --- | --- |
   | `CPP` (or empty) | `<name>_duckdb_cpp_init` |
   | `C_STRUCT_UNSTABLE`, or `C_STRUCT` with C API `v2.x.y` | `<name>_init_c_api_v2` |
   | any other `C_STRUCT` | `<name>_init_c_api` |

   So a binary built as `y` cannot be published as `x`. This is a naming check, not a code review:
   a publisher granted a name controls the code behind it.
5. The trailing 256 bytes (a signature the upload may carry) are ignored and replaced by the
   channel's signature.

To see what a file's footer says before you publish it, run `kista ext inspect <file>`: it prints
the platform, ABI, DuckDB or C API version, extension version, body hash, and the licence keys
of an extension built with the kista SDK.

The channel must also serve the build's DuckDB version: a `cpp` or `c_struct_unstable` build needs
its exact DuckDB version in the channel's list, a `c_struct` build a channel version whose C API
maximum accepts it (see [Tenants and channels](administration/tenants-and-channels.md)). Otherwise
the answer is `400`.

## Versions, platforms and slots

A release fills a **slot**: (channel, name, extension version, platform, and the DuckDB version for
`cpp` and `c_struct_unstable` builds, or `capi:<major>` for `c_struct` builds). One extension version
therefore ships one file per platform and DuckDB version, and may ship both a C API v1 and a v2
build.

- **A released version is immutable.** The same body in the same slot with the same choices is the
  existing release (`200`); another body, or other choices, is `409`. A fix is a new version.
- A yanked release keeps its slot.
- The extension version is matched exactly and case-sensitively: `1.0` and `v1.0` differ, and DuckDB
  asks for the string as typed in `INSTALL x FROM r VERSION '…'`.
- Platforms follow DuckDB's names: `linux_amd64`, `linux_arm64`, `osx_arm64`, `windows_amd64`, ...

**Current.** A release published with `current=true` (the default) becomes the one DuckDB gets on
the plain path (`INSTALL x FROM r`). With `current=false` it is served only on the versioned path
(`INSTALL x FROM r VERSION 'v1.2.0'`) until someone makes it current. "Current" is decided per
caller: a caller who may not see private releases gets the newest public one.

## Visibility

A release is `public` (served to anyone) or `private` (served only to a token holding `install`; see
[Access](access.md)). Over the API a publication is **private unless you pass
`visibility=public`**, so a forgotten parameter never publishes to everyone. The CLI's
`release add` is public unless you pass `-private`.

## Reserved names

Names DuckDB owns are **reserved**. The server keeps the list as data, and it only grows:

- the core extensions of every registered DuckDB version, given when the version is registered
  ([DuckDB versions](administration/tenants-and-channels.md#duckdb-versions));
- the names a `duckdb-core` or `duckdb-community` upstream mirrors, learned as it mirrors them
  ([Upstreams](upstreams.md));
- duckdb/community-extensions' names, read every day when `reserved.community_sync` is on
  ([Configuration](configuration.md#reserved));
- the names a server administrator adds (`kista admin reserved add`, below);
- the core and community names kista knew before the list was data (its seed).

A name core in any version is core; a community name may become core, never the other way. Names
an upstream of the tenant provides are reserved in that tenant too (see [Upstreams](upstreams.md)),
and so are the names a server administrator or a distribution admin reserves in the tenant for a
holder, a distributor such as Enterest (`kista admin tenant-reserved add <tenant> <name> -holder
<text>`, [the API](api.md#names-reserved-in-a-tenant)). A tenant's administrators read those, and
change none; they may still grant `publish` that names the extension, and manage its releases with
`admin`. A tenant reservation sets no `shadows`.

For `publish` and `promote`, a reserved name is reached only by a grant that **names the
extension**. A tenant-wide or channel-wide grant, or a grant on an extension group, never reaches
`httpfs`. Other verbs are not affected: a tenant administrator still manages `httpfs`'s releases.
A release under a reserved name shows `shadows: core`, `community` or `upstream` in the index.

DuckDB's alias names (`http`, `https`, `s3`, `postgres`, `sqlite`, ...) and Windows device names are
refused by the name grammar: DuckDB rewrites an alias before it builds the URL, so nothing could ever
install them.

## Who may publish

| Action | Needs |
| --- | --- |
| Publish name `n` into channel `c` | `publish` covering (`c`, `n`) |
| Promote `n` from channel `s` to channel `t` | `promote` covering (`t`, `n`) **and** `publish` covering (`s`, `n`) |
| Read the extension's releases in `c` | `admin`, `publish` or `promote` covering (`c`, `n`) |
| Block a body, manage publishers | `admin` on the tenant |

`publish` and `promote` are **never implied by `admin`**, and an issuer-wide grant
(`issuer:<record>`) cannot carry them: publishing native code is granted explicitly. Both channels
must be `signed` channels of the tenant; nothing is published or promoted into a passthrough
channel.

A person's token used to publish, promote or block must be fresh: issued within
`auth.admin_token_max_age` (default 1h), with a `sub` or client claim naming the writer. A DuckDB
secret holding a long-lived install token is not enough.

## Publishing over the API

```text
POST /api/v1/tenants/{t}/channels/{c}/extensions/{ext}/releases
     ?version=<ext version>&platform=<platform>[&visibility=public|private][&current=true|false]
     Content-Type: application/octet-stream
     Content-Length: <required>
     body: the extension file
```

```bash
curl --fail-with-body -sS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/octet-stream" \
  --data-binary @build/release/extension/acl/acl.duckdb_extension \
  "https://kista.example.com/api/v1/tenants/acme/channels/staging/extensions/acl/releases?version=v0.3.0&platform=linux_amd64&visibility=private"
```

The answer is `201` with the release and `Location: …/extensions/acl/releases/<id>` (`200` when the
same body was already released in that slot with the same choices):

```json
{
  "id": "4f0c…",
  "name": "acl",
  "version": "v0.3.0",
  "platform": "linux_amd64",
  "slot": "capi:1",
  "abi": "c_struct",
  "build_c_api": "v1.2.0",
  "state": "active",
  "visibility": "private",
  "seq": 7,
  "body_hash": "9a1b…",
  "created_at": "2026-10-12T09:30:00Z",
  "origin": "publication",
  "etag": "\"v1\""
}
```

| Status | Meaning |
| --- | --- |
| `400` | a footer, prefix, format or entry-point mismatch; an unknown query parameter; a DuckDB version the channel does not serve; an upload that ended before its length |
| `401` | no valid token, or a stale one; for a publisher's signed request, see [Signed requests](#signed-requests) |
| `404` | the token may not publish this name in this channel (also for a reserved name without a grant naming it) |
| `408` | the upload was slower than `serve.min_rate` |
| `409` | the slot holds another body or other choices; the body is blocked; the channel has no active key |
| `411` | no `Content-Length` (chunked uploads are refused) |
| `413` | longer than `blob.max_body` plus the 256-byte signature |
| `415` | a `Content-Type` other than `application/octet-stream` |
| `429` | too many uploads at once; retry after `Retry-After` |

A client that sends `Expect: 100-continue` gets its `401` or `404` before sending the file.

**Limits.** The upload must arrive at no less than `serve.min_rate` (default 16 KiB/s) and within
`Content-Length / min_rate` plus a minute. At most `publish.max_per_principal` uploads run at once
per caller and `publish.max_per_tenant` per tenant, and uploads leave one of `blob.max_ingests` free
for the CLI and upstream intake. The counters are per replica.

```yaml title="kista.yaml"
publish:                     # file-only
  max_per_principal: 2       # 1..64, default 2
  max_per_tenant: 8          # 1..256, default 8
```

See [Configuration](configuration.md) for every setting.

## Publishing with the CLI

On a host with the server's configuration, the server administrator adds a file directly:

```bash
kista admin -config /etc/kista/kista.yaml \
  release add acme/staging build/acl.duckdb_extension -name acl -private
```

| Flag | Meaning |
| --- | --- |
| `-name <name>` | the extension name (required; the footer has none) |
| `-private` | a private release (the CLI's default is public) |
| `-not-current` | served on the versioned path only |
| `-unchecked` | skip the prefix, format and entry-point checks, for a body the operator vouches for (a test fixture, a platform kista cannot parse). An unchecked release cannot be promoted (`409` over the API); adding or publishing the same body with the checks marks it checked |

`-` reads the file from standard input. The command prints the release id. The CLI is not limited by
grants or reserved names. See [commands](administration/commands.md) for the rest of `release`.

## Promotion

Promotion releases, in the target channel, Builds that are already in another channel of the tenant,
signed with the target channel's key.

```text
POST /api/v1/tenants/{t}/channels/{target}/extensions/{ext}/releases/promote
{"from_channel": "staging", "release": "<id>"}        one release, or
{"from_channel": "staging", "version": "v0.3.0"}      every active release of that version
optional: "visibility": "private", "current": false
```

```bash
curl --fail-with-body -sS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"from_channel":"staging","version":"v0.3.0"}' \
  https://kista.example.com/api/v1/tenants/acme/channels/prod/extensions/acl/releases/promote
```

A publisher promotes with a [signed request](#signed-requests) whose `sha256` is that of the exact
JSON bytes sent (`--data-binary`).

- Promoting a **version** takes every active release of (`ext`, `version`) in the source: every
  platform and every slot. All of them are inserted, or none (a slot conflict on any is `409` for
  all). The answer lists the releases: `{"releases": [...]}`.
- Promoting one **release** by id answers that release with its `Location`.
- The answer is `201`, or `200` when the target already holds them all with the same choices.
- Source releases must be `active` (not deprecated or yanked), checked, and not blocked.
- **Visibility** is the source's, unless you ask for `private`. Promotion never makes a private
  build public; that is `POST …/releases/{id}/public` by the extension's administrator.
- The target must serve each build's DuckDB version (`400` names the platform and version).
- A source channel you may not publish from answers `404`, like a missing release.

The CLI does the same:

```bash
kista admin -config /etc/kista/kista.yaml \
  release promote acme/prod -name acl -from staging -version v0.3.0
```

`-release <id>` promotes one release; `-private` and `-not-current` narrow the result.

## Changing a release

An administrator of the extension (or of its channel or tenant) changes a release with
`POST …/channels/{c}/extensions/{ext}/releases/{id}/{change}` and `If-Match` with the release's ETag
(`*` for any), or `kista admin release <change> <tenant>/<channel> <release-id>`:

| Change | Effect |
| --- | --- |
| `yank` | final: never served again; the previous release becomes current again |
| `deprecate` | still served on the versioned path, no longer current |
| `activate` | a deprecated release is active again |
| `current` | makes an active release current (a rollback, or a release published with `current=false`) |
| `public`, `private` | changes visibility |

`DELETE …/releases/{id}` (and `kista admin release purge`) deletes a yanked release for good; its
slot stays taken (see [Storage](storage.md)).

Every change bumps the channel's release version, so every replica serves the change at the next
request.

## Blocking a body

A tenant administrator can **block** a body hash: every release of that body in every channel of the
tenant, passthrough channels included, is yanked, and the body is never released again in the
tenant (publication, promotion, `release add` and upstream intake all check it).

```bash
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"body_hash":"9a1b…","reason":"CVE-2026-1234"}' \
  https://kista.example.com/api/v1/tenants/acme/blocks
```

| Route | |
| --- | --- |
| `GET /blocks`, `GET /blocks/{hash}` | list, read |
| `POST /blocks {body_hash, reason}` | `201`; blocking a blocked hash sweeps again and answers `200` |
| `DELETE /blocks/{hash}` | `204`; the body may be released again, yanked releases stay yanked |

CLI: `kista admin block add <tenant> <hash> -reason …`, `block list`, `block remove`. A hash the
tenant does not hold may be blocked ahead of time. A tenant holds at most 10,000 blocks. The index
shows `blocked: true` on a yanked row whose body is blocked. A block stops one artifact; to stop a
publisher, remove its grants or the publisher.

## Publishers

A **publisher** is a named identity in a tenant that may only publish and promote. Grants name it
as `publisher:<name>`. It authenticates by **signing its requests** with its developer's SSH key,
whose public half is registered on it, like an SSH key on GitHub: CI on GitHub, GitLab, Gitea or
anywhere else publishes the same way, and kista holds no secret that publishes. Its keys can change
without touching its grants.

What a publisher may do, whatever its grants say: publish, promote, read the releases its grants
cover, add attachments to what it publishes (see [Attachments](attachments.md)), and
`GET …/whoami`. Nothing else: no install, no management. A grant to a publisher may carry only
`publish` and `promote`.

```bash
# the developer: a key for CI (the private half becomes a CI secret)
ssh-keygen -t ed25519 -N '' -C acl-ci -f publisher_key

# a tenant administrator, or the server administrator with the CLI
kista admin -config /etc/kista/kista.yaml publisher add acme ci
kista admin -config /etc/kista/kista.yaml publisher key add acme ci publisher_key.pub -expires 365d
kista admin -config /etc/kista/kista.yaml grant add acme -principal publisher:ci -verb publish -channel staging
kista admin -config /etc/kista/kista.yaml grant add acme -principal publisher:ci -verb publish -channel staging -extension httpfs
```

The last grant is needed only for a reserved name. One publisher per family of extensions keeps
grants readable; a grant may also name an extension group (see [Access](access.md)).

Over the API (tenant administrators):

| Route (under `/api/v1/tenants/{t}`) | |
| --- | --- |
| `GET /publishers`, `POST /publishers {"name": …}` | list, add; a publisher's answer lists its keys |
| `GET /publishers/{name}`, `DELETE /publishers/{name}` | read, remove |
| `GET /publishers/{name}/keys`, `POST /publishers/{name}/keys {"public_key": …, "expires_at": …}` | list, register a key |
| `GET /publishers/{name}/keys/{id}`, `DELETE /publishers/{name}/keys/{id}` | read, remove one |

Removing a publisher removes its keys and grants; caches drop it at the next request.

### Keys

- **Types**: `ssh-ed25519` (recommended) and `ecdsa-sha2-nistp256`, for a key held in a KMS. RSA,
  DSA, certificates and hardware (`sk-`) keys are refused.
- **The input** is the line of the `.pub` file `ssh-keygen` writes: `ssh-ed25519 AAAA… comment`. The
  comment becomes the key's title (printable ASCII, at most 100 characters).
- **A key is known by its fingerprint**, `SHA256:…`, as `ssh-keygen -lf publisher_key.pub` prints
  it. A fingerprint is unique in a tenant (`409`); the same key may serve publishers of other
  tenants.
- **Expiry** is optional: `-expires` takes `<days>d` or an RFC 3339 time, within a year; over the
  API, `expires_at` in RFC 3339. A year is recommended. An expired key verifies nothing.
- **A publisher holds at most 10 keys**, expired ones included.
- **Removing a key** stops it at once: keys are looked up on every request, never cached.

**Rotation**: register the new key, replace the CI secret, watch the old key's `last_used_at` stop
(`publisher key list`), then remove the old key.

### Signed requests

A publisher's request carries two headers and no `Authorization`:

```text
Kista-Statement: <the statement, standard base64>
Kista-Signature: <the SSH signature, standard base64 on one line, without its armor lines>
```

The **statement** names the request exactly: five lines, each ending in LF.

```text
kista-request v1
method POST
path /api/v1/tenants/acme/channels/staging/extensions/acl/releases?version=1.4.0&platform=linux_amd64
sha256 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
signed-at 2026-10-19T12:00:00Z
```

- `path` is the request's path and query, exactly as sent: the same parameters in the same order,
  encoded the same way (a `+` in a version is sent, and stated, as `%2B`). A reverse proxy in front
  of kista must pass the request URI unchanged.
- `sha256` is the plain SHA-256 of the request body: the file for a publication, the JSON for a
  promotion, and the SHA-256 of nothing for a `GET`.
- `signed-at` is UTC, to the second.

The **signature** is OpenSSH's `ssh-keygen -Y sign` over the statement in the namespace
`kista-request@v1`, so a signature made for git commits or anything else never verifies here.
OpenSSH 8.1 or later makes it; any CI host has it.

```bash
ssh-keygen -Y sign -f publisher_key -n kista-request@v1 < statement > statement.sig
```

**What kista checks**:

| Answer | When |
| --- | --- |
| `400` | the headers or the statement are malformed; the signature is malformed, of a key type kista does not take, or not in the namespace `kista-request@v1` or with a hash other than `sha256` and `sha512`; `method` or `path` is not this request's; a request carries both a signature and `Authorization`; the body's SHA-256 is not the statement's (checked as it is read, before anything is written) |
| `401` | `signed-at` is more than five minutes from kista's clock; the key is not one of the tenant's, or it expired; the signature does not verify |
| `404` | the publisher's grants do not cover the request, as for any caller |

A `401` names the key's fingerprint that kista found and kista's clock, so a wrong key and a skewed
clock can be told apart; it does not say which check failed. A client sending `Expect:
100-continue` gets its `401` or `404` before the file.

A captured request can be replayed only within its five minutes, to the same path, with the same
body: a publication then answers the existing release (`200`) and changes nothing; a promotion
promotes the same; an attachment is added again. The statement and the signature a release keeps
are shown to the extension's administrators only, so another publisher of the channel cannot
replay them.

### Publishing from CI

Any CI publishes with a shell, OpenSSH 8.1 or later (`openssh-client` on Debian and Ubuntu,
`openssh-keygen` on Alpine), `curl`, and the private key as a secret:

```bash
# the key, from a secret; ssh-keygen wants a file of mode 0600 ending in a newline
install -m 600 "$KISTA_PUBLISHER_KEY_FILE" publisher_key   # a file variable (GitLab), or
# printf '%s' "$KISTA_PUBLISHER_KEY_B64" | base64 -d > publisher_key && chmod 600 publisher_key
# (the key in base64: GitLab's masked variables hold no newlines)

KISTA=https://kista.example.com
sha256() { if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi | cut -d' ' -f1; }
signed() {   # signed <method> <path and query> [file]: the two headers' values, one per line
  { printf 'kista-request v1\nmethod %s\npath %s\n' "$1" "$2"
    printf 'sha256 %s\n' "$( if [ -n "${3:-}" ]; then sha256 < "$3"; else printf '' | sha256; fi )"
    printf 'signed-at %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"; } > statement
  ssh-keygen -Y sign -q -f publisher_key -n kista-request@v1 < statement > statement.sig
  base64 < statement | tr -d '\r\n'; echo
  grep -v -- ----- statement.sig | tr -d '\r\n'; echo
}

# first, the key and its grants: whoami
W=/api/v1/tenants/acme/whoami
{ read -r ST; read -r SIG; } < <(signed GET "$W")
curl --fail-with-body -sS "$KISTA$W" -H "Kista-Statement: $ST" -H "Kista-Signature: $SIG"

# then each build
FILE=build/release/extension/acl/acl.duckdb_extension
P="/api/v1/tenants/acme/channels/staging/extensions/acl/releases?version=1.4.0&platform=linux_amd64"
{ read -r ST; read -r SIG; } < <(signed POST "$P" "$FILE")
curl --fail-with-body -sS -X POST "$KISTA$P" -H "Kista-Statement: $ST" -H "Kista-Signature: $SIG" \
  -H "Content-Type: application/octet-stream" --data-binary @"$FILE"
```

A retry signs again, so it never outlives `signed-at`. A client written in Go can sign with
`golang.org/x/crypto/ssh` instead, for example over a KMS's signer (`ssh.NewSignerFromSigner`).

```yaml title=".gitlab-ci.yml"
publish:
  stage: deploy
  image: alpine:3
  rules: [{if: $CI_COMMIT_TAG =~ /^v/}]
  script:
    - apk add --no-cache bash curl openssh-keygen
    - bash scripts/publish.sh   # the commands above; KISTA_PUBLISHER_KEY_FILE is a protected File variable
```

**The developer kit.** The [extension template](development.md#starting-an-extension-the-template)
publishes from GitHub Actions with the fork's `kista_publish.py`, which does the above for every
build of a distribution: it reads each build's version and platform from its footer, skips wasm
builds (refused, above), signs each request, and retries a `429`, a `408`, a `5xx` or a broken
connection, signing again. Its `--check` signs a `whoami` first. It needs only Python 3 and OpenSSH,
so it also runs in another CI: see the fork's
[KISTA.md](https://github.com/hugr-lab/extension-ci-tools/blob/main/KISTA.md).

The release keeps the statement and the signature as they came (`publisher_statement`,
`publisher_signature` in its management answer, for the extension's administrators), and its
provenance records the credential, `key:<fingerprint>`. The signature carries the public key, so who
asked for this body at this path can be checked after the key is removed.

GitHub Actions trusted publishing and API keys (before spec 0023) are removed: a configuration with
`publish.providers` is refused at startup. An issuer record cannot have a CI token issuer's URL
(GitHub Actions', GitHub Enterprise's), since any workflow there can request a token for any
audience; records that had one stay unused.

### Sources of licence keys

A licensed extension's build declares its licence keys in a key block (see
[Licensing](licensing.md#the-keys-a-licence-verifies-against)). kista records them only when a
trusted source brings the build. A publisher is one when a **server administrator marks** it, for
example the build pipeline of the issuer that signs the extension's licences:

```bash
kista admin -config /etc/kista/kista.yaml publisher add acme tresor-ci
kista admin -config /etc/kista/kista.yaml publisher license-keys acme tresor-ci on   # off unmarks it
kista admin -config /etc/kista/kista.yaml grant add acme -principal publisher:tresor-ci -verb publish -channel prod -extension tresor
kista admin -config /etc/kista/kista.yaml publisher key add acme tresor-ci tresor-ci.pub -expires 365d
```

Over the API: `POST` (mark) and `DELETE` (unmark) `/api/v1/tenants/{t}/publishers/{name}/license-keys`,
server administrators only. Each records a `publisher.change` event.

- **Its publications record keys.** Each publication by the marked publisher records the keys the
  body declares, including a publication of a body that is already released (from an unmarked path,
  say). The publisher must hold `publish` on the name itself.
- **A malformed key block is refused**: `400`, with `license_keys` in the detail. `kista ext
  inspect <file>` shows what kista reads.
- **Its keys and grants are the server administrator's** (or a distribution admin's, spec 0030,
  which marks only a publisher without keys or grants). A publisher holding a key is not marked
  (`400`): keys a tenant administrator registered would outlive the mark. While it is marked,
  adding or removing its keys and grants, and changing the members of an extension group its grants
  name, is refused to a tenant administrator (`404`). A tenant administrator can still remove the
  publisher, which drops the licence keys it brought.
- **Unmarking** keeps the keys already recorded. Its credentials are then a tenant administrator's
  again, so marking it again needs it to hold none.

Publications by any other publisher, by a principal, and `release add` record no keys.

## Provenance in the index

Every release has an `origin`: `admin` (the CLI), `publication`, `promotion` or `upstream`. Its
`provenance` (the actor, a publisher's key, the source channel and release for a promotion) is shown to the extension's administrators and to holders of `publish` or `promote`
covering it. Who made the release is shown to administrators only, not to publishers.

## Signatures and key rotation

A publication or promotion is signed with the channel's active key (and its serving key, when they
differ) outside any transaction, and each signature is verified before it is stored. When a channel's
active key changes, the re-signer (`serve.resign: true` on a replica, or
`kista admin key resign <tenant>/<channel>`) signs every non-yanked release with the new key, and
then moves the serving key. Nothing has to be published again. See
[Key rotation](administration/key-rotation.md).

Publications, promotions and blocks are recorded as events (see [Observability](observability.md)).
