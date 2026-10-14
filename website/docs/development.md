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
| CMake, Ninja, a C/C++ compiler, OpenSSL and libcurl headers; ccache recommended | `make e2e-build` |

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
| `console` | `npm ci` and `npm run build` in `web/console` |
| `console-test` | The console's type check, unit tests and license check |
| `console-e2e` | The console end to end with Keycloak and Playwright |
| `reserved` | Regenerate the reserved extension names, aliases and DuckDB's keys from the pins |
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

### What CI runs

| Job | Runs |
| --- | --- |
| `go-linux` | `go vet`, `govulncheck`, `golangci-lint`, and `go test -race ./...` with PostgreSQL, SQL Server, Vault, OpenBao, SeaweedFS, Azurite, moto and fake-gcs-server, every `REQUIRE` switch on |
| `go-macos` | `go vet` and `go test -race ./...` (SQLite and the file system only) |
| `console` | type check, unit tests, licenses, `npm audit`, build |
| `console-e2e` | `make console`, Playwright's Chromium, `make console-e2e` |
| `network` | Real DuckDB core and community files through the intake (`KISTA_TEST_NETWORK=1`); depends on DuckDB's servers, so it does not block |
| `reserved` | `make reserved`, then fails if the committed lists changed |
| `e2e` (own workflow) | DuckDB at the pin against kista, on pull requests and pushes to `main` that touch `internal/`, `cmd/`, `e2e/`, `go.mod`, `go.sum` or the `Makefile`, or on demand |
| `docs-build` (own workflow) | This site, on changes under `website/` |

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

The same tests run in GitHub Actions (`.github/workflows/gcp-live.yml`, on demand and on pull
requests that touch the GCP code) through kista's own `external_account` path: the job writes
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
DUCKDB_COMMIT=eb0d9df48ee5341ea4ede1a3cba93251cce571df
DUCKDB_TAG=v2.0-cyanoptera
HTTPFS_COMMIT=5e34903685e4d429cbb19b063406abdd8ce30591
```

```bash
make e2e-build   # DuckDB at the pin, then the runner
make e2e
```

- The first build takes 30-45 minutes; with ccache installed, a rebuild takes minutes.
- The build goes to `e2e/.build` (override with `E2E_BUILD=/path`): `lib/libduckdb`,
  `include/duckdb.h`, the test extensions (`loadable_extension_demo`, `demo_capi`, `httpfs`,
  unsigned as built) and the `runner`.
- The checkout is verified against `DUCKDB_COMMIT` and must have no local changes.
- `make e2e` sets `KISTA_E2E_DUCKDB` to the build directory; without it the e2e cases skip.

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

Write pages in plain, short sentences. In prose, escape `<` and `{` (or put them in backticks), or
the MDX build fails. Link between pages with relative `.md` paths.

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
