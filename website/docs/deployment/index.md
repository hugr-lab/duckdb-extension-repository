---
title: Deployment
---

# Deployment

kista ships as a container image and as binaries for linux and darwin, published from this
repository and signed. Until the first version tag, the only published image is `:edge`, built
from `main`; the version images and the binaries come with the first release. This page says what is published, how to verify it, and how to run the
image with Docker. [Kubernetes](kubernetes.md) covers the Helm chart, [AWS](aws.md) the ECS
recipe in Terraform.

The design is in
[spec 0013](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0013-deployment/spec.md).

## What is published

| Artifact | Where | Built from |
| --- | --- | --- |
| The image, `linux/amd64` and `linux/arm64` | `ghcr.io/hugr-lab/kista:vX.Y.Z` | a tag `vX.Y.Z` (`release.yml`) |
| The image, the newest `main` | `ghcr.io/hugr-lab/kista:edge` and `:sha-<commit>` (12 hex digits) | every push to `main` (`image.yml`) |
| The Helm chart, version `X.Y.Z` (its default image is `:vX.Y.Z`) | `oci://ghcr.io/hugr-lab/charts/kista` | a tag `vX.Y.Z` (`release.yml`) |
| The binaries: `kista-vX.Y.Z-{linux,darwin}-{amd64,arm64}`, `SHA256SUMS`, a `.sigstore.json` bundle for each | the GitHub release `vX.Y.Z` | a tag `vX.Y.Z` (`release.yml`) |

- There is no `:latest`. A deployment names a version, and is best pinned by digest:
  `ghcr.io/hugr-lab/kista@sha256:…`. The release notes give each version's digest.
- `:edge` moves with `main` and is for trying changes, not for production.
- A version tag is never overwritten: the release workflow refuses to push a version that is
  already in the registry.
- Every image and binary embeds the administration console and reports its version:
  `kista version` prints, for example, `kista v1.2.0 (go1.27.2, linux/amd64)`. Edge builds report
  `edge-<commit>`.

## The image

- A static binary at `/kista` in `gcr.io/distroless/static-debian12:nonroot`: no shell, no package
  manager, the CA certificates of the base image.
- It runs as uid and gid `65532` (`nonroot`).
- `ENTRYPOINT ["/kista"]`, `CMD ["serve", "-config", "/etc/kista/kista.yaml"]`. Any other command
  replaces the `CMD`: `docker run … ghcr.io/hugr-lab/kista:vX.Y.Z admin -config … migrate`.
- `/var/lib/kista` is the state directory, owned by `65532`. `TMPDIR` is `/var/lib/kista/tmp`, so
  SQLite's temporary files land there and the root filesystem can be read-only. `kista serve` and
  `kista admin` create that directory when a volume mounted at `/var/lib/kista` hides it.
- The image exposes no port: the listeners are in the configuration.

## Verifying

Every image, chart and binary is signed with cosign, keylessly: the signature's certificate names the
GitHub workflow that built it, and the signature is logged in Rekor, the public transparency log.
Verify the identity, not only that a signature exists. The signatures are Sigstore bundles,
stored in the registry as OCI referrers: the commands below need cosign 3 or later, and an
admission controller must support that format.

A release image (here `v1.2.0`):

```bash
cosign verify ghcr.io/hugr-lab/kista@sha256:<digest> \
  --certificate-identity https://github.com/hugr-lab/duckdb-extension-repository/.github/workflows/release.yml@refs/tags/v1.2.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

A release chart, the same way: `cosign verify ghcr.io/hugr-lab/charts/kista@sha256:<digest>` with
the same identity (the release notes give the chart's digest).

An edge image is signed by `image.yml` on `main` (resolve `:edge` to its digest first, with
`crane digest` or `docker buildx imagetools inspect`, and deploy that digest):

```bash
cosign verify ghcr.io/hugr-lab/kista@sha256:<digest> \
  --certificate-identity https://github.com/hugr-lab/duckdb-extension-repository/.github/workflows/image.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

A binary, with its bundle and the checksums from the same release:

```bash
sha256sum -c SHA256SUMS --ignore-missing
cosign verify-blob kista-v1.2.0-linux-amd64 \
  --bundle kista-v1.2.0-linux-amd64.sigstore.json \
  --certificate-identity https://github.com/hugr-lab/duckdb-extension-repository/.github/workflows/release.yml@refs/tags/v1.2.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

- **Provenance.** Each image and each release's binaries carry a build provenance attestation
  from GitHub's `actions/attest-build-provenance`, which names the workflow, the commit and the
  run. Verify it with the workflow named, since `--repo` alone accepts any of the repository's
  workflows:
  `gh attestation verify oci://ghcr.io/hugr-lab/kista@sha256:<digest> --repo hugr-lab/duckdb-extension-repository --signer-workflow hugr-lab/duckdb-extension-repository/.github/workflows/release.yml`
  (`image.yml` for an edge image), or the same with a binary's path. kista claims no SLSA level
  beyond what that action states.
- **SBOM.** Each image carries an SPDX SBOM of its packages, the console's npm packages among
  them: `docker buildx imagetools inspect ghcr.io/hugr-lab/kista:v1.2.0 --format '{{ json .SBOM }}'`.
  The SBOM is part of the signed image index, so the image's signature covers it.
- **Admission.** In a cluster, an admission policy can refuse unsigned images: the sigstore
  policy-controller or Kyverno's `verifyImages`, with the identity above.

The darwin binaries are not notarised. A binary downloaded with a browser carries the quarantine
attribute, and macOS refuses to run it; after verifying it, clear the attribute:
`xattr -d com.apple.quarantine kista-v1.2.0-darwin-arm64`.

## Docker

The image reads its configuration from `/etc/kista/kista.yaml`. A single container with SQLite:

```yaml title="kista.yaml"
store:
  kind: sqlite
  sqlite:
    path: /var/lib/kista/kista.db
  migrate: auto
serve:
  public_url: https://kista.example.com
  listeners:
    - addr: 0.0.0.0:8443
      scheme: https
      tls:
        cert_file: /etc/kista/tls/tls.crt
        key_file: /etc/kista/tls/tls.key
    - addr: 0.0.0.0:8081
      scheme: health
# signers, blob, auth: see Configuration
```

```bash
docker volume create kista-state
docker run -d --name kista --read-only \
  --cap-drop ALL --security-opt no-new-privileges \
  -v "$PWD/kista.yaml:/etc/kista/kista.yaml:ro" \
  -v "$PWD/tls:/etc/kista/tls:ro" \
  -v kista-state:/var/lib/kista \
  -p 443:8443 -p 127.0.0.1:8081:8081 \
  ghcr.io/hugr-lab/kista@sha256:<digest>
```

- **Listeners.** This example serves https only. DuckDB's built-in HTTP client and a first
  bootstrap may need a plain `http` listener or a `dual` one: see
  [Using kista from DuckDB](../duckdb.md).
- **State.** A new named volume takes the image's `/var/lib/kista`, owned by `65532`. A bind
  mount or a tmpfs must be owned by `65532` (`chown 65532:65532 state`); kista creates `TMPDIR`
  (`/var/lib/kista/tmp`) in it at start when it is missing. Without any volume, the state lives in the
  container and is lost with it. Back it up as [Backup and restore](../administration/backup-restore.md)
  says.
- **Files.** Every file kista reads, the certificate too, must be owned by root or by `65532`,
  and writable by nobody else ([Files kista reads](../security.md#files-kista-reads)). A file in
  a bind mount keeps its owner and mode on the host. A secret, such as the key, must also be
  unreadable by others: owned by `65532` with mode `0400` (`chown 65532 tls.key && chmod 0400
  tls.key`), or owned by root, group `65532`, mode `0440`. The directory must be readable by
  `65532`.
- **Docker secrets.** Swarm secrets mount as root with mode `0444` by default, which kista
  refuses: give the secret `uid: "65532"` and `mode: 0400`. Compose's file secrets are bind
  mounts, where `uid` and `mode` are ignored: set the host file's owner and mode instead.
- **Probes.** The `health` listener answers `/healthz` and `/readyz` only. Publish it on loopback,
  or not at all; Docker's `HEALTHCHECK` needs a shell or a client in the image, which this image
  does not have. Use Kubernetes probes or the load balancer's HTTP check.
- **Administration.** `kista admin` runs in the same container, with the same files:
  `docker exec kista /kista admin -config /etc/kista/kista.yaml tenant list`. For work before the
  server first starts, run a one-off container with the same mounts and an `admin …` command.
- **Shutdown.** `docker stop` sends SIGTERM and waits 10 seconds by default. kista's shutdown can
  take longer, 86 seconds at most with the default timeouts
  ([Commands](../administration/commands.md#kista-serve)): use `docker stop -t 100` or
  `stop_grace_period: 100s` in Compose, and more if you raise `serve.drain_timeout` or
  `serve.shutdown_timeout`.
