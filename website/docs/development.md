---
title: Development
---

# Development

How to build kista, run its test suites, and work on the console and this site. Contributions
follow the specs process described at the end.

## Prerequisites

| Tool | Needed for |
| --- | --- |
| Go, the version in `go.mod` (1.27) | everything |
| Docker | the PostgreSQL, SQL Server, S3, Azurite, moto and fake-gcs-server test suites, the console end-to-end run |
| `golangci-lint` v2 (CI uses v2.14.0) | `make lint` |
| Node 22 and npm | the console and this site |
| CMake, Ninja, a C/C++ compiler, OpenSSL and libcurl headers; ccache recommended | `make e2e-build`, `make sdk-test` |

Every make target runs with `GOWORK=off`, so a `go.work` in a parent directory cannot change what
is built. Run plain `go` commands the same way (`GOWORK=off go test ./...`) when such a file
exists.

## Building

```bash
make build            # bin/kista
go build ./cmd/kista  # the same, in the current directory
```

The binary has three commands: `kista serve`, `kista admin` and `kista ext inspect|verify`. See
[Commands](administration/commands.md).

The administration console is embedded at build time. Without a console build, kista serves a
placeholder at `/ui/`. To embed the real console, build it first:

```bash
make console   # cd web/console && npm ci && npm run build
make build
```

## Make targets

| Target | What it does |
| --- | --- |
| `build` | `go build -o bin/kista ./cmd/kista` |
| `image` | Build the image for this machine's platform as `kista:dev` and run its smoke test (`deploy/docker/smoke.sh`) |
| `test` | `go test ./...` |
| `lint` | `go vet ./...` and `golangci-lint run ./...` |
| `test-db` / `test-db-down` | Start / stop PostgreSQL 17 and SQL Server 2022 in Docker Compose (`internal/store/testdb/compose.yaml`) |
| `test-s3` / `test-s3-down` | Start / stop SeaweedFS (S3-compatible, pinned by digest) on `127.0.0.1:58333` |
| `test-azurite` / `test-azurite-down` | Start / stop Azurite (the Azure Storage emulator) on `127.0.0.1:50000` |
| `test-moto` / `test-moto-down` | Start / stop moto 5.1.22 (an AWS emulator, pinned by digest) on `127.0.0.1:55000`, for the AWS KMS source |
| `test-gcs` / `test-gcs-down` | Start / stop fake-gcs-server 1.52.2 (a Google Cloud Storage emulator, pinned by digest) on `127.0.0.1:54443`, for the `gcs` store |
| `e2e-duckdb` | Build DuckDB at `e2e/DUCKDB_PIN` with the e2e extensions |
| `e2e-runner` | Build the small C runner linked against that DuckDB |
| `e2e-build` | Both of the above |
| `e2e` | Run the DuckDB end-to-end cases (`go test -count=1 ./e2e/...`) |
| `sdk-test` | Build the C++ SDK (from hugr-lab/extension-ci-tools at `e2e/CI_TOOLS_PIN`) against that DuckDB and check it against the licence vectors |
| `console` | `npm ci` and `npm run build` in `web/console` |
| `console-test` | The console's type check, unit tests and license check |
| `console-e2e` | The console end to end with Keycloak and Playwright |
| `reserved` | Regenerate DuckDB's extension aliases and keys from the pin (the reserved names are the server's data: `kista duckdb core-extensions`) |
| `docs` | Build this site as CI does (`npm ci` and `docusaurus build` in `website`) |

## Tests

`make test` runs every Go test. Without extra services, the store suite runs on SQLite only, the
blob suite on the file system only, and the suites that need a database, an S3 server, Azurite,
fake-gcs-server, moto, Vault or OpenBao skip themselves. Set the variables below to run them.

### The three databases

The store suite (`internal/store/storetest`) runs the same cases on every engine.

```bash
make test-db
KISTA_TEST_POSTGRES='postgres://postgres@127.0.0.1:56432/postgres?sslmode=disable' \
KISTA_TEST_POSTGRES_PASSWORD=kista-test \
KISTA_TEST_SQLSERVER='sqlserver://sa@127.0.0.1:52433?encrypt=disable' \
KISTA_TEST_SQLSERVER_PASSWORD='Kista-Test-2026!' \
KISTA_TEST_REQUIRE_DBS=1 \
make test
make test-db-down
```

`KISTA_TEST_REQUIRE_DBS=1` turns a missing database into a failure instead of a skip; CI sets it.
The SQL Server image is `linux/amd64`, so on Apple silicon it runs under emulation.

### S3 and Azurite

```bash
make test-s3 test-azurite
KISTA_TEST_S3='http://kista-test:kista-test-secret@127.0.0.1:58333/kista-test' \
KISTA_TEST_AZUREBLOB='http://127.0.0.1:50000/devstoreaccount1' \
KISTA_TEST_REQUIRE_BLOB=1 \
make test
make test-s3-down test-azurite-down
```

### fake-gcs-server (Google Cloud Storage)

```bash
make test-gcs
KISTA_TEST_GCS=http://127.0.0.1:54443 KISTA_TEST_REQUIRE_BLOB=1 make test
make test-gcs-down
```

The `gcs` store's contract suite creates its bucket in fake-gcs-server and runs against it.
fake-gcs-server takes any caller, so the tests put a small proxy in front of it that refuses a
request without the test's bearer token, as GCS does for a private bucket, and rewrites resumable
session URIs to point back at the proxy. `KISTA_TEST_REQUIRE_BLOB=1` turns a missing
`KISTA_TEST_GCS` into a failure; CI sets it. What fake-gcs-server does not emulate (partial `308`
answers, the status query, a cancelled upload, escaped names, Google's error shapes) is tested
against an `httptest` fake.

There is no Cloud KMS emulator. The `gcpkms` source, and the metadata server and `external_account`
rules of `internal/cloud/gcp`, are tested against `httptest` fakes of the REST shapes; they need
nothing to run.

### moto (AWS KMS)

```bash
make test-moto
KISTA_TEST_MOTO=http://127.0.0.1:55000 KISTA_TEST_REQUIRE_MOTO=1 make test
make test-moto-down
```

The `awskms` suite creates keys in moto and runs the metadata, tag and public-key checks, and the
sign call, against it. `KISTA_TEST_REQUIRE_MOTO=1` turns a missing `KISTA_TEST_MOTO` into a failure;
CI sets it.

moto ignores `MessageType = DIGEST` and hashes the digest again, so its signatures are over
`SHA-256(digest)` and do not verify. The test expects kista to refuse them. The known answer (a
signature that verifies) is tested against a fake of the KMS operations instead.

### Vault and OpenBao

There is no make target; CI starts both in dev mode:

```bash
docker run -d --name vault -p 127.0.0.1:58200:8200 -e VAULT_DEV_ROOT_TOKEN_ID=root -e SKIP_SETCAP=true \
  hashicorp/vault:2.1.1 server -dev -dev-listen-address=0.0.0.0:8200
docker run -d --name openbao -p 127.0.0.1:58210:8200 -e BAO_DEV_ROOT_TOKEN_ID=root -e SKIP_SETCAP=true \
  openbao/openbao:2.7.1 server -dev -dev-listen-address=0.0.0.0:8200
KISTA_TEST_VAULT=http://127.0.0.1:58200 KISTA_TEST_VAULT_TOKEN=root \
KISTA_TEST_OPENBAO=http://127.0.0.1:58210 KISTA_TEST_OPENBAO_TOKEN=root \
KISTA_TEST_REQUIRE_VAULT=1 \
make test
```

The database login's tests (spec 0027) also need the store suite's PostgreSQL (`make test-db` and
`KISTA_TEST_POSTGRES`). The static-role test lets Vault's database secrets engine manage a role
there: `KISTA_TEST_VAULT_POSTGRES` names that PostgreSQL as Vault and OpenBao reach it from their
containers (`host.docker.internal:56432`; on Linux, start the containers with
`--add-host=host.docker.internal:host-gateway`, as CI does). Without it, that test is skipped.

### What CI runs

| Job | Runs |
| --- | --- |
| `go-linux` | `go vet`, `govulncheck`, `golangci-lint`, and `go test -race ./...` with PostgreSQL, SQL Server, Vault, OpenBao, SeaweedFS, Azurite, moto and fake-gcs-server, every `REQUIRE` switch on |
| `go-macos` | `go vet` and `go test -race ./...` (SQLite and the file system only) |
| `console` | type check, unit tests, licenses, `npm audit`, build |
| `console-e2e` | `make console`, Playwright's Chromium, `make console-e2e` |
| `network` | Real DuckDB core and community files through the intake (`KISTA_TEST_NETWORK=1`); depends on DuckDB's servers, so it does not block |
| `reserved` | `make reserved`, then fails if the committed aliases or keys changed |
| `e2e` (own workflow) | DuckDB at the pin against kista, on pull requests and pushes to `main` that touch `internal/`, `cmd/`, `e2e/`, `go.mod`, `go.sum` or the `Makefile`, or on demand |
| `docs-build` (own workflow) | This site, on changes under `website/` |
| `air-gapped` (own workflow) | `deploy/air-gapped/test.sh`: the [Air-gapped sites](deployment/air-gapped.md) page's shell blocks, the site's in a container on an internal Docker network, against the last release and kista built from the commit; on changes to the page, the test, `internal/`, `cmd/`, `go.mod` or `go.sum`, and weekly |

### A disposable GCP stack

`deploy/gcp/up.sh` builds a real stack for the live tests, and for trying kista by hand, in a
Google Cloud project of its own; `deploy/gcp/live.sh` runs the live tests against it;
`deploy/gcp/down.sh` deletes it.

A dedicated project is the only clean way out: Cloud KMS key rings and keys can never be deleted,
only their versions destroyed. Deleting the project removes the keys, the bucket and its objects,
the service account, the role and the federation together.

```bash
KISTA_GCP_BILLING=XXXXXX-XXXXXX-XXXXXX deploy/gcp/up.sh            # a new project kista-<date>-<n>
KISTA_GCP_BILLING=XXXXXX-XXXXXX-XXXXXX deploy/gcp/up.sh kista-dev  # or a given id
deploy/gcp/live.sh kista-dev                                       # the live tests
deploy/gcp/down.sh kista-dev
```

| Variable | Meaning |
| --- | --- |
| `KISTA_GCP_BILLING` | required: the billing account the project is linked to |
| `KISTA_GCP_REGION` | the location of the key ring and the bucket; default `europe-west4` |
| `KISTA_GCP_FOLDER`, `KISTA_GCP_ORG` | where the project is created: a folder id or an organisation id |
| `KISTA_GCP_HSM` | `0` for software keys, which kista accepts only with `profile: dev` and `require_hsm: false`; default HSM keys |
| `KISTA_GCP_GITHUB_REPO` | `owner/repo`: let that repository's GitHub Actions use the stack through workload identity federation, without keys |

`up.sh` creates the project with the label `purpose=kista-test` and enables Cloud KMS, Cloud
Storage, IAM, IAM Credentials and STS. It makes:

- a key ring `kista` with two keys, `ext-channel` (labelled `kista-purpose=channel-signing`) and
  `ext-attestation` (`attestation-signing`);
- the service account `kista-signer` and a custom role `kistaSigner` with the four Cloud KMS
  permissions, bound on each key; the account running `up.sh` gets
  `roles/iam.serviceAccountTokenCreator` on it, so `live.sh` acts as it without a key;
- a private bucket `<project>-bodies` (uniform bucket-level access, public access prevention), with
  `roles/storage.objectUser` for the service account;
- with `KISTA_GCP_GITHUB_REPO`, a workload identity pool `kista` with a provider `github` that
  admits only that repository (`deploy/gcp/github.sh`, which also adds it to a stack later).

It prints the `signers` and `blob` configuration for the stack, and records the project, region,
service account and bucket in `deploy/gcp/.state-<project>` (git-ignored). The project id must
start with `kista-`.

`live.sh` gets an access token of `kista-signer` by impersonation and runs `go test -tags live`
on `gcpkms` and `gcs`, with extra `go test` flags passed on (`-v`). The tests sign with both keys
and verify as DuckDB does, refuse the attestation key as a channel key, run the blob contract suite
on the bucket, upload 20 MiB in two resumable chunks and read across the boundary, check that the
bucket is private, and check that GCS refuses bytes whose CRC32C differs from the one sent. They
cover the metadata server only through fakes.

The same tests run in GitHub Actions (`.github/workflows/gcp-live.yml`, by hand only:
`gh workflow run gcp-live.yml --ref <branch>` while a stack is up) through kista's own
`external_account` path: the job writes
GitHub's OIDC token to a file and an `external_account` file with that file source, impersonating
the principal, so no key exists. `deploy/gcp/github.sh <project> <owner/repo>` (or `up.sh` with
`KISTA_GCP_GITHUB_REPO`) creates the federation and prints the repository variables the workflow
reads: `KISTA_GCP_PROVIDER`, `KISTA_GCP_PRINCIPAL`, `KISTA_GCP_KEYRING`, `KISTA_GCP_BUCKET`.
Without them the job is skipped. Once the stack is deleted, delete the variables too
(`gh variable delete`).

`down.sh` deletes only a project whose id starts with `kista-` and that carries the label
`purpose=kista-test`. It asks for the project id to be typed again unless given `--yes`. Google
Cloud keeps a deleted project recoverable for 30 days (`gcloud projects undelete <project>`), then
purges it; the key versions are destroyed then.

### Live runs on AWS

`deploy/aws/recipe-live.sh` deploys kista on AWS for real and runs the kind test's checks through a
load balancer: EKS through the chart (`up`, `check`) and the ECS recipe in `deploy/aws-ecs`
(`ecs-up`, `ecs-check`, which needs `up`'s cluster for its server administrator's tokens). It
costs money while it runs: a run needs the owner's yes, and is deleted the same day or when the
cloud work it serves is done.

```bash
export AWS_PROFILE=<an IAM user or role for the run> KISTA_LIVE_ZONE=<a Route 53 zone of the account>
deploy/aws/recipe-live.sh up && deploy/aws/recipe-live.sh check
deploy/aws/recipe-live.sh ecs-up && deploy/aws/recipe-live.sh ecs-check
deploy/aws/recipe-live.sh down && deploy/aws/recipe-live.sh leftovers
```

- Needs `aws`, `eksctl`, `kubectl`, `helm`, `docker`, `jq`, `go`, `terraform`, and the DuckDB e2e
  build (`make e2e-build`).
- `KISTA_LIVE_HOST` and `KISTA_LIVE_ECS_HOST` default to `kista.` and `kista-ecs.` in the zone;
  `KISTA_LIVE_NODE` (`t4g.medium`) picks arm64 nodes, so the image builds natively on Apple silicon.
- Every resource carries `purpose=kista-live` and a `kista-live` name. `leftovers` must print
  nothing marked `LEFT` after `down`; KMS keys pending deletion (7 days) are expected. Delete the
  run's IAM user afterwards.

### Live runs on GCP

`deploy/gcp/recipe-live.sh` applies the GKE recipe in `deploy/gcp-gke` in a Google Cloud project of
its own and runs the same checks through the load balancer and the API; the server administrator
and DuckDB's tokens come from the cluster's own OIDC issuer. It costs money while it runs: a run
needs the owner's yes, and is deleted the same day or when the cloud work it serves is done.

```bash
export KISTA_GCP_BILLING=<billing account> KISTA_LIVE_HOST=<a name you can point at an address>
deploy/gcp/recipe-live.sh up && deploy/gcp/recipe-live.sh check
deploy/gcp/recipe-live.sh destroy        # optional: the recipe's own destroy
deploy/gcp/recipe-live.sh down && deploy/gcp/recipe-live.sh leftovers
```

- Needs `gcloud` with `gke-gcloud-auth-plugin`, `terraform`, `kubectl`, `jq`, `dig`, `go`, and the
  DuckDB e2e build (`make e2e-build`).
- With `KISTA_LIVE_ROUTE53_ZONE` (a Route 53 zone the host is in) the script writes and removes the
  host's A record with the AWS CLI; without it, point the host at the address `up` prints.
- The image is `:edge` pinned by its digest at `up` (`KISTA_LIVE_IMAGE` for another), and the chart
  this checkout's.
- `destroy` runs `terraform destroy` with the project kept and lists what it left; a later `up`
  applies again in the same project (the key ring gets a new name).
- `down` deletes the project (30 days pending deletion, not billed); `leftovers` confirms it.

### Live runs on Azure

`deploy/azure/recipe-live.sh` deploys the Container Apps recipe in `deploy/azure-container-apps` in
a resource group of its own and runs the same checks through Container Apps' ingress and the API.
Administration is Entra ID's: the script registers two apps (the server's audience and tenant
acme's), and the first, with a client secret that expires in two days, is the caller. It costs
money while it runs: a run needs the owner's yes, and is deleted the same day or when the cloud work
it serves is done.

```bash
az login                                   # an Owner of the subscription, allowed to register apps
KISTA_AZURE_LOCATION=<region> deploy/azure/recipe-live.sh up && deploy/azure/recipe-live.sh check
deploy/azure/recipe-live.sh down && deploy/azure/recipe-live.sh leftovers
```

- Needs `az` with Bicep, `jq`, `curl`, `go`, and the DuckDB e2e build (`make e2e-build`).
- `KISTA_AZURE_DATABASE=sqlserver` deploys Azure SQL instead of PostgreSQL.
- `down` deletes the group, purges the vault, deletes the custom role (which outlives the group)
  and the app registrations; `leftovers` must print nothing.

`deploy/azure/managed-app-live.sh` runs the Marketplace package (`deploy/azure-marketplace`) as a
service-catalog managed application: it builds the package, makes a definition (lock `None`, Reader
for a principal other than the runner) and an application from it with the answers the UI gives
(the parameters equivalent to the UI's answers, administrators by Entra app role). Its resources are
tagged `purpose=kista-live` (and `run=kista-ma`). `check` proves:
- the bootstrap made tenant `main`: its channel serves the channel key, and its attestation key was
  added;
- a server and a tenant administrator act by their roles;
- the managed group has no deny assignment;
- the runner can start a job.

```bash
deploy/azure/managed-app-live.sh up && deploy/azure/managed-app-live.sh check
deploy/azure/managed-app-live.sh down && deploy/azure/managed-app-live.sh leftovers
```

It also needs the owner's yes. It needs `az` with Bicep, `jq`, `curl` and `zip`.

`deploy/azure-marketplace/build.sh <image@sha256:…> <out.zip>` builds the package locally; a
byte-identical rebuild of a release's needs bicep 0.47.16. Run Microsoft's checks on it as CI does:
arm-ttk's `Test-AzTemplate` on the unzipped package, under `pwsh`. The form's expressions run only in
the portal: try a changed `createUiDefinition.json` in its sandbox before a release.

## Lint

```bash
make lint
```

It runs `go vet ./...` and `golangci-lint run ./...` with the repository's `.golangci.yml`.

## DuckDB end to end

The e2e suite runs a real DuckDB against kista: `INSTALL ... FROM` a kista repository, signature
checks, private extensions behind a token, the stores and the upstream intake. DuckDB is built from source at the commit in
`e2e/DUCKDB_PIN`:

```bash title="e2e/DUCKDB_PIN"
DUCKDB_COMMIT=d54cbbe3df17e8e5ce68312f483b4155836112aa
DUCKDB_TAG=v2.0.0
HTTPFS_COMMIT=77392a72077a85897db2519fae11e2c0da972b8a
```

```bash
make e2e-build   # DuckDB at the pin, then the runner
make e2e
```

- The first build takes 30-45 minutes; with ccache installed, a rebuild takes minutes.
- The build goes to `e2e/.build` (override with `E2E_BUILD=/path`): `lib/libduckdb`,
  `include/duckdb.h`, the test extensions (`loadable_extension_demo`, `demo_capi`, `httpfs`, and
  `kista_demo`, built with the C++ SDK and its test key; unsigned as built) and the `runner`.
- The C++ SDK is not in this repository: it is in hugr-lab's fork of extension-ci-tools
  ([The developer kit](#the-developer-kit-for-extensions)). `e2e/CI_TOOLS_PIN` names the fork's
  commit, as `e2e/DUCKDB_PIN` names DuckDB's. `e2e/fetch-ci-tools.sh` checks it out into
  `e2e/.build/ci-tools`, and `kista_demo` is built from its `kista/sdk/cpp/test/extension`. A new
  pin rebuilds `kista_demo` with the rest (ccache keeps it short).
- The checkout is verified against `DUCKDB_COMMIT` and must have no local changes.
- `make e2e` sets `KISTA_E2E_DUCKDB` to the build directory; without it the e2e cases skip.
- The build also fills `e2e/.build/sdk` with what the C++ SDK builds against: DuckDB's headers,
  its bundled mbedtls and the library. `make sdk-test` runs
  every licence vector (`internal/license/testdata/vectors.json`) through the C++ verifier, which
  must agree with the Go one on each status, rule and claim. It also checks the key blocks
  (`kista_sdk_license_keys`, spec 0021): a target trusts exactly its own keys, a target without
  them does not link, and malformed key files are refused at configure time.
- The SDK's tests use a copy of the licence vectors, kept in the fork. `e2e/sdk_test.go` checks that
  the pinned fork's copy and test keys are this repository's `internal/license/testdata`. A change
  to the vectors here therefore needs the fork's copy updated (`kista/scripts/kista-vectors.sh`) and
  a new pin.
- `e2e/sdk_test.go` reads `kista_demo`'s key block back, and runs `kista_demo` against kista
  serve: the licence fetched with an http secret and kept beside the module, an upgrade, the
  file offline, `Check()` on an expired or a missing licence, and usage in the statistics.

## The developer kit for extensions

Extension developers build and publish with hugr-lab's fork of DuckDB's extension-ci-tools,
[hugr-lab/extension-ci-tools](https://github.com/hugr-lab/extension-ci-tools) (spec 0022; its
[KISTA.md](https://github.com/hugr-lab/extension-ci-tools/blob/main/KISTA.md)). It holds:

- **the C++ SDK** (`kista/sdk/cpp`): licences and usage reports ([Licensing](licensing.md#the-c-sdk));
- **`scripts/kista_publish.py`**: publishes a distribution build to a kista channel, each request
  signed with the publisher's SSH key ([Signed requests](publishing.md#signed-requests)); it needs
  only Python 3 and OpenSSH, so it runs in any CI. **`_kista_publish.yml`** runs it from GitHub
  Actions, the key in the `KISTA_PUBLISHER_KEY` secret;
- **`makefiles/kista.Makefile`**: the licence keys at build time.

There are two kinds of extension:

- **An open extension** (also in DuckDB's community repository) builds with DuckDB's own
  extension-ci-tools, exactly as the community builds it, and uses only the fork's publication job.
  The community's build replaces an extension's `extension-ci-tools` with DuckDB's, so an open
  extension's build must not depend on the fork.
- **A licensed extension** has the fork as its `extension-ci-tools` submodule and links the SDK. Its
  build is `make release KISTA_LICENSE_KEYS='<pem>;…'`, which is how Enterest's builders build it:
  - `KISTA_LICENSE_KEYS` names one to four PEM public keys;
  - without it, the build takes the committed `license-key.pem`.
  - In CI it calls the fork's `_extension_distribution.yml` with
    `override_ci_tools_repository: hugr-lab/extension-ci-tools`. That workflow checks out DuckDB's
    ci-tools by default, which has no SDK.

### Starting an extension: the template

[hugr-lab/extension-template](https://github.com/hugr-lab/extension-template) is DuckDB's C++
extension template with the kit, built against the DuckDB commit kista is tested against
(`e2e/DUCKDB_PIN`, `v2.0.0`). Make a repository from it, clone it with its submodules, and
run once:

```bash
./bootstrap.sh <name>              # an open extension
./bootstrap.sh <name> --licensed   # a licensed one
```

- **What bootstrap does.** It renames the example extension, puts the kind's files in place,
  formats the sources as DuckDB's format check wants them, and stages the result. For a licensed
  extension it also points `extension-ci-tools` at the fork.
- **Both kinds.** `.github/workflows/distribution.yml` builds every platform of DuckDB's matrix. On a
  `v*` tag it publishes to the kista named by the repository's `KISTA_URL`, `KISTA_TENANT` and
  `KISTA_CHANNEL` variables, signed with the publisher key in the `KISTA_PUBLISHER_KEY` secret.
- **A licensed one** shows the SDK in use: `Register`, `Check`, `Limit` and `<name>_license()`.
  - Its `license-key.pem` starts as the SDK's test key.
  - A local build warns about it, and a tag build fails until the key is the issuer's public key.

The extension's README covers the rest: making the publisher key and registering it, marking the
publisher for licence keys, and updating DuckDB. The template's own CI publishes a copy of each kind to a kista built from
a pinned commit, then installs and loads both with DuckDB.

## The console

The console is a React and TypeScript app in `web/console` (Vite, Vitest, Playwright).

```bash
cd web/console
npm ci
npm run dev         # Vite dev server
npm run typecheck
npm test            # Vitest
npm run licenses    # every shipped package's license
npm run build       # the app and the micro-frontend build, embedded by the next make build
```

The end-to-end run starts Keycloak in Docker (two realms with passwords made for the run), runs
`kista serve` on https with the console built and a tenant set up through the CLI, and drives it
with Playwright in Chromium:

```bash
make console
(cd web/console && npx playwright install chromium)   # once
make console-e2e
```

It needs Docker, Go, Node, Python 3 and OpenSSL. Screenshots of failures go to
`web/console/test-results/`.

## This site

The documentation is a Docusaurus site in `website/`; the pages are `website/docs/*.md`.

```bash
cd website && npm ci && npm start   # live-reloading dev server
make docs                            # the production build, as CI runs it
```

The build fails on a broken link or anchor. Pull requests that touch `website/` are built by the
`docs-build` workflow; `main` is deployed to GitHub Pages.

A shell block on the Air-gapped sites page marked `ci=connected` or `ci=site` (in the fence's
meta, after the title) is run by the `air-gapped` workflow, in order; `ci=connected-release` and
`ci=site-release` only once a release exists. Run it locally with
`TOOLS=<linux cosign, oras and gh> deploy/air-gapped/test.sh`.

Write pages in plain, short sentences. In prose, escape `<` and `{` (or put them in backticks), or
the MDX build fails. Link between pages with relative `.md` paths.

## Images and releases

The `Dockerfile` at the root builds the console (with its licence check and `npm audit`), then
kista with the console embedded, into a distroless image ([Deployment](deployment/index.md)).
`make image` builds it for this machine and runs `deploy/docker/smoke.sh`: `kista version`, then
`kista serve` as uid `65532` on a read-only root filesystem until `/readyz` answers.

Two workflows publish:

- **`image.yml`.** On a pull request it builds `linux/amd64` and `linux/arm64` and runs the smoke
  test on amd64; it pushes nothing. On `main` it also pushes `:sha-<commit>` and `:edge`, then
  signs the image and attests its provenance.
- **`release.yml`.** On a tag `vX.Y.Z` (or `vX.Y.Z-rc.N`, published as a pre-release) it builds
  the image and the binaries without a build cache, pushes `:vX.Y.Z` unless that tag already
  exists, signs and attests them, and creates the GitHub release with the binaries,
  `SHA256SUMS` and the signature bundles.

In both, the build job has read access to the repository only. The jobs that push, sign or
release take the build's output as workflow artifacts and run no repository or npm code.

To release, tag a commit on `main` and push the tag: `git tag -s v1.2.0 && git push origin
v1.2.0`. The workflow refuses a tag that is not on `main`. If signing or the release fails after
the push, re-run the failed jobs (not all jobs: a rebuild is a different build): the publish job
accepts a version tag that already holds the same build. A broken build is fixed with a new version, never by moving the tag.

The packages `ghcr.io/hugr-lab/kista` and `ghcr.io/hugr-lab/charts/kista` are public and linked to
this repository (the image index's `org.opencontainers.image.source` annotation links them, and
gives the repository's Actions access). Two tag rulesets guard `v*`: only maintainers and admins
may create a version tag, and nobody may update or delete one. Cut a release candidate first
(`vX.Y.Z-rc.N`) and verify its image, its chart and a binary as
[Deployment](deployment/index.md#verifying) shows; after a release, run the `air-gapped` workflow
by hand (it tests the last release, and otherwise waits for its weekly run).

## Specs and the working process

Every feature starts as a lightweight spec, `specs/NNNN-slug/spec.md`, from `specs/TEMPLATE.md`:
the problem, the design, security, tests and the alternatives considered. The list is in
[specs/README.md](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/README.md);
start with the
[architecture spec](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0001-architecture/spec.md).

- The spec is written and reviewed before the code. Its status becomes `implemented` when the work
  is done; a changed decision gets a new spec.
- One branch per spec. A large spec may be delivered in phases (`NNNN-slug-phase`), each its own
  pull request with its own reviews; the spec says which phases are implemented.
- Every feature lands with tests. Three adversarial review passes come before a pull request.
- Review your own diff before every commit, and again after opening the pull request (fixes go in a
  follow-up commit).
- Everything in the repository and on GitHub is in English: code, comments, specs, commit messages,
  issues, pull requests and reviews.
- The docs say how to run and use kista; the specs say why it is built that way.
