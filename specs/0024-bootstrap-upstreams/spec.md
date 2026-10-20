# Spec 0024: The bootstrap's upstreams, and the chart in another namespace

- **Status**: implemented; released in v0.1.0 (2026-10-20)
- **Date**: 2026-10-20
- **Author**: vgsml, Claude

## Summary

Two things a packaged install needs from kista without anyone running `kista admin` in a pod:

- **Upstreams in the bootstrap.** The `bootstrap` section (spec 0020, 4a) gains `upstreams`, so a
  channel can be created with its mirrors in one step. The sources are DuckDB core and community,
  another kista (a public channel of hugr-lab's), or a private repository (an Enterest tenant)
  through a configured credential (spec 0009). They are made once, with the rest of the section.
  The bootstrap checks them before it writes anything, as it checks everything else.
- **`namespaceOverride` in the chart.** Every object the chart makes goes to that namespace instead
  of the release's, so an umbrella chart installs kista in a namespace of its own.

Then comes the first release, through the checklist the development page already has:

1. a release candidate, `v0.1.0-rc.1`;
2. then `v0.1.0`.

`release.yml` (spec 0013) publishes the image and the chart at `oci://ghcr.io/hugr-lab/charts/kista`.

## Problem

The hugr platform ships an umbrella chart, `hugr`, made of hugr-platform, tresor-server and kista.
It is meant for the providers' catalogues: IONOS, OTC, Hetzner and the Azure Marketplace.

- **First deployment.** It configures itself through a Job, `hugr-init`, with no `kubectl exec`.
- **Mirroring.** The kista in the cluster mirrors its channels, and the nodes reach only that kista.
- **Hosted tenants.** A hosted tenant (Enterest) is one of its upstreams. No node ever calls it.

What stands in the way today:

- **The bootstrap makes channels but not their upstreams.** A channel without an upstream is
  empty. Today someone runs `kista admin upstream add` in the pod for each one. A Job cannot do
  that, and the providers' catalogues have nobody to do it.
- **The chart installs everything in the release's namespace.** The umbrella's release namespace
  is the platform's, and that namespace is a trust boundary: whoever creates pods there can act as
  a node group. kista belongs in its own.
- **There is no release.** The repository has no tag, so the umbrella cannot pin a version of the
  image or the chart.

## Design

### The bootstrap's upstreams

**The section.** `bootstrap.upstreams` lists upstreams. Each carries the parameters of
`upstream add` (spec 0009), in mirror or pull-through mode:

```yaml
bootstrap:
  channels:
    - { name: prod, kind: signed, versions: [v1.4.1], key: "<signer reference>" }
  upstreams:
    - name: core                       # unique in the tenant
      kind: duckdb-core                # duckdb-core | duckdb-community | repository
      channel: prod                    # one of bootstrap.channels
      mode: mirror                     # mirror (default) | pull-through
      platforms: [linux_amd64, linux_arm64]
      extensions:
        - { name: httpfs }
        - { name: json, versions: [], allow_reserved: false }
      visibility: public               # public | private (default; public on a passthrough channel)
    - name: vendor
      kind: repository
      prefix: https://repo.example.com/vendor/prod
      keys: ["<fingerprint>"]          # pinned: at least one is in the upstream's .well-known
      channel: prod
      platforms: [linux_amd64]
      extensions: [{ name: acl }]
    - name: hosted
      kind: repository
      prefix: https://repo.example.com/acme/prod
      keys: ["<fingerprint>"]
      credential: hosted-acme          # one of upstreams.credentials (spec 0009, phase 3)
      channel: prod
      platforms: [linux_amd64]
      extensions: [{ name: tresor }]
```

Bundle mode (spec 0016) is not the bootstrap's: it takes its releases from files an operator
brings.

**The parsers.** The field checks of `upstream add` that were unexported in `internal/upstream`
(which imports `credential`, which imports `config`) move to a leaf package,
`internal/upstream/upspec`, which `config` can import:
- the prefix grammar and its normal form;
- fingerprints;
- entries: canonical names, aliases, reserved names, `"*"`;
- deduplication;
- a credential's prefix coverage (from `credential`).

Names and platforms keep their exported checks (`store.ValidName`, `release.ValidPlatform`).
`upstream add` and the configuration use the same functions. The rules about kinds, the passthrough
channel and credentials are written again at load, so a mistake stops the load; the bootstrap's
check runs `upstream add`'s own code again, so the two cannot drift into a write.

**Validation at load** (`validateBootstrap`):

- **Fields:** the name, the kind, the mode, the visibility and the platforms, by `upspec`.
- **The allowlist:** a mirror has at least one entry. The names are canonical (an alias is refused
  with its canonical name). A core name comes from a core upstream and a community name from a
  community upstream, unless the entry has `allow_reserved`. `"*"` appears only on a pull-through
  upstream, never on `duckdb-core`, and never with versions or `allow_reserved`.
- **A `repository` upstream** has a `prefix` and at least one key; the other kinds have neither.
  The prefix is checked in its normal form, and a port other than 443 must be one that egress
  allows (`egress` is in the same configuration).
- **`credential`**, on a `repository` upstream only, names one of `upstreams.credentials`, and that
  credential has to fit:
  - its `tenants` include `bootstrap.tenant.name` (or `*`);
  - its `prefixes` cover the normalised prefix;
  - `allow_public` is set when `visibility` is `public`.

  All of this is configuration, so a mistake stops the load, not the bootstrap.
- **`channel`** names a channel of `bootstrap.channels`, as a grant's scope does.
- **A passthrough channel** takes only `duckdb-core` upstreams, and only in mirror mode. Their
  visibility is `public`: an explicit `private` is refused.
- **Nothing is listed twice:**
  - upstream names;
  - an extension within an upstream;
  - an extension name on two upstreams of one channel.

  A platform, a key or a version listed twice is deduplicated, as `upstream add` deduplicates it.
- **The limits of spec 0009:** at most 100 upstreams, 1,000 entries per upstream, 100 versions per
  entry, 10 keys, and the cells of the section's channels (their `versions` times the platforms
  times the entries, `"*"` counted as one, as the store counts it).
- **`bootstrap.tenant`** is required when `upstreams` is set, as it is for channels and grants.

**Defaults** (`Defaulted`, so the digest is stable):
- `mode` is `mirror`;
- `visibility` is `public` on a passthrough channel and `private` otherwise;
- keys, platforms and entry versions are deduplicated and sorted;
- prefixes are normalised.

**The check, before any write.** The upstream service gains two read-only checks:

- **`CheckSource(tenant, spec, channel kind)`** makes `Add`'s checks of the upstream itself and reads
  nothing of the store: its fields, the passthrough rules, its credential, and the source's
  `.well-known` with a pinned key among its keys. The bootstrap uses it for an upstream of a channel
  it is to make (a first install): a new channel has no other upstream, no replacement and no cells,
  and the section's own collisions and limits were checked at load.
- **`CheckAddAll(tenant, specs)`**, for the upstreams of channels that exist, makes every check `Add` makes:

- the `.well-known` read through egress, with a pinned key required among its keys;
- an existing upstream of that name;
- the tenant's count of upstreams and of cells;
- a name that another upstream of the channel lists (`ChannelEntryOwner`);
- a live replacement of that name in the channel (`LiveReplacement`);
- the passthrough and credential rules again (they can also fail against what exists).

`CheckAddAll` is `Add`'s own transaction rolled back, with every upstream of the section that goes
into a channel that exists inserted in turn, so two of the section's that collide, or that cross a
limit together, are refused before any write; the rules stay in one place. In an adopted tenant,
the tenant's count of upstreams is checked with the section's new ones. Its cells are not summed
with the new channels' (the section's channels' versions are added during the apply): a section
that crosses the tenant's cell limit only so is refused by `Add` in the apply, and the record stays
pending. A tenant the bootstrap did not make, without `-adopt`, is refused whatever its upstreams
are, so their sources are not read then: the refusal is the answer, not a wait on the network.
For each upstream the bootstrap gets one plan item:

| Object | The same | Different (refused) |
| --- | --- | --- |
| upstream (by name) | the same kind, normalised prefix, channel, mode, visibility and credential; its platforms and keys include the section's; for each entry, an entry of that name with the same versions (empty means any) and the same `allow_reserved`. More than the section lists is the administrator's. | any of those fields differs; a key, a platform or an entry is missing or different; a collision, a replacement, a limit, or a `.well-known` without a pinned key |

An absent upstream is to be created. There is no "add" action: `Add` makes an upstream with its
allowlist, platforms and keys in one transaction, so a run that resumes never finds half an
upstream. The adoption rules of spec 0020 apply unchanged: the bootstrap creates nothing in a
tenant it did not make, except under `admin bootstrap -adopt`.

**Retries.** A `.well-known` that cannot be read for now is a new sentinel,
`upstream.ErrSourceFetch`. It is added to the bootstrap's `transient` set, so the check is tried
again with backoff for its four minutes, as it is for a signer or an issuer. The failures that
count are:
- a connection failure;
- a timeout;
- a 5xx or 429 answer.

These are refusals, and are not retried:
- an address egress refuses;
- any other 4xx (a 404 at an Enterest tenant not provisioned yet included: the order of the
  install puts the tenant first);
- a file without a pinned key.

`-dry-run` reads the network too, and says so.

**Apply.** The upstreams become step 10 of spec 0020's apply, after the grants. Each goes through
`Upstreams.Add` as `system:bootstrap`, with its usual events: `upstream.add`, and `shadow.add` for
a first passthrough upstream. `serve` and the CLI build the services with `authz.ServerAdmin`,
which admits `system:bootstrap` (spec 0020). `bootstrap.Deps` gains the upstream service.

**The first run.** A new mirror is due at once: the upstream workers list a mirror whose
`next_run_at` is not set. The bootstrap runs before the listeners and the workers start, so the
first run starts at the workers' first poll, on whichever replica takes its lease. The bootstrap
does not ask for a run.

**The credential's token** is not fetched by the bootstrap. `Add` checks the credential's name and
rules from configuration, and the token is first fetched by a run. A credential that cannot get a
token shows in that run's status and events (spec 0009). The bootstrap cannot see it, because the
upstream is then already made.

**The digest.** `Bootstrap` gains `Upstreams []BootstrapUpstream` with
`json:"upstreams,omitempty"`. A section without upstreams keeps the digest it had, so upgrading
kista changes nothing for an installation already bootstrapped.

A bootstrap applied before this spec, whose section now gains upstreams, gets spec 0020's warning
("the section changed after it was applied"), and nothing more.

**Upstreams after the first start.** An Enterest subscription bought later is added through the
API or `kista admin upstream add`, with a server or tenant administrator's token. The bootstrap is
not a reconciler (spec 0020). `admin bootstrap -rearm` followed by `-adopt` would make the
upstreams, but it also restores grants revoked since. Run `-dry-run -adopt` first, or use
`upstream add`.

**Egress at startup.** In a cluster that denies egress by default, the bootstrap's reads of
`.well-known` need the upstreams' hosts on 443, both in `egress.allow` and in the NetworkPolicy.
Without them `serve` retries for its four minutes and exits. The docs say so beside the bootstrap
and the chart.

### The chart's namespace

- **`namespaceOverride`.** It defaults to empty: the release's namespace. When set, every object
  the chart makes takes `metadata.namespace` from it. The chart's objects are all namespaced:
  - the Deployment;
  - the Service;
  - the ConfigMap;
  - the ServiceAccount;
  - the PodDisruptionBudget;
  - the NetworkPolicy;
  - the PersistentVolumeClaims;
  - the admin CronJobs;
  - the Ingress;
  - the GKE objects.

  One helper, `kista.namespace`, decides. It is rendered always, so `helm template | kubectl apply`
  puts each object where it belongs. A render without the override now also names the release's
  namespace, which is the same place. NOTES.txt uses the helper too.
- **Its form.** The value is a DNS-1123 label, checked by the values schema.
- **No namespace object.** The chart creates no namespace; the umbrella (or the operator) creates
  it, with its labels: Pod Security Admission and the NetworkPolicy selectors. If the umbrella
  creates the namespace inside its own release, `helm uninstall` deletes it, and with it a SQLite
  or file-blob volume. The docs say to keep such volumes (`helm.sh/resource-policy: keep`) or
  create the namespace outside the release.
- **What must live there too.** Everything a pod or the Ingress refers to: `tlsSecret`,
  `secretVolumes`, `imagePullSecrets`, the Secrets of `envFrom`, `existingClaim`, the Ingress's TLS
  Secrets.
- **Cloud identities name the namespace.** The IRSA trust's `sub`, the Azure federated credential's
  subject and the GKE Workload Identity member name it, so they name the override.
- **The NetworkPolicy.** It admits the configured ingress controller's pods and the load balancers.
  A `podSelector` without a `namespaceSelector` selects in the policy's own namespace, which is now
  kista's: an ingress controller elsewhere needs its `namespaceSelector`. Nodes reach kista through
  the ingress, at `serve.public_url`. They do not call its Service from the platform's namespace,
  which the policy refuses.

### The first release (after the code)

Once this spec is implemented and merged, the development page's "Before the first release" is
done, in order:

1. The packages are public and linked: `ghcr.io/hugr-lab/kista` now, the chart's after its first
   push.
2. A tag ruleset for `v*` is in place.
3. `v0.1.0-rc.1` is cut and verified as Deployment shows.
4. `v0.1.0` is cut. `release.yml` sets the chart's `version` and `appVersion` from the tag.
5. Right after the first tag that is not a prerelease, the air-gapped workflow is dispatched: its
   release blocks run then for the first time.

Release notes are generated from the merged pull requests (`--generate-notes`); there is no
CHANGELOG. The docs' "until the first release" text is updated with the tag:
- index.md;
- deployment/index.md;
- gcp.md;
- aws.md;
- air-gapped.md;
- development.md.

## Security

- **No new authority.** The bootstrap acts as `system:bootstrap` through the upstream service, which
  makes the checks `upstream add` makes. Credentials are configuration (spec 0009): the section
  names one and holds no secret, and a credential is used only for the tenants and prefixes it
  names. A key is pinned by its fingerprint; nothing from an upstream is trusted unless a pinned key
  signed it.
- **Egress** is unchanged: the `.well-known` reads at startup and the runs go through the egress
  allowlist.
- **The namespace** moves objects, not rights. The ServiceAccount and the pods move together, and
  the chart grants nothing cluster-wide. The cloud identity bindings name the new namespace, so
  they bind the same pods.

## Testing

- **upspec:** the moved checks keep their tests; `upstream add` and the bootstrap's validation share
  them.
- **Config:** every refusal above (including each credential rule and the passthrough rules), a
  section with upstreams of each kind accepted, and the digest of a section without upstreams
  unchanged by the new field.
- **Upstreams.CheckAddAll:**
  - each refusal `Add` makes, found without a write;
  - `Add` refusing what `CheckAddAll` refuses;
  - a `.well-known` served 503 and then 200 is `ErrSourceFetch`, then fine;
  - a 404 is a refusal.
- **The bootstrap**, on the store suite's engines, with a test upstream (an httptest kista with
  loopback egress):
  - upstreams made after the grants, with their events, and due for a first run;
  - "the same" on a second start;
  - a refusal before any write: a missing key, a different allowlist, a collision;
  - an unreachable `.well-known` retried, then converging;
  - `-dry-run` listing the upstreams;
  - adoption refused without `-adopt`.
- **The end to end** (`e2e/`): a kista bootstrapped with a repository upstream at a second kista.
  Its first run mirrors a release, and DuckDB installs it from the first kista. The kind test is
  unchanged: it reaches no upstream.
- **The chart:**
  - `deploy/helm/chartcheck`: every rendered document is in the override's namespace with it, and
    in the release's without it;
  - a `tests/fail` case for an override that is not a label;
  - the existing renders still pass.

## Docs

- **Configuration:** `bootstrap.upstreams`, the credential rules, and egress at startup.
- **Upstreams:** the bootstrap as a way to make upstreams, and the day-2 note.
- **Kubernetes:** `namespaceOverride`, the objects that must live there, the identity bindings, the
  NetworkPolicy's `namespaceSelector`, and keeping volumes when the namespace is the umbrella's.
- **Development and deployment:** the release, as above.
- **Specs:** spec 0020's bootstrap section points here, and specs/README.md lists this spec.

## Alternatives considered

- **An init container that runs `kista admin`.** It works today, but each provider's template would
  carry a script against a CLI, with its own retries and no lease. The bootstrap already applies
  once, converges and checks.
- **A reconciler for upstreams.** The bootstrap is not one (spec 0020): it makes a first state, and
  the API owns it afterwards.
- **A `sync` option.** A new mirror is due at once, and pull-through and bundle upstreams cannot be
  asked to run. An option would add nothing but a way to fail.
- **Only `.Release.Namespace`.** The umbrella's objects and kista's would share one namespace and
  its trust boundary.

## Follow-ups

- Serving ADBC drivers the way extensions are served (a spec of its own, after the release).
