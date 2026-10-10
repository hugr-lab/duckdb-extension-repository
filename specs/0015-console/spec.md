# Spec 0015: Administration console

- **Status**: accepted (2026-10-10)
- **Date**: 2026-10-10
- **Author**: vgsml, Claude

## Summary

A web console for kista's administrators. kista serves it itself at `/ui/`, so a deployment on its
own has a UI. The same build is also a **micro-frontend**: the hugr platform's shell, and
Enterest's, mount it beside other components (tresor-server's console among them), so a platform
user sees one interface. The mounting contract follows tresor-server's micro-frontend contract
(its spec 016, "contract, version 1"): one ES module and a custom element. The host owns sign-in,
navigation and the theme; the console asks the host for a token per audience. kista keeps its own
names and adds one option, the tenant, because its identities are per tenant. The console uses
only the management API (spec 0007), so it adds no authority of its own.

Delivery, each phase a pull request:

- **1a. Standalone console**:
  - serving at `/ui/` with a CSP per scope;
  - sign-in in both scopes;
  - issuer console clients (route, CLI, migration, events);
  - reads: tenants, channels, releases, keys, events, statistics;
  - release changes: yank, deprecate, activate, current, public/private, purge.
- **1b. Micro-frontend**: the contract module and custom element, CORS for shells, the test host.
- **2. Administration forms**:
  - grants, issuers and their console clients, audiences;
  - publishers and API keys, blocks;
  - upstreams (entries, platforms, keys, credentials, sync, cells);
  - key activation and retirement;
  - tenants and DuckDB versions (server administrators).

## Problem

- **Command line only.** Everything is done with `kista admin` on the server or with `curl`
  against the management API: yanking a release, reading events, granting `install` to a group,
  watching a re-sign. A tenant administrator of a hosted deployment has no shell on the server,
  and nobody should hand-write `If-Match` headers.
- **One platform.** The hugr platform shows its components in one shell. tresor-server already
  ships a console mountable there; kista must mount the same way, or the platform needs two
  integrations.
- **Per-tenant identities.** kista differs from tresor-server here, and it matters for the UI:
  - a tenant administrator's token is issued by the tenant's own issuer records, for the tenant's
    audience (spec 0006: the canonical `<public_url>/<tenant>` or an assigned one such as
    `api://kista-acme`);
  - a server administrator's token comes from the server issuers, for a server audience (spec
    0007).

  Spec 0007 makes a token one or the other, never both, so one sign-in cannot serve both.

## Design

### Stack

The stack is tresor-server's (one platform, one way of building consoles):

- React 18, TypeScript, Vite, Tailwind 3, react-router-dom 6, lucide-react, oidc-client-ts;
- Manrope and JetBrains Mono, self-hosted (`@fontsource`);
- **no component library**: dialogs are native `<dialog>`s (a library that injects `<style>`
  tags, like Radix's scroll lock, breaks the CSP below);
- a licence gate in CI: MIT, ISC, Apache-2.0, BSD and OFL dependencies only.

The source is in `web/console/`. Two Vite builds:

- **standalone**: `dist/index.html`, hashed assets under `dist/assets/`, no inline script (no
  module-preload polyfill), no `data:` fonts;
- **micro-frontend** (1b): one ES module `dist/mfe/kista.js` (a fixed name) with its own React,
  its CSS and dynamic imports inlined; only its fonts are files, under `dist/mfe/assets/`.

### Build and repository

- **Embedding.** `web/console/console.go` embeds `dist` (`//go:embed all:dist`). Only a
  placeholder `dist/index.html` is committed (`.gitignore`: `dist/*`, `!dist/index.html`); a
  binary built without the console serves the placeholder, which says so.
- **Make targets.** `make console` runs `npm ci && npm run build` in `web/console`. `make build`
  does not need Node, so Go contributors and the existing `go-linux`/`go-macos` jobs build with
  the placeholder. Every target keeps `GOWORK=off`.
- **CI.** `.github/workflows/ci.yml` gains two jobs:
  - `console`: `npm ci`, typecheck, unit tests, licences, `npm audit --audit-level=high`, build;
  - `console-e2e`: Playwright, traces as artifacts.
- **Releases.** A release build runs `make console` before `go build`. There is no Dockerfile
  yet; the one that comes later builds the console in a Node stage.
- **Dependencies.** Dependabot covers `web/console`.

### Serving

`ui` is already a reserved tenant name, so `/ui/` never shadows a DuckDB route. Only `GET` and
`HEAD` are allowed.

- **Paths:**
  - `/ui` redirects to `/ui/`;
  - under `/ui/`, a path without an extension gets `index.html` (the SPA fallback);
  - a missing file with an extension is `404`;
  - `index.html` is served with `<base href="/ui/">` rewritten to `<public_url path>/ui/`, so
    assets resolve from any SPA path (`/ui/t/acme/channels/prod`).
- **Three scopes, by path**, each its own document. Moving between them is a full page load,
  never an SPA navigation.
  - `/ui/` (and `/ui/index…`): the landing, which asks where to sign in;
  - `/ui/server/…`: the server scope;
  - `/ui/t/{tenant}/…`: a tenant's scope.
- **Callbacks**: `/ui/server/callback` and `/ui/t/{tenant}/callback`. The same document serves
  the redirect and the popup sign-in; both are registered at the IdP as the client's redirect
  URIs.
- **CSP, computed per scope** when `index.html` is served:
  - the policy: `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:;
    font-src 'self'; connect-src 'self' <the scope's IdP origins> <ui.connect_src>;
    object-src 'none'; base-uri 'self'; form-action 'self' <the scope's IdP origins>;
    frame-ancestors <ui.frame_ancestors or 'none'>`;
  - the scope's IdP origins: none for the landing; the server console clients' issuers for the
    server scope; that tenant's issuers with a console client for a tenant scope. A tenant never
    adds an origin to another scope's policy;
  - an issuer's origins are those of its discovery document's `issuer`, `token_endpoint`,
    `revocation_endpoint`, `end_session_endpoint` and `userinfo_endpoint`, read through egress
    and the issuer cache (spec 0006). An issuer whose discovery cannot be read contributes its
    own origin only, and its sign-in fails visibly.
- **Other headers**: `Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`.
- **Cache**:
  - `assets/*` and `mfe/assets/*`: `public, max-age=31536000, immutable`;
  - `index.html` and `mfe/kista.js`: `no-cache` with a strong ETag (SHA-256 at start), so an
    upgrade is picked up at once.

### Sign-in configuration

**Server**: `GET /api/v1/console` (public, `no-store`, CORS in 1b):

```json
{"environment": "staging", "admin_token_max_age": 3600, "audience": "<auth.server_audiences[0]>",
 "issuers": [{"name": "ops", "issuer": "https://…", "client_id": "…", "scopes": ["openid", "…"],
              "audience_parameter": ""}]}
```

`audience` is always present, since an embedded console needs it even with no server client.
`issuers` lists `ui.server_clients` only.

**A tenant**: `GET /api/v1/tenants/{t}/console` (public, `no-store`) returns
`{"issuers": [{name, issuer, client_id, scopes, audience_parameter, audience}]}`:

- it lists the issuer records that have a console client;
- each entry has the audience its tokens must carry: the client's `audience` if it set one (Entra
  needs the assigned `api://…`), otherwise the canonical `<public_url>/<tenant>`;
- an unknown or suspended tenant answers `404`, like every tenant route (spec 0007);
- listing an issuer here makes its URL public. That identifies the tenant's IdP (an Entra tenant
  id, an Okta domain), so the tenant opts in per record by setting a console client.

**Issuer console clients.** Issuer records are immutable (spec 0007: their ETag is their id), so
a record's console client is a sub-resource, set and removed by a tenant administrator:

- **API.** `POST /api/v1/tenants/{t}/issuers/{name}/console {client_id, scopes,
  audience_parameter, audience}` sets or replaces it and answers `200` with it; `DELETE` answers
  `204`.
  - Both need `If-Match: "<issuer id>"`, the record's ETag, as removing the record does: a record
    removed and re-added under the same name never inherits a client meant for the old one.
  - `audience`, when set, must be one of the tenant's audiences (canonical or assigned).
- **CLI.** `kista admin issuer console <tenant> <name> -client-id <id> [-scopes …]
  [-audience …]`, and `-remove`.
- **Table.** `issuer_console_clients(issuer_id PRIMARY KEY, client_id, scopes,
  audience_parameter, audience, created_at, created_by)`, migration 0015; the client is removed
  with its record.
- **Limits.**
  - `client_id`: 1..200 printable characters;
  - `scopes`: up to 20 tokens without spaces, `openid` required;
  - `audience_parameter`: a parameter name or empty.
- **Events.** `issuer.console.set` and `issuer.console.remove`, with `issuer`, `client_id` and
  `audience` (spec 0010's catalogue).
- **A new tenant.** Its first issuer record and that record's console client are added by a server
  administrator (API or CLI), or by the tenant administrator over the API.

### Configuration

```yaml
ui:                           # file-only
  enabled: true               # false: no /ui/, no console routes, no CORS; issuer console clients stay manageable
  environment: ""             # a badge in the console's header (e.g. "staging")
  allowed_origins: []         # shells on other origins (1b)
  frame_ancestors: []         # who may frame the standalone console; default none
  connect_src: []             # extra connect-src origins (an OTel collector, a proxy)
  server_clients:             # the console's clients at server issuers; none: no server sign-in
    - issuer: ops             # a server issuer's name
      client_id: "…"          # a public client (PKCE)
      scopes: [openid, profile, offline_access]
      audience_parameter: ""  # e.g. "audience" for IdPs that take it as a parameter
```

Validation:

- origins are `https://host[:port]`, no path, no `*`; `http` only for loopback;
- a server client's `issuer` must name a server issuer, with one client per issuer;
- `scopes` follow the tenant rules.

### Two scopes

- **Server**, under `/ui/server/…`: a server administrator, with a server token.
  - The gate: `GET /api/v1/whoami` answers `200` with `administrator: true`. A server token that
    is not an administrator's sees "not a server administrator".
  - The server scope shows every tenant and opens one under `/ui/server/tenants/{t}/…`, keeping
    the server token (a server administrator acts with every right on tenant routes, spec 0007).
  - It never calls tenant `whoami`, which answers a server token `404`, and shows every section.
- **Tenant**, under `/ui/t/{tenant}/…`: a token for that tenant, from one of its issuers.
  - The gate: `GET /api/v1/tenants/{t}/whoami` with `holds_admin` (an admin grant on the tenant,
    a channel or an extension) or `holds_audit`.
  - The console shows what the grants allow:
    - tenant administrators see everything;
    - channel administrators see their channels;
    - extension administrators see their extensions, through
      `channels/{c}/extensions/{ext}/releases` (names from the grants);
    - auditors see events.
  - A token holding neither sees "nothing to administer here". Sections whose reads answer `404`
    are hidden.
- **Sessions.** A session belongs to one (scope, tenant, issuer). The API client binds the token
  to its scope's route prefix: a request outside it is a bug that fails in tests. Sign-out ends
  that session only. Leaving a scope is a full page load, so no token outlives its scope.

### Standalone sign-in (1a)

- **The landing.** `/ui/` offers a tenant name field and, when server clients exist, "Server
  administration". The last tenant typed is kept in `localStorage` (a convenience only).
- **Choosing an issuer.**
  - A scope with several issuers lets the user pick one, each shown with its IdP's host, never a
    name alone.
  - A tenant scope never redirects on its own, even with one issuer. A link to `/ui/t/evil` shows
    "Sign in at `login.example.org`" and waits for a click: kista's origin never sends a user to
    an unknown login page unannounced.
- **The flow.** Authorization Code with PKCE (oidc-client-ts), redirect to the scope's callback.
  The access and refresh tokens stay in memory (`InMemoryWebStorage`); only the PKCE state is in
  `sessionStorage`.
- **Renewal.** Management needs `iat` within `admin_token_max_age` (spec 0007).
  - The console renews at `min(exp - 60 s, iat + admin_token_max_age - 5 min)`, through the
    refresh token, never a hidden iframe.
  - When renewal fails, a banner offers a popup sign-in, so a form being edited is kept.
  - A reload signs in again through the IdP's session, with one click on the issuer.
- **Sign-out** revokes the refresh token where the IdP has a revocation endpoint, then ends the
  IdP session.

### API calls

- **Requests**: `Authorization: Bearer`, `credentials: 'omit'`, `cache: 'no-store'`. No cookies,
  so no CSRF surface.
- **Changes**: every change sends the `If-Match` it read (spec 0007). A `412` re-reads and tells
  the user the object changed.
- **Errors**:
  - a `401` starts one shared renewal and one retry; after that, the banner (standalone) or
    `onUnauthorized` (embedded), at most once per 30 seconds;
  - a `404` or `400` never starts a renewal: a `404` is "not there, or not yours" (spec 0007).
- **No polling** in the background. A view refreshes when opened and on demand. A running
  re-sign's progress refreshes every 10 seconds while its view is open.

### Screens (1a)

The visual system is the Hugr Lab design system (teal and navy, Manrope, JetBrains Mono, Lucide
icons, no shadows, pill buttons). Its tokens are copied into the console's Tailwind theme, as
tresor-server and hub do.

- **Tenant home**: the channels (kind, DuckDB versions) and, for a channel administrator, its key
  summary (`keys`: active and serving key, `unsigned_by_active`, the re-signer).
- **Channel**: releases from the management list (spec 0007: filtered by state; name and platform
  filtered over the loaded pages).
  - A release's page shows its build (ABI, DuckDB or C API version, body hash), provenance,
    origin, and state history (created, changed, by whom).
  - Its changes are yank, deprecate, activate, current, public, private and purge, each confirmed
    in a dialog. A purge says what stays (the slot) and asks for the release's name to be typed.
- **Keys** (read): trusted, active and serving keys, key events, re-sign progress.
- **Events**: the API's filters, a detail view, kinds from `event-kinds`; server events in the
  server scope. Event data, provenance and everything else from the API are rendered as text.
- **Statistics**: downloads by extension, version and platform over time (spec 0010 phase 2a).
- **Server scope**: the tenants list (name, state, storage domain), and a tenant opened inside.

Release signatures are not listed: the API has no field for them. The keys view shows which keys
sign the channel.

### The micro-frontend contract (1b; kista's contract version 1)

This is tresor-server's contract with kista's names, plus `tenant`. kista's contract number is
its own; a shell checks each module's. An adapter that also passes `tenant` to tresor-server is
safe, since tresor ignores unknown options.

```js
const m = await import(`${apiBase}/ui/mfe/kista.js`)   // exports contract (= 1), mountKista, KistaConsole
const h = m.mountKista(el, {
  apiBase,            // kista's origin or the shell's proxy prefix, no trailing slash
  getToken,           // (audience, {renew}?) => Promise<string>
  tenant,             // optional: a tenant scope; absent: the server scope
  audience,           // optional: overrides the audience kista names
  basePath,           // the host path the console lives under
  theme,              // "light" | "dark"
  locale,             // reserved: "en" only
  onNavigate,         // (path, {replace}) => void
  onTitle,            // (title) => void
  onUnauthorized,     // () => void
})
h.update({ theme, path, locale, tenant })
h.unmount()
```

- **Audience.**
  - The console reads it from `/api/v1/console` or the tenant's `console` route (the first
    issuer's), unless `audience` overrides it, and calls `getToken(audience)` with the raw `aud`
    value.
  - The host maps it to what its IdP needs: an Entra `api://kista-acme/.default` scope, a ZITADEL
    project, a Keycloak audience mapper.
  - The host issues tokens only for audiences it knows, never whatever a component names.
- **`getToken`.**
  - The console keeps no token: it calls `getToken` before each request (the host caches).
  - `{renew: true}` means a **newly issued** token with a fresh `iat` (MSAL's `forceRefresh`),
    not another cached one. The console checks `iat` against `admin_token_max_age` and asks
    `renew` before the API would refuse the token.
  - A rejected `getToken` shows "no token for audience X" and does not call `onUnauthorized`.
- **Routing.** Paths are relative to `basePath`:
  - with `tenant`, the tenant's paths (`/channels/prod`), default `/channels`;
  - without it, the server scope's paths (`/tenants`, `/tenants/acme/channels/prod`), default
    `/tenants`;
  - standalone adds `/server` or `/t/{tenant}`.

  Inside, a memory router; moves are reported by `onNavigate`. Without `onNavigate`, the console
  pushes to `window.history` and ignores `popstate` outside its `basePath`, so two consoles do
  not reset each other. Embedded, `onNavigate` is recommended.
- **`update`.**
  - A changed `tenant` remounts: a new router, the default route, answers in flight dropped. With
    `path` in the same call, `tenant` applies first.
  - An unknown tenant shows "no such tenant" (the `console` route's `404`).
  - A server-scope mount with a tenant token gets `400` from `/api/v1/whoami` and shows "not a
    server administrator".
- **Custom element** `<kista-console api-base tenant audience base-path theme locale>`:
  - observed attributes: `tenant`, `theme`, `locale`;
  - `getToken` and the callbacks are properties, set before or after the element is upgraded; a
    token is never an attribute. The element mounts once `getToken` is set;
  - events: `kista-navigate` (detail `{path, replace}`), `kista-title` (detail the title),
    `kista-unauthorized`;
  - a `navigate(path)` method, a `data-contract` attribute, and `data-kista-theme` for the dark
    defaults.
- **Rendering.**
  - Into the element's open shadow root, one console per element.
  - Styles are constructed stylesheets (`adoptedStyleSheets`), so the host's CSP needs no
    `'unsafe-inline'`. A browser without them would need it; kista supports none.
  - `@font-face` is added to the document once, skipping a family `document.fonts` already has,
    so a platform that also loads tresor's console downloads Manrope once.
  - A minimum width of 960 px; below that, it scrolls.
- **Theme.** tresor-server's CSS variables, on the host element and unprefixed: `--surface,
  --surface-soft, --ink, --ink-muted, --border, --brand, --brand-strong, --on-brand, --focus,
  --success(-soft), --warning(-soft), --danger(-soft), --row`. A platform sets them once for both
  consoles; host values win.
- **Entry gating.** The shell decides whether to show the kista entry with the token it would
  pass in, by the scope's gate above. The platform knows which tenant a user works in: by design,
  kista has no route listing the tenants a token administers (spec 0007: no customer
  enumeration).
- **Versioning.** Additions keep `contract = 1`; a breaking change is 2.
- **Delivery to a shell.**
  - Recommended: the shell proxies kista under its own origin (`/kista/…`), which needs no CORS.
  - Otherwise: `ui.allowed_origins`, and the shell's CSP allows kista's origin in `script-src`,
    `font-src` and `connect-src`.
  - CORS (1b) on `/ui/mfe/*` and `/api/v1/*`, exact origins only:
    - allowed headers: `Authorization, Content-Type, If-Match, If-None-Match, traceparent`;
    - exposed headers: `ETag, Location, X-Request-Id`;
    - `Access-Control-Max-Age: 600`, `Vary: Origin`, no credentials;
    - a preflight (`OPTIONS`) from an allowed origin is answered before the rate limiter and the
      method check; any other preflight gets `403`.
- **Test host.** `npm run host` serves a page on another origin that mounts the console as a
  shell would.

**The platform.** The hugr platform's shell (and Enterest's) lists its components; tresor-server
and kista are two entries, each loaded from its own service by the contracts above. Each also runs
on its own at `/ui/` with its own sign-in. Nothing in kista depends on the platform. The shell's
own spec settles:

- proxying;
- the IdP client that issues tokens for both services' audiences;
- navigation between components.

### Package layout

```text
web/console/                   the console: src/main.tsx (standalone), src/mfe.tsx (contract, 1b), e2e/
web/console/console.go         embed, /ui/ serving, the per-scope CSP
internal/config                + ui:
internal/api                   + /api/v1/console, tenants/{t}/console, issuers/{name}/console, CORS (1b)
internal/store, migrations/*/0015_console.sql   + issuer_console_clients
internal/tenants, internal/audit, cmd/kista     + console clients, their events, kista admin issuer console
Makefile, .github/workflows/ci.yml               + make console; console, console-e2e jobs
```

## Security

- **No new authority.** Every action is a management API call with the user's own token,
  authorized by the API exactly as `curl` is. The console's checks only hide things.
- **Tokens**:
  - in memory only;
  - never in `localStorage`, a URL or an attribute;
  - one session per scope, tenant and issuer;
  - embedded, the host holds them.
- **XSS containment.** An XSS could still use a live token, so:
  - the CSP is strict and per scope: no inline script, no `eval`, `connect-src` limited to kista
    and that scope's IdPs, and a tenant cannot widen another scope's policy;
  - no third-party scripts;
  - React's escaping, no `dangerouslySetInnerHTML`.
- **Phishing.** kista's origin never redirects to a tenant's IdP without showing its host first.
- **Scopes cannot mix.** A server token is used only in the server scope, a tenant token only
  under its tenant's prefix. The API refuses the other way round anyway (spec 0007).
- **Public routes.** `/api/v1/console` names the server sign-in clients. A tenant's `console`
  route follows spec 0007's `404` for unknown and suspended tenants and lists only issuers whose
  administrators opted in. Public client ids are not secrets.
- **Framing** is refused unless `ui.frame_ancestors` allows it. **CORS** allows only configured
  origins, without credentials.
- **Supply chain**: a lockfile (`npm ci`), the licence gate, `npm audit`, Dependabot.

## Testing

- **Go**:
  - `/ui/` serving: the fallback, `404` for missing assets, methods, `<base href>`, cache headers,
    the ETag;
  - the per-scope CSP: a tenant's issuer origin never appears in another tenant's or the server's
    policy, and discovery endpoints on other hosts are included;
  - `/api/v1/console`, and a tenant's `console` route (`404` for unknown and suspended, the
    per-client audience);
  - issuer console clients:
    - the shared store suite on three dialects (set, replace, removed with the record);
    - the API: `If-Match` with the issuer id, and a re-added record not inheriting;
    - the CLI and the events coverage test;
  - CORS and preflight (1b); the `ui:` validation.
- **Unit** (Vitest, jsdom, Testing Library):
  - the API client: If-Match, one renewal per `401`, none on `404` or `400`, the throttle, the
    token bound to its scope prefix;
  - the renewal schedule;
  - the contract (1b): `mountKista`, the element, `update` with `tenant`, events, routing under
    `basePath`, `popstate` outside it ignored, a rejected `getToken`.
- **e2e** (Playwright, Chromium, against `kista serve`, with Keycloak in docker as tresor-server
  does):
  - sign-in in both scopes;
  - a tenant administrator yanks and purges a release;
  - an extension administrator sees only its extension;
  - a token without admin or audit sees nothing to administer;
  - `/ui/t/other` shows the IdP host and does not redirect;
  - an expired session's popup sign-in keeps a form;
  - (1b) the micro-frontend in the test host on another origin, two instances on one page: the
    token per audience, `renew` semantics, theme, `onUnauthorized`, navigation.

## Alternatives considered

- **A server-rendered UI with a cookie session**: CSRF, a session store across replicas, and no
  micro-frontend.
- **Module federation** couples the host's and kista's build versions. **An iframe**: sizing,
  navigation and the token cross a frame by `postMessage`, which is more contract, not less.
  tresor-server rejected both. The ES module plus custom element can move to federation later
  without a contract change.
- **One sign-in for both scopes**: spec 0007 makes a token one or the other by design.
- **One CSP for all of `/ui/`**:
  - every tenant's IdP origins would be public;
  - the header would grow with the tenants;
  - any tenant could widen the policy the server administrators' console runs under.
- **Tenant console clients in server config**: a hosted deployment's tenants bring their own IdP,
  and only they can register a client there.
- **A component library** (Radix, shadcn): its injected styles need `'unsafe-inline'`.

## Follow-ups

- **The platform shell's spec**, outside this repository: proxying, its IdP client, navigation
  between components.
- **A shared Hugr Lab design-token package**, replacing the copies once there is a third console.
- **Localisation**: `locale` is reserved.
- **Release signatures in the API**, if a release's page should list them.
- **Enterest's cabinet views** mount beside this console by the same contract; they live in
  Enterest's repository.
