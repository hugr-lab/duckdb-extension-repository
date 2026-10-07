# duckdb-extension-repository — Development Guidelines

The product is **kista** (open server, binary `kista`, env `KISTA_*`); **Enterest** is our hosted
service on top of it (closed, separate repository, `enterest.hugr-lab.com`).

A trusted repository for DuckDB extensions (Apache-2.0, Go). It serves what DuckDB 2.0's trusted
extension repositories expect - `CREATE EXTENSION REPOSITORY <name> WITH PREFIX 'https://…'`,
`INSTALL x FROM <name>` - and is the one place the hugr platform's nodes and people install from:

- **mirror** the official core / community binaries, verify DuckDB's signature at intake, and
  **re-sign everything with our own key** (the signature means "checked and allowed");
- **publish** our own builds (duckdb-acl, acl-otel, tresor, hugr_node) under the same key;
- **public and private extensions**: a public one is served to anyone, a private one only with a
  Bearer token (tresor-issued); `/.well-known/duckdb-extension-repo.json` (the signature keys) is
  always public.

Start with **design/000-kickoff/KICKOFF.md** (local, gitignored): the owner's decisions, what was
verified on the duckdb pin, what is still to check, and a first cut of the MVP. The platform context
is in `design/000-kickoff/017-repository-sections.md` (an excerpt of duckdb-acl's design/017).

## Related repositories

- `hugr-lab/duckdb-acl` - the node's access-control extension; its spec 093 (cluster profile) pins
  extensions by sha256 and installs them `FROM <repo>`. The duckdb pin to test against is its
  submodule's (v2.0-cyanoptera).
- `hugr-lab/hugr-node` (next) - the node agent, installs only from here.
- `hugr-lab/tresor` - tokens for private extensions (an http secret with SCOPE on our prefix).

## Working process

- One lightweight spec per feature under `specs/NNNN-slug/spec.md` (`specs/TEMPLATE.md`): problem,
  design, security, tests, alternatives. Written and reviewed before code; status `implemented` when
  done. One branch per spec.
- Every feature lands with tests. Three adversarial review passes before a PR.
- `design/` is local scratch (gitignored); specs and code are the record.
- The owner reads Russian; code, specs and commit messages are English.
