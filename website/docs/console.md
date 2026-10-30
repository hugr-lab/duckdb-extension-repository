---
title: Administration console
---

# Administration console

kista serves a web console for its administrators at `/ui/`. It does nothing the
[HTTP API](api.md) does not: every action is a management API call made with the signed-in user's
own token, authorised exactly as a `curl` request would be. The console's own checks only hide what
the user cannot do.

The same build is also a **micro-frontend**: the hugr platform's shell can mount it beside other
components. The design is in
[spec 0015](https://github.com/hugr-lab/duckdb-extension-repository/blob/main/specs/0015-console/spec.md).

## Building it in

The console is a React application in `web/console/`, embedded in the `kista` binary at build time.

```bash
make console    # npm ci && npm run build in web/console (needs Node 22)
make build      # go build -o bin/kista ./cmd/kista, embedding web/console/dist
```

`make console` writes `web/console/dist/app/` (the standalone console) and `web/console/dist/mfe/`
(the micro-frontend module). Run it before `make build`. A binary built without it serves a
placeholder page at `/ui/` that says so, and no micro-frontend; everything else works.

Other targets:

| Target | What it does |
| --- | --- |
| `make console-test` | typecheck, unit tests, licence check |
| `make console-e2e` | Playwright against `kista serve` with Keycloak in Docker (run `make console` first) |

## How it is served

- `/ui/` is served on **https** listeners only, like the API; on http it answers `404`.
- `/ui` redirects to `/ui/`. A path without a file extension gets the application (it routes
  itself); a missing file with an extension is `404`. Only `GET` and `HEAD` are allowed.
- Three scopes, each its own page load:
  - `/ui/`: the landing page, which asks where to sign in;
  - `/ui/server/…`: server administration;
  - `/ui/t/{tenant}/…`: a tenant.
- Each scope gets its own **Content Security Policy**: no inline script, `connect-src` and
  `form-action` limited to kista and that scope's identity providers, and `frame-ancestors` from
  `ui.frame_ancestors` (none by default). A tenant's identity provider never appears in another
  scope's policy.
- `Referrer-Policy: no-referrer` and `X-Content-Type-Options: nosniff` on every answer. Hashed assets
  are cached for a year; `index.html` and the micro-frontend module are `no-cache` with a strong
  ETag, so an upgrade is picked up at once.

`ui.enabled: false` turns off `/ui/`, the console's API routes and CORS.

## Configuration

```yaml title="kista.yaml"
ui:                           # file-only
  enabled: true               # default true
  environment: staging        # a badge in the header; at most 32 characters
  allowed_origins: []         # shells on other origins that mount the micro-frontend
  frame_ancestors: []         # who may frame the standalone console; default none
  connect_src: []             # extra connect-src origins (a proxy, an OTel collector)
  server_clients:             # the console's clients at server issuers; none: no server sign-in
    - issuer: ops             # a name from auth.server_issuers
      client_id: kista-console
      scopes: [openid, profile, offline_access]   # the default
      audience_parameter: ""  # e.g. "audience" for an IdP that takes it as a parameter
```

| Setting | Meaning |
| --- | --- |
| `ui.enabled` | serve the console (default `true`) |
| `ui.environment` | a badge in the console's header |
| `ui.allowed_origins` | exact origins (`https://host[:port]`, `http` only on loopback, no path, no `*`) allowed by CORS on `/ui/mfe/*` and `/api/v1/*` |
| `ui.frame_ancestors` | origins that may frame the standalone console |
| `ui.connect_src` | extra origins the console may connect to |
| `ui.server_clients` | one public (PKCE) client per server issuer, for server sign-in |

A server client's `scopes` must include `openid` (at most 20 scopes). See
[Configuration](configuration.md) for the rest of the file.

## Sign-in

The console signs in with OpenID Connect, Authorization Code with PKCE, as a public client. kista
has two kinds of identity, and a token is one or the other (see [Access](access.md)), so the two
scopes sign in separately.

### Server scope

1. Configure `auth.server_issuers`, `auth.server_admins` and `auth.server_audiences`.
2. Register a public client at the server issuer with the redirect URI
   `https://<public host>/ui/server/callback`, and list it in `ui.server_clients`.
3. Open `/ui/`, choose **Server administration**, pick the issuer.

The console asks for a token for the server audience (the first of `auth.server_audiences`). It
opens only for a token whose `GET /api/v1/whoami` says `administrator: true`; another server token
sees "not a server administrator".

### Tenant scope

A tenant's administrators sign in through the tenant's own issuer records. A record offers console
sign-in once it has a **console client**:

1. Register a public client at the tenant's identity provider with the redirect URI
   `https://<public host>/ui/t/<tenant>/callback`.
2. Attach it to the issuer record:

   ```bash
   kista admin -config kista.yaml issuer console acme corp-idp -client-id kista-console
   # [-scopes openid,profile,offline_access] [-audience api://kista-acme] [-audience-parameter audience]
   kista admin -config kista.yaml issuer console acme corp-idp -remove
   ```

   Over the API: `POST /api/v1/tenants/{t}/issuers/{name}/console` with
   `{client_id, scopes, audience_parameter, audience}` and `If-Match: "<issuer id>"`; `DELETE` removes
   it.
3. Open `/ui/`, type the tenant's name, and pick the issuer.

`audience`, when set, must be one of the tenant's audiences; otherwise tokens are requested for the
canonical `<public_url>/<tenant>`. Setting a console client makes the issuer's URL public through
`GET /api/v1/tenants/{t}/console`. A server issuer cannot be a tenant's console issuer.

The console opens for a token whose tenant `whoami` shows `holds_admin` (an `admin` grant on the
tenant, a channel, an extension or an extension group) or `holds_audit`. A token holding neither
sees "Nothing to administer here".

### Sessions

- The landing page never redirects to an identity provider on its own: it shows the provider's host
  and waits for a click.
- Access and refresh tokens are kept in memory only; only the PKCE state is in `sessionStorage`.
- Management needs a token issued within `auth.admin_token_max_age`. The console renews through the
  refresh token before that, and before the token expires. If renewal fails, a banner offers a
  popup sign-in, so a form being edited is kept.
- A `401` triggers one renewal and one retry. A `404` means "not there, or not yours".
- Every change sends the `If-Match` it read; a `412` re-reads and says the object changed.
- A `429 tenant-limit` or `catalogue-full` is shown with its detail (which limit, or the source's
  cap) and never retried: waiting does not clear it (spec 0038). Other `429`s (a busy server) are
  retried after their `Retry-After` where the console retries (a bundle's chunks).
- Sign-out revokes the refresh token where the provider supports it, then ends the provider session.

## Screens

The tenant's sections appear according to the caller's grants: tenant administrators see all of
them; channel and extension administrators see their channels and extensions; auditors see events.

| Section | Who | What it does |
| --- | --- | --- |
| **Channels** | any administrator | the channels; a new channel (tenant administrators); a channel's DuckDB versions added and removed (its administrators) |
| Channel: Extensions | channel administrators; an extension administrator sees its own extensions | the channel's extensions from the index, by name prefix, with the current version for a chosen DuckDB version and platform |
| Channel: All releases | channel administrators | every release, filtered by state and name prefix, with its source, whether it is served, and whether its distributor withdrew it |
| Channel: Sources | channel administrators | each extension's [rule](catalogue.md): its [source](upstreams.md#a-names-source-in-a-channel) in the channel, the rule's mode (`auto`, `manual`, `follow`, or as it lands), the version the channel holds and, when newer, the newest available in the catalogue (not held, revoked or withdrawn), the placement pass's last outcome, who set it and when, the sources with releases of it. **Change…** switches the source (to a source with releases there, the tenant, or any `upstream:<name>` or `publisher:<name>` of the tenant) and the mode (never `auto` for the tenant; a switch without a mode keeps the rule's, but a switch to the tenant turns `auto` to `manual`, and a follow's rule moved to another source becomes `manual`; the dialog says which); **Place…** places a version of the source by hand (with `promote` on the name; early for a held version); **Clear…** removes the rule. In a signed channel, the channel's **hold** (`24h`, `2d`, `0` for none) and its **Follows**: each followed source with the names it skips and why (as of the placement pass's last look), followed and unfollowed there |
| Channel: Keys | channel administrators | trusted, active and serving keys, key events, re-sign progress; activate and retire a trusted key; a server administrator adds a key by signer reference |
| Extension page | extension administrators | its releases, its **Reports**, and (tenant administrators) who may use it and through which grant |
| Release page | extension administrators | build, body hash, provenance, origin, state history; yank, deprecate, activate, make current, public or private, purge (typing the name to confirm); its Reports |
| **Catalogue** | tenant administrators | the tenant's [catalogue](catalogue.md): every source and name with its icon (24 px) and summary (spec 0036), its versions (held, revoked and withdrawn marked) and the channels whose rules take it, by name prefix and source (a removed source's entries are listed without a page: they are not read by name); an entry's page with its icon (64 px) and summary and, on the tenant's own sources (never a distributor's entry), **Edit…**: the summary, a PNG file, or removing the icon (a summary-only edit sends the icon back by its hash); then each version's builds, their state (held until when, withdrawn and why, revoked), visibility and where each is placed. **Place…** places a version into a signed channel where the caller holds `promote` on the name (which `admin` does not imply), privately, not current, or early for a held version (a channel administrator's); **Purge…** deletes a build placed nowhere; a server administrator withdraws a distributor's version with **Withdraw…** and a reason |
| **Upstreams** | tenant administrators | upstreams, each with its home channel and a mirror's [hold](upstreams.md#holds) (read only: set with the CLI or the API): add, sync and dry run, pause, resume, public, private, remove; platforms, pinned keys, credential, allowlist (sync, dry run and the credential not for bundle mode, which shows its last bundle); a mirror's fetch mode, eager or [lazy](upstreams.md#lazy-mirrors), set when it is added and switched later, its entries' eager mark, and for a lazy mirror **Fetch now** (its whole matrix) and the cells not asked for yet; last run and cells; a repository upstream's usage forwarding (on or off) and its last run; the tenant's shadows; **Feeds**; in the server scope, **Upload a bundle** |
| **Groups** | tenant administrators, group administrators (read only) | extension groups: members, description, grants, recent events, removal |
| **Access** | tenant administrators | principals with their grant counts; a principal's grants; grant and revoke |
| **Identity** | tenant administrators | issuer records and their console clients; the tenant's audiences (changed by a server administrator) |
| **Publishers** | tenant administrators | publishers, their keys (the public SSH keys their CI signs with: register, remove, last used), and what they may publish; each publisher's [hold](upstreams.md#holds) (for a [distributor's publisher](publishing.md#distributors-publishers), its effective hold) and, for a distributor's publisher, its distributor (both set with the CLI or the API). A publisher's page shows its `id` (spec 0037). The **Minimum hold for distributors' publishers** card shows and sets the tenant's [minimum](access.md#the-minimum-hold-for-distributors-publishers) (spec 0037; a caller who may not read it does not see it). A release made by a publisher shows its signed request |
| **Blocks** | tenant administrators | block a body by its hash with a reason (its releases are yanked in every channel); unblock |
| **Licences** | tenant administrators | the tenant's licences, each with its catalogue **Entry** (spec 0035) (expiring within 30 days flagged); a licence's claims and limits; import a file into an entry: the dialog reads the file's extension and key, lists the entries whose live keys include it, each with its kind and, for a distributor's publisher, the distributor, preselects one only when it is the only one, and lets another be named (kista's `400 kid_untrusted` is shown); the licence stored in that entry is replaced after a confirmation; public or private, remove. A distributor's publisher's licence is read-only to a caller who is not a server administrator (spec 0037: no Remove, Make public or Make private; its licence manager writes it), and the import dialog lists such an entry without letting them choose it ("a distributor's: its licence manager writes it"); a server administrator's console does not change; each relaying upstream's last run, refusals and **Sync now**; the licence keys of the tenant's live builds, each with its entries and their builds, with a licence that no live build of its entry declares flagged. A publisher marked as a source of licence keys is badged on **Publishers**. See [Licensing](licensing.md) |
| **Limits** | tenant administrators | the tenant's [limits](administration/tenants-and-channels.md#limits) (spec 0038), read only: for bytes, channels, upstreams and publishers, what is used, what applies (a reached limit marked) and what is set (`default`, `unlimited` or a value). A server administrator sets them with the CLI or the API |
| **Events** | auditors | the tenant's events with the API's filters, and an event's detail |
| **Statistics** | administrators, auditors | **Downloads**: per day over 7, 30 or 90 days, and by extension, version or platform; **Usage**: the users per day, and users and events by day, extension or version, with use without a valid licence and counts received from downstream kistas apart; within the caller's grants |

**Reports** lists the documents attached to a release's body: kind, title, origin (the tenant, a
publisher, or a feed and its verified issuer), date, expiry, and summary. Administrators download a
document (saved as a file, never opened) and remove it, and attach one on a release's page; tenant
administrators also mark it `readers` or feedable. **Feeds**, on the Upstreams page, lists subscriptions with their status, last run and last
success; they are added, synced, paused, resumed and removed there. Changing a feed's credential,
keys, kinds or readers is done with the API or the CLI. See [Attachments and the feed](attachments.md).

The server scope adds:

| Section | What it does |
| --- | --- |
| **Tenants** | every tenant but deleted ones (name, state, storage domain, expected holder); create (optionally with an expected holder), suspend, resume, delete; set and clear a tenant's expected holder; open a tenant with every right |
| **DuckDB versions** | list and add DuckDB versions with their maximum C API per major; add a C API |
| **Server events** | the server's own events |

**Warnings.** A change whose answer carries [warnings](api.md#warnings) (today, an entry holding a
licence served publicly: [Licensing](licensing.md#public-serving-of-licensed-entries)) went
through; the console shows the last change's warnings above the page, saying nothing was refused,
until **Dismiss** (the focus then goes to the page's heading) or the next change; a failed change
clears them. Only an administrator of what the change touched is shown them.

**Uploading a bundle.** In the server scope, a tenant's Upstreams page has **Upload a bundle**: a
file picker, an optional SHA-256 pasted from the export's output (the hex, or its `sha256 …` line),
the upstreams to import into and a dry run. The file is sent in chunks, three at once, with a
progress bar. A chunk the server is busy for is sent again when its `Retry-After` says; one the
network drops, that times out or that the server could not read whole is sent again a few times; the
session's token is renewed as the upload goes. **Stop**, or leaving the page, ends sending at once.
This browser keeps the upload's id: on the page again (after a reload too), it says how many chunks
the server has, and picking the same file (name, size and date) resumes it with the options it
started with, sending only the chunks missing; **Abort it…** gives it up. An upload completed
meanwhile is followed instead, never sent twice. Once every chunk is there, the page follows the
import to its end and shows its state, each target's outcome and counts, the licences the bundle
carried (stored, replaced, unchanged, refused, with each refusal's rule), or why it stopped.
**Recent uploads** lists the tenant's latest 100 uploads, with **Import again** for a failed one and
**Abort…** for one uploading, queued or failed. Uploads abandoned or failed count against
`bundles.max_uploads` until aborted or expired. See [Uploading a bundle](upstreams.md#uploading-a-bundle).

**A tenant's expected holder.** A tenant's page in the server scope shows the holder its licences
must name, or none. **Set holder…** (or **Change holder…**) asks for the holder id, a licence's
`sub` of 1 to 128 bytes; **Clear holder…** removes it after a confirmation. Both send the tenant's
`If-Match`. Setting it while the tenant holds a licence of another holder is refused, and the page
shows kista's message naming those licences: remove them first. The new tenant form takes an
optional expected holder too. See [An expected holder](licensing.md#an-expected-holder).

**Deleting a tenant.** A suspended tenant's page has **Delete…**, enabled once the tenant has been
suspended for `tenants.min_suspended` (until then a note says from when). The dialog says what is
removed and what stays, reminds you to retire the tenant's keys in their key sources, and asks for
the tenant's name typed in full. A deleting or deleted tenant's page shows its deletion instead of
its screens: when and by whom it started, the rows deleted and the passes, the table the reaper
reached, the last error, the next attempt, and the keys to retire (kind, fingerprint, signer
reference). **Refresh** reloads it. See
[Deleting a tenant](administration/tenants-and-channels.md#deleting-a-tenant).

Lists are read page by page (50, 100 or 200 rows) from the API's cursors. Name search is a prefix
search done by the API; the catalogue and a channel's Sources, which the API answers whole, are
filtered in the page. Nothing refreshes in the background, except a running re-sign's progress
every 10 seconds while its view is open, and a completed bundle upload's state every 2 seconds
until its import ends. Event data, provenance and documents are rendered as text.

## Micro-frontend

A shell on another page can mount the console. It loads one ES module from kista and either calls
`mountKista` or uses the `<kista-console>` custom element. The contract is version 1 (the module
exports `contract = 1`).

```js
const m = await import(`${apiBase}/ui/mfe/kista.js`)
const h = m.mountKista(el, {
  apiBase,         // kista's origin or the shell's proxy prefix, no trailing slash
  getToken,        // (audience, {renew}?) => Promise<string>
  tenant,          // optional: a tenant scope; absent: the server scope
  audience,        // optional: overrides the audience kista names
  basePath,        // the host path the console lives under
  theme,           // "light" | "dark"
  locale,          // reserved: "en" only
  onNavigate,      // (path, {replace}) => void
  onTitle,         // (title) => void
  onUnauthorized,  // () => void
})
h.update({ theme, path, locale, tenant })
h.unmount()
```

- **The host owns sign-in.** The console calls `getToken(audience)` before each request and keeps no
  token. `{renew: true}` asks for a newly issued token with a fresh `iat`. The audience comes from
  `/api/v1/console` or the tenant's `console` route unless `audience` overrides it; the host should
  issue tokens only for audiences it knows.
- **Routing.** The console's paths are below `basePath`; `onNavigate` receives the host's full path.
  Without `onNavigate`, the console pushes to `window.history` itself.
- **The custom element** `<kista-console api-base tenant audience base-path theme locale>` takes
  `getToken` and the callbacks as properties (never attributes); without a callback it emits the
  matching `kista-navigate`, `kista-title` or `kista-unauthorized` event instead.
- **Rendering** goes into an open shadow root with constructed stylesheets, so the host's CSP needs
  no `'unsafe-inline'`. The theme follows the host's CSS variables (`--surface`, `--ink`, `--brand`,
  …). The minimum width is 960 px.

**Delivery.** The simplest setup is for the shell to proxy kista under its own origin, which needs no
CORS. Otherwise list the shell's origin in `ui.allowed_origins`, and allow kista's origin in the
shell's `script-src`, `font-src` and `connect-src`. `npm run host` in `web/console` serves a test
shell on another origin.
