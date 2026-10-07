# Spec 0002: Extension file format, the file signer, and the DuckDB e2e harness

- **Status**: draft
- **Date**: 2026-10-07
- **Author**: vgsml, Claude

## Summary

The first code in kista:

1. `internal/extfile`: read, hash, verify and re-sign a DuckDB extension file, and assemble the gzip
   form kista serves from a precompressed body plus a per-channel signature.
2. `internal/signer`: the `Signer` interface and its first implementation, a local key file. Azure
   Key Vault comes with key sets and rotation in spec 0003.
3. An end-to-end harness that builds DuckDB at kista's own pin, runs it against signed repositories,
   and confirms the rows of spec 0001's "DuckDB behaviour relied on" table that so far come from
   reading the source.

It also adds `kista ext inspect` and `kista ext verify` for development.

**Done** means:

- every case in the row-to-case table below passes, or is cut there with a reason;
- spec 0001's table is updated with the results, including the corrections this spec already makes
  to it.

## Problem

Spec 0001 rests on facts about DuckDB's extension format and loader that come from reading its
source: the footer layout, the composite hash, the signature form, the transport paths and secret
scopes. Nothing can be built on them until they are code with tests, and the DuckDB facts are proven
against a real DuckDB binary.

## Design

### Repository and toolchain

- **Go module.** `github.com/hugr-lab/duckdb-extension-repository`; renaming the repository later
  only changes the path. Go 1.26, and CI reads the version from `go.mod`.
- **Binary.** One, `cmd/kista`. Its CLI uses stdlib `flag` subcommands.
- **Makefile targets:** `test`, `lint` (`go vet` + `staticcheck`), `e2e-build`, `e2e`. Every target
  runs with `GOWORK=off`, so a parent `go.work` cannot interfere.
- **Dependencies.** kista depends on DuckDB only. No other hugr-lab repository is needed to build or
  test it.

### `internal/extfile`

**File layout**, as DuckDB reads it (`FOOTER_SIZE = 512`, `src/include/duckdb/main/extension.hpp:45`):

```text
[ code ... ][ 19-byte prefix ][ metadata: 8 x 32 bytes ][ signature: 256 bytes ]
 └──────────────────────── body (hashed and signed) ──┘
```

- **Size.** A file shorter than 512 bytes is not an extension.
- **The prefix** (`00 93 04 10 "duckdb_signature" 80 04`) is a wasm custom-section header that
  `scripts/append_metadata.cmake` writes into every file, native ones included. DuckDB does not read
  it. `inspect` reports whether it is present but does not require it. Spec 0006's publishing
  appends it.

**Metadata.** Eight 32-byte fields, zero-padded, stored in reverse order
(`src/main/extension/extension_load.cpp:346-385`). After reversing:

| # | Field |
| --- | --- |
| 0 | magic: exactly `4` |
| 1 | platform |
| 2 | DuckDB version (`CPP`, `C_STRUCT_UNSTABLE`) or C API version (`C_STRUCT`) |
| 3 | extension version |
| 4 | ABI type: `CPP` (also when empty), `C_STRUCT`, `C_STRUCT_UNSTABLE` |
| 5-7 | unused |

`ParseMetadata` is stricter than DuckDB on purpose, because these strings later become URL path
segments:

- each used field must match `[A-Za-z0-9._-]{1,32}` before its zero padding, and every byte after
  the first zero byte must also be zero;
- an unknown ABI is an error;
- unused fields are ignored.

```go
type Metadata struct {
    Platform, DuckDBVersion, CAPIVersion, ExtensionVersion string
    ABI ABIType // CPP, CStruct, CStructUnstable
}
func ParseMetadata(block [256]byte) (Metadata, error)
```

**Composite hash.** The body is split into chunks of 1 MiB; the last chunk is shorter and never
empty (`extension_load.cpp:287-297`). The hash is the SHA-256 of the concatenated SHA-256 of each
chunk. It is computed streaming, through an `io.SectionReader`, with a caller-given maximum size
that is checked before hashing.

```go
type BodyHash [32]byte
func HashBody(r io.Reader, maxSize int64) (BodyHash, int64, error)
```

**Signatures and keys.**

- **Verify.** A signature is RSA PKCS#1 v1.5 with the SHA-256 DigestInfo over the 32-byte composite
  hash, which is what `mbedtls_pk_verify(MD_SHA256, …)` checks
  (`third_party/mbedtls/mbedtls_wrapper.cpp:73-97`). In Go that is
  `rsa.VerifyPKCS1v15(pub, crypto.SHA256, hash[:], sig)`.
- **Key type.** Only RSA keys of exactly 2048 bits, with `e = 65537` and the `rsaEncryption` OID,
  are accepted. The check happens when a key is loaded.
- **Public key forms.** The two that DuckDB accepts (`extension_repository_manager.cpp:48-83`):
  - SPKI PEM, exactly one block, no trailing data;
  - strict base64 of SPKI DER.

  Any other `-----BEGIN` header is refused, as DuckDB refuses it, PKCS#1 `RSA PUBLIC KEY` included.
  Keys are deduplicated by fingerprint.
- **Fingerprint.** `"sha256:" + lowercase hex(SHA-256(SPKI DER))`, the string
  `CREATE EXTENSION REPOSITORY` reports (`extension_repository_manager.cpp:116-125`).

```go
func Verify(hash BodyHash, sig []byte, keys []*rsa.PublicKey) (fingerprint string, ok bool)
func Fingerprint(pub *rsa.PublicKey) string
func ParsePublicKey(s string) (*rsa.PublicKey, error) // PEM or base64 DER
```

**File helpers.**

- `Open(r io.ReaderAt, size, maxSize int64) (*File, error)` parses the metadata, hashes the body
  and keeps the signature.
- `WriteSigned(dst io.Writer, f *File, sig []byte)` writes the body followed by the given signature.

**Gzip assembly.** DuckDB's decompressor is a raw inflate up to the end of the deflate stream
(`UncompressGZIPString`, `src/common/gzip_file_system.cpp:477-533`). It:

- refuses the `FTEXT`, `FHCRC`, `FEXTRA`, `FCOMMENT` and encrypt header flags;
- never checks the CRC-32 or the size;
- ignores anything after the stream.

So:

- **`Precompress(body io.Reader, dst io.Writer) (Precompressed, error)`** writes a raw deflate
  stream of the body and ends it with a sync flush (`flate.Writer.Flush`). That leaves the stream
  byte-aligned and with no final block. Before returning, it **re-inflates** its own output and
  checks two things:
  - the output decompresses to the body, with the same body hash;
  - no block has `BFINAL` set.

  It returns `{BodyHash, BodyCRC32, BodyLen, StreamLen, StreamSHA256}`.
- **`WriteGzip(dst, pre Precompressed, stream io.Reader, sig []byte)`** writes, in order:
  1. a gzip header with `FLG = 0`, `MTIME = 0` and `OS = 255`;
  2. exactly `StreamLen` bytes of the stream. It fails if there are fewer, or if more remain;
  3. a final stored block: `01 00 01 FF FE` followed by the 256 signature bytes;
  4. the trailer: `crc32.Update(BodyCRC32, IEEE, sig)` and `(BodyLen + 256) mod 2^32`.
- **`VerifyStream(pre, stream)`** checks a stored stream against `StreamSHA256`. The serving spec
  (0004) decides whether to call it on every read or on a schedule.

  A stored stream is trusted only through this hash. A stream with a final block of its own would
  make DuckDB stop early and ignore the appended signature, so a tampered stream must never be
  served.
- The `.gz` is served as a file, without `Content-Encoding`. DuckDB does not check the trailer, so
  unit tests do: Go's `gzip.Reader` with `Multistream(false)` must reach EOF with a valid CRC, and
  `gzip -t` must pass.

### `internal/signer`

```go
type Signer interface {
    // Sign signs a body hash for a DuckDB extension. Nothing else can be passed in.
    Sign(ctx context.Context, h extfile.BodyHash) ([]byte, error)
    Public() *rsa.PublicKey
    ID() string // a stable reference for config and audit
}
```

- `Sign` takes the typed `BodyHash`, not arbitrary bytes. Licence signing (spec 0010) gets its own
  type with a domain-separated input, so neither can be used to sign the other's data.
- Every implementation checks its own result: 256 bytes long, and `rsa.VerifyPKCS1v15` against
  `Public()` passes.
- **`file`** is for development and tests. It requires explicit opt-in (`allow_file_signer: true`)
  outside a dev profile. It:
  - opens the path and `fstat`s the opened file;
  - refuses anything that is not a regular file, is not owned by the current user, or has a mode
    broader than `0600`. On Windows the mode check is skipped; this is documented;
  - accepts PKCS#8 or PKCS#1 PEM with exactly one block, and refuses encrypted PEM with a clear
    error;
  - calls `Validate()` and checks for 2048 bits and `e = 65537`;
  - never logs key material.
- The same key fingerprint in two roles (two channels of different trust, or channel and licence)
  is refused when keys are configured. That check lives with key sets in spec 0003, and is
  recorded here as a requirement.

### `kista ext` CLI

```text
kista ext inspect <file>                    prefix present, metadata, body hash, size, signature fingerprint (with --key)
kista ext verify  <file> --key <pub>...     exit 0 if any key verifies; prints its fingerprint
```

The CLI does not sign. Signing with a production key is a release action (spec 0003 / 0006), and the
harness calls the packages directly.

### The e2e harness (`e2e/`)

**Pin.** `e2e/DUCKDB_PIN` holds the full 40-character DuckDB commit, the tag for information, and
the duckdb-httpfs commit that the pinned tree's `.github/config/extensions/httpfs.cmake` fetches.

**Build.** `e2e/build-duckdb.sh <out>`:

- runs `git fetch --depth 1 origin <sha>` from `duckdb/duckdb`, then asserts
  `git rev-parse HEAD` equals the pin. The `.github` tree is kept, because httpfs's patches live
  there;
- builds release with `STATICALLY_LINK_EXTENSIONS=core_functions`. The default would also link
  json, parquet and icu statically;
- builds named targets only:
  - the shared `libduckdb`;
  - `loadable_extension_demo` (CPP), with `BUILD_UNITTESTS=ON` but without building the unittest
    binary;
  - `demo_capi` (C_STRUCT);
  - httpfs (loadable). httpfs needs OpenSSL and libcurl development headers: `apt libssl-dev
    libcurl4-openssl-dev`; on macOS brew `openssl@3`.

Tests read the platform and the version directory name from the runner (`PRAGMA platform`,
`pragma_version()`), never from constants.

**Runner.** `e2e/runner/runner.c` is written against DuckDB's **C API**, so there are no C++ ABI
concerns. It is built against the same build tree, with an rpath to its `libduckdb`.

- It sets startup options with `duckdb_set_config`:
  - `allow_extension_repositories='allowed'`;
  - `extension_directory` (a temporary directory);
  - `autoinstall_known_extensions=false` and `autoload_known_extensions=false`, unless a case turns
    them on. This keeps the harness hermetic: nothing reaches `extensions.duckdb.org`.
- It reads statements from stdin and writes one JSON line per statement:
  `{"stmt","ok","error","columns","rows"}`.
- **One runner process per case.** Once httpfs is loaded, DuckDB rewrites `http://` downloads to
  `https://` (`extension_install_dynamic.cpp:313-315`).

**Test servers.** The cases are written against a small interface: a base URL plus a request log
(method, path, `Authorization` present, `If-None-Match`, `Range`). Spec 0004 can swap in
`kista serve` and rerun the same suite.

- The implementation for 0002 is a loopback-only file server over a signed tree, in plain HTTP
  and TLS variants. It can require a Bearer token, serves ETags, and answers `HEAD` and `Range`
  through `http.ServeContent`.
- The TLS variant uses a test CA that is generated per run and never added to a system trust
  store.
- Test keys are generated per run. No private key is committed.

**Transport paths.** DuckDB takes three, depending on the prefix and on whether httpfs is loaded:

| Path | When | Behaviour |
| --- | --- | --- |
| local | the prefix is a directory | file reads only |
| built-in | an `http://` prefix and httpfs never loaded | one `GET` of `.gz`, no plain-name fallback, no `HEAD`, never `Authorization`; `If-None-Match` only from `UPDATE EXTENSIONS` |
| httpfs | an `https://` prefix, or `http://` after httpfs is loaded (bumped to https) | file-system access: an exists check on `.gz`, then the plain name, then reads (possibly ranged); Bearer from http secrets; no ETag |

Two consequences:

- `.well-known` is read through DuckDB's file system, so it needs **httpfs**. Without httpfs,
  `CREATE … WITH PREFIX 'http://…'` must use `USING PUBLIC KEY`.
- The bootstrap for every remote client is therefore:
  1. `CREATE … USING PUBLIC KEY` on `http://`;
  2. `INSTALL httpfs FROM r`;
  3. `LOAD httpfs FROM r`;
  4. only then `https://` and `.well-known`.

  httpfs itself is served by the repository.

**Cases.** Tier A uses local directories and needs neither HTTP nor httpfs. Tier B uses the built-in
client. Tier C uses httpfs.

| # | Tier | Case | Expects | 0001 row |
| --- | --- | --- | --- | --- |
| 1 | A | signed tree, `USING PUBLIC KEY` (PEM, then base64 DER), `INSTALL x FROM r`, `LOAD x FROM r` | works for `loadable_extension_demo` (CPP) and `demo_capi` (C_STRUCT); the reported fingerprint equals `extfile.Fingerprint` | keys; signature |
| 2 | A | the same, signed with another key | `INSTALL` refused: "doesn't have a valid signature" | signature |
| 3 | A | `USING PUBLIC KEY` with a PKCS#1 PEM or a 3072-bit key | refused at `CREATE` | keys |
| 4 | A | `INSTALL x FROM r VERSION 'v'` | read from `<r>/<name>/<v>/<version dir>/<platform>/…`; `v` is used verbatim | paths |
| 5 | A | rotation: install with key A, `CREATE OR REPLACE` with B only, then `LOAD x FROM r` | load fails; `FORCE INSTALL` of a B-signed file fixes it | keys checked on every LOAD |
| 6 | A | rotation with keys A and B | A- and B-signed installs both load with `LOAD x FROM r` | keys |
| 7 | A | install `x` from r1 and from r2 | both coexist under `repositories/<r>/`; `LOAD x FROM r2` refuses an install that came from r1 | user-repository installs |
| 8 | A | `SET custom_extension_repository='<our tree>'; INSTALL x` | refused: core-typed, so our key is not trusted | core typing |
| 9 | B | bootstrap: `CREATE … 'http://…' USING PUBLIC KEY`, `INSTALL httpfs FROM r` | exactly one `GET <prefix>/<version dir>/<platform>/httpfs.duckdb_extension.gz`, no `HEAD`, no fallback | http path |
| 10 | B | `.gz` served as `WriteGzip` output, not as plain gzip | installs: DuckDB inflates the assembled stream and the signature verifies | serving |
| 11 | B | an http secret with a matching `SCOPE` | no `Authorization` header on the built-in path | http path |
| 12 | B | `UPDATE EXTENSIONS (x)` | `If-None-Match` with the served ETag; `304` keeps the install. A repeated plain `INSTALL` makes no request | `.info`, ETag |
| 13 | B | `CREATE … 'http://…'` without a key | fails: `.well-known` needs httpfs | `.well-known` |
| 14 | C | after `LOAD httpfs FROM r`: `CREATE` on an `https://` prefix without a key | keys read from `.well-known`; a trailing `/` on the prefix is trimmed; a 64 KiB + 1 file is refused | `.well-known` |
| 15 | C | `https://` install: the request log | exists check on `.gz`, then the plain name, then reads; `Range` honoured | https path |
| 16 | C | http secret, `SCOPE '<prefix>/'` | `Authorization: Bearer` on downloads under the prefix and none under `…/prod2/` | secret scope |
| 17 | C | `SCOPE '<prefix>'` without the slash | the token is also sent to `…/prod2/`; this documents why the slash is required | secret scope |
| 18 | C | private CA: `SET ca_cert_file`, `CREATE` without a key, then with `USING PUBLIC KEY` | without a key `CREATE` fails even with `ca_cert_file`; with a key it works and the install uses `ca_cert_file` | `.well-known` without context |
| 19 | C | autoload: `autoinstall_known_extensions=true`, `custom_extension_repository` = our tree, query an `https://` file in a fresh process without httpfs | httpfs is not loaded afterwards: the autoinstall is refused (errors are swallowed, so assert the state, not a message) | autoload is core-only |

Rows of spec 0001's table that this spec does not test:

- **community keys with `allow_community_extensions`**: there are no community-signed binaries for
  a dev pin. It stays a source-only row until the pin is a release.
- **a passthrough channel with DuckDB's real core signature**: DuckDB publishes no extensions for
  `eb0d9df`. `extensions.duckdb.org` answers `404` and `nightly-extensions.duckdb.org` answers `403`.
  Case 8 proves the typing from the negative side.

### Corrections to spec 0001's table, made by this spec

- `If-None-Match` is sent only on the built-in path, and only by `UPDATE EXTENSIONS`.
- There are three transport paths, not two. `.well-known` needs httpfs.
- An http secret is never used on the built-in path.

### CI

- **`go` job, always.** `go vet`, `staticcheck`, `go test ./...` (fuzz seeds included). Linux and
  macOS.
- **`e2e` job.** On PRs touching `internal/extfile`, `internal/signer`, `e2e/` or the pin.
  - Linux only, `linux_amd64`; macOS on `workflow_dispatch`.
  - The build output (libduckdb, the three extensions, the runner) is cached under a key of both
    pinned commits plus the compiler version. A cold build is estimated at 30-45 minutes and runs
    only when the pin changes.
- Actions are pinned by commit SHA.
- No cloud credentials are needed in 0002.

### Unit tests

- **Golden composite hashes.** Committed fixtures of 0 bytes, 1 MiB - 1, 1 MiB, 1 MiB + 1 and
  3 MiB + 7, with hashes computed once by `duckdb/scripts/compute-extension-hash.sh` on the body
  only. The script hashes its whole input, and `split` limits it to 676 chunks.
- **Metadata.** The committed 512-byte footers of the pin's real test extensions (CPP,
  C_STRUCT_UNSTABLE, C_STRUCT), plus malformed blocks: wrong magic, bytes after the padding, `../`,
  an unknown ABI.
- **Keys.** PEM / DER round trips; refusal of PKCS#1, 3072-bit keys, `e ≠ 65537` and trailing data;
  fingerprints against the e2e value.
- **Gzip.**
  - `WriteGzip` output decodes to body plus signature with a valid trailer.
  - A stream with a premature final block is refused by `Precompress`, and by `VerifyStream` once
    tampered.
  - A short or long stream is refused by `WriteGzip`.
- **Signer.** Sign-then-verify, and the file checks: mode, owner, symlink to a key, encrypted PEM.
  Keys are generated in the test and `chmod`ed.
- **Fuzz.** `ParseMetadata`, `Open`, `HashBody`, `ParsePublicKey`.

## Security

- **Verification recomputes everything.** The body hash comes from the bytes. Footer strings are
  validated before any later code can use them in a path.
- **Bounded input.** Every input has a maximum size, checked before hashing. Parsers return
  errors, never panic. The fuzz tests cover them.
- **The signer signs only body hashes**, and checks every signature it produces. Key-role
  separation is a recorded requirement for spec 0003.
- **The `file` signer** cannot be used in production without explicit opt-in. It refuses keys that
  are shared, symlinked or encrypted.
- **Serving a stored stream** relies on `StreamSHA256` and the exact length. A stream that does not
  match is never served.
- **The e2e harness** is hermetic and loopback-only, and its keys and CA are ephemeral. Its DuckDB
  checkout is verified against the pinned commit hash.

## Alternatives considered

- **Shell out to openssl, as the first experiment did.** Go's `crypto/rsa` performs the same
  operation.
- **A Go DuckDB driver.** None builds against an arbitrary dev pin, and startup options need the
  config API. A small C runner is simpler.
- **The DuckDB CLI.** Startup-only options cannot be set from it.
- **A C++ runner.** It brings C++ ABI coupling to the build. The C API avoids that.
- **A duckdb submodule.** A heavy checkout for a Go service. A pin file and a verified fetch keep
  the repository light.
- **Key Vault in this spec.** It adds the Azure SDK and cloud credentials without proving anything
  about DuckDB that the file signer does not. It moves to spec 0003, with key sets and rotation.
- **A generator for DuckDB's built-in keys here.** Its only consumer is intake (spec 0007).

## Follow-ups

- **Spec 0003:** the Azure Key Vault signer. Its requirements from this spec's review:
  - versioned key ids only;
  - the public key comes from the JWK and is checked: 2048 bits, `e = 65537`, enabled, in its
    validity window, with `sign` in `key_ops`;
  - keys whose ops include `wrapKey` / `unwrapKey` / `encrypt` / `decrypt` are refused;
  - every signature is checked after signing: the returned `kid` is the pinned one, and the
    signature verifies;
  - explicit managed or workload identity, not the developer credential chain;
  - a timeout per call; a disabled key fails the release closed;
  - one dedicated key per role, never shared with tresor;
  - a fake vault in CI and a live test only in a protected environment with federated credentials.
- **Spec 0007:** DuckDB's built-in core and community keys. They are generated from the pinned
  checkout; CI regenerates them and fails on any diff.
- **Spec 0006:** the init-symbol check, and appending the metadata prefix when publishing.
- **A spec for core extensions built for the pin:** build DuckDB's core extensions at kista's pin
  and publish them into a signed channel. Then every extension installs with `INSTALL … FROM`,
  whoever built it: DuckDB, community, us or a publisher. They are not autoloaded, because DuckDB
  autoloads core only under its own keys. kista never relies on a patched DuckDB, and we do not ship
  a DuckDB build of our own.
- **When the pin is a release:** the passthrough and community-key cases.
