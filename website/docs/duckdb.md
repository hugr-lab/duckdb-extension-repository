---
title: Using kista from DuckDB
---

# Using kista from DuckDB

DuckDB 2.0 uses a kista channel as a trusted extension repository with no plugin. This page shows
the SQL a client runs, what DuckDB requests, and the server settings each case needs. The behaviour
described here is that of the DuckDB commit in `e2e/DUCKDB_PIN` (v2.0-cyanoptera), most of it
confirmed by kista's end-to-end tests; the source locations are in
[spec 0001](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0001-architecture/spec.md)
("DuckDB behaviour relied on").

## The short version

```sql
SET allow_extension_repositories = 'allowed';
CREATE EXTENSION REPOSITORY corp WITH PREFIX 'https://extensions.example.com/acme/prod';
INSTALL tresor FROM corp;
LOAD tresor FROM corp;
```

The prefix is `<public_url>/<tenant>/<channel>`. Without `USING PUBLIC KEY`, DuckDB reads the keys
from the channel's `.well-known` file, which needs httpfs (see [Keys](#keys)).

## URL layout

Every channel answers these paths under its prefix:

| Path | What |
| --- | --- |
| `/<tenant>/<channel>/.well-known/duckdb-extension-repo.json` | the channel's trusted keys, `{"signature_keys": ["-----BEGIN PUBLIC KEY-----…"]}`; signed channels only, always public |
| `/<tenant>/<channel>/<duckdb_version>/<platform>/<name>.duckdb_extension.gz` | the **flat** path: the current release for that DuckDB version and platform |
| `/<tenant>/<channel>/<name>/<ext_version>/<duckdb_version>/<platform>/<name>.duckdb_extension.gz` | the **versioned** path: one extension version |

- `<duckdb_version>` is DuckDB's release tag (`v2.0.0`) or, on a development build, its source id
  (`eb0d9df48e`). `<platform>` is what `PRAGMA platform` returns (`linux_amd64`, `osx_arm64`, ...).
- Each binary path also answers without `.gz`. The `.gz` answer is always gzip; the plain one is the
  uncompressed file. Both answer `GET` and `HEAD` with an `ETag` and `304` on a matching
  `If-None-Match`; the `.gz` answer also takes a single `Range`.
- kista never redirects: DuckDB's built-in client does not follow redirects.
- `/healthz` and `/readyz` are the health checks; `/api/` and `/ui/` are the HTTP API and the
  console, on `https` only.

## Two ways DuckDB downloads

Which HTTP client DuckDB uses decides what works:

| Prefix | Client | What it does |
| --- | --- | --- |
| `http://…`, httpfs not loaded | DuckDB's built-in client | one `GET` of the `.gz`; no `HEAD`, no fallback, never an `Authorization` header |
| `https://…` | httpfs | checks the `.gz`, then the plain name; sends the Bearer token of a matching http secret |
| `http://…`, httpfs loaded | httpfs | DuckDB changes the scheme to `https` on the **same host and port** first |

So:

- public releases can be installed over plain `http` with no extension loaded, which is how a client
  gets httpfs in the first place;
- private releases need `https` and httpfs, because only httpfs sends a token;
- once httpfs is loaded, an `http://host:8080` prefix is fetched as `https://host:8080`. Serve both
  schemes on one port with a `dual` listener, or use the default ports (`http` on 80, `https` on
  443).

```yaml title="kista.yaml"
serve:
  public_url: https://extensions.example.com
  listeners:
    - addr: 0.0.0.0:443
      scheme: https
      tls:
        cert_file: /etc/kista/tls.crt
        key_file: /etc/kista/tls.key
    - addr: 0.0.0.0:80
      scheme: http            # public releases for the bootstrap; no tokens, no API
```

A `dual` listener (`scheme: dual`, with `tls`) answers TLS and plain `http` on one port. Behind a TLS
proxy, use `scheme: https` with `behind_proxy: true` and set `serve.trusted_proxies`. See
[Configuration](configuration.md) for every listener setting.

## Allowing repositories

```sql
SET allow_extension_repositories = 'allowed';
```

The setting starts as `undecided`: repositories created earlier are still trusted, but no new one
can be added. While the database is running it can move from `undecided` to `allowed` or
`forbidden`, and from `allowed` to `forbidden`, never back. `forbidden` also distrusts existing
repositories. It can be set to any value at startup.

DuckDB keeps each repository in its `extension_repository_directory`, so it survives the session.
`CREATE OR REPLACE EXTENSION REPOSITORY` changes one; `DROP EXTENSION REPOSITORY` removes it.
`duckdb_extension_repositories()` lists them with their prefixes and key fingerprints.

## Keys

DuckDB pins a channel's keys when the repository is created and checks them on **every `LOAD`**, not
only at `INSTALL`. There are two ways to give them.

**From `.well-known`.** With no key in the statement, DuckDB reads
`<prefix>/.well-known/duckdb-extension-repo.json`:

```sql
CREATE EXTENSION REPOSITORY corp WITH PREFIX 'https://extensions.example.com/acme/prod';
```

DuckDB reads this file through httpfs: on an `http(s)` prefix without httpfs loaded, the statement
fails. The file is also read without the session's secrets and without `ca_cert_file`, so a server
whose certificate comes from a private CA cannot serve it; give the key instead.

**In the statement.** `USING PUBLIC KEY` and `USING PUBLIC KEYS` are the same clause: a
comma-separated list of one or more keys, each an SPKI PEM string or base64 DER:

```sql
CREATE EXTENSION REPOSITORY corp WITH PREFIX 'http://extensions.example.com/acme/prod'
  USING PUBLIC KEYS '-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA…
-----END PUBLIC KEY-----', '-----BEGIN PUBLIC KEY-----
…
-----END PUBLIC KEY-----';
```

Get the keys from the server administrator (`kista admin wellknown acme/prod`) or from the
`.well-known` URL. Compare the fingerprints `CREATE` reports with the ones the administrator lists
(`kista admin key list acme/prod`): both are `sha256:` and the hex SHA-256 of the key.

When the channel's keys rotate, a client recreates the repository with the new key set
(`CREATE OR REPLACE …`, or both keys in `USING PUBLIC KEYS`). An extension installed before the
rotation keeps loading as long as its signing key is still pinned. See
[Key rotation](administration/key-rotation.md).

## Bootstrapping httpfs

A client that needs `https` (private extensions, `.well-known`) first needs httpfs. If DuckDB
publishes httpfs for the client's version, it comes from DuckDB's autoinstall or from a passthrough
channel. Otherwise it comes from a public signed channel over plain `http`, with the key given:

```sql
SET allow_extension_repositories = 'allowed';

-- 1. plain http, DuckDB's built-in client, key in the statement
CREATE EXTENSION REPOSITORY boot WITH PREFIX 'http://extensions.example.com/acme/prod'
  USING PUBLIC KEY '-----BEGIN PUBLIC KEY-----
…
-----END PUBLIC KEY-----';
INSTALL httpfs FROM boot;
LOAD httpfs FROM boot;

-- 2. https from now on: .well-known, tokens
CREATE EXTENSION REPOSITORY corp WITH PREFIX 'https://extensions.example.com/acme/prod';
INSTALL tresor FROM corp;
LOAD tresor FROM corp;
```

The channel must hold httpfs as a public release for the client's DuckDB version and platform.
kista serves public releases over plain `http` for this reason: the signature protects them, not the
transport. Private releases are never served over `http`.

## Install and load

```sql
INSTALL my_ext FROM corp;      -- the current release, flat path
LOAD my_ext FROM corp;
```

- DuckDB keeps an install from a named repository under `…/repositories/<repo>/`. Installs of the
  same name from two repositories coexist, and `LOAD my_ext FROM r2` refuses one installed from
  `r1`.
- A bare `LOAD my_ext` checks DuckDB's own core keys, not the channel's. Always load with `FROM`.
- A replacement of a core extension (your own `httpfs` in a signed channel) is never autoloaded:
  load it explicitly with `LOAD httpfs FROM corp`.
- A `C_STRUCT` (C API) build is served under every DuckDB version of the channel that accepts it;
  a `CPP` or `C_STRUCT_UNSTABLE` build only under its exact DuckDB version.

## Version pinning

```sql
INSTALL my_ext FROM corp VERSION '1.2.0';
LOAD my_ext FROM corp;
```

`VERSION` makes DuckDB request the versioned path with the version verbatim. That path serves an
`active` or `deprecated` release of that version; a yanked one is served nowhere. Without `VERSION`
DuckDB gets the channel's current release.

A version in a channel never changes its body: a fix is a new version. So pinning the extension
version and the repository is enough; there is no need to pin a hash, which would change on every
key rotation anyway.

## Updating

`UPDATE EXTENSIONS` does not see installs from a named repository. Update with `FORCE INSTALL`:

```sql
FORCE INSTALL my_ext FROM corp;                     -- the current release
FORCE INSTALL my_ext FROM corp VERSION '1.3.0';     -- a given version
```

Moving a client from one channel to another means a new repository name, or
`CREATE OR REPLACE EXTENSION REPOSITORY` of the same name with the new prefix followed by
`FORCE INSTALL`. A channel's prefix never changes, because DuckDB records it with each install.

## Private extensions

A private release is served only over `https`, to a request with a Bearer token that holds `install`
on it. DuckDB sends a token through httpfs from an http secret whose `SCOPE` matches the URL:

```sql
LOAD httpfs FROM boot;   -- see "Bootstrapping httpfs"

CREATE SECRET corp_token (
  TYPE http,
  BEARER_TOKEN 'eyJhbGciOi…',
  SCOPE 'https://extensions.example.com/acme/prod/'
);
CREATE EXTENSION REPOSITORY corp WITH PREFIX 'https://extensions.example.com/acme/prod';
INSTALL private_ext FROM corp;
LOAD private_ext FROM corp;
```

- **End the scope with a slash.** DuckDB matches a scope by plain string prefix (the longest match
  wins), so `…/acme/prod` without the slash also sends the token to `…/acme/prod2/` and to any
  tenant whose name starts the same way.
- The token is a JWT from one of the tenant's issuers. Its audience must be the tenant's canonical
  URL, `<public_url>/<tenant>` (for example `https://extensions.example.com/acme`), or an audience
  the server administrator assigned to the tenant (`kista admin tenant audience add`).
- Without a valid token every non-public path answers `401`; with a valid token but no grant it
  answers `404`, exactly like a missing path. DuckDB reports both as a failed install.
- The token is only read on `https` requests. DuckDB's built-in client never sends one.
- tresor keeps the secret's token fresh on hugr nodes.

On the server side the administrator adds an issuer, a grant and makes the release private:

```bash
kista admin -config kista.yaml issuer add acme -name corp -url https://idp.example.com/realms/corp
kista admin -config kista.yaml grant add acme -principal 'subject:corp|alice' -verb install -channel prod
kista admin -config kista.yaml release add acme/prod private_ext.duckdb_extension -name private_ext -private
```

See [Access](access.md) for issuers, claims, principals and grants.

## Autoloading and the passthrough channel

DuckDB treats `custom_extension_repository`, `autoinstall_extension_repository` and
`INSTALL x FROM '<url>'` as **core-typed**: a binary from such a URL must carry DuckDB's own core
signature, and autoloading works only there. A signed channel's binaries are refused on that path.

A **passthrough** channel serves DuckDB's core binaries byte-identical, so a client can autoload core
extensions from your server, for example on a network without internet access:

```sql
-- where autoloading installs core extensions from
SET autoinstall_extension_repository = 'http://extensions.example.com/acme/core';
SET autoinstall_known_extensions = true;
SET autoload_known_extensions = true;
SELECT '127.0.0.1'::INET;   -- inet is installed from the passthrough channel and loaded

-- where a plain INSTALL x (no FROM) installs from
SET custom_extension_repository = 'http://extensions.example.com/acme/core';
INSTALL spatial;
LOAD spatial;
```

`duckdb_extensions()` shows the channel's prefix in `installed_from`. A plain `http://` URL here uses DuckDB's built-in client, so autoloading works
before httpfs is available; the channel answers the same bytes over `https`.

- A passthrough channel serves only `duckdb-core` extensions, mirrored from `extensions.duckdb.org`
  for the channel's DuckDB versions. Community extensions would fail DuckDB's core-key check.
- It has no keys and no `.well-known` file, and it never looks at a token: its releases are public
  to anyone who can reach it. Its guarantee is the allowlist of the upstream that feeds it.
- If the tenant releases its own build of a core name in any signed channel, the passthrough
  channels stop serving DuckDB's build of that name, so autoloading cannot silently pick it over
  the replacement. `kista admin shadow list` and `shadow remove` manage this.

The server side:

```bash
kista admin -config kista.yaml channel create acme/core -kind passthrough
kista admin -config kista.yaml channel versions acme/core -add v2.0.0
kista admin -config kista.yaml upstream add acme core-pass -kind duckdb-core -channel core \
  -platforms linux_amd64,osx_arm64 -extensions inet,spatial,httpfs -public
```

`-public` is required: a passthrough channel's releases are always public. See
[Upstreams](upstreams.md).

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| `CREATE EXTENSION REPOSITORY` refused | `allow_extension_repositories` is not `allowed` |
| `CREATE` without a key fails on an `http` prefix | httpfs is not loaded; give `USING PUBLIC KEY` |
| `CREATE` without a key fails on a certificate error | the server's CA is private; give `USING PUBLIC KEY` |
| install fails with a signature error | the pinned keys do not include the key that signed the served file; recreate the repository with the channel's current keys |
| `LOAD x FROM r` fails after a key rotation | the key that signed the installed file is no longer pinned; `FORCE INSTALL` or pin both keys |
| a private extension fails to install | no httpfs, an `http` prefix, a scope without the trailing slash, a wrong audience, or no `install` grant |
| `http://host:port` fails after `LOAD httpfs` | the port does not speak TLS; use a `dual` listener |
| `UPDATE EXTENSIONS` says the extension is not installed | installs from a named repository are updated with `FORCE INSTALL x FROM r` |
