# duckdb-extension-repository — Development Guidelines

The product is **kista** (open server, binary `kista`, env `KISTA_*`); **Enterest** is our hosted
service on top of it (closed, separate repository, `enterest.hugr-lab.com`).

A trusted repository for DuckDB extensions (Apache-2.0, Go). It serves what DuckDB 2.0's trusted
extension repositories expect - `CREATE EXTENSION REPOSITORY <name> WITH PREFIX 'https://…'`,
`INSTALL x FROM <name>` - and is the one place the hugr platform's nodes and people install from:

- **mirror** the official core / community binaries, verify DuckDB's signature at intake, and
  **re-sign with the channel's key** (the signature means "checked and allowed"); the one exception
  is a passthrough channel, which serves core byte-identical so DuckDB can autoload it;
- **publish** our own builds (duckdb-acl, acl-otel, tresor, hugr_node) into signed channels;
- **public and private extensions**: a public one is served to anyone, a private one only with a
  Bearer token (tresor-issued); `/.well-known/duckdb-extension-repo.json` (the signature keys) is
  always public.

Start with **specs/0001-architecture/spec.md**: the model, the URL layout, the open-core boundary,
and the DuckDB behaviour the design relies on (with source locations on the duckdb pin).

## Related repositories

- `hugr-lab/duckdb-acl` - the node's access-control extension; its spec 093 (cluster profile) names
  extensions by version and repository and installs them `FROM <repo>`. The duckdb pin to test
  against is its submodule's (v2.0-cyanoptera).
- `hugr-lab/hugr-node` (next) - the node agent, installs only from here.
- `hugr-lab/tresor` - tokens for private extensions (an http secret with SCOPE on our prefix).

## Working process

- One lightweight spec per feature under `specs/NNNN-slug/spec.md` (`specs/TEMPLATE.md`): problem,
  design, security, tests, alternatives. Written and reviewed before code; status `implemented` when
  done. One branch per spec.
- Every feature lands with tests. Three adversarial review passes before a PR.
- Self-review the diff before every commit, and again after opening a PR (fix in a follow-up
  commit).
- `design/` is local scratch (gitignored); specs and code are the record.
- Everything in the repository and on GitHub is in English: code, comments, specs, commit
  messages, issues, PRs and reviews.
- Personal, untracked instructions go in `CLAUDE.local.md` (gitignored).
