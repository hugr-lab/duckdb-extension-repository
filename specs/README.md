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
| [0004](0004-signer-backends/spec.md) | Signer backends: key sources for vaults and KMSs | phases 1-2 implemented; 3-4 deferred |
| [0005](0005-blob-storage/spec.md) | Blob storage for extension bodies | phases 1-2 implemented; 3 deferred |
| [0006](0006-serve/spec.md) | Serving, tokens and grants | implemented |
| [0007](0007-api/spec.md) | The HTTP API: index and management | implemented |
| [0008](0008-publication/spec.md) | Publication and promotion | implemented |
| [0009](0009-upstreams/spec.md) | Upstreams: mirror, passthrough, pull-through | phases 1a, 1b, 2 implemented; 3 by amendment |
| [0010](0010-audit/spec.md) | Audit: events, hash chain, retention, statistics, sinks | draft |
