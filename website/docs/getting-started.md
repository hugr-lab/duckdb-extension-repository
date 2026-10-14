---
title: Getting started
---

# Getting started

This page runs kista on your machine with the smallest setup: SQLite for the metadata, a local
directory for extension bodies and a signing key in a file. You create a tenant and a signed
channel, put an extension into it (your own build, or one mirrored from DuckDB), start the server,
and install the extension from DuckDB.

The setup uses `profile: dev`, which is meant for development only. For a real deployment, read
[Configuration](configuration.md), [Signing keys](signing-keys.md) and [Storage](storage.md).

## What you need

- Go, at the version in `go.mod`, to build kista (or a release binary, see [Deployment](deployment/index.md)).
- `openssl`, to create a signing key.
- DuckDB 2.0, which has `CREATE EXTENSION REPOSITORY`. kista is tested against the DuckDB commit in
  `e2e/DUCKDB_PIN`.
- An extension to serve: a `.duckdb_extension` file built for your DuckDB version and platform, or
  network access to `extensions.duckdb.org` to mirror one.

## Build the binary

```bash
git clone https://github.com/hugr-lab/duckdb-extension-repository.git
cd duckdb-extension-repository
make build          # writes bin/kista; the same as: GOWORK=off go build -o bin/kista ./cmd/kista
export PATH="$PWD/bin:$PATH"
```

The administration console is embedded only if you run `make console` (Node 22) before
`make build`; without it `/ui/` serves a placeholder. This page does not need the console. The
published image and the release binaries embed it ([Deployment](deployment/index.md)): a release
binary can replace this step.

## Create a signing key

A signed channel signs every release with an RSA-2048 key. Here the key is a PEM file in a directory
that kista reads `file:` keys from. The file must be a regular file (not a symlink), owned by the
user that runs kista, with no permissions for group or others (mode `0600` or stricter).

```bash
mkdir -p /tmp/kista/keys && chmod 700 /tmp/kista/keys
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out /tmp/kista/keys/prod.pem
chmod 600 /tmp/kista/keys/prod.pem
```

kista accepts an unencrypted PKCS#8 or PKCS#1 RSA key of exactly 2048 bits with exponent 65537:
DuckDB requires a 256-byte signature.

## Write the configuration

```yaml title="/tmp/kista/kista.yaml"
profile: dev
store:
  kind: sqlite
  sqlite:
    path: /tmp/kista/kista.db
signers:
  file_dir: /tmp/kista/keys        # file:<name> keys resolve here; an absolute path
blob:
  domains:
    - name: default
      kind: fs
      fs:
        root: /tmp/kista/blobs
  spool_dir: /tmp/kista/spool
serve:
  public_url: http://127.0.0.1:8080
  listeners:
    - addr: 127.0.0.1:8080
      scheme: http
```

- `profile: dev` allows `file:` keys without further settings, an `http` public URL on loopback,
  and rotation minimums below 24 hours. Outside `dev`, file keys need `signers.allow_file: true`
  as well as `signers.file_dir`; production keys belong in a vault or HSM (see
  [Signing keys](signing-keys.md)).
- SQLite means one replica. PostgreSQL and SQL Server are the multi-replica stores (see
  [State](state.md)).
- `blob.domains` must include a domain named `default`. With `profile: dev` and an absolute SQLite
  path you may leave `blob` out: the default domain is then a `blobs` directory beside the database
  file, and the spool directory a `spool` directory beside it.
- One plain `http` listener is enough for public extensions and DuckDB's built-in HTTP client. The
  HTTP API (`/api/`), the console and Bearer tokens work only on `https`; see
  [Using kista from DuckDB](duckdb.md) for TLS listeners.

A few settings (the store, `azure.cloud` and the Azure identity's ids, the rotation minimums and
`serve.public_url`) can also be given as `KISTA_` environment variables; the rest are file-only.
See [Configuration](configuration.md).

## Create the schema, a tenant and a channel

`kista admin` is the server administrator's command line. It runs with the server's configuration
and talks to the database directly; the server does not need to be running.

```bash
alias ka='kista admin -config /tmp/kista/kista.yaml'

ka migrate                                   # apply the schema; prints "schema ok"
ka key check -signer file:prod.pem           # open the key, sign a probe; prints its fingerprint
ka tenant create acme                        # prints the tenant id
ka channel create acme/prod -kind signed
```

With `profile: dev` every command prints `WARNING: profile dev` on stderr first. Each change is also
logged on stderr with the operating-system user that made it, for example
`admin tenant create acme by os:501:alice`.

## Tell kista which DuckDB versions the channel serves

A channel serves a list of DuckDB versions. A version is first declared on the server, then added to
the channel. Read the version from the extension you want to serve:

```bash
kista ext inspect my_ext.duckdb_extension
```

```text
platform:          osx_arm64
abi:               CPP
duckdb version:    v2.0.0
extension version: 1.2.0
```

The DuckDB version is the release tag (`v2.0.0`) for a release, or the source id (for example
`eb0d9df48e`) for a development build. Declare it and add it to the channel:

```bash
ka version add v2.0.0 -kind release          # -kind dev for a source id
ka channel versions acme/prod -add v2.0.0
```

An extension built against the C API (`abi: C_STRUCT`) shows a `c api version` instead. It is served
under every DuckDB version of the channel that accepts that C API, so declare the maximum C API
version each major accepts: `ka version add v2.0.0 -kind release -c-api v1.5.6`, or later
`ka version c-api v2.0.0 -c-api v2.0.0`. A major's maximum is never changed once set.

## Add the key to the channel

```bash
ka key add acme/prod -signer file:prod.pem -active
```

It prints the key id and its fingerprint (`sha256:…`). The channel's `.well-known` file now lists the
key:

```bash
ka wellknown acme/prod
```

```json
{"signature_keys":["-----BEGIN PUBLIC KEY-----\nMIIBIjANBgkq…\n-----END PUBLIC KEY-----\n"]}
```

## Put an extension into the channel

Do one of the two.

### Publish your own build

```bash
ka release add acme/prod my_ext.duckdb_extension -name my_ext
```

kista reads the footer (platform, DuckDB or C API version, extension version), checks that the
binary exports the entry point DuckDB calls for `my_ext`, stores the body once, signs it with the
channel's active key and prints the release id. The file may be unsigned, as your build produces it:
its last 256 bytes are replaced by the channel's signature. The release is public; add `-private`
for a private one (see [Access](access.md)). Running the same command again prints the same id.

```bash
ka release list acme/prod
```

People and CI publish through the HTTP API instead; see [Publishing](publishing.md).

### Or mirror one from DuckDB

An upstream fetches builds from `extensions.duckdb.org`, verifies DuckDB's signature and releases
them in the channel re-signed with your key. Add an upstream for one core extension and the
platforms you need:

```bash
ka upstream add acme core -kind duckdb-core -channel prod -platforms osx_arm64,linux_amd64 -extensions json -public
ka upstream sync acme core
```

Upstream runs are done by `kista serve`: `sync` only requests a run, which the server picks up within
30 seconds. A mirror also runs on its own schedule (`upstreams.interval`, every
6 hours by default). Without `-public` the mirrored releases are private. DuckDB publishes binaries
only for its releases, so a channel version that is a source id shows the cell as `missing`.

```bash
ka upstream cells acme core
```

```text
DUCKDB  PLATFORM   NAME  OUTCOME   FETCHED               DETAIL
v2.0.0  osx_arm64  json  released  2026-10-12T09:10:00Z
```

See [Upstreams](upstreams.md) for community extensions, other repositories, pull-through and
passthrough channels.

## Run the server

```bash
kista serve -config /tmp/kista/kista.yaml
```

The server logs JSON lines on stdout, one `request` line per request. It stops on `SIGTERM` or
Ctrl-C, letting downloads in flight finish (for up to `serve.shutdown_timeout`, 30 seconds by
default). Check it:

```bash
curl http://127.0.0.1:8080/healthz      # ok
curl http://127.0.0.1:8080/readyz       # ready
curl http://127.0.0.1:8080/acme/prod/.well-known/duckdb-extension-repo.json
```

## Install from DuckDB

The channel's prefix is `http://127.0.0.1:8080/acme/prod`. DuckDB reads `.well-known` only through
httpfs, so over plain `http` without httpfs you give the key yourself. Take the PEM string from the
`.well-known` file (`signature_keys[0]`) and paste it as one quoted string, line breaks included:

```sql
SET allow_extension_repositories = 'allowed';

CREATE EXTENSION REPOSITORY local
  WITH PREFIX 'http://127.0.0.1:8080/acme/prod'
  USING PUBLIC KEY '-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA3M1S9qzEr2GXQauaUIZz
...
-----END PUBLIC KEY-----';

INSTALL my_ext FROM local;
LOAD my_ext FROM local;
```

`CREATE EXTENSION REPOSITORY` answers with the repository's name, prefix and the fingerprints of the
keys it pinned; they match the fingerprint `key add` printed. The repository is kept by DuckDB (in
its `extension_repository_directory`) for later sessions.

- `allow_extension_repositories` starts as `undecided`, which blocks adding a repository. In a
  running database it can move from `undecided` to `allowed` once; it can also be set at startup.
- Use `LOAD my_ext FROM local`, not a bare `LOAD my_ext`: a bare `LOAD` checks DuckDB's own keys.
- Run this in a session where httpfs is not loaded. Once httpfs is loaded, DuckDB fetches an
  `http://` prefix over `https://` on the same host and port, which this plain `http` listener does
  not answer.

You can watch the requests in the server's log: one `GET` of
`/acme/prod/<duckdb_version>/<platform>/my_ext.duckdb_extension.gz`.

## Next steps

- [Using kista from DuckDB](duckdb.md): `https`, keys from `.well-known`, private extensions with a
  token, autoloading through a passthrough channel, version pinning.
- [Tenants and channels](administration/tenants-and-channels.md): a `staging` channel with its own
  key, and promotion to `prod`.
- [Access](access.md): issuers, grants and private extensions.
- [Commands](administration/commands.md): every `kista admin` command.
