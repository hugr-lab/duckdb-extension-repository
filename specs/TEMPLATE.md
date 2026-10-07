# Spec NNNN: <feature name>

- **Status**: draft | accepted | implemented | superseded by NNNN
- **Date**: YYYY-MM-DD
- **Author**: <who>

## Summary

One paragraph: what this feature is and why, in plain language.

## Problem

What is missing or wrong today, and who is affected: an organisation running its own repository, our
public service, an extension publisher, a DuckDB client (person or node). Concrete examples.

## Design

The chosen approach. Cover, as applicable:

- **API / CLI**: new HTTP routes (method, path, auth), admin operations, CLI commands, config keys.
- **Data model**: tables and columns, for all three dialects (PostgreSQL, SQL Server, SQLite);
  migrations.
- **Storage and layout**: what goes to the blob store, what paths DuckDB requests and what they serve.
- **DuckDB behaviour relied on**: what the client does (with the source location on the duckdb pin)
  and how it was confirmed.
- **Interaction**: how it composes with tenants, channels, grants, upstreams, signing.

## Security

Why this is safe. Cover: fail-closed behaviour, what an anonymous caller and a caller without a
grant can learn (no existence leaks), key handling (keys never leave the signer), tenant isolation,
what is trusted (admin input, upstream signatures) and what is verified.

## Testing

How it is proven: Go unit tests, the shared store suite on all three dialects, and end-to-end tests
that run the pinned duckdb against the server (`INSTALL … FROM`, `LOAD`).

## Alternatives considered

Options rejected and why (briefly).

## Follow-ups

Anything intentionally left out of scope, and what a later spec changes.
