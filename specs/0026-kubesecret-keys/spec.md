# Spec 0026: Signing keys from a Kubernetes Secret, as an explicit opt-in

- **Status**: implemented (2026-10-21), but for the kind case (Testing), which is to come
- **Date**: 2026-10-21
- **Author**: vgsml, Claude

## Summary

A new key source kind, `kubesecret`, signs with RSA keys held in a Kubernetes Secret and mounted
into kista's pods. Its root of trust is the cluster's: etcd encryption at rest, and RBAC on Secrets
in kista's namespace. It is meant for clouds where kista supports no KMS (OTC, Hetzner, IONOS,
STACKIT) and no Vault or OpenBao is available. It is opt-in at three levels:

- the source is configured;
- it carries a required acknowledgment;
- kista checks at open that the files really come from a Secret volume.

The file signer's rules stay as they are.

## Problem

- **What kista refuses today.** A channel key in a Kubernetes Secret is refused (spec 0013, "Not
  unified"; [Signing keys](../../website/docs/signing-keys.md#file-keys)). The `file` signer
  (`internal/signer/file.go`, `checkKeyFile`) wants:
  - a regular file opened with `O_NOFOLLOW`, never a link;
  - owned by kista's uid;
  - mode `0600` or narrower.

  A Secret volume fails all three. Its files are links (`<name> -> ..data/<name>`, `..data ->
  ..<timestamp>`), owned by root, group `fsGroup`, mode `0440`.
- **Who needs it.** The hugr umbrella chart targets the providers' catalogues. On OTC, Hetzner,
  IONOS and STACKIT kista has no KMS backend: spec 0004 covers Azure Key Vault, AWS KMS, Google
  Cloud KMS, and Vault or OpenBao Transit. The umbrella's options today:
  - an external Vault or OpenBao, which many of these customers do not run;
  - an OpenBao inside the cluster, whose unseal needs a KMS or an operator at every restart;
  - `signers.allow_file` with a key written by an init container into an `emptyDir` owned by
    kista: this works, but disguises a Secret as a file key, and loses every check this spec adds.
- **What they accept.** A Secret's protection is the cluster's: whoever reads Secrets in the
  namespace, or etcd's disks unencrypted, has the key. Some operators accept that for their own
  channels, when it is stated plainly and chosen explicitly.

## Design

### Configuration

```yaml
signers:
  sources:
    - name: k8s
      kind: kubesecret
      allow: ["ext-"]                       # key-name prefixes, as for every source
      kubesecret:
        dir: /etc/kista/signing/prod        # the Secret volume's mountPath
        etcd_encryption_at_rest: true       # required: the acknowledgment (below)
```

- **`kubesecret`** joins the source kinds and the reserved source names.
- **`dir`** must be absolute, clean, and not under `signers.file_dir` or any other configured
  secret path.
- **`etcd_encryption_at_rest: true`** is required outside `profile: dev`, like `vault`'s
  `software_keys: true`. It is the operator's statement that the cluster encrypts Secrets at rest.
  kista cannot check it. It is logged at every start.
- **References** are `<source>:<name>`, which open `<dir>/<name>`:
  - the name matches `[A-Za-z0-9_-]{1,63}`: a Secret data key without dots, so never `..data` or
    a timestamped directory;
  - the name passes `allow`.

  The name is the version: a key is never replaced under the same name (see Rotation).

### What kista checks at open

The point is to accept exactly a Secret volume, and refuse a look-alike made by mistake:

1. **The mount** (Linux only; other platforms refuse the kind). `/proc/self/mountinfo` shows
   `dir` as a mount point, `tmpfs`, mounted read-only (`ro`). The kubelet mounts Secret volumes, and
   projected volumes with Secret sources, as read-only tmpfs. A ConfigMap volume (on the node's
   disk), a `hostPath`, a PVC or a writable `emptyDir` with `medium: Memory` are refused.
2. **The layout**: the kubelet's atomic writer.
   - `<dir>/..data` is a relative link to a `..<timestamp>` directory inside `dir`.
   - `<dir>/<name>` is the relative link `..data/<name>`.
   - The file is opened through `os.OpenRoot(dir)`, as `internal/secretfile` opens Secrets, so
     no link may leave `dir`.
3. **The file**, by the secret rule of spec 0013, shape (b):
   - a regular file with one hard link;
   - owned by root, group kista's egid (not 0);
   - mode at most `0440`: the chart's `defaultMode`, or `0400` under `fsGroup`, which the kubelet
     makes `0440`.

   Shape (a) is not accepted: a file owned by kista's uid is not a Secret volume's.
4. **The key**: one unencrypted PKCS#8 or PKCS#1 RSA key, 2048 bits, `e = 65537`, at most 64 KiB,
   as the file signer parses it. Then the common contract of spec 0004: the known answer, and
   the public key and fingerprint against the stored row (`OpenSigner`).

Checks 1 and 2 detect a misconfiguration; they are no defence against an attacker. Whoever
controls the pod spec can mount any Secret, and a pod that can write its own mounts has won
already.

### While in use

- **Re-checked every `recheck`** (default and maximum 10 minutes), and at every revalidation
  (spec 0003): the file is read again with every check.
  - A different key under the same name fails closed: the fingerprint no longer matches the
    stored row.
  - A file that is gone fails closed too.
- **The key in memory**: the parsed key stays in kista's memory, as a file key does. It is never
  logged, never in an error, never written anywhere.
- **Metrics and readiness** as for every source: `kind="kubesecret"`.

### Making it visible

- **At every start**, a warning naming each `kubesecret` source and its `dir`: its keys are
  protected by the cluster's etcd encryption and RBAC, not by an HSM or a KMS.
- **`kista admin key check`** and the management API show the protection tier `kubesecret`. So
  does `key list`.
- **Audit events** (spec 0010): `key.add` with a `kubesecret` signer carries `protection:
  kubesecret`. A start with such a source records `signer.source.weak` once per source, so an
  auditor finds it without reading the configuration.
- **The public key set** (`.well-known`) does not show it. Clients trust the channel's key; how it
  is stored is the operator's business.

### Rotation

kista's rotation (trusted, then active, then retired; spec 0003) is unchanged:

1. Add a key under a **new** name, in the same Secret or another one mounted in the source's `dir`
   (the source reads one directory: one Secret volume per source).
2. `kista admin key add acme/prod -signer k8s:ext-prod-2027`, and promote it after
   `rotation.min_trusted`.
3. Remove the old data key only once it is retired. A reference that no longer resolves fails
   closed, but a retired key signs nothing.

Rewriting a key under the same name is refused at the next check, by the fingerprint. The docs say
never to do it. There is no rotation beyond this: no versions, no automatic rotation, no
non-exportable keys.

### The chart

- **The mount** uses the existing `secretVolumes` (mode `0440`, group `65532`): the source's `dir` is
  its `mountPath`.
- **`kista.validate` refuses** a `kubesecret` source whose `dir` is not a `secretVolumes` mount.
- **`NOTES.txt`** repeats the start warning.
- **The docs** replace "kista refuses a key file from a Kubernetes Secret" with this opt-in and its
  warnings.

### What the documentation must say

Prominently, on [Signing keys](../../website/docs/signing-keys.md) and the Kubernetes page:

- **Etcd encryption at rest is required.** Self-managed: an `EncryptionConfiguration`, ideally with
  a KMS provider; k3s: `--secrets-encryption`. Managed: check what the provider does; it is often
  not stated.
- **RBAC on Secrets in kista's namespace is access to the key.** That covers `get`, `list` and
  `watch` on Secrets, and the right to create pods, Jobs or CronJobs there (a pod can mount any
  Secret of its namespace), `exec` into kista's pods, and the node: a kubelet holds the Secrets of
  its pods.
- **Copies.** Backups (Velero, etcd snapshots), GitOps sources (Sealed Secrets, SOPS) and
  `kubectl get -o yaml` output are copies of the key. An audit policy that logs `get` and `list` on
  Secrets in the namespace is the only record of a read.
- **No HSM, no non-exportability, no usage log** of signatures beyond kista's own events.
- **What a compromise means.** Anyone holding the key can sign extensions that DuckDB clients of
  the channel accept. The response: rotate as above, and retire the old key at once (spec 0003's
  minimums apply outside dev).

## Security

- **Weaker than every other source, by design.** The spec does not claim otherwise, and the
  configuration, the start log, the audit trail and the admin output all say so.
- **Fail closed**: a mount, a layout, an owner, a mode or a key that does not fit; a key changed
  under its name; a file gone.
- **No path from a tenant.** Only the server administrator sets references (spec 0003). The
  directory comes from the configuration, the name is one element with no dots, and the file is
  opened inside `dir`.
- **Separation of roles.** There are no tags, so the role is held by the fingerprint registry and
  by `allow` prefixes. Attestation keys keep their own sources: a `kubesecret` source opens
  channel keys only (decided 2026-10-21, open question 4). Licence keys are not kista's to sign
  (spec 0012).

## Testing

- **Unit tests** (`internal/keysource/kubesecret`):
  - the mountinfo parser, on lines in the shapes kind, EKS and k3s show (a Secret, a ConfigMap, an
    `emptyDir` in memory, an escaped path, a mount stacked over another);
  - the layout checks on a directory built like the atomic writer's, with the mount check injected;
  - refusals: not a mount point, not `tmpfs`, `rw`, a ConfigMap's layout on disk, a link leaving
    `dir`, an absolute link, a dotted name, owner and mode outside shape (b), shape (a), two hard
    links, a key of the wrong size, a key changed under its name, a file removed;
  - the known answer, through the contract suite;
  - on Linux, with no seam, a real read-only tmpfs in the atomic writer's layout, owned as the
    kubelet writes it (`KISTA_TEST_KUBESECRET_DIR`; run in a privileged container as a non-root
    user), and a writable tmpfs refused.
- **Config**: the acknowledgment required outside dev, `dir` rules, reserved names.
- **Chart**: a pass fixture with a `kubesecret` source on a `secretVolumes` mount, and a fail
  fixture with `dir` on no Secret.
- **kind** (`deploy/helm/kind/run.sh`): a Secret with a key, a release signed through it, and
  DuckDB's `INSTALL` verifying the signature. A key rewritten under its name is refused at the next
  revalidation.

## Alternatives considered

- **An external Vault or OpenBao Transit** (spec 0004, available). The key stays in Vault: it is
  not exportable, it is audited, and a policy limits its use. This remains the recommendation.
- **The hugr umbrella's own OpenBao in the cluster.** Transit keys, as above. But its storage is in
  the cluster, and its unseal needs a KMS (the same gap) or Shamir keys at every restart. With
  Shamir unseal it is stronger than a Secret: the key is not in etcd, and reads go through
  OpenBao's policy and audit.
- **Relaxing the `file` signer** to accept a Secret's shape under `allow_file`. Rejected: `file` is
  the development signer. Mixing the two would hide the weaker production choice behind the
  development switch, and lose the mount check.
- **A KMS backend for these clouds** (OTC KMS, STACKIT KMS). It would be the proper fix where such a
  service exists (Hetzner and IONOS have none today). It is a follow-up of spec 0004, not a
  substitute here.
- **Reading the Secret from the API server.** kista would need API credentials, which it has
  avoided on principle (spec 0013). The mount is simpler and keeps RBAC in the kubelet.

## Open questions

1. **The name of the kind**: `kubesecret`, `k8s-secret` or `secret`. (decided 2026-10-21)
   `kubesecret`, as the draft writes it: no other kind has a dash, and `secret` would not say where
   the secret lives.
2. **Projected volumes.** Accept a projected volume that mixes a Secret source with others?
   Projected volumes are tmpfs too, and their layout is the same. (decided 2026-10-21, the draft
   gave no recommendation; the conservative option) kista cannot tell a projected volume from a
   Secret volume, so it accepts both as they come, but nothing invites a mixed one: the chart
   mounts plain Secret volumes only (`secretVolumes`), and the documentation says a projected
   volume used as a source must have Secret sources only, since a ConfigMap source in it would put
   the key in a ConfigMap.
3. **Detection strictness.** Is the mount check worth its Linux-only code? Without it, the rule is
   shape (b) plus the layout, and a ConfigMap with a key in it is accepted. (decided 2026-10-21)
   Yes, as designed: the mount check stays, and the kind is refused on other platforms with an
   error that says why.
4. **Licence and attestation keys.** Refuse `kubesecret` for licence keys (spec 0012) and the
   tenants' attestation keys, or allow it with the same acknowledgment? (decided 2026-10-21) Refuse,
   as the draft proposes: a `kubesecret` source opens a key for the channel-signing purpose only,
   and an attestation key through it is refused at open. kista signs no licences (Enterest is the
   licence root, spec 0012), so there is nothing to refuse for them here.
5. **Dev profile.** Allow it without the acknowledgment in dev (kind), as other acknowledgments are
   relaxed there? (decided 2026-10-21) Yes, like `vault`'s `software_keys`: the acknowledgment is
   required outside `profile: dev` only. Every check at open, the start warning and the audit event
   stay in dev.
6. **The audit event's name**, and whether a channel served with such a key should be flagged in
   the console. (decided 2026-10-21) `signer.source.weak`, subject `source:<name>`, with the fields
   `source`, `kind` and `protection`. No console flag in this spec (the draft gave no
   recommendation; the conservative option): the management API's `protection` field carries it,
   and a badge in the console is a follow-up.
7. **Managed clusters.** Which of OTC CCE, IONOS MK8s, STACKIT SKE and Hetzner (k3s, Talos)
   encrypt Secrets at rest by default, and with which provider. This is for the docs, to be
   verified per provider. (decided 2026-10-21) The documentation names no provider as verified: it
   says what to check and how to turn encryption on where the operator runs the control plane, and
   to ask the provider otherwise. A per-provider table is a documentation follow-up, once each one
   is verified.
