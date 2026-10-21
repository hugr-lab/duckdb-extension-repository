# Specs

One lightweight spec per feature: `NNNN-slug/spec.md`, from `TEMPLATE.md`. A spec covers the
problem, the design, enforcement and security, tests, and the alternatives that were considered. It
is written and reviewed before the code. Its status becomes `implemented` when the work is done,
and it is superseded by a new spec when a decision changes.

| Spec | Title | Status |
| --- | --- | --- |
| [0001](0001-architecture/spec.md) | Architecture | accepted |
| [0002](0002-extfile-signer/spec.md) | Extension file format, file signer, DuckDB e2e harness | implemented |
| [0003](0003-store-tenants-keys/spec.md) | Store, tenants, channels, signing keys and rotation | implemented |
| [0004](0004-signer-backends/spec.md) | Signer backends: key sources for vaults and KMSs | implemented (phases 1-4) |
| [0005](0005-blob-storage/spec.md) | Blob storage for extension bodies | phases 1, 2, 3a and 3b implemented |
| [0006](0006-serve/spec.md) | Serving, tokens and grants | implemented |
| [0007](0007-api/spec.md) | The HTTP API: index and management | implemented |
| [0008](0008-publication/spec.md) | Publication and promotion | implemented; publisher credentials superseded by 0023 |
| [0009](0009-upstreams/spec.md) | Upstreams: mirror, passthrough, pull-through, private upstreams | implemented |
| [0010](0010-audit/spec.md) | Audit events, download statistics, metrics | implemented |
| [0011](0011-attachments/spec.md) | Attachments and the feed | implemented |
| [0012](0012-licensing/spec.md) | Licensing and usage reports | implemented (phases 1a-1d, 2) |
| [0013](0013-deployment/spec.md) | Deployment: image, Helm chart, Azure | phase 1 implemented; phase 2 moved to 0020 |
| [0014](0014-bundles/spec.md) | Bundles for air-gapped sites | implemented (phases 1a, 1b, 2) |
| [0015](0015-console/spec.md) | Administration console, the micro-frontend contract | implemented |
| [0016](0016-storage-gc/spec.md) | Storage garbage collection, purging yanked releases | implemented |
| [0017](0017-extension-groups/spec.md) | Extension groups: grants on named sets of extensions | implemented |
| [0018](0018-blob-verify/spec.md) | Verifying stored bodies | implemented |
| [0019](0019-tenant-deletion/spec.md) | Deleting a tenant | implemented |
| [0020](0020-cloud-recipes/spec.md) | Cloud recipes: AWS and GCP in Terraform, Azure in Bicep | phases 0-3, 4a and 4b implemented; 4b's live run to come |
| [0021](0021-licence-keys/spec.md) | Licence keys in the build, and licences in bundles | implemented |
| [0022](0022-developer-kit/spec.md) | The developer kit: the extension-ci-tools fork with the SDK, the template, publishing from CI | implemented |
| [0023](0023-publisher-keys/spec.md) | Publisher keys: signed requests from any CI | implemented |
| [0024](0024-bootstrap-upstreams/spec.md) | The bootstrap's upstreams, and the chart in another namespace | implemented |
| [0025](0025-chart-umbrella/spec.md) | The chart in an umbrella: an egress NetworkPolicy and a templated config | implemented |
| [0026](0026-kubesecret-keys/spec.md) | Signing keys from a Kubernetes Secret, as an explicit opt-in | implemented; the kind run to come |
| [0027](0027-store-login-vault/spec.md) | The database password from Vault or OpenBao | phase 1 implemented; the kind case to come; phase 2 deferred |
