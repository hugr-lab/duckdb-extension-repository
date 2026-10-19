---
title: Air-gapped sites
---

# Air-gapped sites

An air-gapped site runs kista without a route to the internet. Two things cross its boundary, on
media its operators carry:

- **kista itself**: the image, the chart and the binary, verified inside the site against the
  signatures made when they were built ([Verifying](index.md#verifying)).
- **Extensions**, in bundles. A gateway kista on the connected side mirrors the sources the site
  needs; the site writes a plan, the gateway exports a bundle answering it, and the site imports
  the bundle into its bundle-mode upstreams ([Bundle mode](../upstreams.md#bundle-mode)).

The design is in
[spec 0014](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0014-bundles/spec.md).
CI runs this page (`.github/workflows/air-gapped.yml`), so a command that stops working fails a
job. It runs the shell blocks titled "On the connected side" in a container with a network, and
those titled "At the site" in a container whose only network reaches the site's registry. It sets
the variables below to its own values (`file:` signers, an older DuckDB release), and it does not
run the Helm install. Its kista is built from the same commit as the page. The image it verifies
is the last release's; until the first release it verifies `:edge` and skips the chart and the
binary.

## What crosses

The media carry a `transfer` directory. Whoever carries it can change it, so the site trusts
nothing on it that it cannot verify against two values that come another way.

| What | Made on the connected side by | Checked at the site by |
| --- | --- | --- |
| The image, with its signature and provenance, as an OCI layout | `oras copy -r` (the referrers too) | `cosign verify`, `cosign verify-attestation` |
| The chart, with its signature | `oras copy -r` | `cosign verify` |
| The binary and its `.sigstore.json` | `gh release download` | `cosign verify-blob` |
| Sigstore's trusted root | `cosign initialize` | its SHA-256, carried out of band |
| The plan (site to gateway) | `kista admin bundle plan` at the site | the gateway checks it |
| A bundle (gateway to site), with the licences the plan asks for | `kista admin bundle export` | the import, with the pinned bundle key |

- **Out of band.** Two values must not come from the media: the trusted root's SHA-256 and the
  bundle key's fingerprint. Carry them through a channel the carrier cannot change: read them out
  over the phone, or print them on paper. With a forged trusted root, a carrier could sign any
  image as the release; with a forged fingerprint, any bundle.
- **Tools.** The site needs cosign 3 (this page is tested with v3.1.3) and oras, approved and
  brought in by the site's own process. The connected side also uses gh, to download the release.
- **The trusted root.** cosign fetches it through Sigstore's TUF repository and verifies it against
  the TUF root it embeds. It changes when Sigstore rotates a key: carry a fresh one, with its hash,
  with each release you bring in.

## Both sides

Each side runs a configured kista: a store, a blob domain with its spool, and on the gateway a
signer source for its mirror's channel ([Getting started](../getting-started.md),
[Configuration](../configuration.md)). The blocks below use these variables:

```bash
VERSION=v1.2.0                                  # the kista release
IDENTITY=https://github.com/hugr-lab/duckdb-extension-repository/.github/workflows/release.yml@refs/tags/$VERSION
SITE_REGISTRY=registry.site.internal            # the site's OCI registry
PLATFORM=linux_amd64                            # the site's DuckDB platform
DUCKDB_VERSION=v2.0.0                           # the site's DuckDB version
GATEWAY_SIGNER=vault:kista-gateway:v1           # the gateway mirror's signing key
TRUSTED_ROOT_SHA256=…                           # at the site: carried out of band
alias ka='kista admin -config /etc/kista/kista.yaml'
```

## Bringing kista in

On the connected side, copy the image with its signature and provenance (OCI referrers) into
`transfer`, with Sigstore's trusted root (`cosign initialize` fetches it, verified, into
`~/.sigstore`). Compare the digest with the release notes, and send the trusted root's SHA-256 to
the site out of band.

```bash title="On the connected side" ci=connected
mkdir -p transfer
oras resolve ghcr.io/hugr-lab/kista:$VERSION > transfer/kista.digest
oras copy -r ghcr.io/hugr-lab/kista@$(cat transfer/kista.digest) --to-oci-layout transfer/kista:$VERSION
cosign initialize
cp ~/.sigstore/root/tuf-repo-cdn.sigstore.dev/targets/trusted_root.json transfer/
sha256sum transfer/trusted_root.json | tee trusted-root.sha256
```

The chart and the binary, the same way:

```bash title="On the connected side" ci=connected-release
oras resolve ghcr.io/hugr-lab/charts/kista:${VERSION#v} > transfer/chart.digest
oras copy -r ghcr.io/hugr-lab/charts/kista@$(cat transfer/chart.digest) --to-oci-layout transfer/chart:${VERSION#v}
gh release download $VERSION -R hugr-lab/duckdb-extension-repository -D transfer \
  -p "kista-$VERSION-linux-amd64" -p "kista-$VERSION-linux-amd64.sigstore.json"
```

At the site, check the trusted root against the hash carried out of band, push the image into the
site's registry, keeping its digest and referrers, and verify its signature and build provenance
there. Nothing reaches beyond the registry: the trusted root replaces Sigstore's online services.
The identity names the release's tag, so a digest changed on the media can only name another
artifact signed for the same release.

```bash title="At the site" ci=site
echo "$TRUSTED_ROOT_SHA256  transfer/trusted_root.json" | sha256sum -c
DIGEST=$(cat transfer/kista.digest)
oras copy -r --from-oci-layout transfer/kista:$VERSION $SITE_REGISTRY/hugr-lab/kista:$VERSION
cosign verify $SITE_REGISTRY/hugr-lab/kista@$DIGEST --trusted-root transfer/trusted_root.json \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify-attestation $SITE_REGISTRY/hugr-lab/kista@$DIGEST --type slsaprovenance1 \
  --trusted-root transfer/trusted_root.json \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The chart and the binary:

```bash title="At the site" ci=site-release
CHART=$(cat transfer/chart.digest)
oras copy -r --from-oci-layout transfer/chart:${VERSION#v} $SITE_REGISTRY/hugr-lab/charts/kista:${VERSION#v}
cosign verify $SITE_REGISTRY/hugr-lab/charts/kista@$CHART --trusted-root transfer/trusted_root.json \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify-blob transfer/kista-$VERSION-linux-amd64 \
  --bundle transfer/kista-$VERSION-linux-amd64.sigstore.json --trusted-root transfer/trusted_root.json \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Install with the chart from the site's registry, the image by digest
([Kubernetes](kubernetes.md)):

```bash
helm install kista oci://$SITE_REGISTRY/hugr-lab/charts/kista --version ${VERSION#v} \
  -n kista --create-namespace -f values.yaml \
  --set image.repository=$SITE_REGISTRY/hugr-lab/kista --set image.digest=$DIGEST
```

An admission policy at the site (the sigstore policy-controller, Kyverno) verifies the same way,
given the same trusted root.

## The gateway

The gateway is a kista on the connected side that mirrors what the site needs. Its mirrors of
DuckDB's core and community extensions must be **public**: a bundle never carries a private
release of DuckDB's kinds. Here it mirrors DuckDB's `json` and `httpfs` core extensions:

```bash title="On the connected side (the gateway)" ci=connected
ka migrate
ka tenant create gw
ka channel create gw/core -kind signed
ka version add $DUCKDB_VERSION -kind release
ka channel versions gw/core -add $DUCKDB_VERSION
ka key add gw/core -signer "$GATEWAY_SIGNER" -active
ka upstream add gw core -kind duckdb-core -channel core -platforms $PLATFORM -extensions json,httpfs -public
```

## The bundle key

The bundle key signs each bundle's manifest; the site imports only bundles signed by a key it
pinned. Make it on a machine that is not the gateway, and keep it there: export unsigned and sign
there ([Exporting a bundle](../upstreams.md#exporting-a-bundle)). This page keeps it beside the
gateway, for brevity. `keygen` prints the fingerprint: send it to the site out of band.

```bash title="On the connected side" ci=connected
kista bundle keygen -o bundle-key.pem | tee bundle-key.fingerprint
```

At the site, pin the fingerprint for the tenant in `kista.yaml`. `kista admin` reads it on each
run.

```yaml title="/etc/kista/kista.yaml at the site" ci=site-config
upstreams:
  bundle_keys:
    - fingerprint: "sha256:…"   # what keygen printed, carried out of band
      tenants: [acme]
```

## The site

The site's bundle-mode upstreams say what it takes in. Here DuckDB's core extensions go into a
**passthrough** channel, which serves them with DuckDB's own signature, so DuckDB can autoload
them from the site ([Autoloading and the passthrough channel](../duckdb.md#autoloading-and-the-passthrough-channel)).
A signed channel takes core, community or another kista's extensions the same way, re-signed with
its own key. The channel's DuckDB versions are those of the site's DuckDB, with the C API maximums
that C API extensions need
([Getting started](../getting-started.md#tell-kista-which-duckdb-versions-the-channel-serves)).

```bash title="At the site" ci=site
ka migrate
ka tenant create acme
ka channel create acme/core -kind passthrough
ka version add $DUCKDB_VERSION -kind release
ka channel versions acme/core -add $DUCKDB_VERSION
ka upstream add acme core -mode bundle -kind duckdb-core -channel core -platforms $PLATFORM \
  -extensions json,httpfs -public   # a passthrough channel's releases are public
```

## The cycle

Each cycle carries a plan out and a bundle back.

1. **Plan**, at the site: what the bundle-mode upstreams ask for, what the site already holds,
   and the pinned bundle keys. The site keeps its own copy.

   ```bash title="At the site" ci=site
   ka bundle plan acme -o plan.json
   cp plan.json transfer/
   ```

2. **Export**, on the gateway: `-sync` runs the gateway's mirrors first.

   ```bash title="On the connected side (the gateway)" ci=connected
   ka bundle export gw -plan transfer/plan.json -key bundle-key.pem -o transfer/acme.kbundle -sync
   ```

   It prints what each source gave and what it could not, and exits `1` when a source failed as a
   whole. A plan names the site's sources, platforms, versions and the bodies it holds: treat it as
   the site's configuration, and the bundle as the extensions it carries.

3. **Import**, at the site: inspect the bundle against the site's own copy of the plan, try it
   with `-dry-run`, then import it. The import checks the bundle key against the pins in
   `kista.yaml`, not against the plan.

   ```bash title="At the site" ci=site
   kista bundle inspect transfer/acme.kbundle -plan plan.json
   ka bundle import acme transfer/acme.kbundle -dry-run
   ka bundle import acme transfer/acme.kbundle
   ka upstream cells acme core
   ```

The next cycle starts with a new plan. A bundle older than the last one imported is refused, and
a cell takes a body only when it is newer at its source than the one it holds, so carrying an old
bundle back changes nothing. In Kubernetes, the import runs in the serving pod or as a Job
([Importing a bundle](kubernetes.md#importing-a-bundle)).

## Licences

A licensed extension needs its licence at the site as well ([Licensing](../licensing.md)). A
site without a route to the issuer receives it in the bundle, beside the extension's files:

- **The site asks**: `ka bundle plan acme -licenses -o plan.json`. With
  `-license-holder <sub>` it names the holder it expects: the gateway carries no other holder's
  licence, and the import takes none.
- **The gateway agrees**: `ka bundle export gw … -licenses`. Without the flag it carries none, and
  says so. It carries, for each repository source, the licences its tenant holds for the names
  whose files the bundle carries or the site already holds, in force, at most 1,000 per source; a
  private release's name only with `-private`. It prints each licence's name, holder and expiry.
  Its tenant holds them as any tenant does: imported by an administrator, or relayed from a
  credentialed upstream such as the issuer's.
- **The import** takes each licence after the files, into the first bundle-mode upstream of its
  source that supplies the name, verified with the keys that upstream's builds declare. A licence
  an administrator imported at the site is never replaced by a bundle's, and a bundle removes no
  licence: one ends at its expiry plus grace. The import prints the licences stored, replaced,
  unchanged and refused, with each refusal's reason
  ([Importing a bundle](../upstreams.md#importing-a-bundle)).

Both sides need a kista of the release that brought licences in bundles, or a later one: an older
one refuses a plan that asks for licences, and a bundle that carries them.

## Before you start

- **Without `kista admin` or the object store.** A site that can reach the server's API, but can
  neither run `kista admin` beside the server nor write to its object store, uploads
  the bundle over the API, or from the console, instead
  ([Uploading a bundle](../upstreams.md#uploading-a-bundle)).
- **Sizing.** The import inflates one file at a time into the spool: about `blob.max_body` free
  beside what the server uses. In Kubernetes, give it `spool.importHeadroom`. The bundle itself is
  read once, from a file, standard input or the object store, and never extracted.
- **A slot holds one body forever.** Once a release is in a slot (its DuckDB version, platform and
  name), another body for the same version is a `conflict`, permanently. Check a bundle from a new
  source with `kista bundle inspect` and `-dry-run` before its first import.
- **Rotation.** When a repository source rotates its key, the export lists the new fingerprint, so
  you notice. Pin it at the site only after comparing it with the publisher's own fingerprint,
  given out of band, before the old key leaves the source's `.well-known` file. When you rotate
  the bundle key, pin the new key beside the old one, then remove the old one.
- **Statements.** Reviews and other signed statements travel the same way: an export feed on the
  gateway, a bundle-mode feed at the site, in the same bundles
  ([Feeds for air-gapped sites](../attachments.md#feeds-for-air-gapped-sites)). A `gap` makes the
  import exit `1`: write a new plan ([Importing a bundle](../upstreams.md#importing-a-bundle)).
- **Autoload.** DuckDB autoloads from the passthrough channel over plain http: set
  `autoinstall_extension_repository` on the clients, and give the site a listener DuckDB's
  built-in client can reach ([Using kista from DuckDB](../duckdb.md#autoloading-and-the-passthrough-channel)).
