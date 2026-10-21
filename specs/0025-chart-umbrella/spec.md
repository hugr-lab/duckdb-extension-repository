# Spec 0025: The chart in an umbrella: an egress NetworkPolicy and a templated config

- **Status**: implemented
- **Date**: 2026-10-21
- **Author**: vgsml, Claude

## Summary

Two opt-ins to kista's Helm chart (spec 0013) that the hugr umbrella chart, `hugr`, asked for. It
installs kista from `oci://ghcr.io/hugr-lab/charts/kista` in a namespace of its own (spec 0024).

- **`networkPolicy.egress`**: a second NetworkPolicy that lets kista's pods reach only DNS,
  public https, the private networks kista's configuration allows, the cloud identity's metadata
  endpoint, and the operator's own rules.
- **`configTemplate`**: the strings of `config` that hold `{{` are rendered by Helm's `tpl`, so a
  parent chart can fill kista's configuration from its globals.

Both are off by default; an existing values file renders exactly as before.

## Problem

- **Egress.** The chart's NetworkPolicy covers ingress only. A cluster that denies egress by
  default (the providers' catalogues: IONOS, OTC, Hetzner, STACKIT, the Azure Marketplace) needs a
  policy for kista's pods. Today each umbrella writes one. It has to know what kista reaches: DNS,
  the upstreams' and issuers' https, the database, a key source, a collector, a metadata
  endpoint. Get it wrong, and kista fails at start (the bootstrap reads its upstreams' `.well-known`
  before serving) or, worse, much later (a cloud identity's token refresh). Where nobody writes one,
  a compromised pod reaches everything in the cluster's networks, cloud metadata included.
- **Templated config.** An umbrella knows the database's host, kista's public host and its
  namespace as globals. The chart takes `config` literally, so the umbrella repeats each value in
  `kista.config`, or renders kista's configuration through a values file of its own making.

## Design

### The egress NetworkPolicy

**Values** (`networkPolicy.egress`, schema-checked):

```yaml
networkPolicy:
  egress:
    enabled: false              # off by default; apart from networkPolicy.enabled (ingress)
    dns:
      enabled: true
      namespaceSelector: {}     # both empty: k8s-app: kube-dns in kube-system
      podSelector: {}
      ports: [53, 8053]         # UDP and TCP each
      ipBlocks: []              # DNS that is no pod: {cidr, ports (default: ports)}
    public:
      enabled: true
      ports: [443]
      cidrs: [0.0.0.0/0, "::/0"]
      except: [<kista's refused ranges, and their NAT64 forms>]
    cloudMetadata: []           # gke | aws-pod-identity | imds
    extraRules: []              # NetworkPolicy egress rules, rendered as given
```

**The object.** `<fullname>-egress`, `policyTypes: [Egress]`. Its `podSelector` is the release's
selector labels without `app.kubernetes.io/component`, so it selects the server's pods and the
`adminJobs`' pods alike: an admin job reaches the same store, blobs and key sources. An empty list of
rules renders `egress: []`, which denies all.

**The rules, in order:**

1. **DNS**: UDP and TCP on `dns.ports` to the DNS pods.
   - **The default peer.** With both selectors empty, it is `k8s-app: kube-dns` in the namespace
     labelled `kubernetes.io/metadata.name: kube-system`. That label is the same on EKS, AKS, k3s,
     kind, GKE with kube-dns and Gardener, for kube-dns and CoreDNS alike. The default is applied in
     the template, not in `values.yaml`: Helm merges maps, so a default selector in `values.yaml`
     would be merged with the operator's (OpenShift's) and match nothing.
   - **The ports.** CNIs match a policy after the kube-dns Service's translation, so the port is the
     pods' own: 53 mostly, 8053 on Gardener (STACKIT SKE), 5353 on OpenShift. The default `[53,
     8053]` admits both common ones, to the DNS pods only.
   - **`dns.ipBlocks`** is for DNS that is no pod:
     - NodeLocal DNSCache: `169.254.20.10`, and in iptables mode the kube-dns ClusterIP itself,
       which the cache answers on untranslated;
     - GKE Cloud DNS: `169.254.20.10` on Autopilot, `169.254.169.254` port 53 in cluster scope.

     Each entry takes `ports`, by default `dns.ports`.
   - **Refused**: a `podSelector` that selects every pod. That means none beside a
     `namespaceSelector`, or one without `matchLabels` and `matchExpressions`
     (`{matchLabels: {}}`).
2. **Public https**: TCP on `public.ports` (443) to each of `public.cidrs`. A cidr ending in `/0`
   gets `except`: the entries of `public.except` of its family.
   - **The default `except`** is kista's own refused ranges (`internal/egress`): the IANA
     special-purpose ranges, RFC 1918, CGNAT, link-local, ULA, multicast, and Azure's
     `168.63.129.16`. kista itself reaches a public address only on 443, and these ranges only
     when the configuration allows them.
   - **IPv6.** The `::/0` block also excepts:
     - the NAT64 forms under `64:ff9b::/96` of every refused IPv4 range. kista judges such an
       address by the IPv4 address inside (`embedded` in `internal/egress`), so on an IPv6-only
       cluster with DNS64 the policy would otherwise admit `64:ff9b::a00:1`, the NAT64 form of
       `10.0.0.1`;
     - `::/96`, the IPv4-compatible addresses.
   - **One range is left out**: `::ffff:0:0/96`, the IPv4-mapped addresses. They never go on the
     wire, and an API server with strict CIDR validation refuses them.
   - **A narrower cidr** is taken as written, without `except`: an `except` outside its cidr is
     refused by the API server.
3. **What the configuration allows**: each entry with a `cidr` in the rendered configuration
   (`egress.allow`, `auth.server_egress_allow`, `upstreams.credentials[].allow`,
   `events.sinks[].allow`), with its ports or 443. The chart walks the configuration, as it does for
   identities.
   - **The ranges kista never reaches**, even when allowed, are left out: link-local, Azure's
     `168.63.129.16`, `100.100.100.200`, `fd00:ec2::/112` (`never` in `internal/egress`). A cidr
     that contains one gets it as `except`, so an allowed `0.0.0.0/0` does not open the metadata
     endpoints. A cidr inside one gets no rule.
   - **Containment** is computed in the template on the addresses' bits, for IPv4 and for IPv6
     written in hex. An IPv6 cidr with a dotted IPv4 tail is taken as written.
   - **Services.** An entry may name a Service's ClusterIP or a LoadBalancer IP that kista dials.
     The policy matches after the translation, so the docs say to add an `extraRules` entry
     naming the pods behind it.
4. **The proxy**: `egress.proxy` on its port, as a `/32` or `/128`, when its host is an IP address
   (and not inside a never-reached range). A proxy named by host is refused without any
   `extraRules` (below). An acknowledgment value was the alternative, but it would only repeat what
   an `extraRules` entry for the proxy says.
5. **Cloud metadata**, by name:
   - `gke`: `169.254.169.254/32:80` and `169.254.169.252/32:988`, the GKE metadata server as GKE's
     documentation lists it for network policies;
   - `aws-pod-identity`: `169.254.170.23/32:80` and `fd00:ec2::23/128:80`, the EKS Pod Identity
     agent;
   - `imds`: `169.254.169.254/32:80`, EC2's instance metadata and Azure's managed identity.
6. **`extraRules`**, as given: the database, a Vault or OpenBao key source, an OpenTelemetry
   collector, an S3 in the cluster, a token service, a proxy named by host. The schema makes each
   rule name its peers: `to` is required, with at least one peer, and no peer may be empty. `ports`,
   when present, has at least one entry. A rule without `to`, or with an empty `to` or `ports`,
   admits every peer or every port, `169.254.169.254:80` included.

**Refusals** (`kista.validate`, with `egress.enabled`), each naming the value to set. The first two
are a guard against forgetting, not a proof: any `extraRules` entry satisfies them, since the chart
cannot tell what a rule is for.

- **The database.** The store is not SQLite and `extraRules` is empty. The chart cannot read a host
  name, and kista's DSN names one, so it cannot write the rule. Without one, kista never becomes
  ready.
- **A proxy named by host**, with `extraRules` empty.
- **The metadata endpoint.** The configuration needs one that `cloudMetadata` lacks:
  - `gke`, when a `gcpkms` source, a `gcs` domain or the `gcp` login has no `credentials_file`
    (the principals the chart already matches with `gcp.serviceAccount`);
  - `aws-pod-identity`, for an identity of kind `pod_identity`;
  - `imds`, for an identity of kind `imds` or `managed`.

  IRSA and Azure workload identity need none: they exchange a projected token at STS or Entra ID on
  443.
- **A DNS selector that selects every pod**, as above.

**What needs nothing.** The probes: the kubelet's requests are ingress, and the answers belong to
those connections. Image pulls: the node makes them. The Kubernetes API: kista mounts no service
account token (spec 0013).

### `configTemplate`

`configTemplate: true` renders each **string** of `config` that holds `{{` with `tpl` and the
chart's root context (`.Values`, `.Values.global`, `.Release`, `.Chart`). Every read of the
configuration in the chart goes through one helper, `kista.userConfig`, so the checks, the
ConfigMap and the `checksum/config` annotation all see the rendered configuration.

**Per string, not the whole document.** `tpl (toYaml .Values.config) .` would be simpler. But a
value would then be spliced into YAML, and could add keys: a global holding
`"db\nprofile: dev"` would turn the dev profile on, and one holding a flow mapping could add an
`egress.allow`. Rendering each string keeps every template inside the string that holds it, so
the configuration's structure is the one written in the values. The cost: only strings can be
templated, not numbers, booleans or lists. kista's settings that an umbrella derives from its
globals are strings: DSNs, URLs, host names, paths, CIDRs.

**Once per run.** The rendered configuration is computed once per `helm template`, `install` or
`upgrade`, and kept on the root context (`kistaRenderedConfig`), which Helm shares between a
chart's template files (confirmed on Helm 3.22 and 4.1). Every read, from the ConfigMap, the
checksums and the checks, gets that one result, so a function like `randAlphaNum` cannot give
the ConfigMap and the checksum different values. Such functions would still change the
configuration, and roll the pods, at every upgrade. The docs say config templates must be
deterministic.

**Values only.** Before rendering, each `{{ … }}` action in a string is checked for `lookup`, `tpl`,
`include`, `template` and `getHostByName`. A match fails the install, naming the setting:

- `lookup` runs at install with the installer's credentials, and could copy a Secret into the
  ConfigMap;
- `tpl`, `include` and `template` reach further templates;
- `getHostByName` makes the configuration depend on DNS at install.

**Missing values.** Helm's `tpl` renders a missing leaf value as nothing, and reading through a
missing map (`.Values.global.nope.host`) fails. A string that renders entirely empty fails the
install, naming the setting. A missing value inside a longer string (a DSN without its host) is
not detectable in general, so the docs use `required` in every example.

**Off by default**, so a configuration with a literal `{{` keeps working. On, a literal `{{` is
written `{{ "{{" }}`. A rendered string is never rendered again.

**Only `config`.** The identity values (`azure.workloadIdentity.clientId`, `aws.irsa.roleArn`,
`gcp.serviceAccount`) are not templated. An umbrella writes them as literals, equal to what the
rendered configuration names, and the chart checks them against it as before.

**The secret rule holds.** The chart has no check of its own for secret-looking keys. kista refuses
a password in a DSN, or a token inline, when it loads the configuration. That configuration is the
rendered ConfigMap, so the refusal applies to what the templates produce, as it does to text written
out. The chart's own checks (listeners, SQLite paths, identities, the egress policy) run on the
rendered configuration too.

## Security

- **Egress is fail-closed when on.** Only the listed rules pass. The defaults mirror what kista's
  own egress client allows, so the policy adds a second, independent layer under it: a
  compromised pod cannot reach the cluster's private ranges (nor their NAT64 forms), the cloud
  metadata endpoints, or a public address on another port. The metadata endpoints open only by
  name, and the chart insists on the name only where the configuration uses that identity. An
  allowed network never opens what kista itself never reaches.
- **No rule admits everything by accident.** Rules without peers, with empty peers or with empty
  ports are refused, and so is a DNS selector that selects every pod.
- **Refusals at install, not at runtime.** A missing database or proxy rule, or a missing metadata
  endpoint, fails `helm install` with the value to set. The chart cannot see every need, such as a
  private KMS or STS endpoint or an in-cluster S3. The documentation lists them.
- **Off by default.** Existing installs render the same objects. The ingress policy is unchanged.
- **`configTemplate` cannot change the configuration's structure, nor read the cluster.** It
  changes only the strings it renders, from values, `.Release` and `.Chart`. `lookup` and the other
  functions that reach beyond the values are refused, so templating cannot copy a Secret the
  installer can read into a ConfigMap. kista and the chart check the result.

## Testing

- **`deploy/helm/test.sh`** (helm lint `--strict` and kubeconform on each pass fixture, and the
  expected refusal on each fail fixture), run with Helm 3.22 and 4.1:
  - pass fixtures:
    - `egress.yaml`: EKS Pod Identity, a database and an OpenBao in the cluster, an OTel
      collector, `egress.allow` with IPv4 and IPv6 entries, an admin job;
    - `egress-sqlite.yaml`: SQLite, NodeLocal DNSCache in `dns.ipBlocks`, a proxy at an IP
      address, IPv4 only;
    - `config-template.yaml`: globals in the DSN, `public_url` and `trusted_proxies`, and
      `.Release.Name` in a bucket;
  - fail fixtures:
    - `np-egress-no-db`: PostgreSQL without a rule;
    - `np-egress-proxy-host`: a proxy named by host;
    - `np-egress-dns-pods`: a `namespaceSelector` alone;
    - `np-egress-dns-all-pods`: `podSelector: {matchLabels: {}}`;
    - `np-egress-gke` and `np-egress-imds`: a metadata endpoint missing from `cloudMetadata`;
    - `np-egress-empty-rule` (`{}`), `np-egress-rule-no-to`, `np-egress-rule-empty-to`,
      `np-egress-rule-empty-ports`, `np-egress-rule-empty-peer`;
    - `config-template-missing`: reading through a missing map;
    - `config-template-empty`: a missing leaf;
    - `config-template-lookup`: `lookup` in a DSN;
    - `config-template-checked`: a templated SQLite path that the chart's check refuses after
      rendering.
- **`deploy/helm/chartcheck`** (Go, with helm):
  - `TestRenderedConfig`: kista loads the configuration each pass fixture renders, the templated
    one included.
  - `TestEgressPolicy`:
    - no egress policy by default;
    - on, the policy selects the Deployment's and the CronJob's pods, has
      `policyTypes [Egress]`, and admits exactly the expected peer and port pairs, in order;
    - the ingress policy keeps `policyTypes [Ingress]`;
    - every range `internal/egress` refuses (`egress.Refused()`), and the NAT64 form of each IPv4
      one, is covered by the default `except` in its own family, so the two cannot drift apart.
  - `TestEgressAllowed`:
    - allowed `0.0.0.0/0` and `::/0` carry exactly `egress.Never()` as `except`;
    - a wider cidr excepts the never-reached ranges inside it;
    - a cidr inside one gets no rule;
    - the proxy's IPv6 address gets its rule;
    - the DNS `ipBlocks` render with their ports.
  - `TestConfigTemplate`:
    - on, the templates are rendered;
    - off, the same strings stay literal;
    - a global with newlines and YAML adds no key (`profile`, `egress`) and stays in the DSN;
    - `{{ "{{" }}` renders a literal `{{`.
  - `TestConfigTemplateOnce`: with `randAlphaNum` in a bucket, the Deployment's and the CronJob's
    `checksum/config` equal the ConfigMap's content's hash.
- **kind** (`deploy/helm/kind/run.sh`): unchanged here. A live run with `networkPolicy.egress` on
  is left for the next bench run. The rules it needs: the e2e namespace's PostgreSQL, SeaweedFS and
  OpenBao pods, and the API server at the control-plane node's address on 6443. That address is
  not the `kubernetes` Service's ClusterIP, which kista's `egress.allow` names: CNIs match a
  policy after the Service's translation.

## Alternatives considered

- **Egress inside the existing NetworkPolicy object.** That object selects only the server's
  pods and exists only with the ingress policy on (by default with `behind_proxy` only). A separate
  object covers the admin jobs and works with `mode: tls` or the ingress policy off.
- **Detecting every metadata endpoint automatically.** The endpoints differ by platform and
  version (GKE's moved between releases), and Azure's managed identity can be implicit. Naming
  them, and refusing when the configuration clearly needs one, is explicit and still catches the
  common mistake.
- **Resolving the database's host into a rule.** Helm cannot resolve names, and an address that
  changes on failover would break the policy silently.
- **Whole-document `tpl`.** Rejected above: a value could add keys.
- **`tpl` always on.** It would break any configuration with a literal `{{`, and would surprise
  operators who do not use an umbrella.
- **Rendering the config templates in every read.** Simpler, but each string would render dozens of
  times per run, and a non-deterministic function would give the ConfigMap and its checksum
  different values.
- **Resolving the proxy's or the database's host.** Helm cannot resolve names at template time
  (`getHostByName` resolves on the installer's machine, which may see other addresses).

## Follow-ups

- The kind bench with `networkPolicy.egress` on, and a probe that a pod of the release cannot reach
  a private address outside the rules.
- FQDN-based egress (Cilium's `CiliumNetworkPolicy`, Calico's `NetworkSet` domains) for the database
  and the upstreams, as a separate opt-in, if a platform asks.
