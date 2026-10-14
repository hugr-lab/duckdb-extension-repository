# Spec 0013: Deployment: image, Helm chart, Azure

- **Status**: phase 1 implemented (1a server changes, 1b image and releases, 1c chart); phase 2 (Azure) moved to spec 0020
- **Date**: 2026-10-14
- **Author**: vgsml, Claude

## Summary

kista ships as one container image and one Helm chart, both published from this repository. Later
it also ships as an Azure deployment: Bicep for Container Apps first, then an Azure Marketplace
managed application.

**Phase 1** is:
- the image and the release workflows;
- the chart;
- the server changes a container platform needs: a probe-only listener, one set of rules for the
  files kista reads secrets and tokens from (including the shapes Kubernetes mounts), a bounded
  background shutdown, and a `version` command.

**Phase 2** is Azure: Bicep, the bootstrap a managed install needs, and the Marketplace package.

The packaging follows tresor-server's: the same registry, tags, base image and chart conventions,
so the platform's services deploy alike. kista adds signed images, charts and binaries with an
SBOM and provenance, because kista is itself a supply-chain control.

## Problem

Today kista is built with `make build` and run by hand. The docs say there is no image and no
chart (`website/docs/index.md`, `administration/index.md`, `administration/upgrading.md`). An
organisation that writes its own manifests runs into these problems:

- **Probes.** The kubelet's probes reach a `behind_proxy` listener from outside
  `trusted_proxies`, and kista aborts them (`internal/serve/listen.go`). The pod never becomes
  ready.
- **Service links.** A Service named `kista` makes the kubelet inject `KISTA_SERVICE_HOST`,
  `KISTA_PORT_443_TCP` and similar variables into every pod in the namespace. kista refuses any
  `KISTA_*` variable that names no setting (`internal/config/config.go`), so it does not start.
- **Secret files.** The rules for the files kista reads secrets from are uneven, and none of them
  takes a Kubernetes Secret volume (root-owned, group `fsGroup`, mode `0440`, behind `..data`
  symlinks). Today:
  - `store.login.password_file`, TLS `key_file`, OTLP `headers_file` and the upstream credential
    files are not checked at all;
  - the Vault token file, the S3 key files and the Azure Blob key file need mode `0600` or
    narrower, and follow links anywhere;
  - the file signer and the AWS static keys refuse any link; the file signer also wants its own
    uid;
  - the GCP `credentials_file` wants root or kista as owner, and nobody else may write it;
  - platform tokens (AWS, GCP, the Azure federated token) take any mode.
- **Storage.** The default blob domain is `fs` at `/var/lib/kista/blobs`, which fails on a
  read-only root filesystem. With two replicas on per-pod volumes it would silently split the
  store.
- **Platform needs.** hugr-node, the platform's node agent, installs only from kista, so the
  platform needs a published image to deploy kista next to tresor-server.
- **GCP proof.** The GCP metadata-server path (spec 0004 phase 4) is proven only by fakes. A chart
  on GKE is the natural live check.
- **Marketplace.** Azure Marketplace ("a couple of clicks", spec 0001) needs a package and a
  first-run bootstrap that does not depend on someone running `kista admin` by hand.

## Design

### Phase 1: the image, releases, the chart

#### Server changes

1. **A probe listener.** This adds a new listener scheme, `health`:
   - It is plain http. It serves only `GET` and `HEAD` on `/healthz` and `/readyz`, from its own
     small handler that reads the same readiness state. Every other path gets `404` with no body.
   - It writes no access log, no event and no metric.
   - It takes neither `tls` nor `behind_proxy`. At most one is allowed. A config with only a
     `health` listener is refused.
   - Its server timeouts are short: 5 s to read or write, 30 s idle, and `MaxHeaderBytes` 1 KiB
     (Go's server reads up to 4 KiB more). `OPTIONS *` is a `404` too
     (`DisableGeneralOptionsHandler`), paths are matched as the main router matches them (no
     unescaping, no empty `?`), and a `GET` with a body is a `400`, as on the main listeners.
   - `/healthz` and `/readyz` keep answering on every other listener, as today.

   The scheme is added to spec 0006's listener rules and to the configuration page. It is meant
   for a port only the kubelet, or a load balancer's health check, reaches. It is never in a
   Service, so a load balancer must reach the pod port directly (see the chart notes below).
2. **Files kista reads** (`internal/secretfile`). A secret or a token file is opened through
   `os.OpenRoot(dir).Open(base)`, where `dir` and `base` are the configured path's directory and
   name. A relative link may lead anywhere under `dir`, such as the
   Kubernetes atomic-writer layout `x -> ..data/x`, `..data -> ..2026_10_14…`; a link that leaves
   `dir`, or an absolute link (even one pointing inside it), is refused. `dir` must be readable by
   kista (the `r` bit, not only `x`): `/etc/ssl/private` at `0710 root:ssl-cert` fails, and the
   error names the directory. A public file's links are followed anywhere: certbot's `live/`
   links and a system CA bundle's links work for a certificate. certbot's `privkey.pem` is a
   secret, and its `live/` link leaves the directory, so `key_file` names the real file under
   `archive/` (if its owner, mode and directory fit the secret rule), or a copy, for example
   made by a certbot deploy hook. For every class a missing file is retried once, for a `..data`
   swap in flight. The file must be a regular file, and nobody but its owner may write it (no
   group or other write bit); a secret or a token must have exactly one hard link. Then each
   class has its own rule:

   | Class | Files | Owner and mode | Bound |
   | --- | --- | --- | --- |
   | **secret** | `store.login.password_file`; Vault `token_file` (auth `token_file`); S3 `access_key_file` and `secret_key_file`; the Azure Blob key file; TLS `key_file`; OTLP `headers_file`; upstream `client_secret_file` and `key_file`; AWS static key files (dev) | **(a)** owned by kista's euid, no group or other bits; or **(b)** owned by root, group equal to kista's egid (not 0), mode at most `0440`. Shape (b) is a Secret volume under `fsGroup`, with `defaultMode` `0440` or `0400` (the kubelet makes it `0440`). | 64 KiB; 4 KiB for the S3 and Azure Blob keys; 1 MiB for TLS keys |
   | **token** | projected tokens: Vault `kubernetes` and `jwt` auth; the AWS web identity and Pod Identity token files; GCP `external_account` file sources; upstream `token_file` and `assertion_file` | owned by root or by kista's euid, any read bits. The kubelet makes a projected token `65532:65532 0640`, or root-owned, depending on `runAsUser`. A token is short-lived and audience-bound. | 64 KiB |
   | **public** | TLS `cert_file`; `ca_file`s (egress, Vault, S3); the GCP `credentials_file` (an `external_account` JSON without secrets, often a ConfigMap at `0644`) | owned by root or by kista's euid, any read bits | 1 MiB (64 KiB for the GCP file) |

   - The Azure federated token (`AZURE_FEDERATED_TOKEN_FILE`) is read by the Azure SDK, not by
     kista, so these rules do not reach it.
   - With `profile: dev`, the GCP `credentials_file` may be a `service_account` key file, which
     holds a private key, yet is checked as public: a gap accepted in dev only.
   - **Not unified:**
     - The file signer keeps its own stricter rule (no link, own uid, `0600`). A channel key in a
       Kubernetes Secret is readable by anyone with `get secrets` in the namespace, and sits in
       etcd and often in GitOps sources. `signing-keys.md` says this instead of today's line,
       which suggests mounting a Secret into `file_dir` works.
     - The SQLite file keeps its own handling (chmod to `0600` at open).
     - The jsonl sink and the spool keep their rules.
   - **Shape (b) and groups.** Shape (b) trusts root, who can read everything anyway, and kista's
     primary group, nothing wider: not a supplementary group and never gid 0 (OpenShift runs
     containers with gid 0). The chart sets `runAsGroup` equal to `fsGroup`. On a plain host the
     docs say kista's group must not be shared.
   - **Files that gain a check.** The files with no check today now get one, for example the
     database password. A host or Docker secret file must be `0400` and owned by uid 65532, or
     `0440` with kista's group. Docker Swarm's default secret mount, root-owned `0444`, is
     refused; Compose's file-based secrets are bind mounts and keep the host file's owner and
     mode. The error names the file and the rule it breaks (the path, owner, group and mode,
     never the content). The upgrade notes on the upgrading page list what changes for existing
     files.
3. **Bounded background shutdown.** After the HTTP shutdown, the runners (re-signer, upstreams,
   feeds, reaper, GC) release their leases within one more `serve.shutdown_timeout`. A runner
   still busy then is abandoned, and its lease expires as it would after a crash. Then the event
   writer writes what they and the drain recorded, and the download counter its last counts, in
   11 s of their own, so a stuck runner cannot take that time from them. Metrics are flushed
   within 10 s after that. Then the blob service and the store close; `database/sql` waits for
   queries in progress, so a runner abandoned in the middle of one can delay the exit somewhat.
   The total from SIGTERM is at most `drain_timeout + 2 × shutdown_timeout + 21 s`, plus closing
   the store and storage. A second signal ends the process at once, with the signal's exit
   status (143 for SIGTERM, 130 for SIGINT).
4. **`kista version`** (and `kista --version`) prints the version, the Go version and the
   platform, then exits. The help text tells it apart from `kista admin version …` (DuckDB
   versions). `serve` logs the same at start.

#### The image

- **Dockerfile.** It lives at the repository root and has three stages, plus a `binary` stage for
  the release binaries (below):
  - `node:22-alpine` on `$BUILDPLATFORM` builds the console: `npm ci`, the licence gate
    (`npm run licenses`), `npm audit --omit=dev --audit-level=high`, then `npm run build`, as CI
    does. Its SBOM is scanned (`BUILDKIT_SBOM_SCAN_STAGE`), since its packages end up in the
    binary.
  - `golang:1.27.2` on `$BUILDPLATFORM` cross-compiles with `CGO_ENABLED=0`, `-trimpath` and
    `-ldflags "-s -w -X …/internal/app.Version=$VERSION"`, with the console embedded.
  - `gcr.io/distroless/static-debian12:nonroot` holds the binary at `/kista`, runs as uid and gid
    65532, and carries the CA certificates.
- **Pinning.** Every `FROM` is a literal `image:tag@sha256:…`, so Dependabot can update it.
  Dependabot covers the docker, github-actions, gomod and npm ecosystems. Image bumps are not
  auto-merged.
- **Entrypoint.** `ENTRYPOINT ["/kista"]` and `CMD ["serve", "-config", "/etc/kista/kista.yaml"]`.
  `ENV TMPDIR=/var/lib/kista/tmp`, so SQLite's temp files land on the writable volume. There is no
  shell. One-off work runs as `kista admin …` through `kubectl exec`, which needs no shell, or in a
  Job.
- **Build context.** `.dockerignore` keeps `cmd/`, `internal/`, `web/`, `go.mod` and `go.sum`. It
  drops `web/console/node_modules`, `web/console/dist/{app,mfe}`, the console's e2e and Playwright
  files, and `website/`, so a stale local build never reaches the Go stage.
- **Platforms.** `linux/amd64` and `linux/arm64`, built by buildx. Go cross-compiles, so no QEMU
  is needed.

#### Releases

- **Workflows** `image.yml` (pull requests and `main`) and `release.yml` (tags `v*`).
  - Top-level `permissions: {}`. Every action is pinned by SHA.
  - Build jobs have `contents: read` only. Images build through buildkit, where no `RUN` step
    sees the runner's OIDC token. Binaries and the chart are uploaded as workflow artifacts.
  - The jobs that push, sign or create the release (`packages: write`, `id-token: write`,
    `contents: write`, `attestations: write`) run no repository or npm code. They take digests and
    artifacts from the build jobs. The build job writes the image as an OCI layout (with buildx's
    SBOM) and uploads it; the publish job pushes it with `crane` (a pinned release, checked by
    its SHA-256) and refuses unless the registry's digest is the built one. Releases are signed
    in that job too, and the GitHub release is created by a third job with `contents: write`
    only.
  - Tag builds use no build cache.
  - Tags `v*` are protected by a ruleset. A release refuses to overwrite an existing version tag in
    GHCR.
- **The image `ghcr.io/hugr-lab/kista`:**
  - A pull request builds both platforms, then loads and runs an amd64 build (the smoke test
    below). It pushes nothing.
  - `main` pushes `:edge` and `:sha-<short>`. A tag pushes `:vX.Y.Z`. There is no `:latest`.
  - Every pushed image is signed by digest with cosign, keyless (GitHub's OIDC identity, Rekor).
    It carries buildx's SBOM, and a build provenance attestation from
    `actions/attest-build-provenance`. The docs claim no SLSA level beyond what that action
    states.
- **The chart:** `helm package --version X.Y.Z --app-version vX.Y.Z`, pushed to
  `oci://ghcr.io/hugr-lab/charts` (so `oci://ghcr.io/hugr-lab/charts/kista`), signed by digest
  in the same way. It has an `org.opencontainers.image.source` annotation.
- **Binaries:** `kista` for linux and darwin, amd64 and arm64 (`kista-vX.Y.Z-<os>-<arch>`, built
  by the Dockerfile's `binary` stage, so with the same console gates), on the GitHub release, with
  `SHA256SUMS` and cosign `sign-blob --bundle` files. The darwin builds are not notarised, and the
  docs say how to clear the quarantine attribute.
- **Verifying.** The docs show `cosign verify` with `--certificate-identity` set to the release
  workflow at that tag, and the GitHub OIDC issuer. Edge images are verified against `image.yml`
  at `refs/heads/main`. The docs recommend deploying by digest, and the chart takes
  `image.digest`. They also mention an admission policy (sigstore policy-controller, Kyverno).
- **First release.** A one-time checklist links the GHCR packages to the repository and makes
  them public.
- No goreleaser: the workflows stay short and readable.

#### The chart (`deploy/helm/kista`)

- `Chart.yaml` has `kubeVersion: ">=1.30.0-0"` (the native `preStop` sleep). There is a
  `values.schema.json`.
- **Config.** `values.config` is kista's config file. It goes into a ConfigMap mounted at
  `/etc/kista/kista.yaml`, and a `checksum/config` annotation rolls the pods when it changes.
  - kista already refuses secrets in config (a DSN password, inline tokens), so a ConfigMap is
    enough.
  - Helm renders large numbers from values as floats, so sizes are written as strings with units
    (`max_body: 1GiB`). The values comments say so.
  - The chart writes these keys itself:
    - **The health listener.** It always appends `{addr: ":8081", scheme: health}` and refuses a
      config that defines a `health` listener or uses `:8081`. Lists are replaced, not merged, so
      a user's `serve.listeners` never drops the probes.
    - **The serving listener**, if `serve.listeners` is not set. One of:
      - `behind_proxy` (the default): `:8443` `https` behind a TLS-terminating ingress or L7 load
        balancer that sends `X-Forwarded-For` and `X-Forwarded-Proto`. The ingress serves http
        and https on the same host, as spec 0006 needs for DuckDB's http bootstrap.
      - `tls`: `:8443` `dual` with the certificate from `tlsSecret`. One port serves DuckDB's http
        bootstrap and https. It suits an L4 load balancer (`Service` type `LoadBalancer`,
        `externalTrafficPolicy: Local`, so client addresses are kept) or TLS passthrough. The pod
        is the TLS end, and nothing is in clear on the pod network.
    - **`blob.spool_dir`**: `/var/lib/kista/tmp/spool`. It sits under an `emptyDir` at
      `/var/lib/kista/tmp` with a `sizeLimit` and an `ephemeral-storage` request, both
      `(2 × max_ingests + 1) × max_body` unless set (upstream downloads go through the same
      ingest slots). kista creates the
      subdirectory itself with mode `0700`.
  - The root filesystem is read-only.
- **Storage.**
  - With `store.kind: postgres` or `sqlserver` and more than one replica, `blob.domains` must name
    a remote store (`s3`, `azureblob`, `gcs`), or an `fs` domain on a `ReadWriteMany` volume
    (`blobVolume`). `helm install` fails otherwise. A `blobVolume` of another access mode means one
    pod at a time: `Recreate`, and no `adminJobs`.
  - With `store.kind: sqlite`, the chart:
    - forces one replica and the `Recreate` strategy;
    - uses one `ReadWriteOnce` PVC on block storage (not Azure Files or NFS: SQLite's locking and
      WAL fail there), holding the database and an `fs` blob root;
    - sets `fsGroupChangePolicy: OnRootMismatch`, so the kubelet does not rewrite the database's
      mode on every start.
- **Environment.**
  - `enableServiceLinks: false` on every pod the chart makes, so the kubelet injects no `KISTA_*`
    service variables.
  - `env` and `envFrom` are for `OTEL_*` and the like, with secret values through `secretKeyRef`.
    A `KISTA_*` variable is allowed (the store DSN, `public_url`). The comments say config belongs
    in `config`, and an unknown or file-only `KISTA_*` variable stops kista.
- **Secrets** come from `secretVolumes`: each Secret is mounted as a directory, never with
  `subPath`, which would freeze updates. `defaultMode` is `0440`, under
  `fsGroup = runAsGroup = 65532`. The config names files in those directories. kista reads
  some of them again whenever it uses them, so a rotated Secret takes effect without a restart:
  - the database password, on each new connection;
  - the Vault token, at each login;
  - the S3 keys, on each credential refresh;
  - the TLS certificate and key, when the files change.

  An OTLP events sink's `headers_file` is read only at start, so changing it needs a restart. There are no
  secret values in `values.yaml`.
- **Identity.**
  - `serviceAccount.{create, name, annotations}`. `automountServiceAccountToken: false` always:
    kista does not use the Kubernetes API, and the cloud webhooks inject their own token volumes.
  - The values carry one identity block per cloud. The chart writes the annotation and the pod
    label, and checks the matching config:
    - `azure.workloadIdentity.clientId`: the annotation, `azure.workload.identity/use`, and equal
      to the `client_id` of every identity with `kind: workload` in config;
    - `aws.irsa.roleArn`: equal to the config's `role_arn`. EKS Pod Identity needs no annotation.
    - `gcp.serviceAccount`: `iam.gke.io/gcp-service-account`, equal to the `principal` of every
      `gcpkms` source and `gcs` domain that uses the metadata server.
    - Each check runs both ways: a value without a matching identity in config, or the reverse,
      fails. `profile: dev`, which relaxes these rules, needs `devProfile: true`.
  - `projectedTokens` adds service account tokens at a path. Each needs an audience that is not
    the API server's, and an `expirationSeconds` of at most 3600. They serve Vault or OpenBao
    `jwt`/`kubernetes` auth, and a GCP `external_account` file source on AKS or EKS.
- **The pod.**
  - Defaults: `replicas: 2` and a `RollingUpdate` strategy with `maxSurge: 1` and
    `maxUnavailable: 0`.
  - Security context: `runAsNonRoot`, `runAsUser`, `runAsGroup` and `fsGroup` 65532,
    `seccompProfile: RuntimeDefault`, no privilege escalation, a read-only root filesystem, and
    all capabilities dropped.
  - Probes, on `8081`:
    - readiness: `/readyz` every 5 s;
    - liveness: `/healthz` every 10 s, `failureThreshold: 6`;
    - startup: allows the first migration, 5 minutes by default (a value).
  - Shutdown:
    - `preStop`: the native `sleep` action for 5 s (the image has no shell), so the endpoints
      update before the drain.
    - `terminationGracePeriodSeconds` is derived from the values: `preStop` + `drain_timeout` +
      2 × `shutdown_timeout` + 21 s, plus a 10 s margin for closing the store and storage. That
      is 101 s with the defaults.
  - `topologySpreadConstraints` by zone and by node, with `whenUnsatisfiable: ScheduleAnyway`,
    so kind, on-premises and single-zone clusters still schedule.
  - `resources`: requests of 100m and 128Mi, and a 512Mi memory limit.
- **Service.** `ClusterIP`, port 80 to `8443` (`behind_proxy`), or a `LoadBalancer`, port 80 and
  443 to `8443` (`dual`). The health port is never in the Service.
- **Ingress** (optional: class, hosts, TLS). The values comments carry spec 0006's notes:
  - DuckDB follows no redirect, so the http→https redirect must be off for the hosts that serve
    DuckDB;
  - `public_url` is the audience base.
- **Cloud load balancers that reach pods directly** check health on the serving port unless told
  otherwise, where `/` answers `404`:
  - GKE Ingress with NEG: the chart renders a `BackendConfig` for `8081` `/readyz` (the Gateway
    API's `HealthCheckPolicy` is documented, not rendered).
  - AWS ALB with IP targets: the values comments give `healthcheck-port: "8081"` and
    `healthcheck-path: /readyz`.
  - Application Gateway: the equivalent probe setting.

  The load balancer's source ranges (GKE `35.191.0.0/16` and `130.211.0.0/22`, the ALB subnets,
  the gateway subnet) go into `trusted_proxies` and into the NetworkPolicy.
- **Who may connect: NetworkPolicy and `trusted_proxies` together.**
  - What trusting a peer grants: its `X-Forwarded-For` sets the client address (access logs,
    events, the per-client download limit), and a request counts as https only with exactly one
    `X-Forwarded-Proto: https` (spec 0006); a peer outside `trusted_proxies` is refused. Pod IPs are seldom in a range of their own, so `trusted_proxies`
    is in practice the cluster's pod CIDR.
  - So with `behind_proxy` the chart renders, by default, a NetworkPolicy that admits `8443` only
    from the ingress controller's pods (`namespaceSelector` and `podSelector` from the values) and
    the load balancer ranges. Turning it off takes an explicit `networkPolicy.enabled: false`.
    The selectors have no default (ingress-nginx, the obvious guess, was retired in March 2026):
    `helm install` fails until the pod selector or the load balancer ranges are set. A
    `namespaceSelector` alone is refused (it admits every pod there); a `podSelector` alone means
    kista's namespace.
  - With `behind_proxy`, a Service other than `ClusterIP` is refused: it would expose the
    plain-http listener to clients.
    The kubelet is exempt from NetworkPolicy on every mainstream CNI; `8081` admits only the
    configured load-balancer ranges, plus an optional node CIDR for CNIs that do not exempt it.
  - Spec 0006 notes that the hop from the ingress to the pod is plain http and carries Bearer
    tokens. The hardened options are `dual` with in-pod TLS, or a mesh with mTLS.
- **PodDisruptionBudget** `minAvailable: 1`, rendered only when there is more than one replica.
  With a single SQLite replica it would block every node drain.
- **Resigning.** `serve.resign` stays as the config says, and the comments say to turn it on only
  where the pod may sign (spec 0003).
- **Migrations** run at start under the store's lock (`store.migrate: auto`, spec 0003), which is
  safe with several replicas.
  - A migration that raises `min_reader` (0018 and 0019 did) makes every older pod unready as soon
    as it commits. There is a short window with no ready pod, until the new pod's first readiness
    check. The upgrading page states this and the setting that avoids it: `store.migrate: check`
    on the pods, with `kista admin migrate` run first through `kubectl exec` or a Job.
  - `/readyz` depends on the database, so a database outage takes every pod out of the endpoints.
    That is intended.
- **Admin work.**
  - **First run** is `kubectl exec deploy/<fullname> -- /kista admin -config /etc/kista/kista.yaml
    …`, which needs no shell. `release add -` reads stdin through `kubectl exec -i`.
  - **Recurring commands** (`blob verify`, spec 0018) are `adminJobs`: one CronJob per entry,
    suspended unless a schedule is set, with the same config, Secrets, spool `emptyDir` and
    identity. With SQLite they are refused: a second pod cannot mount the `ReadWriteOnce` volume
    on another node. Run them by `kubectl exec` there.
  - Every container acts as `os:65532:nonroot` (spec 0007), so the audit cannot tell who ran a
    command. Namespace RBAC is the admin boundary: who may exec into pods, create Jobs or read
    Secrets. The hardening list says so, with Pod Security Admission `restricted` on the
    namespace.
- **`NOTES.txt`** prints the readiness check and the first admin commands.

#### Documentation

- **A Deployment section** in the site:
  - the image: tags, verifying signatures, platforms;
  - Docker: `docker run` with the config mounted, secret files `0400` and owned by uid 65532, a
    tmpfs or volume at `/var/lib/kista`;
  - Kubernetes: values, Secrets as files, identities per cloud, ingress and load balancers,
    NetworkPolicy, replicas and storage, upgrading, admin work;
  - a hardening list, including egress: block `169.254.169.254` where no metadata identity is
    used, and an IMDS hop limit of 1 on EKS unless kista's AWS identity is `kind: imds`.
- **Corrections.**
  - The pages that say there is no image or chart.
  - The new file rules: configuration, security, and the pages for each backend.
  - `signing-keys.md` on Secrets in `file_dir`.
- **Drift found while writing this spec.**
  - `upgrading.md` says this release carries 18 migrations; it carries 19.
  - `state.md` names `0018_feeds` as the newest; it is `0019_tenant_deletion`, with `min_reader`
    19.
  - `state.md`'s lease table lacks `kista/tenant-delete/<id>`.

### Phase 2: Azure (outline; a phase 2 addendum details it before its code)

Superseded by spec 0020 (cloud recipes): the Container Apps Bicep is its phase 3, the bootstrap
and the Marketplace package its phase 4. The outline below is kept as it was written.

- **Bicep for Azure Container Apps** (`deploy/azure`), like tresor-server's:
  - a user-assigned managed identity;
  - PostgreSQL Flexible Server or Azure SQL, with Entra-only auth, the identity as the database's
    Entra admin, and `sslmode=verify-full`;
  - Key Vault Premium or Managed HSM with the channel and attestation keys (`RSA-HSM`, tags
    `kista-purpose`) and a custom role per key;
  - a Storage account with a private container (`allowBlobPublicAccess = false`);
  - Log Analytics;
  - the app, with ingress `allowInsecure: true` for DuckDB's http bootstrap (spec 0006), and a
    health probe on the `health` port;
  - a Container Apps job per admin command.
- **Bootstrap from config** (spec 0003, "Later"): a `bootstrap` section that creates, once and
  idempotently:
  - the first tenant and channel;
  - the versions;
  - the keys;
  - the issuer;
  - the admin grant.

  Each creation is recorded as an event. The bootstrap is refused when those objects exist and
  differ. A managed install then starts usable.
- **Azure Marketplace managed application:** a `mainTemplate.json` built from the Bicep, a
  `createUiDefinition.json`, and the package as a release asset. The offer states the publisher's
  access to the managed resource group.

## Security

- **The image.**
  - A static binary with no shell and no package manager. It runs as non-root on a read-only root
    filesystem, from pinned base images.
  - The image, the chart and the binaries are signed keylessly, and the docs pin the verify
    identity to the release workflow and tag. They carry an SBOM, which includes the console's
    packages, and a provenance attestation.
  - Build jobs hold no token that can push, sign or release. Signing jobs run no repository or npm
    code.
  - A version tag is never overwritten.
- **The probe listener** answers two fixed paths with fixed bodies. It carries no tenant data, no
  tokens and no events, so a misconfigured Service exposes only liveness.
- **Files.**
  - **Secret files.** Shape (b) trusts root and kista's own primary group, never gid 0 and never a
    supplementary group. A link cannot lead out of the configured directory, and an absolute
    link is refused: `os.OpenRoot` refuses it while it opens, with no separate check before. A
    secret has exactly one hard link. Files that anyone but the owner may write are always
    refused. Every configured file now has a check, also those that had none.
  - **Tokens and public files.** Both must be owned by root or by kista's euid, with any read
    bits, and nobody but the owner may write them. Tokens need no narrower mode, because they are
    short-lived and audience-bound; a token, like a secret, is opened inside its directory and has
    one hard link. Public files follow links anywhere: their content is not secret.
  - **Signing keys.** The file signer is not relaxed: channel keys belong in a KMS, an HSM or
    Transit (spec 0004). A Kubernetes Secret is not a key store.
- **Who may connect.**
  - The NetworkPolicy is on by default with `behind_proxy`. It is what makes trusting the pod
    CIDR acceptable, because only the ingress controller and the load balancers reach `8443`.
  - The plain-http hop from the ingress to the pod is named, and in-pod TLS (`dual`) is the
    hardened option.
- **No Kubernetes privileges.**
  - The chart grants kista no RBAC, and does not mount the API token.
  - Projected tokens need an audience that is not the API server's, and expire within an hour.
- **Admin boundary.** In a cluster, the namespace's RBAC decides who can act as kista's server
  administrator: whoever may exec into its pods, create Jobs in the namespace, or read its
  Secrets. The docs say so plainly.

## Testing

- **Go.**
  - `internal/secretfile`, per class:
    - each accepted shape;
    - refusals: group- or other-writable, other-readable secrets, a foreign owner, a foreign or
      supplementary group, gid 0, a link out of the directory, a link to a sibling directory with
      a common prefix, a directory, a FIFO, an oversized file;
    - a `..data` swap during the open.
  - The `health` listener: the two paths, `404` elsewhere, no log, event or metric, and the config
    rules.
  - The bounded shutdown: a runner that never returns.
  - `kista version`.
  - Each migrated caller keeps its tests.
- **Image** (every pull request): build both platforms. Then load amd64 and run it as uid 65532
  with a read-only root filesystem, a tmpfs at `/var/lib/kista`, and a dev config (SQLite, `fs`
  blobs). Check `/readyz` on the health port, and `kista version`.
- **Chart, static** (every pull request):
  - `helm lint --strict`, with `values.schema.json`;
  - `kubeconform -strict` (Kubernetes 1.30) on renders for SQLite, PostgreSQL with S3,
    `behind_proxy` and `dual`, the AKS, EKS and GKE identity values, and a long release name;
  - `helm template` must fail for:
    - more than one replica with `fs` on a `ReadWriteOnce` volume;
    - a user `health` listener;
    - `behind_proxy` without `trusted_proxies`;
    - an identity that does not match the config.
- **Chart, kind.** This runs in `e2e.yml` after its DuckDB cache restore, on changes to
  `deploy/**`, the chart, the Dockerfile or `internal/secretfile`, and nightly. About 15 minutes
  warm.
  - The cluster:
    - kind with Traefik (ingress-nginx is retired), NetworkPolicy enforced (kindnet);
    - the chart with `behind_proxy` and the NetworkPolicy on;
    - a TLS ingress with a test CA, at `kista.localhost`: curl uses `--resolve`, DuckDB the system
      resolver (macOS and systemd-resolved map `*.localhost` to loopback; the workflow also adds
      an `/etc/hosts` entry).
  - The backends:
    - PostgreSQL, with its password from a Secret;
    - SeaweedFS (S3), with its key files from Secrets;
    - OpenBao Transit with `kubernetes` auth through a projected token (the harness binds
      `system:auth-delegator` for OpenBao);
    - PostgreSQL and SeaweedFS over TLS with the test CA, as outside `profile: dev` they must be;
    - the cluster's own service account issuer as the tenant's token issuer (its JWKS through
      `egress` with the API server's CA), so the Bearer tokens are real `kubectl create token`
      tokens and no IdP is deployed.
  - A step asserts the `stat` of every mounted file, to pin the shapes this spec relies on.
  - Then:
    1. Two replicas become ready.
    2. `kubectl exec` bootstraps a tenant, a channel and a key.
    3. The pinned DuckDB (`e2e/DUCKDB_PIN`) installs and loads an extension through the ingress,
       public and with a Bearer token. This uses a new e2e mode, `KISTA_E2E_URL` with the CA,
       that targets a running kista instead of starting one.
    4. A pod other than the ingress controller's cannot reach `8443`.
    5. A rolling restart: a poll through the ingress sees `/readyz` up throughout.
  - The harness is `deploy/helm/kind/run.sh`; it runs locally as well.
- **GKE, by hand.** `deploy/gcp/gke.sh` adds a GKE Autopilot cluster to a stack made by
  `deploy/gcp/up.sh`. It installs the chart with GKE Workload Identity, a GKE Ingress and the
  `BackendConfig`. This proves spec 0004 phase 4's metadata-server path, and the load-balancer
  health check, live. `down.sh` still removes everything with the project.

## Alternatives considered

- **Probes through `trusted_proxies`** (adding the node CIDR): the kubelet's source differs by
  CNI, and it adds trust only to make probes pass. Rejected for the dedicated listener.
- **Probes on an `http` listener:** it serves public releases on a port meant for the kubelet.
  `health` serves nothing else.
- **An init container that copies Secrets into an owner-only `emptyDir`:** no server change.
  But every secret then exists twice, rotation needs a restart, and the copy needs a shell the
  image does not have.
- **Ignoring `*_SERVICE_*` variables in the config loader** instead of `enableServiceLinks: false`:
  that weakens the rule that catches misspelled settings, for a problem the chart can avoid.
- **A pre-upgrade migration Job by default:** the store's lock already makes migrations at start
  safe, and hooks do not run under Argo CD or `helm template`. The opt-in `store.migrate: check`
  path covers the `min_reader` window.
- **Config in a Secret instead of a ConfigMap:** kista refuses secrets in config, so it would only
  hide a readable file. An operator who wants it can mount their own.
- **The CSI Secrets Store driver:** not required. Its providers default to mode `0644` and do not
  always apply `fsGroup`, so the shapes are documented per provider only if tested.
- **goreleaser:** one more tool for three short workflows.
- **`:latest`:** never; a deployment names a version.
