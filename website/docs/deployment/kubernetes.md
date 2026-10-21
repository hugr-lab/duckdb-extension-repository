---
title: Kubernetes
---

# Kubernetes

The Helm chart `deploy/helm/kista` deploys kista on Kubernetes 1.30 or later. Each release
publishes it signed at `oci://ghcr.io/hugr-lab/charts/kista` ([Deployment](index.md)). Every
value is described in the chart's
[`values.yaml`](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/deploy/helm/kista/values.yaml).
The design is in
[spec 0013](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0013-deployment/spec.md).

```bash
helm install kista oci://ghcr.io/hugr-lab/charts/kista --version 1.2.0 \
  -n kista --create-namespace -f values.yaml \
  --set image.digest=sha256:<the release's image digest>
```

## A first values file

PostgreSQL, S3 and an ingress controller in front:

```yaml title="values.yaml"
replicas: 2
config:
  store:
    kind: postgres
    dsn: postgres://kista@db.internal:5432/kista?sslmode=verify-full
    login:
      password_file: /etc/kista/secrets/db/password
  serve:
    public_url: https://kista.example.com
    trusted_proxies: [10.244.0.0/16]     # the pod CIDR: see "Who may connect"
  blob:
    domains:
      - name: default
        kind: s3
        s3:
          endpoint: https://s3.eu-central-1.amazonaws.com
          bucket: acme-kista
          region: eu-central-1
          lookup: dns
          sse: s3
          access_key_file: /etc/kista/secrets/s3/access-key
          secret_key_file: /etc/kista/secrets/s3/secret-key
  # signers: a KMS, an HSM or Vault Transit (Signing keys)
secretVolumes:
  - {name: db, secretName: kista-db, mountPath: /etc/kista/secrets/db}
  - {name: s3, secretName: kista-s3, mountPath: /etc/kista/secrets/s3}
ingress:
  enabled: true
  className: traefik
  hosts: [{host: kista.example.com}]
  tls: [{secretName: kista-tls, hosts: [kista.example.com]}]
networkPolicy:
  ingressController:
    namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: traefik}}
    podSelector: {matchLabels: {app.kubernetes.io/name: traefik}}
```

```bash
kubectl -n kista create secret generic kista-db --from-file password=./db-password
kubectl -n kista create secret generic kista-s3 --from-file access-key=./ak --from-file secret-key=./sk
```

## What the chart writes

`config` is kista's configuration file, as YAML. It goes into a ConfigMap at
`/etc/kista/kista.yaml`, and a change rolls the pods. The chart adds:

- **A `health` listener** on `:8081` for the kubelet's probes and load-balancer health checks. Do
  not define one, and do not use `:8081`.
- **The serving listener** on `:8443`, unless `config.serve.listeners` is set (it must then listen
  on `:8443`). `mode` picks it:
  - `behind_proxy` (the default): `https` with `behind_proxy: true`, behind an ingress controller
    or L7 load balancer that terminates TLS and sends `X-Forwarded-For` and `X-Forwarded-Proto`.
    The Service is `ClusterIP`, port 80.
  - `tls`: `dual` on one port with the certificate from `tlsSecret` (a `kubernetes.io/tls`
    Secret). The pod terminates TLS; the Service is a `LoadBalancer` on 80 and 443 with
    `externalTrafficPolicy: Local`, so client addresses are kept. Use it with an L4 load
    balancer or TLS passthrough.
- **`blob.spool_dir`** `/var/lib/kista/tmp/spool`, on an `emptyDir` at `/var/lib/kista/tmp` (also
  `TMPDIR`). Its size limit is `(2 × max_ingests + 1) × max_body` unless `spool.sizeLimit` is set,
  plus `spool.importHeadroom` ([Importing a bundle](#importing-a-bundle)).
- **With SQLite**: `store.sqlite.path` `/var/lib/kista/data/kista.db` and, without `blob.domains`,
  an `fs` domain at `/var/lib/kista/data/blobs`, both on one `ReadWriteOnce` volume.

Write sizes as strings with units (`max_body: 1GiB`): Helm turns large numbers into floats.
`helm install` fails, before anything is created, on mistakes such as:

- a `health` listener, or `:8081`, in `config.serve.listeners`, or listeners without `:8443`;
- a `behind_proxy` listener without `config.serve.trusted_proxies`, or with a Service other than
  `ClusterIP` (it would put the plain-http listener in front of clients);
- an `fs` blob domain on no volume, or more than one replica on anything but a remote store or a
  `ReadWriteMany` `blobVolume`;
- an identity in the values that does not match the configuration (below);
- the NetworkPolicy without the ingress controller's pods or load-balancer ranges;
- the egress NetworkPolicy without a rule for the database, or without the metadata endpoint the
  configuration's cloud identity uses ([What kista may reach](#what-kista-may-reach));
- `profile: dev` without `devProfile: true`: it relaxes the key and identity rules.

## Another namespace

`namespaceOverride` puts every object the chart makes in that namespace instead of the release's
(spec 0024). An umbrella chart uses it to install kista apart from the platform, whose namespace is
a trust boundary of its own. The chart creates no namespace:
create it, with its labels (Pod Security Admission, the ones the NetworkPolicy selects on).

- **What must live there too:** everything a pod or the Ingress refers to: `tlsSecret`,
  `secretVolumes`, `imagePullSecrets`, the Secrets of `envFrom`, `existingClaim`, and the Ingress's
  TLS Secrets.
- **Cloud identities name the namespace:** the IRSA role's trust (`sub`), the Azure federated
  credential's subject, and the GKE Workload Identity member name it, so they name the override.
- **The NetworkPolicy** selects the ingress controller's pods in kista's namespace unless
  `networkPolicy.ingressController.namespaceSelector` names the controller's. Clients (DuckDB,
  nodes) reach kista through the ingress, at `serve.public_url`.
- **A namespace made by the umbrella** is deleted by its `helm uninstall`, and with it everything in
  it, the volumes' claims included. Create the namespace outside that release, or annotate the
  Namespace object `helm.sh/resource-policy: keep`; a StorageClass with `reclaimPolicy: Retain`
  keeps the disks.
- **`helm template`** renders the namespace too: give it `-n` (or the override), or the objects
  name the current context's namespace.
- **Egress at startup.** A bootstrap with upstreams reads their `.well-known` before serving: in a
  cluster that denies egress by default, kista's pods must reach them on 443, and DNS.
  `networkPolicy.egress` admits exactly that and what else kista needs
  ([What kista may reach](#what-kista-may-reach)); without it, a policy of the umbrella's must.

## Values from a parent chart

`configTemplate: true` lets a parent (umbrella) chart write kista's `config` with its own values:
every string in `config` that holds `{{` is rendered by Helm's `tpl` with the chart's root
context, `.Values.global` included (spec 0025). It is off by default, so a configuration with a
literal `{{` keeps working.

```yaml title="the umbrella's values.yaml"
global:
  database: {host: db.platform.svc}
  kista: {host: kista.example.com}
kista:
  namespaceOverride: kista
  configTemplate: true
  config:
    store:
      kind: postgres
      dsn: postgres://kista@{{ .Values.global.database.host }}:5432/kista?sslmode=verify-full
      login: {password_file: /etc/kista/secrets/db/password}
    serve:
      public_url: https://{{ .Values.global.kista.host }}
```

- **Within its string.** Each template renders inside the string that holds it, so a value can
  never add a key to the configuration: a global holding `"db\nprofile: dev"` stays a part of the
  DSN. The rendered result is a string; a number, a boolean or a list cannot be templated.
- **Checked after rendering.** The chart's checks and kista's own (no password in a DSN, no
  token inline: secrets stay files from `secretVolumes`) apply to the rendered configuration,
  as they would to the same text written out. A template that fails (a missing value) fails
  `helm install`.
- **A literal `{{`** is then written `{{ "{{" }}`.
- **What the pods read** is in the ConfigMap, rendered; a change of a value the templates use rolls
  the pods.

## Storage and replicas

- **PostgreSQL or SQL Server** with a remote blob store (`s3`, `azureblob`, `gcs`) runs any
  number of replicas: `replicas: 2` by default, a `RollingUpdate` that adds a pod before removing
  one, and a PodDisruptionBudget of `minAvailable: 1`. Migrations run at start under the
  store's lock.
- **An `fs` domain** shared by replicas needs `blobVolume` with `ReadWriteMany` (NFS, Azure Files,
  EFS), mounted at `/var/lib/kista/blobs`, the default domain's root. With another access mode
  only one pod may mount it: one replica, the `Recreate` strategy, no `adminJobs`.
- **SQLite** forces one replica and the `Recreate` strategy, and keeps the database and the bodies
  on one `ReadWriteOnce` volume (`persistence`). Use block storage: SQLite's locking fails on NFS
  and Azure Files. There is no PodDisruptionBudget, which would block every node drain. The
  volume is kept on `helm uninstall`.

## Secrets

`secretVolumes` mounts each Secret as a directory, mode `0440`, group `65532`: the shape kista
accepts for a secret file ([Files kista reads](../security.md#files-kista-reads)). The
configuration names files in those directories. Never mount a Secret with `subPath`: the file
would never change again.

kista reads these again when it uses them, so a rotated Secret takes effect without a restart:
the database password (each new connection), the Vault token file (each login), the S3 keys (each
credential refresh), the TLS certificate and key (when they change). An OTLP events sink's
`headers_file` is read only at start: roll the pods after changing it.

`env` and `envFrom` are for `OTEL_*` and the like, with secret values through `secretKeyRef`. A
`KISTA_*` variable overrides the setting it names; an unknown or file-only one stops kista. The
chart sets `enableServiceLinks: false`, so a Service named `kista` injects no `KISTA_*`
variables.

## Cloud identities

kista needs no Kubernetes API credentials, so the chart never mounts the service account's API
token. The
cloud webhooks add their own token volumes. Each identity value writes the annotation (and
label) the platform needs, and the chart checks it against the configuration both ways:

| Platform | Value | The chart writes | The configuration |
| --- | --- | --- | --- |
| AKS workload identity | `azure.workloadIdentity.clientId` | annotation `azure.workload.identity/client-id`, pod label `azure.workload.identity/use` | every identity with `kind: workload` has this `client_id` |
| EKS IRSA | `aws.irsa.roleArn` | annotation `eks.amazonaws.com/role-arn` | every identity with `kind: irsa` (sources, domains, `aws.identity`) has this `role_arn` |
| EKS Pod Identity | none | nothing | `kind: pod_identity` |
| GKE Workload Identity | `gcp.serviceAccount` | annotation `iam.gke.io/gcp-service-account` | every `gcpkms` source, `gcs` domain and the `gcp` database login without `credentials_file` has this `principal` |

`projectedTokens` adds service account tokens with your own audience, for Vault or OpenBao
`kubernetes` or `jwt` auth, or a GCP `external_account` file on AKS or EKS. The audience must not
be the API server's, and `expirationSeconds` is at most 3600:

```yaml
projectedTokens:
  - {name: bao, mountPath: /var/run/secrets/kista/bao, audience: openbao, expirationSeconds: 3600}
config:
  signers:
    sources:
      - name: bao
        kind: vault
        allow: ["ext-"]
        vault:
          address: https://bao.internal:8200
          mount: transit
          software_keys: true
          auth: {kind: kubernetes, role: kista, token_file: /var/run/secrets/kista/bao/token}
```

Channel keys belong in a KMS, an HSM or Transit ([Signing keys](../signing-keys.md)). kista
refuses a key file from a Kubernetes Secret: a Secret is not a key store.

## Who may connect

- **`trusted_proxies`.** A `behind_proxy` listener refuses any peer outside
  `serve.trusted_proxies`. A trusted peer sets the client address with `X-Forwarded-For` (the
  access log, events, the per-client download limit), and a request counts as https only with
  exactly one `X-Forwarded-Proto: https`. Ingress controller pods seldom have a range of their
  own, so this is in practice the cluster's pod CIDR.
- **The NetworkPolicy** narrows it. It is on by default with `behind_proxy`: port 8443 admits only
  the ingress controller's pods and `loadBalancerRanges`. Name the controller's pods with
  `networkPolicy.ingressController.podSelector`, and its namespace with `namespaceSelector` unless
  it runs in kista's namespace; a `namespaceSelector` alone is refused, as it would admit every
  pod there.
  Port 8081 admits `loadBalancerRanges` and `nodeCIDRs`; the kubelet's probes are exempt from
  NetworkPolicy on the mainstream CNIs. Turn it off only with `networkPolicy.enabled: false`.
- **The plain hop.** The request from the ingress controller to the pod is plain http and carries
  Bearer tokens. Where that matters, use `mode: tls` (the pod terminates TLS) or a service mesh
  with mTLS.
- **DuckDB's http bootstrap.** DuckDB follows no redirect. Turn the http→https redirect off for
  the hosts DuckDB uses, so its first `INSTALL httpfs` over http works
  ([Using kista from DuckDB](../duckdb.md)).

## Load balancers that reach the pods

A cloud load balancer that sends traffic straight to pod IPs checks their health itself, by
default on the serving port, where `/` answers `404`. Point it at `/readyz` on 8081, and put its
ranges in `trusted_proxies` and `networkPolicy.loadBalancerRanges`:

- **GKE Ingress (NEG).** `gke.backendConfig.enabled: true` renders a `BackendConfig` for 8081
  `/readyz` and the Service annotations. The checks come from `35.191.0.0/16` and
  `130.211.0.0/22`. The `BackendConfig` also sets the load balancer's limit on a whole response,
  `gke.backendConfig.timeoutSec` (600 seconds; GKE's default of 30 cuts large publishes and
  downloads). GKE Autopilot has no `IngressClass` object: take the Ingress with the annotation
  `kubernetes.io/ingress.class: gce`, not `className`. `gke.managedCertificate.enabled: true`
  renders a Google-managed certificate for the Ingress's hosts and names it on the Ingress; a
  global static IP is the annotation `kubernetes.io/ingress.global-static-ip-name`. GKE's default
  SSL policy accepts TLS 1.0: `gke.frontendConfig.sslPolicy` names one of yours (TLS 1.2 and up)
  in a `FrontendConfig` on the Ingress. The external
  load balancer appends its own address to `X-Forwarded-For`: put the static IP's `/32` in
  `trusted_proxies` too, or every client appears as the load balancer. GKE's NEG readiness gate
  can pass before the load balancer has checked a new pod, so a rolling restart may leave no
  healthy backend for seconds: set `minReadySeconds: 60` and `preStopSeconds: 20`
  (`gke.backendConfig.drainingTimeoutSec`, 30, lets requests in flight finish). [GCP](gcp.md) has
  a recipe.
- **AWS Load Balancer Controller, IP targets.** Ingress annotations
  `alb.ingress.kubernetes.io/healthcheck-port: "8081"` and
  `alb.ingress.kubernetes.io/healthcheck-path: /readyz`; the ranges are the ALB's subnets. An ALB
  keeps sending to a target for some seconds after its endpoint goes, longer than the chart's
  default `preStopSeconds: 5`: set `preStopSeconds: 20`, and label the namespace
  `elbv2.k8s.aws/pod-readiness-gate-inject=enabled` so a rollout waits for the ALB to see each new
  pod healthy. Without both, a rolling restart answers some requests `503` (measured on EKS).
- **Azure Application Gateway for Containers.** A `HealthCheckPolicy` for port 8081, path
  `/readyz`; the range is the gateway's subnet.

## What kista may reach

`networkPolicy.egress.enabled: true` adds a second NetworkPolicy, `kista-egress` for a release
named `kista`, on every pod of the release: the server and the `adminJobs`. It is off by default,
and apart from `networkPolicy.enabled`, which is the ingress policy. With it on, kista's pods
reach only:

| What | The rule | Values |
| --- | --- | --- |
| DNS | UDP and TCP 53 to `k8s-app: kube-dns` in `kube-system` (EKS, AKS, GKE, k3s, kind) | `dns.podSelector`, `dns.namespaceSelector`, `dns.ports`; `dns.enabled: false` |
| Public https | TCP 443 to `0.0.0.0/0` and `::/0`, except the private and special ranges | `public.ports`, `public.cidrs`, `public.except`; `public.enabled: false` |
| Private networks the configuration allows | each entry of `egress.allow`, `auth.server_egress_allow`, `upstreams.credentials[].allow` and `events.sinks[].allow`, with its ports (443 by default) | the configuration |
| The cloud identity's metadata endpoint | `gke`: `169.254.169.254:80` and `169.254.169.252:988`; `aws-pod-identity`: `169.254.170.23:80` and `[fd00:ec2::23]:80`; `imds`: `169.254.169.254:80` | `cloudMetadata` |
| Everything else | your NetworkPolicy egress rules, rendered as given | `extraRules` |

Public https mirrors kista's own egress: it refuses the private and special ranges (RFC 1918,
CGNAT, link-local and with it the cloud metadata, loopback, ULA, documentation, multicast) unless
the configuration allows them, and reaches a public address only on 443. That covers the
upstreams, the issuers, the cloud APIs and KMSs, and public blob stores.

What the chart cannot tell from the configuration goes in `extraRules`: the database, a Vault or
OpenBao key source, an OpenTelemetry collector, an S3 in the cluster, a token service on a
private address. `helm install` refuses the policy without any `extraRules` when the store is
PostgreSQL or SQL Server, and without the `cloudMetadata` entry the configuration's identity
needs: `gke` for a `gcpkms` source, `gcs` domain or `gcp` database login without
`credentials_file` (GKE Workload Identity); `aws-pod-identity` for an AWS identity of kind
`pod_identity`; `imds` for kind `imds` or an Azure identity of kind `managed`. IRSA and Azure
workload identity need none: they reach STS and Entra ID on 443. A regional STS or KMS endpoint
behind a VPC endpoint has a private address: add it to `extraRules`.

```yaml
networkPolicy:
  egress:
    enabled: true
    extraRules:
      # the database
      - to: [{ipBlock: {cidr: 10.20.0.5/32}}]
        ports: [{port: 5432, protocol: TCP}]
      # OpenBao in the cluster, for a vault key source
      - to:
          - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: openbao}}
            podSelector: {matchLabels: {app.kubernetes.io/name: openbao}}
        ports: [{port: 8200, protocol: TCP}]
      # the OpenTelemetry collector
      - to:
          - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: observability}}
            podSelector: {matchLabels: {app.kubernetes.io/name: otel-collector}}
        ports: [{port: 4318, protocol: TCP}]
```

- **A peer in the cluster** is best named by its pods: most CNIs match a policy against the pod's
  address after the Service's translation, so an `ipBlock` with a Service's ClusterIP may match
  nothing.
- **DNS elsewhere.** On OpenShift:

  ```yaml
  networkPolicy:
    egress:
      dns:
        namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: openshift-dns}}
        podSelector: {matchLabels: {dns.operator.openshift.io/daemonset-dns: default}}
        ports: [5353]
  ```

  NodeLocal DNSCache answers on `169.254.20.10`: set `dns.enabled: false` and add a rule for it
  to `extraRules`. A `dns.namespaceSelector` without `dns.podSelector` is refused, as it would
  admit every pod there.
- **An empty rule** (`{}`) would admit everything, and is refused.
- **The probes need nothing**: the kubelet's requests are ingress, and their answers belong to
  them. Neither do image pulls, which the node makes.
- **IPv4 only.** `public.cidrs: [0.0.0.0/0]` drops the IPv6 block. A `cidr` that does not end in
  `/0` is taken as written, without `except`.

## The pods

- Non-root (uid and gid `65532`), a read-only root filesystem, every capability dropped,
  `seccompProfile: RuntimeDefault`, no privilege escalation.
- Probes on 8081: readiness `/readyz` every 5 s; liveness `/healthz` every 10 s, six failures; a
  startup probe that allows `probes.startupSeconds` (300) for the first migration.
- `/readyz` depends on the database: a database outage takes every pod out of the endpoints, on
  purpose.
- Shutdown: `preStop` sleeps 5 s so the endpoints drop the pod first, then kista drains.
  `terminationGracePeriodSeconds` is `preStop + drain_timeout + 2 × shutdown_timeout + 21 s +
  10 s`, 101 s with the defaults ([kista serve](../administration/commands.md#kista-serve)).
- The spool `emptyDir` at `/var/lib/kista/tmp`, also `TMPDIR`: `(2 × max_ingests + 1) × max_body`,
  or `spool.sizeLimit`, plus `spool.importHeadroom` ([Importing a bundle](#importing-a-bundle)).
  The pod requests as much ephemeral storage.
- Topology spread by zone and by node, `ScheduleAnyway`, so small clusters still schedule.
- Requests of 100m CPU and 128Mi memory, a 512Mi memory limit.

## Administration

The image has no shell; `kubectl exec` runs kista directly:

```bash
alias ka='kubectl -n kista exec -i deploy/kista -- /kista admin -config /etc/kista/kista.yaml'
ka tenant create acme
ka channel create acme/prod -kind signed
ka release add acme/prod - -name my_ext < my_ext.duckdb_extension   # stdin through exec -i
```

`adminJobs` runs recurring commands as CronJobs with the pods' configuration, Secrets and
identity. The CronJob is named `<release>-admin-<name>` (`kista-admin-blob-verify` for a release
named `kista`). An entry without a `schedule` is suspended; start it by hand with
`kubectl -n kista create job blob-verify-now --from=cronjob/kista-admin-blob-verify`. With SQLite,
or a `blobVolume` that is not `ReadWriteMany`, they are refused: a second pod cannot mount the
volume. Run the command through `kubectl exec` there.

```yaml
adminJobs:
  - {name: blob-verify, args: [blob, verify], schedule: "0 3 * * 0"}
```

### Importing a bundle

An air-gapped site imports bundles ([Air-gapped sites](air-gapped.md)). The import inflates one
file at a time into the spool, about `blob.max_body`, beside what the server's own ingests use.
The spool `emptyDir` is sized for the server alone, so give it the import's share with
`spool.importHeadroom`. The chart adds it to the volume's limit and to the pod's
ephemeral-storage request, also over an explicit `spool.sizeLimit`, which must then be a quantity
in bytes, `Ki`, `Mi`, `Gi` or `Ti`. A `resources.requests` entry for `ephemeral-storage` is kept
as written.

```yaml
spool:
  importHeadroom: 1Gi   # blob.max_body
```

- **In the serving pod**, the bundle on standard input:
  `kubectl -n kista exec -i deploy/kista -- /kista admin -config /etc/kista/kista.yaml bundle import acme - < acme.kbundle`.
  With SQLite, or a `blobVolume` that is not `ReadWriteMany`, this is the only way: a second pod
  cannot mount the volume.
- **As a Job**, wherever `adminJobs` are allowed: upload the bundle under the blob domain's
  `imports/` prefix (with the object store's own tools), then run a suspended `adminJobs` entry.
  The Job has the pods' configuration and identity and its own spool `emptyDir`, of the same size.
  The entry names one object: upload each bundle under that name, and remove it after the import.

```yaml
adminJobs:
  - {name: bundle-import, args: [bundle, import, acme, "blob:default/imports/acme.kbundle"]}
```

```bash
# the CronJob is <release>-admin-bundle-import: kista-admin-bundle-import for a release named kista
kubectl -n kista create job bundle-import-1 --from=cronjob/kista-admin-bundle-import
kubectl -n kista logs -f job/bundle-import-1
```

- **Uploaded over the API** ([Uploading a bundle](../upstreams.md#uploading-a-bundle)), a bundle is
  imported by a serving replica: its spool needs `spool.importHeadroom` too, and room for the
  chunks in flight (`blob.max_ingests × bundles.chunk_size`, each spooled until stored). Each chunk is one
  request of up to `bundles.chunk_size` (16 MiB by default): raise the ingress's body limit to it
  (ingress-nginx: `nginx.ingress.kubernetes.io/proxy-body-size: 16m`; its default is 1 MiB), or
  have the client ask for smaller chunks.

Every command in a pod runs as `os:65532:nonroot`, so the audit log cannot tell who ran it. **The
namespace's RBAC is the administration boundary**: whoever may exec into kista's pods, create Jobs
in its namespace or read its Secrets acts as kista's server administrator.

## Upgrading

`helm upgrade` with the new chart version and image digest rolls the pods one at a time. With
`store.migrate: auto` (the default) the first new pod applies the migrations under the store's
lock, then serves. A migration that raises `min_reader` makes the older pods unready as soon as it
commits; they leave the endpoints within about 15 seconds, while the new pod passes its probes, so
for a few seconds no pod may be ready. The rolling update then replaces the rest.

`store.migrate: check` is for pods whose database identity may not change the schema. A new pod
refuses to start until `kista admin migrate` has run from the new image with an identity that may;
run it right after `helm upgrade`. Once it commits a migration that raises `min_reader`, the old
pods turn unready. The order and the rules are in [Upgrading](../administration/upgrading.md).

## Hardening

- Pod Security Admission `restricted` on the namespace: the chart's pods meet it.
- Keep the NetworkPolicy on, with the ingress controller's pods named exactly.
- Restrict egress with `networkPolicy.egress` ([What kista may reach](#what-kista-may-reach)): it
  blocks `169.254.169.254` unless the identity needs it. On EKS, set the
  instance metadata hop limit to 1 so pods cannot use the node's role, unless kista's AWS
  identity is `kind: imds` (IRSA and Pod Identity do not use the instance metadata).
- Deploy by digest, and verify signatures at admission ([Deployment](index.md#verifying)).
- Limit who may `exec`, create Jobs and read Secrets in the namespace (above).
- `serve.resign` only on pods that may sign.
