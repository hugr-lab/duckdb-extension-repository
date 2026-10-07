# kista

A trusted repository for [DuckDB](https://duckdb.org) extensions. *Kista* is Old Norse / Swedish for
"chest".

- **Mirror** the official core and community extensions: DuckDB's signature is verified at intake,
  and every binary is **re-signed with your channel's key** (core extensions can also be served
  byte-identical, so DuckDB can still autoload them). A signature from your repository then means
  "checked and allowed by us", not only "built by DuckDB".
- **Publish** your own extension builds under the same key.
- **Public and private extensions**: a public extension is served to anyone, a private one only
  with a bearer token.

DuckDB 2.0 uses it as a trusted extension repository with no plugin:

```sql
CREATE EXTENSION REPOSITORY corp WITH PREFIX 'https://extensions.example.com/acme/prod';  -- keys from .well-known
INSTALL httpfs FROM corp;
LOAD httpfs FROM corp;
```

Status: early development: the extension file format and signing are in place (spec 0002); the
server comes next. See `specs/`. Enterest is the hosted service built on kista.

Licensed under the Apache License, Version 2.0.
