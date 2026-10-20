# Spec 0023: Publisher keys: signed requests from any CI

- **Status**: accepted; phase 1 (kista) implemented
- **Date**: 2026-10-19
- **Author**: vgsml, Claude

## Summary

A publisher authenticates by signing its requests with its developer's SSH key.
- The public half is registered in kista on the publisher.
- The private half is a secret of the developer's CI.
- The publish step signs each request with `ssh-keygen -Y sign`, and kista verifies it against the
  publisher's registered keys.

It works the same from GitHub, GitLab, Gitea or any other CI, because kista never talks to the forge.

GitHub Actions trusted publishing (OIDC) and API keys, both from spec 0008, are removed. A signed
request is the only way a publisher authenticates.

## Decisions

The owner's decisions (2026-10-19), which this spec records:

1. **kista does not depend on the forge.** A developer's repository may be on GitHub, GitLab, Gitea
   or anywhere else. kista never verifies a forge's identity token and never clones.
2. **Two ways an extension reaches a channel:**
   - **the developer builds**, in their own CI, and publishes the binary to kista (this spec);
   - **a central build**: the developer tells Enterest that a new tag exists, and Enterest clones the
     repository, builds and publishes it (Enterest's spec 0003, not kista's).
3. **A publisher is authenticated by its developer's key, like an SSH key on GitHub.** The private
   key is a CI secret; kista holds only the public key.
4. **GitHub trusted publishing is removed.**
5. **API keys are removed.** A signing key replaces them.

## Problem

Spec 0008 gave publishers two credentials:
- **GitHub trusted publishing.** It works only from GitHub Actions, and its binding is GitHub's
  claims (`repository_id`, `workflow_ref`), so a developer on GitLab or Gitea has no equivalent.
- **API keys.** An API key is a bearer secret: the developer's CI holds it, and kista holds its hash.

The developer kit (spec 0022) publishes with GitHub trusted publishing by default. The kit should not
need a particular forge, and kista should not carry a verifier for one.

## Design

### Keys

A publisher holds at most ten **public keys**, expired ones included.

- **Types**: `ssh-ed25519` (recommended) and `ecdsa-sha2-nistp256`. Anything else is refused when
  added:
  - RSA and DSA;
  - certificates (`…-cert-v01@openssh.com`);
  - hardware keys (`sk-…`), which cannot sign in CI.

  ECDSA P-256 is there for keys held in a KMS (below).
- **Input**: an OpenSSH public key line (`ssh-ed25519 AAAA… comment`), as `ssh-keygen` writes it
  and as GitHub takes it. The comment becomes the key's title: at most 100 characters, printable
  ASCII.
- **Identity**: the key's fingerprint, `SHA256:<unpadded base64>` of its wire form, as `ssh-keygen
  -l` prints it.
  - A fingerprint is unique within a tenant (`409` otherwise).
  - The same key may belong to publishers in different tenants. A request names its tenant in its
    path, so the lookup is always within one tenant.
- **Expiry**: optional (`expires_at`). An expired key verifies nothing. The CLI takes `-expires` as
  for other expiries: `<days>d` or an RFC 3339 time, at most 366 days ahead. The docs recommend a
  year.
- **Removal** deletes the key: it stops at once, because keys are looked up on every request and
  never cached. Removing the publisher removes its keys.

Registering a key needs no proof that the registrant holds the private half. A tenant administrator
already decides which publisher holds which grants. A key registered on the wrong publisher of their
own tenant gives them nothing they could not do directly. A key in another tenant is never looked up
for this tenant's requests.

```text
publisher_keys   id, tenant_id, publisher_id, type, public_key (wire form), fingerprint, title,
                 expires_at null, created_at, created_by, last_used_at null
                 unique (tenant_id, fingerprint)
```

**Management**: the tenant `admin`. For a publisher marked as a source of licence keys (spec 0021),
only the server administrator: marking is refused while the publisher has keys, and while it is
marked, adding or removing its keys is a server administrator's act, as its credentials are today.
- **API**: `GET, POST /api/v1/tenants/{t}/publishers/{name}/keys` (`{"public_key": "ssh-ed25519 …",
  "expires_at": …}`), and `GET, DELETE …/keys/{id}`. The path and the CLI command are spec 0008's
  API-key ones, with a new body and meaning. A key's answer has:
  - its id, type, fingerprint and title;
  - its expiry and creation;
  - `last_used_at`.
- **CLI**: `kista admin publisher key add <tenant> <publisher> <file|-> [-expires 365d]`, `publisher
  key list` and `publisher key remove`.
- **Console**: the publisher screen lists, adds and removes keys.
- **Audit**: `publisher.key.add` and `publisher.key.remove`, with the fingerprint, type and title
  (spec 0010).

**Rotation**:
1. Add the new key.
2. Replace the CI secret.
3. Watch the old key's `last_used_at` stop.
4. Remove the old key.

The ten-key limit leaves room for the overlap. A developer sends the `.pub` file to the tenant's
administrator; self-service registration is a follow-up.

### The signed request

A publisher's request carries two headers and no `Authorization`:

```text
Kista-Statement: <the statement, base64>
Kista-Signature: <the SSHSIG blob, standard base64 on one line, without armor>
```

**The statement** names the request exactly:

```text
kista-request v1
method POST
path /api/v1/tenants/acme/channels/staging/extensions/acl/releases?platform=linux_amd64&version=1.4.0&visibility=private
sha256 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
signed-at 2026-10-19T12:00:00Z
```

- The five lines, in this order, each ending in LF; nothing else.
- `path` is the request's path and query, exactly as sent.
- `sha256` is the plain SHA-256 of the request body: the file for a publication, the JSON for a
  promotion, and the SHA-256 of nothing for a `GET`.
- `signed-at` is UTC, to the second.
- A statement that is not exactly this, or longer than 4 KiB, is `400`.

One kind covers every route a publisher may use:
- publication and promotion;
- reading releases;
- attachments (spec 0011);
- `whoami`.

**The signature** is an SSH signature (`SSHSIG`, OpenSSH's `PROTOCOL.sshsig`) over the statement's
bytes. kista accepts it only when:
- the magic is right, the version is 1, and nothing follows;
- the namespace is `kista-request@v1`, so a signature made for git commits or anything else never
  verifies here;
- the hash is `sha256` or `sha512`;
- the embedded public key's fingerprint is a key of the path's tenant, and the embedded key equals
  the registered wire form byte for byte;
- the signature's format is the key type's, and it verifies with the registered key.

kista frames SSHSIG itself (a few dozen lines) and verifies with `golang.org/x/crypto/ssh`. ECDSA
signatures are malleable, which is harmless: a signature is never used to identify a request.

Making a signature needs only OpenSSH 8.1 or later:

```sh
ssh-keygen -Y sign -f publisher_key -n kista-request@v1 < statement > statement.sig
```

**kista checks, in order:**
1. **Before the body.**
   - The headers and the statement must parse (`400`).
   - `method` and `path` must equal the request's (`400`).
   - `signed-at` must be within five minutes of the server's clock, either way (`401`).
   - The key and the signature must verify (`401`).
   - The key's publisher is the caller, and its grants decide, as for any publisher (spec 0008),
     including the `404` that says nothing about what exists.

   A `100-continue` client gets its `401` or `404` before it sends the file.
2. **The body.** It is hashed as it is read. If its SHA-256 differs from the statement's, the answer
   is `400` ("the body is not the signed one"), before anything is written to the store: the hash
   check is in `release.Add` after the spool and before any store write (licence keys, release,
   Build state). A promotion's JSON is checked the same way before it is parsed.
3. Then everything as in spec 0008.

A `401` names the fingerprint kista found and its clock, so a developer can tell a wrong key from a
skewed clock. It never names which check failed. `signed-at` is the caller's issue time wherever
spec 0006 checks a token's age.

**Replay.** A captured request can be sent again only:
- within its five minutes;
- to the same path;
- with the same body.

What it can do then:
- **A publication** answers the existing release (`200`) and changes nothing, `current` included.
- **A promotion of a version** may also promote a platform that reached the source channel within
  those minutes, which the publisher's grant already allows.

No nonce is needed.

### Provenance

A release made by a publisher keeps:
- the actor, `publisher:<tenant>/<name>`, and the credential, `key:<fingerprint>`, in `provenance`
  (spec 0008's JSON, within its 4,000 bytes);
- the statement and the signature as they came, in two columns of their own on the release
  (`publisher_statement`, at most 4 KiB, and `publisher_signature`, at most 1 KiB; `nvarchar(max)`
  is not needed).

The SSHSIG blob carries the public key. So anyone can later check that this key asked for a body
with this SHA-256 at this path, even after the key is removed. kista's own `body_hash` (the file
without its signature) is beside it. Spec 0008's provenance before this spec keeps its `key:<id>` and
`github:<id>` credentials; the prefix tells them apart.

### What is removed

- **GitHub trusted publishing**:
  - `publisher_github`;
  - the routes `…/publishers/{name}/github`;
  - `kista admin publisher github`;
  - the provider verifier and `publish.providers`. A configuration that still sets
    `publish.providers` is refused at startup, naming this spec.
- **Kept from it**: an issuer record, tenant or server, at GitHub Actions' token URL
  (`https://token.actions.githubusercontent.com`) is still refused, and an existing one stays
  disabled. Any GitHub workflow can request a token for any audience, so trusting that issuer for
  install or administration would let any repository in.
- **API keys**:
  - `api_keys`, and the `kista_…` Bearer.
  - On the DuckDB routes a `kista_` Bearer is still taken as no token (spec 0008), so a stale
    configuration does not break public installs.
  - On the API it is an invalid token (`401`).
- **A publisher's Bearer token of any kind.** Publisher routes take only signed requests. The
  routes open to publishers stay the same.

**Migration** `0028_publisher_keys`, on all three dialects:
- drops `publisher_github` and `api_keys`;
- creates `publisher_keys`;
- adds the two release columns;
- `-- +min_reader 28`, because older binaries read the dropped tables.

Publishers and their grants stay. No deployment publishes through the removed credentials yet:
- kista's own live scripts call only the server's `whoami`;
- Enterest is in draft.

The release notes say that each publisher needs a key.

**Bundles** (spec 0014) carry only upstream origins and are unaffected.

### The developer kit (spec 0022)

- **`kista_publish.py`** (the fork):
  - **The key.** It takes the private key from `KISTA_PUBLISHER_KEY`: the key, its base64, or a
    file path. It writes the key to its own temporary file with mode 0600, with LF endings and a
    final newline, because CI secret stores strip them and `ssh-keygen` then fails with a misleading
    message. It removes the file at the end and never prints the key.
  - **Each request.** It writes the statement, signs it with `ssh-keygen -Y sign`, and sends the two
    headers. It signs again on every retry after `429`, `503` or `408`, so a retry never outlives
    `signed-at`.
  - **Before the first request.** It checks that `ssh-keygen -Y` exists (OpenSSH 8.1 or later), and
    otherwise says what to install.
  - **Modes.** `--check` calls `whoami` with a signed request, so the key and its grants are tested
    before the first tag. `--dry-run` prints the statements and the key's fingerprint.
  - **Requirements.** It needs only Python 3 and OpenSSH, so it runs in any CI. It runs on the CI
    host, not in DuckDB's build containers, whose OpenSSH may be older.
- **`_kista_publish.yml`** takes the secret `KISTA_PUBLISHER_KEY` (required), with no `id-token:
  write`. Its `environment` input stays: a GitHub environment is where the secret is kept, behind the
  environment's protection rules (reviewers, which tags).
- **The fork** is tagged anew (`sdk-v0.2.0`), and `KISTA.md`'s publishing sections are rewritten.
- **The template**:
  - both kinds' `distribution.yml` pass the secret and drop `id-token: write`;
  - the pins move to the new fork tag, and `TEMPLATE_KISTA_COMMIT` to a kista with this spec;
  - the READMEs say how to make a key (`ssh-keygen -t ed25519 -N '' -C ci -f publisher_key`),
    register its `.pub` in kista, and store the private half as an environment secret;
  - `test-kista.sh` registers a generated key instead of issuing an API key.
- **Other CI.** kista's Publishing page gains a GitLab CI and a Gitea example: build with `make
  release`, then run `kista_publish.py`. GitLab's masked variables cannot hold newlines, so the key
  goes in a File variable or as base64.
- **The kit only publishes.** Promotion is a release manager's act, with a token or a signed request
  of their own tooling.

**Keys in a KMS** (Enterest's pipeline). The key never leaves the KMS, so `ssh-keygen -f` cannot
sign with it. Two ways work:
- `ssh-keygen -Y sign -f key.pub` with an `ssh-agent` that holds the key (a PKCS#11 or KMS agent);
- in Go, the KMS's `Sign` as a `crypto.Signer`, wrapped by `ssh.NewSignerFromSigner`, with the
  SSHSIG framing around it.

ECDSA P-256 is the curve AWS KMS, Azure Key Vault and Google Cloud KMS all sign with.

### Scope of change

**kista:**
- `internal/auth`: the verifier for statements and SSHSIG; the provider and API-key code goes;
- `internal/api`: the routing gate for requests without `Authorization`;
- `internal/release`: the hash check before writes;
- `internal/store`: the migration, keys, publisher deletion and tenant deletion (spec 0019 lists the
  dropped tables);
- `internal/tenants`: key management and marking;
- `internal/audit` and `internal/config`;
- `cmd/kista`: the admin commands;
- the console: `Publishers.tsx` (keys instead of workflows and API keys); release provenance shows
  the fingerprint and the statement;
- the e2e tests that publish: `publish_test.go`, `sdk_test.go`, `relay_test.go`,
  `licensebundle_test.go`, `auth_test.go`;
- the docs: `publishing.md`, `access.md`, `api.md`, `concepts.md`, `configuration.md`, `console.md`,
  `index.md`, `security.md`, `licensing.md`, `observability.md`, `development.md`,
  `administration/commands.md`, `administration/index.md`, `administration/backup-restore.md`.

**Also:** the fork and the template, above.

**Phases:**
- **Phase 1:** kista, with its docs and e2e.
- **Phase 2:** the fork and the template.

Between the phases, the template's CI stays on its pinned kista commit.

### Changes to earlier specs

- **0001**: publishers authenticate by signed requests; API keys and trusted publishing are no
  longer part of the open server.
- **0006**: a publisher principal comes from a signed request. `signed-at` stands for a token's issue
  time.
- **0007**: the credential in `whoami` and provenance is `key:<fingerprint>`.
- **0008**: trusted publishing (phase 1b), API keys (phase 2) and the publisher's Bearer token are
  superseded. The routes, checks, grants and the publisher `whoami` stay.
- **0010**: the audit events of publisher keys, as above. The API-key and GitHub events go.
- **0011**: a publisher adds attachments with a signed request.
- **0015**: the console's publisher screen.
- **0019**: tenant deletion removes `publisher_keys`.
- **0021**: marking is refused while the publisher has keys. A marked publisher's keys are the
  server administrator's.
- **0022**: the publish step signs with a publisher key, not OIDC, and the "by hand, once" OIDC check
  is dropped.
- **Enterest 0001 and 0003** (its repository): the pipeline's publisher signs with a key in
  Enterest's KMS, instead of an API key.

## Security

- **kista holds no secret that publishes.** A copy of its database holds public keys only.
- **What a signature binds:**
  - the method;
  - the tenant, channel, extension and every query parameter (all in the path);
  - the body's hash;
  - five minutes.

  A captured request cannot be sent to another slot, channel, tenant or route, and cannot carry
  another body. The body check comes before any write.
- **What a signature does not bind:** other headers. An attachment's title and media type (spec
  0011, `Kista-Title`, `Content-Type`) are the publisher's to state; a captured request replayed
  within its five minutes could restate them with the same document, which the publisher's grant
  already allows.
- **Fails closed.** Each of these is refused, and nothing is written:
  - a key that is removed or expired;
  - a statement that does not match the request;
  - a stale `signed-at`;
  - another namespace;
  - a body with another hash.
- **No existence leaks.** The `400`s before the grant check compare the statement with the request
  only. The grants then decide `404` as in spec 0008.
- **Protecting the key in CI.** The docs recommend:
  - an environment secret, behind the environment's protection rules;
  - one key per publisher, and a publisher per channel that matters (staging, prod);
  - an expiry.

  Removing a key stops it at once. The releases it made are found by `key:<fingerprint>`.
- **Licence keys** (spec 0021): a tenant administrator cannot add a key to a marked publisher.
- **The GitHub issuer URL** stays refused for issuers (above).
- **Clock**: five minutes either way tolerates CI clocks. kista's own clock must be right, as it
  already must be for tokens.

## Testing

- **The statement**: parsing; every malformation (order, extra or missing lines, CR, a missing final
  LF, length); `method` and `path` mismatches.
- **SSHSIG**:
  - signatures made by `ssh-keygen` for Ed25519 and ECDSA P-256 (committed vectors);
  - the wrong namespace, a hash other than `sha256` and `sha512`, trailing bytes;
  - an embedded key that differs from the registered one;
  - an unsupported key type, a certificate;
  - a tampered statement.
- **The store suite** on all three dialects:
  - keys, and fingerprint uniqueness per tenant;
  - deleting a publisher and a tenant;
  - the migration and its `min_reader`.
- **The API**:
  - publication, promotion, release reads, attachments and `whoami` by signed request;
  - `401` for an unknown, removed or expired key, a stale `signed-at` and a bad signature;
  - `400` for a body whose hash differs, with nothing written: no release, no licence keys, no Build
    state;
  - a replay answering `200` and changing nothing;
  - `100-continue` refused before the body;
  - the removed routes answer `404`;
  - a `kista_` Bearer is `401` on the API and no token on the DuckDB routes;
  - `publish.providers` refused at startup;
  - key management on a marked publisher.
- **`kista_publish.py`** (the fork):
  - the statement;
  - signing through `ssh-keygen`;
  - the key from text, base64 and a file, with CRLF and no final newline;
  - re-signing on retry;
  - `--check` and `--dry-run`;
  - the key never printed.
- **End to end**:
  - the template's CI publishes both kinds with a generated key, then DuckDB installs them;
  - kista's e2e publishes `kista_demo` the same way.

## Alternatives considered

- **Keep GitHub trusted publishing next to keys.** It ties kista to one forge's claims, and a
  developer elsewhere gets a second-class path. The owner chose one way for every forge.
- **Keep API keys.** A bearer secret that kista also holds. A signing key costs the developer the same
  effort, and kista keeps only public keys.
- **A statement per route** (publication, promotion, read) with the route's fields. More parsing and
  rules for the same binding. Signing the method, the path and the body's hash binds every route at
  once.
- **Sign the binary only.** A detached signature over the file proves who built it, but not where it
  should go: a captured upload could be sent to another channel.
- **HTTP Message Signatures (RFC 9421).** A general standard that canonicalises headers. It is more
  than a publisher needs, and no common CLI makes such signatures. `ssh-keygen -Y sign` is on every
  CI host.
- **Raw Ed25519 or X.509.** Raw keys have no tooling developers already use, and X.509 brings a CA.
  SSH keys are what developers already make and register on their forge.
- **Source claims in the statement** (repository, ref, commit, CI run). They are unverified, and add
  parsing and display. Left for later.

## Follow-ups

- Enterest's spec 0003: the central build's publisher key in its KMS, and how a developer tells
  Enterest about a new tag.
- Source claims (repository, commit, run) in provenance, if needed.
- Developer self-service registration of keys.
- Short-lived keys signed by a key (SSH certificates), if a tenant wants to delegate publishing.
