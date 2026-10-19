# Spec 0022: The developer kit

- **Status**: implemented (phase 1: the fork and kista's switch; phase 2: the template)
- **Date**: 2026-10-19
- **Author**: vgsml, Claude

## Summary

An extension developer gets two repositories from us:

- **`hugr-lab/extension-ci-tools`**, a fork of `duckdb/extension-ci-tools`. It holds the kista C++
  SDK, moved out of this repository. It adds a reusable workflow that publishes a build's
  artifacts to a kista.
- **`hugr-lab/extension-template`**, a template repository with a `bootstrap.sh`.
  - An **open** extension (MIT, Apache) builds exactly as DuckDB's community extensions build, and
    is published both to DuckDB's community repository and to a kista.
  - A **licensed** extension builds with the fork and the SDK, carrying its licence keys.

One build contract serves the developer's CI, Enterest's builders and a local build:
`make release`, with `KISTA_LICENSE_KEYS` naming the licence keys of a licensed extension.

## Problem

- **No standard way to publish to a kista.** None of our extensions publishes to a kista from CI.
  - hugr-node seeds its test kistas with `kista admin release add` on the server.
  - duckdb-acl and mssql-extension publish to GitHub releases.
  - kista's trusted publishing (spec 0008) is documented with `curl` only.
- **The SDK lives in the server's repository.** A licensed extension links `sdk/cpp` from kista: a
  developer vendors a server's repository to build a client library. Its version is a kista commit.
- **Enterest has no build interface.** Enterest's spec 0003 builds a developer's extension from git
  and sets its licence keys, but nothing says which target it runs or where the keys go.
- **Each extension wires the same things by hand**: the ci-tools submodule, the distribution
  workflow, vcpkg, the SDK, the keys.

## Design

### Two kinds of extension

**Open.** MIT, Apache or similar, usually also in DuckDB's community repository.
- **Submodule**: `duckdb/extension-ci-tools`, unchanged.
- **Build**: DuckDB's own distribution workflow, the community build exactly.
- **SDK**: none.
- **Publication**: our `_kista_publish.yml`, after the build.

**Licensed.** Sold through Enterest, or licensed by its developer.
- **Submodule**: `hugr-lab/extension-ci-tools`, the fork.
- **Build**: the fork's distribution workflow (the same as DuckDB's), with the SDK.
- **SDK**: `kista/sdk/cpp` from the fork; its keys come from `KISTA_LICENSE_KEYS`.
- **Publication**: the same `_kista_publish.yml`; with Enterest, Enterest publishes.

**Why open extensions do not build with the fork.** DuckDB's community build checks out the
extension's repository with its submodules, then puts `duckdb/extension-ci-tools` over its
`extension-ci-tools` directory (`override_ci_tools_repository`, in `_extension_distribution.yml`).
- So an extension built by the community never sees the fork.
- An open extension therefore depends on nothing in the fork for its build.
- kista receives the same build the community builds.

A licensed extension is never built by the community, so it may depend on the fork.

### The fork: `hugr-lab/extension-ci-tools`

**Branches** follow DuckDB's: `main` and a branch per DuckDB line when DuckDB cuts one.
- Each is DuckDB's branch, with our commits on it, merged from DuckDB's when we take a change.
- Our changes are kept apart from DuckDB's files, so that a merge conflicts only where DuckDB
  changes something we changed. We change none of DuckDB's files unless a hook needs it, and such
  a change is listed in the fork's README.

**What the fork adds:**

```text
kista/sdk/cpp/                       the C++ SDK, moved from kista's sdk/cpp with its history
kista/sdk/cpp/VERSION                the SDK's version (semver); tagged sdk-v<version>
kista/sdk/cpp/test/vectors/          a copy of kista's licence vectors and test key
kista/DUCKDB_PIN                     the DuckDB commit the SDK is tested against (kista's own)
kista/scripts/sdk-test.sh            DuckDB at the pin with kista_demo, then the SDK's tests
kista/scripts/kista-vectors.sh       copies or checks the vectors against a kista checkout
makefiles/kista.Makefile             KISTA_LICENSE_KEYS into EXT_FLAGS
scripts/kista_publish.py             the publication client (Python, standard library only)
.github/workflows/_kista_publish.yml the publication job
.github/workflows/kista.yml          the fork's own CI
KISTA.md                             what the fork adds, and what of DuckDB's it changes
```

**The SDK.**
- It moves from kista's `sdk/cpp` with its history (`git subtree split`). Its CMake interface is
  unchanged: `add_subdirectory(<path> kista_sdk)`, `target_link_libraries(... kista_sdk)`,
  `kista_sdk_license_keys(<target> <pem>...)` and `kista_sdk_testkey`.
- A licensed extension adds `extension-ci-tools/kista/sdk/cpp`.
- **Its version** is `kista/sdk/cpp/VERSION`, also compiled in, and tagged `sdk-v<version>`.
  Enterest's provenance records it with the fork's commit.

**The licence keys at build time.** `makefiles/kista.Makefile`, included by a licensed extension's
`Makefile` after DuckDB's:
- turns `KISTA_LICENSE_KEYS` (one to four PEM paths, separated by `;`) into
  `-DKISTA_LICENSE_KEYS=...` in `EXT_FLAGS`, always, so a build directory configured with keys before
  does not keep them; a value with a quote is refused;
- the extension's CMake passes it to `kista_sdk_license_keys`, and takes its committed
  `license-key.pem` when it is empty.

**The vectors.**
- kista's `internal/license/testdata` stays the source of truth: the Go verifier and its generator
  are there.
- The fork keeps a copy, with the kista commit it came from in
  `kista/sdk/cpp/test/vectors/FROM`, and `kista/scripts/kista-vectors.sh <kista checkout>` updates
  it. The fork's CI checks the copy against that commit, which must be on kista's main.
- kista's e2e checks that the pinned fork's copy and test keys equal its own (Testing). So a
  change to the licence format fails kista's CI until the fork has the same vectors.

### Publishing: `_kista_publish.yml`

A reusable workflow that a developer's workflow calls after the distribution build:

```yaml
jobs:
  build:
    uses: duckdb/extension-ci-tools/.github/workflows/_extension_distribution.yml@<commit>   # an open extension
    with: {extension_name: myext, ci_tools_version: <the same commit>}
    # a licensed one: hugr-lab/extension-ci-tools/.github/workflows/_extension_distribution.yml@<commit>,
    # also with override_ci_tools_repository: hugr-lab/extension-ci-tools (the default is DuckDB's)
  kista:
    needs: build
    if: startsWith(github.ref, 'refs/tags/v')
    permissions: {id-token: write, contents: read}
    uses: hugr-lab/extension-ci-tools/.github/workflows/_kista_publish.yml@<commit>
    with:
      extension_name: myext
      ci_tools_version: <the same commit>        # the publication script the job runs
      kista_url: ${{ vars.KISTA_URL }}           # the kista's public URL
      tenant: ${{ vars.KISTA_TENANT }}
      channel: ${{ vars.KISTA_CHANNEL }}
    secrets:
      KISTA_API_KEY: ${{ secrets.KISTA_API_KEY }}  # optional: without it, GitHub's OIDC token
```

**Inputs.**
- `extension_name`, `kista_url`, `tenant` and `channel`.
- `ci_tools_version` (required): the fork's commit to take the script from, the same as the
  workflow's own `@<commit>`. A reusable workflow cannot learn its own ref, and a default of `main`
  would run unpinned code beside the caller's token.
- `visibility`: `private` by default, as kista's own default.
- `artifact_postfix`, as the distribution workflow's.
- `environment`: the GitHub environment the job runs in, for a publisher binding that names one.
  A job that calls a reusable workflow cannot set an environment itself.

The job grants itself `id-token: write`; a called workflow's permissions are at most the caller's,
so the caller grants it too.

**What it does.** It downloads the build's artifacts
(`<ext>-<duckdb_version>-extension-<arch><postfix>`), then runs `scripts/kista_publish.py` with the
inputs passed through the environment, never through the shell. It takes only
`<ext>.duckdb_extension` files of artifact directories ending `-extension-<arch><postfix>`. For
each, the script:

1. **Reads the footer.** It takes the extension version and platform from DuckDB's metadata footer,
   as `kista ext inspect` reads them. A file without a valid footer fails the job.
2. **Skips wasm** (a `.duckdb_extension.wasm` file). kista refuses wasm builds (spec 0008), so the
   script skips them and says so in the log and the job summary. The template builds wasm (below);
   when kista accepts wasm, the skip goes.
3. **Uploads.** It posts to kista's publication route:
   ```text
   POST <kista_url>/api/v1/tenants/<tenant>/channels/<channel>/extensions/<name>/releases?version=…&platform=…
   ```
   - The request carries `Content-Length`, `application/octet-stream` and `visibility`.
   - The extension's own build is uploaded as it is: kista signs it with the channel's key.
4. **Authenticates.** With `KISTA_API_KEY` it uses the key. Otherwise it requests a fresh GitHub
   OIDC token for each file (they live minutes), with audience `<kista_url>/<tenant>` (spec 0008).
   - The URL must be https (http only to a loopback address), and redirects are refused, so a token
     never goes to another host.
   - The token's `workflow_ref` names the caller's workflow, not this reusable one, so the
     publisher's GitHub binding names the developer's workflow file, as today.
5. **Reads the answer.**
   - `201` is published; `200` means already published, with the same body.
   - `429`, `5xx` and a broken connection are retried three times, with backoff or `Retry-After`
     (at most a minute).
   - Anything else fails the job, with kista's error. For example, `400` when the channel does not
     serve the build's DuckDB version, or `409` for another body in a published slot.
   - The job summary lists each platform's outcome.

**The script runs without the job too.** A developer, or another CI, runs it on built files:

```text
python3 scripts/kista_publish.py --url … --tenant … --channel … [--api-key-env KISTA_API_KEY] <file>...
```

Without the job, it takes only an API key: OIDC exists only inside GitHub Actions.

### The template: `hugr-lab/extension-template`

A GitHub template repository, from DuckDB's C++ `extension-template` with its history, with:
- `bootstrap.sh`;
- the two kinds' files under `kinds/open` and `kinds/licensed`, put over the root by bootstrap. The
  root stays DuckDB's template, so merges from it stay simple;
- a README for the template and one for each kind, and the specs layout our extensions use
  (`specs/`).

**DuckDB 2.0** (the owner's decision). DuckDB's template builds the last 1.x release and has no 2.0
branch. Ours builds `v2.0-cyanoptera`, because the SDK is tested there and trusted repositories
(`CREATE EXTENSION REPOSITORY`) are a 2.0 feature.
- The `duckdb` submodule is kista's pin (`e2e/DUCKDB_PIN`), and the distribution workflows build the
  submodule's commit.
- The open kind's ci-tools are DuckDB's `v2.0-cyanoptera`, pinned by commit.
- DuckDB's example builds and passes on it unchanged.

**Bootstrap:** `./bootstrap.sh <name> [--licensed]`, run once in a fresh copy. It:
- checks the name (`[a-z][a-z0-9_]{0,63}`: one of kista's names, and a C++ identifier);
- renames the sources, tests, CMake targets and workflows;
- puts the kind's files and `README.md` in place;
- formats the sources as DuckDB's format check wants them (`make format-fix`, with the tool versions
  of `_extension_code_quality.yml`, in a temporary venv when they are not installed). The renamed
  lines change length, and DuckDB 2.0's check also rewraps DuckDB's own example;
- removes the other kind's files, the template's own and itself, and stages the result.

What each kind gets:
- **Open:**
  - the `duckdb/extension-ci-tools` submodule;
  - `.github/workflows/distribution.yml`: the build, and on a `v*` tag the publication to kista when
    `KISTA_URL` is set.
- **Licensed:**
  - the fork as the submodule, at the commit the workflow pins;
  - the SDK in `CMakeLists.txt` and `include kista.Makefile` in the `Makefile`;
    - `kista::License::Register` in the load function (with its `<name>_license()` table function);
    - a function that needs a licence (`Check`, `Limit`);
    - `kista_sdk` installed with the extension into DuckDB's export set, which needs it;
  - `license-key.pem`: the SDK's test key. The SDK takes a bare PEM block, so the comment that says
    so is in `CMakeLists.txt` and the README;
  - the same workflow, building with the fork. It passes `override_ci_tools_repository`, since the
    distribution workflow otherwise checks out DuckDB's ci-tools, which has no SDK.

**The test key does not leave.** The licensed workflow fails a tag build whose `license-key.pem`
is one of the SDK's test keys (`scripts/check-license-key.sh`), and says to put the issuer's public
key there.
- It compares the keys as DER, so a re-wrapped or CRLF copy is caught too.
- A local build that trusts `license-key.pem` warns instead, and so does the SDK at load.

**All platforms are built**, wasm included, as DuckDB's matrix has them. The publication skips
what kista refuses.

**Configuration** is in GitHub variables and secrets, never in the repository:
- `KISTA_URL`, `KISTA_TENANT` and `KISTA_CHANNEL`;
- `KISTA_API_KEY` only without OIDC.

The README says how to bind the workflow to a publisher (`kista admin publisher github add`), and
how a server administrator marks the publisher for licence keys (spec 0021).

### The build contract (developer CI, Enterest, local)

A licensed extension built from the template builds anywhere with:

```text
git clone --recurse-submodules <repo> && cd <repo>
make release KISTA_LICENSE_KEYS='/path/key1.pem;/path/key2.pem'    # optional: license-key.pem otherwise
make test
```

- The output is `build/release/extension/<name>/<name>.duckdb_extension`, as DuckDB's Makefile
  puts it.
- **Enterest's builders** (Enterest spec 0003) run exactly this, with `KISTA_LICENSE_KEYS` set to
  the public halves of the extension's registered keys. They then read the keys back from the binary
  (spec 0021's key block) and refuse a mismatch.
  - The extension must take its keys only from `KISTA_LICENSE_KEYS` or `license-key.pem`, as the
    template does.
  - Enterest's spec 0003 refers to this section.
- An open extension's contract is DuckDB's: `make release`, `make test`.

### Changes in kista

**The SDK leaves.**
- `sdk/cpp` is removed. Its history stays in kista's and goes to the fork.
- `e2e/CI_TOOLS_PIN` names the fork's repository and commit, as `e2e/DUCKDB_PIN` names DuckDB's.
- `e2e/build-duckdb.sh` fetches the fork at the pin and builds `kista_demo` from
  `kista/sdk/cpp/test/extension`.
- `make sdk-test` builds and runs the SDK's tests from that checkout.
- The e2e workflow's cache key takes the pin instead of `sdk/cpp`'s files.

**The cross-checks move to e2e.** `internal/license/license_test.go` reads the SDK's test keys and
`keys.cpp` from `../../sdk/cpp`. These checks move to an e2e test that runs on the pinned fork's
checkout:
- its vectors and test keys equal `internal/license/testdata`;
- `keys.cpp` names the test key's fingerprint.

**Docs.**
- `development.md` is rewritten around the kit: the two kinds, the template, the publication job
  and the build contract.
- `publishing.md` gains "With the developer kit", beside the `curl` example.
- `licensing.md` points to the SDK's new home.
- The SDK's README moves with it.

Nothing changes in kista's server, API or schema.

## Security

- **No signing key in CI.** The developer's workflow uploads an unsigned build. kista checks it and
  signs it with the channel's key (spec 0008). The fork drops nothing of DuckDB's signing, because
  it never signs.
- **OIDC by default.** The job asks for `id-token: write` only in its own job, and keeps no secret.
  - The token is bound to the caller's repository and workflow.
  - kista refuses tokens from pull-request events, so a fork's pull request cannot publish.
  - The template publishes only on `v*` tags. A publisher binding with `-ref 'refs/tags/v*'`
    enforces that on kista's side.
- **The API key** is a fallback for CI without OIDC. It is a secret, never a variable, and the
  script never prints it.
- **What the kit trusts.**
  - The fork pins nothing of the developer's.
  - The developer pins the fork, by submodule commit and by the reusable workflow's ref.
  - Pinning the workflow by commit, as for any third-party action, is recommended in the README.
- **The test key.** A licensed extension made from the template cannot reach a tag build with the
  SDK's test key (phase 2). The SDK itself lets a release build assert it does not trust that key
  (`kista::TrustsTestKey()`, spec 0021). kista does not refuse it at publication, by the owner's
  decision: licences are Enterest's to issue, and Enterest checks the keys of what it builds.
- **Licence keys.** Only public keys are in the repository or the build. Enterest sets them for its
  builds and checks them in the binary. kista records them only from trusted sources (spec 0021),
  so a key in a developer's own build counts only if the developer's publisher is marked.

## Testing

**The fork's CI (`kista.yml`):**
- the SDK's tests, as kista's `make sdk-test` runs them today, against DuckDB at the fork's pin;
- `kista_publish.py`'s unit tests against a local fake server: the footer, wasm skipped, `200` and
  `201`, retries on `429` and `503`, failure on `400` and `409`, the API key and the OIDC token
  request, the summary;
- the vectors' copy matching its `FROM` commit of kista.

**The template's CI:**
- bootstrap both kinds;
- build each on `linux_amd64`, with `make test` and DuckDB's format check;
- start a kista built from a pinned commit, with a tenant, a channel and a publisher API key;
- publish both builds with `kista_publish.py`;
- with DuckDB at the pin, `INSTALL` and `LOAD` each from that kista. The licensed one reports its
  licence status as `missing`, and the SDK warns that it trusts the test key.
- `scripts/test-bootstrap.sh` checks the bootstrap without a build: the names refused, what each
  kind leaves, and the key check (a tag build with any of the SDK's test keys fails, also re-wrapped
  or CRLF; an issuer's key passes).
- Before the merge, each kind was bootstrapped on a scratch branch and its distribution workflow ran
  DuckDB's whole matrix: Linux, macOS, Windows (MSVC and mingw) and wasm all built.

**kista's e2e:**
- `kista_demo` built from the pinned fork, as today's SDK tests: licence, relay and bundle;
- the vectors and test-key cross-check against the fork's checkout, and the fork's DuckDB pin
  against kista's;
- the pin must be a commit of the fork's main (`e2e/fetch-ci-tools.sh`).

**By hand, once, before the first release:** the OIDC path from a real tag in a scratch repository
made from the template, to a kista with a GitHub binding. GitHub's token cannot be made in a test.

## Alternatives considered

- **Keep the SDK in kista.** A client library inside a server's repository, versioned by server
  commits. Developers vendor the server to build an extension. The owner chose the fork, so the kit
  is in one place.
- **The SDK in the template.** Every extension would carry a copy that drifts. In the fork, it is
  one submodule bump.
- **Build open extensions with the fork too.** The community build replaces `extension-ci-tools`
  with DuckDB's, so an open extension that depended on the fork would not build there. kista would
  also receive a different build than the community.
- **A Go publish client (kista's binary).** It would need kista released for every runner and
  platform. A standard-library Python script runs wherever ci-tools already runs Python.
- **Publishing to kista from DuckDB's `_extension_deploy.yml`** (its `deploy_script` input). That
  workflow expects DuckDB's S3 secrets and signs with DuckDB's key: a separate job is simpler and
  needs neither.

## Delivery

**Phase 1: the fork and kista's switch.** Creating the GitHub repository needs the owner's yes.
- `hugr-lab/extension-ci-tools`:
  - forked, with the SDK moved with its history;
  - `kista.Makefile`, `kista_publish.py` and `_kista_publish.yml`;
  - its CI and `KISTA.md`.
- kista:
  - `sdk/cpp` removed;
  - `CI_TOOLS_PIN`, `build-duckdb.sh` and `make sdk-test` taken from the fork;
  - the cross-checks in e2e;
  - `development.md`, `publishing.md` and `licensing.md`.

**Phase 2: the template.**
- `hugr-lab/extension-template`: `bootstrap.sh`, the two kinds, the CI above and the READMEs.
- kista's `development.md` updated for the template.

## Follow-ups

- **SQL tests in the distribution build.** On DuckDB 2.0, DuckDB's distribution workflow skips an
  extension's own SQL tests (`require <name>` skips them: the test run finds no extension), for both
  kinds and every platform. `make test` runs them, and the template's CI does. To raise with DuckDB.

- **wasm in kista.** Accepting and serving wasm builds is a kista spec of its own. Then the
  publication stops skipping them.
- **Our extensions on the kit.** tresor, acl-otel, duckdb-acl, hugr-node and others move one by one.
  The open ones publish both to the community repository and to our kista.
- **C API extensions** (DuckDB's `extension-template-c`), when needed. Enterest's v1 builds C++
  only.
- **Enterest's spec 0003** refers to "The build contract" above.
