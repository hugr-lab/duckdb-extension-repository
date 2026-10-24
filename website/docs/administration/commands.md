---
title: Command reference
---

# Command reference

`kista` is one binary with three command groups and a version command:

| Command | What it does |
| --- | --- |
| `kista serve -config <file\|env:NAME>` | runs the repository server |
| `kista ext inspect` and `kista ext verify` | read an extension file on disk; no config, no database |
| `kista bundle keygen\|keys\|sign\|inspect` | bundle keys and bundles ([Bundles](#kista-bundle)); no config, no database |
| `kista admin -config <file\|env:NAME> <command> ...` | the server administrator's CLI |
| `kista version` | prints kista's own version |

Build it with `make build` (writes `bin/kista`) or `go build ./cmd/kista`.

## How the CLI reaches kista

`kista admin` does not call the HTTP API. It loads the same configuration file as `kista serve`
(with the same `KISTA_*` environment overrides), opens the database itself and calls the same
service layer the API calls. So:

- run it as the service's OS user, on a host that can reach the database (and, for the commands
  that touch bodies, the blob storage and the key sources);
- it acts as a **server administrator** with every right. The actor recorded in events and key
  history is `os:<uid>:<user name>`;
- every successful change prints a one-line record to stderr, such as
  `admin tenant create acme by os:1000:kista`. The audit event is written to the database as for an
  API call (see [Observability](../observability.md));
- it works with or without a running `kista serve`. A running server sees changes at its next
  request, because every change bumps a version counter that the server's caches check.

Some commands only record a request: `upstream sync` and `feed sync` mark a run as requested, and a
running `kista serve` takes it. Without a server, nothing runs.

:::warning Migrations
`kista admin` opens the store with the configured `store.migrate`. With the default `auto`, **any**
`kista admin` command run with a newer binary applies that binary's pending migrations first. See
[Upgrading](upgrading.md).
:::

## Conventions

- `-config <file>` (or `-config env:NAME`, a variable holding the YAML: [Configuration](../configuration.md#the-file)) comes **before** the command: `kista admin -config /etc/kista/kista.yaml tenant list`.
- A command's own flags may come before or after its positional arguments.
- Flags take one dash (`-force`); Go's flag parser also accepts two (`--force`).
- `<tenant>/<channel>` addresses a channel, such as `acme/prod`.
- Lists print an aligned table with a header row. Timestamps are UTC, RFC 3339.

**Exit status** of every `kista admin` command:

| Status | Meaning |
| --- | --- |
| `0` | done |
| `1` | refused or failed; the reason is on stderr after `kista admin:` |
| `2` | usage error (unknown command, missing argument or flag); the usage text is printed to stderr |

`kista admin help`, or `kista admin` with no command or without `-config`, prints the usage text and
exits `2`.

## kista serve

```bash
kista serve -config /etc/kista/kista.yaml
```

Runs the server until `SIGTERM` or `SIGINT`. Logs are JSON lines on stdout. At start it logs its
version, the Go version and the platform; with `profile: dev` it also logs a warning. Exit status:
`0` after a clean shutdown, `1` when the configuration cannot be loaded or the server fails, `2` on
a usage error, and the signal's status after a second signal (below). What it serves is in
[Configuration](../configuration.md) and [Using kista from DuckDB](../duckdb.md).

On `SIGTERM` or `SIGINT`:

1. `/readyz` fails, and kista waits `serve.drain_timeout` (5 seconds by default), so load
   balancers stop sending.
2. The listeners shut down. Downloads still running after `serve.shutdown_timeout` (30 seconds by
   default) are aborted.
3. The background runners (re-signer, upstreams and pull-through fetches, feeds, the tenant
   reaper, event retention and sinks, daily statistics, garbage collection) stop and release their
   leases, within one more `serve.shutdown_timeout`. A runner still busy then is abandoned, and its
   lease in the database expires as it would after a crash; another replica takes the work over
   then.
4. The event writer writes what the runners and the drain recorded, and the download counter its
   last counts, within 11 seconds of their own, so a stuck runner cannot take that time from them.
5. Metrics are flushed, for up to 10 seconds.
6. The storage service and the database close. Closing the database waits for queries still in
   progress, so a runner abandoned in the middle of one can delay the exit a little.

A wait that runs out is logged as an error. From the signal to the exit takes at most
`drain_timeout + 2 × shutdown_timeout + 21s` (86 seconds with the defaults), plus closing the
database and the storage. A second `SIGTERM` or `SIGINT` (another Ctrl-C) ends the process at
once; the exit status is then the signal's, as the shell reports it (`143` for `SIGTERM`, `130`
for `SIGINT`).

## kista version

```bash
kista version        # or kista --version
```

Prints kista's version, the Go version it was built with and the platform, then exits `0`:

```text
kista v1.2.3 (go1.27.3, linux/amd64)
```

A build without a version set prints `dev`. This is kista's own version. The DuckDB versions a
server serves are a different thing, listed by `kista admin version list` (see
[DuckDB versions](#duckdb-versions)).

## kista ext

Offline tools for one extension file. They read no configuration.

```bash
kista ext inspect <file> [-key <public key file>]...
kista ext verify  <file> -key <public key file> [-key ...]
```

A public key file is SPKI PEM or base64 DER, at most 64 KiB. `-key` is repeatable. Files up to
1 GiB are read.

**`ext inspect`** prints the footer metadata, the body hash and the licence keys:

```text
file:              httpfs.duckdb_extension
body size:         ...
body hash:         sha256:...
metadata prefix:   true
platform:          linux_amd64
abi:               CPP
duckdb version:    v2.0.0
extension version: 1.0
licence keys:      none (no key block)
signature:         sha256:...
```

For a `C_STRUCT` build it prints `c api version:` instead of `duckdb version:`. `licence keys:`
lists the fingerprints of the keys the build names for its licences (the key block of the kista
SDK, spec 0021), one per line; `none (no key block)` for an extension without one, or
`none (licence keys: malformed: …)` with the reason when the block does not parse, or the binary
holds blocks that differ.

The `signature:` line appears only with `-key`: the fingerprint of the key that verifies the
signature, or `not by any given key`. It exits `0` whether or not the signature verifies, `1` if a file cannot be
read or parsed, `2` on a usage error.

**`ext verify`** needs at least one `-key`. It prints the fingerprint of the key that verifies the
signature and exits `0`, or exits `1` with
`the signature does not verify with any given key`. Usage errors exit `2`.

The body hash is the value the API and `kista admin release list` show, and the one `block` and
`attachment` commands take.

## kista duckdb

The names DuckDB owns, read from its sources (spec 0030). No configuration, no database.

```text
kista duckdb core-extensions <tag> | <source id> -trust-commit | -dir <checkout> [-commit <sha>]
kista duckdb community-extensions [ref] | -dir <checkout>
```

- `core-extensions` prints a DuckDB version's core extensions, the list `version add
  -core-extensions` takes: the extensions in its `extension/` directory, the ones it builds in
  (`internal_extensions[]`), knows functions, settings and types of, or autoloads
  (`extension_entries.hpp`), and the out-of-tree ones its `.github/config` loads. It streams the
  source tarball at the ref from `codeload.github.com` (a release tag, or a dev build's source id),
  keeps the few files it reads in memory and never runs anything it fetched. The first line, `#
  commit <sha> (...)`, names the commit read; `version add` records it. With `-dir` it reads a
  checkout instead (`-commit` names its commit). A source id or other commit needs
  `-trust-commit`: GitHub serves any fork's commit under `duckdb/duckdb`, so the operator vouches
  for it; a release tag is DuckDB's own. It fails when a source it reads comes out empty, or the
  list lacks `icu`, `json` or `parquet`: DuckDB's sources changed layout. `version add` refuses
  the community list as a version's core extensions.
- `community-extensions` prints duckdb/community-extensions' extension names (each
  `extensions/<dir>` and the name its `description.yml` gives), at a ref (default: its default branch), for
  `reserved add -kind community`.

## kista bundle

Bundle keys and bundles for air-gapped sites (spec 0014). No configuration, no database.

```text
kista bundle keygen -o <key.pem>
kista bundle keys <https://host/tenant/channel>
kista bundle sign -key <key.pem> -plan <plan.json> -in <unsigned bundle> -o <bundle>
kista bundle inspect <bundle> [-plan <plan.json>] [-max-body <bytes>]
```

- `keygen` writes a new Ed25519 bundle key (PKCS#8 PEM, mode `0600`, never over an existing file)
  and prints its fingerprint, `sha256:<hex>`, the value a site pins in `upstreams.bundle_keys`.
- `keys` fetches a repository's `.well-known` file and prints its keys' fingerprints, to compare
  with what the publisher published before pinning one. Never trust the media for a fingerprint.
- `sign` signs a bundle exported unsigned, on a machine that holds the key: the key must be among
  the plan's bundle keys, the bundle must answer that plan and ask for nothing else (its licence
  request the plan's, each licence of the name it is carried for and of the holder the plan names), and be
  complete, its files inflating to the manifest's sizes and hashes. A feed's entries must be whole,
  and each feed body must start at the plan's cursor (or at `0` for a body of a file the bundle
  brings for the feed's sources). It copies the bundle once, readable by its owner only (`0600`: it
  may carry licences), and never writes over an existing file.
- `inspect` checks a bundle offline: the manifest's signature, every entry's hash and size, every
  file's source signature, footer and the file checks publication runs. With `-plan` it also says
  whether the bundle key is pinned, and each source key pinned, listed only or missing, applies the
  plan's version filters, and reports a bundle answering another plan. It lists each licence the
  bundle carries (name, holder, key, expiry), and reports one not of its name or of the plan's holder. `-max-body` (default 1 GiB,
  the site's `blob.max_body`) bounds what it inflates. It prints the cells per outcome, the sizes,
  the spool an import needs and the bundle's SHA-256, and exits `1` on any problem. Without
  `-plan`, a bundle is reported consistent, not trusted.
  For a bundle's feeds (spec 0014 phase 1b), `inspect` checks their
  entries' order, hashes and sizes, each statement's signature against the bundle's keys file (with
  `-plan`, only the keys the plan's feed pins, printed `pinned` or `listed, not pinned`), its
  issuer, its body within a covered range, and that a statement with content has its document after
  the batch that names it. It prints the feeds' statements, withdrawals, statements without content
  and documents.

## Database

| Command | What it does | Output |
| --- | --- | --- |
| `migrate` | applies pending migrations, whatever `store.migrate` says | `schema ok` |
| `check` | checks the schema without changing it: fails if migrations are pending or the database needs a newer binary | `schema ok` |
| `backup <file>` | SQLite only: a consistent copy (`VACUUM INTO`) into a new file with mode `0600`; refuses an existing file | none |

`backup` on PostgreSQL or SQL Server fails: use the database's own backups. See
[Backup and restore](backup-restore.md).

## Bootstrap

| Command | What it does |
| --- | --- |
| `bootstrap [-adopt]` | applies the configuration's `bootstrap` section once, printing its plan; `-adopt` also creates objects in a tenant the bootstrap did not make |
| `bootstrap -dry-run` | prints the plan (create, add, same or refused, one line per object) after opening the signers, fetching a new issuer's keys and reading each new upstream's `.well-known` (the network, as the bootstrap does); takes no lease, writes nothing; exits `1` when an object is refused |
| `bootstrap -accept` | takes a changed section as the applied one, without applying it (`bootstrap.accept`) |
| `bootstrap -rearm` | forgets that it was applied (`bootstrap.rearm`); the next apply needs `-adopt` for the tenant it made |

`bootstrap` exits `1` when an object exists and differs (nothing written), when a failure persists
past four minutes, and when the section changed since it was applied (naming the fields, until
`-accept`). `-accept` and `-rearm` without a record fail; either with another flag is a usage error.
See [bootstrap](../configuration.md#bootstrap).

## DuckDB versions

DuckDB versions are server-wide. A channel serves a subset of them.

| Command | What it does |
| --- | --- |
| `version add <name> -kind release` or `-kind dev` `[-c-api v1.5.6]... -core-extensions <file or -> [-force]` | registers a version: a release tag (`v2.0.0`) or a 10-character source id (`eb0d9df48e`); one `-c-api` per C API major, the highest version of that major the engine accepts; its core extensions as [`kista duckdb core-extensions`](#kista-duckdb) prints them (required), each reserved as core; a name tenants have releases under needs `-force`, as for `reserved add`; logs the names that were not reserved before |
| `version c-api <name> -c-api <v>` | adds the maximum for a major the version does not have yet; an existing major is never changed |
| `version core-extensions <name> -core-extensions <file or -> [-force]` | reserves more core extensions of a registered version |
| `version list` | `NAME`, `KIND`, `C API` |

Without C API data a version serves no `c_struct` build. Example:

```bash
kista duckdb core-extensions v2.0.0 >core.txt
kista admin -config kista.yaml version add v2.0.0 -kind release -c-api v1.5.6 -c-api v2.0.0 -core-extensions core.txt
```

## Reserved names

The server's reserved extension names, and the names reserved in a tenant (spec 0030; [Reserved
names](../publishing.md#reserved-names)). Server administrators only.

| Command | What it does |
| --- | --- |
| `reserved add <name>... -kind core` or `-kind community` `[-force]` | reserves names, with the source `admin:<actor>`; a name that becomes reserved (or community that becomes core) with releases that are not yanked or purged in some tenant is refused, naming the tenants, unless `-force` (the releases stay, and only grants naming the extension reach the name from then on) |
| `reserved list [-name <name>]` | `NAME`, `KIND`, `ADDED`, `SOURCES` |
| `reserved remove <name>` | removes a name only administrators reserved; a name a version, an upstream or the seed brought stays |
| `tenant-reserved add <tenant> <name> -holder <text>` | reserves a name in a tenant for a holder (or changes its holder); prints the name's live releases there |
| `tenant-reserved remove <tenant> <name>` | releases it; no release changes |
| `tenant-reserved list <tenant>` | `NAME`, `HOLDER`, `ADDED`, `BY` |

## Tenants

| Command | What it does | Output |
| --- | --- | --- |
| `tenant create <name> [-display-name <text>] [-domain <storage domain>] [-holder-id <id>]` | creates a tenant; without `-domain` it lives in `default`; the domain must be configured and never changes; a deleted tenant's name is refused; `-holder-id` sets the holder its licences must name | the tenant id |
| `tenant list [-state <s>,…]` | the tenants in the given states (`active`, `suspended`, `deleting`, `deleted`); by default every state but `deleted` | `NAME`, `STATE`, `DOMAIN`, `DISPLAY NAME`, `CREATED` |
| `tenant show <name>` | a tenant in any state: its record (with its expected holder, when set), and when a suspended one becomes deletable; for a deleting or deleted one, who started the deletion and when, its progress, the last error and the keys to retire | `name:`, `state:`, `holder id:`, `deletable from:`, `deletion by:`, … one per line |
| `tenant holder <name> <id>`, `tenant holder <name> -clear` | sets or clears the holder the tenant's licences must name (their `sub`, 1 to 128 bytes; an empty id is refused, only `-clear` clears it; setting the holder it has, or clearing none, changes nothing); every path that stores a licence then refuses another holder's (`holder_mismatch`); setting it is refused while the tenant holds a licence of another holder, naming those licences ([An expected holder](../licensing.md#an-expected-holder)) | |
| `tenant suspend <name>`, `tenant resume <name>` | a suspended tenant is served nothing and its API answers `404` except to server administrators; both are refused once a deletion started | |
| `tenant delete <name> -confirm <name>` | deletes a tenant suspended for at least `tenants.min_suspended`; the name is given twice; cannot be undone; warns when `gc.interval` is `0` | the keys to retire in their key sources (`KIND`, `ID`, `FINGERPRINT`, `SIGNER`) |
| `tenant reap <name> [-force]` | removes a deleting tenant's rows in this process and writes its tombstone; refused before the settle period (2 hours after the delete) ends, saying how long remains; `-force` skips the wait, only when no `kista serve` and no other command runs | `tenant <name> deleted` |
| `tenant audience add <tenant> <audience>` | assigns an extra token audience; unique across tenants; needs `serve.public_url` | |
| `tenant audience remove <tenant> <audience>` | removes one | |
| `tenant audience list <tenant>` | the assigned audiences, one per line | |

See [Tenants and channels](tenants-and-channels.md) and
[Deleting a tenant](tenants-and-channels.md#deleting-a-tenant).

## Channels

| Command | What it does | Output |
| --- | --- | --- |
| `channel create <tenant>/<channel> -kind signed` or `-kind passthrough` | creates a channel; its kind never changes | |
| `channel versions <tenant>/<channel> [-add <v>]... [-remove <v>]...` | changes the DuckDB versions the channel serves; without flags, lists them | the channel's versions, one per line |
| `channel source <tenant>/<channel> [<name> [-set <source>] [-mode auto\|manual] \| -clear]` | a name's source in the channel ([spec 0031](../upstreams.md#a-names-source-in-a-channel)): `tenant`, `upstream:<name>` or `publisher:<name>`, and its rule's mode ([spec 0033](../upstreams.md#the-catalogue-and-a-channels-rules)); `-mode` alone changes the mode; `-clear` removes it | each name, its source, mode, the version held and the newest in the catalogue, who set it and when, the sources with releases of it, the last outcome |
| `channel follow <tenant>/<channel> [<source>]` | follows a whole source ([spec 0033](../upstreams.md#the-catalogue-and-a-channels-rules)): its names the channel has no rule for are taken automatically; without a source, lists the follows | `SOURCE`, `SET BY`, `SET AT`, `SKIPPED` (each name with its reason) |
| `channel unfollow <tenant>/<channel> <source>` | stops following it; what it placed stays, its rules keep the versions they hold | |
| `channel list <tenant>` | `NAME`, `KIND`, `VERSION`, `CREATED` (`VERSION` is the channel's change counter) | |
| `wellknown <tenant>/<channel>` | the channel's `.well-known/duckdb-extension-repo.json` as served | JSON |

## The catalogue

| Command | What it does | Output |
| --- | --- | --- |
| `catalogue list <tenant> [-source <source>] [-name <name>]` | the tenant's [catalogue](../upstreams.md#the-catalogue-and-a-channels-rules) | `SOURCE`, `NAME`, `VERSIONS` (newest first), `RULES` (channel=mode) |
| `catalogue show <tenant> <source> <name>` | an entry's builds | `VERSION`, `PLATFORM`, `SLOT`, `BODY`, `VISIBILITY`, `ORIGIN`, `CREATED`, `RELEASED IN` |
| `catalogue add <tenant> <file\|-> -name <name> [-private]` | puts a build into the catalogue only, placed nowhere (source `tenant`; the file checks always run) | the build's id |
| `catalogue purge <tenant> <build-id>` | deletes a build that was never released; its slot is free again | |

## Channel keys

| Command | What it does | Output |
| --- | --- | --- |
| `key check -signer <ref>` | opens a key, checks it (RSA-2048, `e = 65537`, the source's rules) and signs a probe; stores nothing | `<ref> <fingerprint> protection=<tier>` |
| `key add <tenant>/<channel> -signer <ref> [-active]` | registers a key as `trusted`; `-active` only for a channel that never had a key | `<key id> <fingerprint>` |
| `key list <tenant>/<channel>` | `ID`, `FINGERPRINT`, `STATE`, `SIGNER`, `PROTECTION`, `TRUSTED SINCE`, `STATE CHANGED` | |
| `key activate <tenant>/<channel> <key id or fingerprint> [-force]` | makes a trusted key active and demotes the current one; refused before `rotation.min_trusted` unless `-force` | a reminder on stderr that existing releases keep the old signatures until a re-sign |
| `key retire <tenant>/<channel> <key id or fingerprint> [-force]` | removes a trusted key from `.well-known` for good; refused before `rotation.min_demoted` unless `-force`; always refused for the active key and the serving key | |
| `key resign <tenant>/<channel>` | signs every non-yanked release with the active key, then moves the serving key to it; runs in the foreground | the count and whether the serving key moved, on stderr |
| `key events <tenant>/<channel>` | the key history: `AT`, `KEY`, `FROM`, `TO`, `ACTOR`, `FORCED` | |

A signer reference is `file:<name>` or `<source>:<key>` (see [Signing keys](../signing-keys.md)).
The protection tier is the source's: `file`, `kubesecret` (a Kubernetes Secret, the weakest),
`software` (Vault Transit) or `kms`; `-` for a reference whose source is no longer configured.
`key check`, `key add` and `key resign` need access to the key source. See
[Key rotation](key-rotation.md).

## Releases

| Command | What it does | Output |
| --- | --- | --- |
| `release add <tenant>/<channel> <file or -> -name <name> [-private] [-not-current] [-unchecked]` | stores and signs a build (`-` reads stdin); adding the same build again returns the existing release | the release id; `the release already exists` on stderr when it did |
| `release list <tenant>/<channel> [-name <name>]` | `ID`, `NAME`, `VERSION`, `PLATFORM`, `SLOT`, `STATE`, `VISIBILITY`, `SEQ`, `CREATED` | |
| `release yank`, `deprecate`, `activate`, `current`, `public`, `private` `<tenant>/<channel> <release id>` | changes a release's state, currency or visibility: `deprecate` an active release, `activate` a deprecated one again, `current` makes an active release the one the flat path serves; `yank` is final | |
| `release purge <tenant>/<channel> <release id> [-force]` | deletes a yanked release for good; its slot stays taken; needs `-force` while `gc.interval` is 0 | |
| `release promote <tenant>/<channel> -name <name> -from <channel> (-release <id> or -version <v>) [-private] [-not-current]` | releases the builds of another channel of the same tenant here, signed with this channel's key | the new release ids |
| `release place <tenant>/<channel> -name <name> -source <source> -version <v> [-private] [-not-current]` | releases a version's builds from the catalogue here ([placing from the catalogue](../publishing.md#placing-from-the-catalogue)); its later builds follow | the new release ids |

`-unchecked` skips the publication checks of a build (such as the init symbol that binds the name).
Details are in [Publishing](../publishing.md).

## Blocks

| Command | What it does |
| --- | --- |
| `block add <tenant> <body hash> -reason <text>` | bans a body tenant-wide; its releases are yanked and it cannot be released again |
| `block list <tenant>` | `BODY HASH`, `REASON`, `CREATED`, `BY` |
| `block remove <tenant> <body hash>` | lifts the ban; yanked releases stay yanked |

## Issuers, audiences and grants

| Command | What it does | Output |
| --- | --- | --- |
| `issuer add <tenant> -name <n> -url <issuer URL> [flags]` | adds an issuer record; discovery and the JWKS are fetched at once and must work | |
| `issuer list <tenant>` | `NAME`, `URL`, `JWKS`, `ALGORITHMS`, `REQUIRED CLAIMS`, `ROLES`, `GROUPS`, `CLIENT`, `MAX LIFETIME` | |
| `issuer remove <tenant> <name>` | removes the record and its grants | |
| `issuer console <tenant> <name> -client-id <id> [-scopes a,b] [-audience <a>] [-audience-parameter <p>]` | sets the administration console's public client at that issuer; scopes default to the console's defaults and must include `openid` | |
| `issuer console <tenant> <name> -remove` | removes it | |
| `grant add <tenant> -principal <principal> -verb <verb> [-verb ...] [-channel <c>] [-extension <x> or -extension-group <g>]` | adds a grant | the grant id |
| `grant list <tenant> [-extension-group <g>] [-extension <x>]` | `ID`, `PRINCIPAL`, `VERBS`, `CHANNEL`, `EXTENSION`; with `-extension`, the grants that reach that extension | |
| `grant remove <tenant> <grant id>` | removes a grant | |

`issuer add` flags:

| Flag | Meaning |
| --- | --- |
| `-name` | record name, `[a-z][a-z0-9-]{0,15}`, required |
| `-url` | the issuer URL, `https`, exact, required |
| `-jwks-uri` | skip discovery and use this JWKS URL |
| `-alg` | an accepted algorithm, repeatable (`RS256`, `PS256`, `ES256`, `EdDSA`, ...) |
| `-require claim=value` | a claim every token must carry, repeatable |
| `-roles-claim`, `-groups-claim`, `-client-claim` | claim paths: keys joined by dots (`realm_access.roles`), or a JSON array of keys when a key has dots (`'["https://example.com/roles"]'`) |
| `-max-lifetime` | the longest `exp - iat` accepted (default 24h, at most 7 days) |

A principal is `subject:<issuer>|<value>`, `role:<issuer>|<value>`, `group:<issuer>|<value>`,
`client:<issuer>|<value>`, `issuer:<issuer>` or `publisher:<name>`. Verbs are `install`, `admin`,
`publish`, `promote`, `audit` and `feed`. Quote principals in the shell: `|` is a pipe. See
[Access](../access.md).

## Extension groups

| Command | What it does |
| --- | --- |
| `extension-group add <tenant> <group> [-description <text>]` | creates a named set of extension names |
| `extension-group list <tenant>` | `NAME`, `MEMBERS`, `GRANTS`, `DESCRIPTION` |
| `extension-group show <tenant> <group>` | the group, its members and the grants on it |
| `extension-group description <tenant> <group> <text>` | replaces the description |
| `extension-group remove <tenant> <group>` | refused while a grant names it |
| `extension-group members <tenant> <group>` | `NAME`, `ADDED`, `BY` |
| `extension-group members <tenant> <group> [-add a,b] [-remove c]` | changes members; removals run first |

## Publishers

| Command | What it does | Output |
| --- | --- | --- |
| `publisher add <tenant> <name>`, `publisher remove <tenant> <name>` | an identity that only publishes and promotes | |
| `publisher list <tenant>` | `PUBLISHER`, `LICENSE KEYS`, `KEY`, `FINGERPRINT`, `TITLE` | |
| `publisher license-keys <tenant> <name> on or off` | marks it a source of licence keys (spec 0021), or unmarks it: a server administrator's act | |
| `publisher key add <tenant> <name> <file.pub or -> [-expires <days>d or <RFC 3339 time>]` | registers a public SSH key (`ssh-ed25519` or `ecdsa-sha2-nistp256`) the publisher signs its requests with (spec 0023); an expiry within a year, or none | the key id |
| `publisher key list <tenant> <name>` | `ID`, `FINGERPRINT`, `TYPE`, `TITLE`, `EXPIRES`, `LAST USED`, `CREATED`, `BY` | |
| `publisher key remove <tenant> <name> <key id>` | removes it: it stops at once | |

See [Publishing](../publishing.md).

## Upstreams and shadows

| Command | What it does |
| --- | --- |
| `upstream add <tenant> <name> -kind <k> -channel <c> -platforms <p,...> [flags]` | adds an upstream; `-kind` is `duckdb-core`, `duckdb-community` or `repository` |
| `upstream list <tenant>` | `NAME`, `KIND`, `MODE` (`mirror (lazy)` for a lazy mirror), `STATE`, `VISIBILITY`, `EXTENSIONS`, `LAST RUN` |
| `upstream show <tenant> <name>` | its settings, entries, requested run, last run and, in bundle mode, the last bundle |
| `upstream remove`, `pause`, `resume`, `public`, `private` `<tenant> <name>` | |
| `upstream sync <tenant> <name> [-dry-run]` | requests a run; a running `kista serve` takes it (a lazy mirror's: its whole matrix; a dry run: what a scheduled run would fetch) |
| `upstream fetch <tenant> <name> eager\|lazy` | a mirror's fetch mode ([Lazy mirrors](../upstreams.md#lazy-mirrors)) |
| `upstream cells <tenant> <name> [-outcome <outcome>]` | `DUCKDB`, `PLATFORM`, `NAME`, `OUTCOME`, `FETCHED`, `DETAIL`; a lazy mirror's `-outcome not_asked` lists the cells offered but not fetched |
| `upstream credential <tenant> <name> <credential>` | sets a private upstream's credential by name; `-` clears it |
| `upstream credentials` | the credentials in the configuration: `NAME`, `KIND`, `TENANTS`, `PREFIXES`, `PUBLIC`, `FORWARDS USAGE` |
| `upstream licenses <tenant> <name> [-sync]` | the upstream's last licence relay; `-sync` requests one, which a running `kista serve` takes ([Licensing](../licensing.md)) |
| `upstream usage <tenant> <name> [forward\|off]` | the upstream's usage switch and its last forwarding run; `forward` or `off` sets it ([Licensing](../licensing.md#forwarding-usage-upstream)) |
| `upstream extension add <tenant> <name> <extension> [-versions <v,...>] [-allow-reserved] [-eager]` | adds or replaces an allowlist entry; `-eager`: a lazy mirror fetches it unasked |
| `upstream extension remove <tenant> <name> <extension>` | |
| `upstream platform add` or `remove` `<tenant> <name> <platform>` | |
| `upstream key add` or `remove` `<tenant> <name> <fingerprint>` | a `repository` upstream's pinned keys |
| `shadow list <tenant>` | core names the tenant replaced: its passthrough channels do not serve them |
| `shadow remove <tenant> <name>` | serves DuckDB's build of that name from passthrough channels again |
| `bundle plan <tenant> [-upstream <name,...>] [-feed <name,...>] [-licenses [-license-holder <sub>]] [-o <plan.json>]` | the plan of the tenant's active bundle-mode upstreams and feeds (all, or the named ones; with only one of `-upstream` and `-feed`, the other kind is left out), with the bundle keys pinned for the tenant (`upstreams.bundle_keys`); to standard output without `-o`. `-licenses` asks for the licences of the names whose files come, of the holder `-license-holder` names (a licence's `sub`) when given |
| `bundle export <tenant> -plan <plan.json> (-key <key.pem> \| -unsigned) -o <out> [-sync] [-private <prefix>]... [-licenses]` | exports a bundle answering a site's plan from this tenant's mirrors and own channels; prints `KIND`, `PREFIX`, `FROM`, `COUNTS`, `BYTES`, `ERROR` per source and `FEED`, `FROM`, `STATUS`, `LAST SUCCESS`, `COUNTS`, `BYTES`, `ERROR` per feed, the listed key fingerprints, the first 20 cells not exported, each licence carried (name, holder, expiry) and the bundle's SHA-256, and exits `1` when a source or a feed failed as a whole. `-licenses` carries the licences the plan asks for; without it none go, and it says so ([Exporting a bundle](../upstreams.md#exporting-a-bundle)) |
| `bundle import <tenant> <file\|-\|blob:<domain>/imports/<name>> [-upstream <name,...>] [-feed <name,...>] [-dry-run] [-any-tenant]` | imports a signed bundle into the tenant's bundle-mode upstreams and feeds (with only one of `-upstream` and `-feed`, the other kind takes no target); prints `TARGET`, `STATUS`, `COUNTS`, `DETAIL` (feeds as `feed:<name>`), the licences' counts and refusals, and exits `1` unless every target was imported, the bundle read whole, no feed body was a gap and the licences could be stored ([Importing a bundle](../upstreams.md#importing-a-bundle)) |
| `bundle list <tenant> [-limit <n>]` | the tenant's imports, newest first (`-limit` 50 by default): `ID`, `STARTED`, `STATE`, `DRY RUN`, `MANIFEST`, `CREATED` |
| `bundle show <tenant> <import id>` | one import: its manifest, exporter, bundle key, input SHA-256, counts per target and the licences it took |
| `bundle reset <tenant> <upstream>` | clears a bundle-mode upstream's last-bundle mark, so it takes older bundles again |
| `bundle reset <tenant> -feed <name>` | clears a bundle-mode feed's last-bundle mark and resets its cursors, so the next bundle takes its history again |

`upstream add` flags: `-extensions <name,...>` or `*`, `-prefix <https://...>` and
`-keys <sha256:...,...>` (for `repository`), `-public`, `-mode mirror`, `-mode pull-through` or
`-mode bundle`, `-fetch eager` or `-fetch lazy` (a mirror's), `-credential <name>`. See [Upstreams](../upstreams.md).

## Attachments, attestation keys and feeds

| Command | What it does | Output |
| --- | --- | --- |
| `attachment add <tenant> <extension> <file or -> -body-hash <h> -kind <k> -media-type <t> -title <t> [-supersedes <id>]` | attaches a document to a body; kinds `sbom`, `vulnerabilities`, `review`, `provenance`, `notes` | the attachment id |
| `attachment list <tenant> <extension> [-body-hash <h>]` | `ID`, `BODY`, `KIND`, `TITLE`, `ORIGIN`, `VISIBILITY`, `HEAD`, `SUMMARY` | |
| `attachment show <tenant> <extension> <id> [-content]` | its record, or with `-content` its bytes | |
| `attachment readers` or `administrators` `<tenant> <extension> <id>` | who may read it | |
| `attachment feedable` or `unfeedable` `<tenant> <extension> <id>` | offered by the tenant's feed, or withdrawn; `feedable` fails while the tenant cannot sign (no active attestation key, or no `serve.public_url`) | |
| `attachment remove <tenant> <extension> <id>` | | |
| `attestation-key add <tenant> <signer ref> [-active]` | registers a key that signs the feed's statements; `-active` only for a tenant that never had one | the key id |
| `attestation-key list <tenant>` | `ID`, `FINGERPRINT`, `STATE`, `TRUSTED SINCE`, `SIGNER` | |
| `attestation-key activate` or `retire` `<tenant> <id or fingerprint> [-force]` | as channel keys; only a trusted key can be retired | |
| `feed add <tenant> <name> -url https://<host>/api/v1/tenants/<t> -credential <c> -key <fp,...> [-kinds <k,...>] [-readers] [-channels <c,...>] [-export]` | subscribes to another repository's feed; `-export` keeps what a bundle needs ([Feeds for air-gapped sites](../attachments.md#feeds-for-air-gapped-sites)) | |
| `feed add <tenant> <name> -mode bundle -url … -key <fp,...> [-kinds …] [-readers] [-channels …]` | a bundle-mode feed: its statements arrive in bundles; no credential, never run | |
| `feed list <tenant>` | `NAME`, `URL`, `MODE`, `STATE`, `STATUS`, `LAST SUCCESS`, `KINDS`, `READERS`, `EXPORT` | |
| `feed show`, `sync`, `pause`, `resume` `<tenant> <name>` | `show` gives the mode, export, when the keys file was kept and the last bundle; `sync` requests a run that a running `kista serve` takes, refused for a bundle-mode feed | |
| `feed set <tenant> <name> [-credential <c>] [-key <fp,...>] [-kinds <k,...> or -] [-readers=true or false] [-export=true or false]` | changes a subscription; `-kinds -` accepts every kind; `-export=true` resets the cursors and reads the history again, `-export=false` deletes what it kept | |
| `feed remove <tenant> <name> [-documents]` | its documents stay unless `-documents` | |

See [Attachments](../attachments.md).

## Licences

| Command | What it does |
| --- | --- |
| `license keys <tenant> [<name>]` | `NAME`, `KEY`, `BUILDS`, `SOURCES`: the licence keys of the tenant's live builds |
| `license list <tenant>` | `NAME`, `PLAN`, `HOLDER`, `STATUS`, `EXPIRES`, `SOURCE`, `VISIBILITY` |
| `license show <tenant> <name>` | a licence's claims, limits and state |
| `license token <tenant> <name>` | the licence as received |
| `license import <tenant> <file or -> [-replace]` | stores a licence under the extension it names; `-replace` replaces the one stored; a refusal names its rule (`kid_untrusted`, `holder_mismatch`, …) |
| `license remove`, `public`, `private` `<tenant> <name>` | |
| `license verify <file or -> -key <pem>` | checks a licence offline against the key given, and prints its claims and status |

See [Licensing](../licensing.md).

## Events

```bash
kista admin -config kista.yaml events list [-server] [-kind <k>] [-subject <prefix>] [-actor <a>] \
  [-since <RFC 3339>] [-limit <n>] [-follow] [-format table|jsonl] [<tenant>]
```

Give exactly one of `-server` (the server's own events) or a tenant; a deleting or deleted tenant's
events are read too. Newest first; `-limit`
defaults to 100. Table columns: `AT`, `KIND`, `OUTCOME`, `ACTOR`, `SUBJECT`, `DATA`. `-format jsonl`
prints one JSON object per event. `-follow` prints oldest first from `-since` (default: one minute
ago), polls every two seconds and runs until interrupted. See [Observability](../observability.md).

## Blob storage

| Command | What it does |
| --- | --- |
| `blob check` | runs the storage startup checks (each domain pinned to its store, its marker, not public) and prints `domain <d> ok`; lists tenants whose domain is not configured and exits `1` if there are any |
| `blob verify [-domain <d>] [-full [-mark] [-unmark]] [-concurrency <n>]` | checks every stored body: present and of its length; `-full` reads every chunk |
| `blob gc [-domain <d>] [-apply] [-force]` | collects storage garbage; a dry run unless `-apply`; `-apply` needs `-force` while `gc.interval` is 0 |

**`blob verify`** prints one tab-separated line per problem (domain, body hash, stream hash,
outcome, detail, and `(marked)`, `(unmarked)` or `(marked before: served 503)`), then a table per
domain: `DOMAIN`, `CHECKED`, `OK`, `SKIPPED`, `MARKED-OK`, `MISSING`, `SHORT`, `LONG`, `CORRUPT`,
`RECORD`, `UNREADABLE`, `MARKED`, `UNMARKED`, `BYTES`. `-concurrency` is 1 to 32 (default 4).
`-mark` and `-unmark` need `-full`. It exits `0` only when every record is `ok` or skipped, so cron
can alert on it. Each pass per domain is recorded as the server event `storage.verify`.

**`blob gc`** prints `DOMAIN`, `BUILDS`, `BODIES`, `MARKED`, `STREAMS`, `TMP`, `UPLOADS`,
`ATTACHMENTS`, `BYTES` per domain, `skipped: another replica is collecting it` when a replica holds
the domain's lease, and `a dry run: nothing was deleted (-apply to collect)` without `-apply`.

See [Storage](../storage.md) and [Backup and restore](backup-restore.md).
