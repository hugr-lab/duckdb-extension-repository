# Spec 0044: Egress hosts, a server-wide host allowlist for what kista fetches for tenants

- **Status**: implemented
- **Date**: 2026-11-05
- **Author**: vgsml, Claude

## Summary

kista's egress client (spec 0006) decides by address: any public address on 443 passes, a private
one only when `egress.allow` names its CIDR. So a tenant's administrator can point an upstream, an
issuer or a feed at any host on the internet, and kista fetches from it. This spec adds a second,
narrower gate: `egress.hosts`, a file-only list of host names (exact, or a wildcard over
subdomains) and IP literals, turned on explicitly by `egress.hosts_enforce: true`. When it is on,
the tenants' egress client fetches only from a URL whose host is on the list, and the address rules
(and ports, which stay with `egress.allow`) still apply to every address dialed: both must pass. The
check is on the name, before any DNS resolution or connection, in kista itself, because kista dials
(and asks a proxy to `CONNECT` to) checked addresses, never names. A refused host gets its own
answer that names it (`400 egress-host` on an add, recorded as an audited refusal; the run's
error), so a tenant knows to ask the server's operator. Operator-configured destinations (server
issuers, credential token endpoints, event sinks, the community sync) are not narrowed: their URLs
are already in the same file. Without `hosts_enforce` nothing changes; Enterest turns it on.

This implements the decision recorded in Enterest's spec 0017 (merged on Enterest's main,
2026-11-05): kista's own outbound traffic is narrowed by host name through a server-wide
allowlist; a customer's mirror or identity provider works only after an operator adds its host.

## Problem

On a hosted service the tenants' administrators are customers. Today each of them can make kista
fetch from any public https host:

- an upstream's prefix (`repository` kind), read at add, at every run, at every lazy or
  pull-through fetch, by the licence relay and the usage forwarder;
- an issuer's URL, its discovery document's `jwks_uri` (often another host: Google's discovery is on
  `accounts.google.com`, its JWKS on `www.googleapis.com`), read at add and refreshed while serving;
- a feed's URL (spec 0011), read at add and at every feed run.

The address rules (spec 0006) stop SSRF into private networks and cloud metadata, but not the
other uses of an open egress: kista as a fetcher of arbitrary public content on the operator's
bandwidth and IP reputation, and as a probe of third-party hosts. Enterest wants to know, and
approve, every host its kista talks to on a customer's behalf.

A network-level filter cannot do this for kista. A Kubernetes NetworkPolicy has no host names
(spec 0025), and kista asks its proxy to `CONNECT` to the address it checked (spec 0006), so a
filtering proxy sees an IP address, not a name. The check has to be in kista.

## Design

### Where kista's outbound calls go

Every outbound call, by the client it goes through. **Tenant** destinations are named by a
tenant's records (or by the bootstrap writing such records); **operator** destinations are named
by the configuration file alone.

| Call | Client | Destination named by | Class |
| --- | --- | --- | --- |
| Issuer add: discovery, then the JWKS (`tenants.AuthAdmin`) | `app.Egress` | the tenant's issuer record (URL, explicit `jwks_uri`) and the discovery document | tenant |
| Issuer JWKS and discovery refresh while serving (`auth.Verifier` in `serve`) | `app.Egress` | as above | tenant |
| Console origins of a tenant's console clients (spec 0015, discovery only) | `app.Egress` | the tenant's issuer record | tenant |
| Upstream add: `.well-known`, the kista probe (`-/revocations`) (specs 0009, 0039) | `app.Egress` | the upstream's prefix, or the built-in hosts of `duckdb-core` and `duckdb-community` | tenant |
| Upstream runs, lazy and pull-through fetches, revocations, names (specs 0009, 0032, 0039) | `app.Egress` | as above | tenant |
| A bundle export's `.well-known` read of its plan's sources (spec 0014) | `app.Egress` | the sources' prefixes | tenant |
| Licence relay (`-/licenses`, spec 0012) and usage forwarding (spec 0012 phase 1c) | `app.Egress` | the upstream's prefix | tenant |
| Feed runs: `attestation-keys`, `/feed`, documents (spec 0011) | `app.Egress` | the tenant's feed URL | tenant |
| The bootstrap's issuer and upstreams (specs 0020 phase 4a, 0024) | `app.Egress` (through the services above) | the configuration, written as tenant records | tenant |
| The community sync (spec 0030 part 3) | `app.Egress` today; its own client after this spec | `reserved.community_sync.url` | operator |
| Server issuers' discovery and JWKS, the server console's origins (spec 0007) | `app.ServerEgress` | `auth.server_issuers` and their discovery documents | operator |
| Private upstreams' token endpoints (spec 0009 phase 3) | `egressWith` per credential | `upstreams.credentials[].token_url` | operator |
| OTLP event sinks (spec 0010) | `egressWith` per sink | `events.sinks[].url` | operator |
| Blob stores, cloud identities and KMSs, Vault and OpenBao, the database, OTel metrics | their own transports, not egress | the configuration | infrastructure |
| `kista duckdb …`, `kista bundle …` (CLI tools on an operator's machine) | their own clients | the command line | not the server |

Bundle-mode upstreams and bundle-mode feeds (spec 0014) contact nothing: they are exempt from the
add check and from the start-time warning.

### The rule

**`egress.hosts` narrows the tenants' client only.** `app.Egress` (the tenant class) applies the
list; `ServerEgress`, the credential posters, the OTLP sinks and the community sync do not.
Operator destinations are fixed in the same file-only configuration, so a list would only repeat
them. The community sync moves to a client of its own (`app.CommunityEgress`: `egress.allow`, no
host list), so its host (`codeload.github.com` by default) need not be on a list every tenant can
use.

**Shape.** In the existing file-only `egress` block:

```yaml
egress:
  hosts_enforce: true                 # the switch: off (absent, false) means no host narrowing
  hosts:                              # with hosts_enforce, the only hosts the tenants' client reaches
    - extensions.duckdb.org           # duckdb-core upstreams
    - community-extensions.duckdb.org # duckdb-community upstreams
    - login.microsoftonline.com       # Entra issuers (discovery and JWKS)
    - "*.okta.com"                    # any subdomain, at any depth; not okta.com itself
    - 203.0.113.10                    # an IP literal, matched exactly
  allow:
    - cidr: 10.20.0.0/16              # unchanged: private networks, and the ports other than 443
      ports: [443, 8443]
```

An entry is one of:

- a **host name**, matched exactly after normalisation: IDNA-mapped (`idna.Lookup`, as egress
  already normalises the own host), lowercase, a trailing dot dropped;
- a **wildcard** `*.<domain>`: any name ending in `.<domain>`, at any depth, never `<domain>`
  itself. `*` is only the whole leftmost label; `<domain>` has at least two labels and its last
  label is not all digits (`*`, `*.com` and `*.0.10` are refused as typos, not as policy: the list
  is trusted operator input);
- an **IP literal** (IPv4, or IPv6 without brackets), compared as a `netip.Addr` after `Unmap`. No
  wildcard, no CIDR (ranges are `egress.allow`'s). A name entry whose last label is all digits must
  parse as an address.

Entries carry **no port**. Ports stay with the address rules: a public address is dialed on 443
only, and another port needs an `egress.allow` CIDR entry naming it, as today.

**Matching.** A URL host that parses as an IP address matches only IP entries (after `Unmap`, so
`::ffff:203.0.113.10` matches `203.0.113.10`); any other host is normalised as above and matches
name and wildcard entries only. With the list on, a host that fails IDNA mapping is refused (today
`normalHost` falls back to the raw name).

**Both must pass.** The host rule is checked on the name before anything else happens; then, for an
allowed host, egress resolves the name and checks every address and the dialed one exactly as today
(special-purpose ranges, the always-refused metadata ranges, `egress.allow` and its ports, the own
`serve.public_url` host, https only, no redirects). A host on the list that resolves to a private or metadata address
is refused as today; a private IdP needs both its name on `egress.hosts` and its network in
`egress.allow`. Spec 0006 rejected "allowlisting egress by host name" because a name can resolve to
a metadata address: this list never lets an address past the address rules, it only removes names.

**Where it is checked.** In `egress.Client.dial`, on the name the transport dials: this is the
authoritative check, since it is the name that is resolved and connected to. `checkURL`, which every
method (`Get`, `Post`, `PostForm`, `PostJSON`, `Download`) calls first, checks the URL's host too,
for an early and clear error (the two agree, since no redirect is followed). A refused host is never
resolved, never dialed, and never sent to `egress.proxy`. TLS verification and SNI stay on the name.
`doErr` and `Download`'s error mapping keep a `*HostError` (`errors.As`) instead of flattening it to
`ErrRefused`.

**On and off.** The switch is explicit, so the narrowing can never fail open by accident:

- `hosts_enforce` absent or `false`, and no `hosts`: no host narrowing, today's behaviour;
- `hosts_enforce: true` with a list: only the listed hosts; `hosts: []` refuses every host (a site
  that takes bundles only);
- `hosts_enforce: true` with `hosts` missing or `null`: a configuration error;
- `hosts` set without `hosts_enforce: true`: a configuration error (a list that would sit unused).

`app.egressWith` passes the tenants' client a non-nil matcher whenever `hosts_enforce` is on, so an
empty list never becomes nil (no narrowing) on its way. kista logs the state at start
(`egress: host list off` or `egress: host list on, N entries`).

**The configuration's own tenant records.** With the list on, configuration refuses a bootstrap
issuer (URL, explicit `jwks_uri`) or a bootstrap upstream (its prefix, or the built-in host of its
kind) whose host is not on it, so the mistake stops the start with the entry named. A host the
configuration cannot see (a discovered `jwks_uri`) is refused when the bootstrap runs: `transient()`
treats `ErrHostNotAllowed` as permanent, so the plan item becomes `refused`, naming the host,
instead of being retried for four minutes.

**Changes take effect at restart**, as for the rest of the file (`configuration.md`: "the file is
read at start"). Replicas of one deployment read one file; during a rolling update a host may be
allowed on one replica and not yet on another.

**Code.**

- `egress.Config` gains `Hosts *Hosts` (nil: off; non-nil, possibly empty: on), built by
  `egress.NewHosts` from the validated entries. The matcher is a set of exact names, a set of
  wildcard domains and a set of addresses; a name is checked by walking its parent domains, so the
  cost does not grow with the list and no limit on its length is needed.
- `egress.Client.HostAllowed(raw string) error` (nil, or a `*HostError`) serves the callers'
  pre-checks.
- `config.Egress` gains `HostsEnforce bool` and `Hosts []string` with the validation above.
- `upstream.Service`, `tenants.AuthAdmin`, `attachment.Feeds`, `licenses.Service` and
  `usage.Forwarder` gain a host-checker field (an interface with `HostAllowed`; nil means off). It
  is set where they are built (`app.NewServices`, and `serve` for the forwarder), so the test fakes
  of their fetchers stay unchanged.

### What a tenant sees

A refused host is an error of its own, distinct from "cannot be reached": it is decided on the name
alone, before DNS, so naming it tells the tenant nothing about internal names.

- **The error.** `egress.ErrHostNotAllowed` wraps `egress.ErrRefused` (existing classifications
  still see a refusal); a `*egress.HostError` carries the host and says
  `egress: refused: <host> is not on this server's egress host list`.
- **API.** A new problem type `egress-host`, status `400`, with a `host` member and the detail
  `<host> is not among the hosts this server may reach (egress.hosts); the server's operator adds
  it`. It answers:
  - **upstream add** (`POST …/upstreams`): the prefix's host, or a `duckdb-core` /
    `duckdb-community` upstream's built-in host, checked first, before any fetch (not for a
    bundle-mode upstream);
  - **issuer add** (`POST …/issuers`): the issuer URL's host and an explicit `jwks_uri`'s host,
    checked first; a discovered `jwks_uri` on a host that is not on the list answers the same, naming
    that host (the Google case);
  - **feed add** (`POST …/feeds`): the feed URL's host (not for a bundle-mode feed).

  The mapping is checked before `ErrIssuerFetch`, `ErrSourceFetch` and `ErrInvalid`, which would
  otherwise turn it into the generic answers. Each such answer is recorded as an **audited refusal**
  with the reason `egress-host` and the host, as `publisher-taken` is (spec 0037,
  `api/manage.go`), so a tenant probing the server-wide list leaves a record.
- **Runs.** An upstream run checks its prefix once at its start: a refused host fails the run before
  its cells, with an error that wraps `errFailed`, so it is scheduled as a failed run (the next
  interval), not as an interrupted one. `last_run.error` is the error above, the `upstream.run`
  event's `error` class is `egress-host`, and no cell record is touched, so adding the host and
  restarting is enough. A feed run fails the same way (`feed.run` class `egress-host`).
- **Lazy mirror and pull-through.** `Puller.fetch` checks the host at its start. For a lazy mirror
  the cell is recorded `failed` with the error as its detail, and the client is answered `404`, as
  spec 0032 does for a fetch that cannot start. A pull-through upstream answers its usual miss and
  records nothing.
- **Licence relay and usage.** Both check the host before `cred.Token`, so no token is requested for
  a refused host. The relay's result carries the error; the usage forwarder records a reason of its
  own, `egress-host: <host>` (not `unreachable`), in its result on `GET …/upstreams/{name}/usage`.
- **Issuer refresh.** The list changes only at restart, and after a restart nothing is cached, so an
  issuer whose host was removed from the list fails at once: its discovery or JWKS fetch is refused,
  and every token of it fails with the reason `issuer host not allowed` (in the token-failure log
  line and the `auth.failure` event, spec 0010). The verifier keeps the last fetch error in
  `jwksEntry` and `discoEntry`, and `key()` returns the cause with the missing key.

### Operator interface

Configuration only, file-only (the `egress` block is file-only already): no API, no CLI command,
no data model, no migration. `kista admin -config <file> check` validates the list as part of
`config.Load`. `server.start` already digests the `egress` section (spec 0010), so a changed list
shows as a changed digest. A server administrator's token can never widen what kista reaches.

### Audit and events

- No new event kind. Each refused add is an audited refusal (reason `egress-host`, the host). The
  `upstream.run` and `feed.run` events gain the class `egress-host`; an `auth.failure` gains the
  reason `issuer host not allowed`.
- Each refusal is logged once, at warn, with the host (and the tenant and record where the caller
  knows them), by the code that refuses: the pre-check's caller, or egress's `checkURL` and `dial`.
- At start, `serve` logs one warning per distinct stored host that is not on the list, with the
  number of records and tenants that name it. It reads, across tenants, `Store.IssuerURLs` (which
  exists, for spec 0023's warning, and is extended with the explicit `jwks_uri`), and two new reads,
  `Store.UpstreamPrefixes` (tenant, name, kind, mode, prefix) and `Store.FeedURLs` (tenant, name,
  mode, URL). It covers stored hosts only: a discovered `jwks_uri` is not stored, so a refused one
  shows only when it is fetched. Bundle-mode records are skipped.
- The list's own history is the configuration's (GitOps, the Helm values) and the `server.start`
  digest.

### Changes to other specs

- **0006**: egress gains the host rule; "`egress.allow` never names hosts" stays true, and
  `egress.hosts` is a separate, narrowing list.
- **0007**: the `egress-host` problem type; server issuers' fetches are not narrowed.
- **0009, 0011, 0024, 0032, 0039**: the add answers and run failures above; the bootstrap check.
- **0010**: the class `egress-host`, the audited refusal, the token-failure reason.
- **0012**: the licence relay and the usage forwarder check the host before a token; the
  forwarder's reason `egress-host`.
- **0014**: bundle-mode upstreams and feeds are exempt; a bundle export's `.well-known` read of its
  sources is narrowed.
- **0020** (phase 4a): a bootstrap item refused for its host is `refused`, never retried.
- **0030**: the community sync has its own client.

## Security

- **Fail-closed when on, never off by accident.** The narrowing is on only with
  `hosts_enforce: true`, and then a missing list is a configuration error and an empty one refuses
  everything. With it on, a tenant destination is fetched only when its name is listed and every
  address passes the address rules. The check runs before resolution and before any connection or
  proxy request, in `dial` (authoritative) and in `checkURL`.
- **No new reach.** The list never admits an address the address rules refuse, a port they refuse,
  `http`, a redirect, or the own `serve.public_url` host. Operator clients are unchanged.
- **Own names.** kista keeps only its existing refusal of its own `serve.public_url` host; there is
  no list of names that may never be added. kista's requests to a tenant destination carry no
  credential of the operator's, and the operator's public endpoints (Enterest's web and API, its
  Keycloak) are public anyway. Enterest's token service is every tenant's issuer, so it is on
  Enterest's list. The real risk is a host that trusts requests by their source address: such a host
  must not trust kista's egress address.
- **Deployment note for Enterest.** Source-restricted hosts (the `enterest-ops` realm's host,
  Keycloak's admin console) must not allow kista's egress address (its NAT gateway or egress IPs);
  restrict them to the operators' networks instead. The deployment documentation says so.
- **DNS of listed names.** Whoever controls a listed name's DNS chooses the addresses kista
  connects to (within the address rules): TLS fails for an address that is not the name's, but a
  connection is made. So list names whose DNS the operator trusts, and avoid wildcards over domains
  that customers control (`*.customer-subdomains.example`): each customer could then point a name of
  their own at any public address.
- **What a tenant learns.** The `egress-host` answer says a name is not on the list. That is a fact
  about the configuration, not about DNS or the network: it is decided before resolution, so the
  spec 0006 rule (a name that does not resolve and a name that resolves inside answer alike) holds.
  It lets a tenant probe whether a guessed host is on a server-wide list, which on a hosted service
  may hint that another customer uses it. Accepted by the owner (Decisions); each probe is an
  audited refusal, and per-tenant entries are a follow-up.
- **Name matching.** Names are compared in the form resolution uses (IDNA-mapped, lowercase, no
  trailing dot), on both sides, and a host that fails IDNA mapping is refused, so `EXAMPLE.com.` and
  a Unicode spelling cannot slip past an entry or around a refusal. Addresses are compared as
  addresses, never as text. A wildcard never matches its apex. TLS still verifies the allowed name.
- **Trusted input.** The list is the operator's, file-only; no API or environment variable sets it.

## Testing

- **egress (unit)**:
  - entry parsing: exact names, wildcards, IPv4 and IPv6 literals, IDNA, a trailing dot;
  - entry refusals: `*`, `*.com`, `*.0.10`, `a.*.b`, `*x.com`, a scheme, a path, a port, a CIDR,
    a bracketed address, an all-digit last label that is no address, an empty entry;
  - matching: case, trailing dot, a Unicode URL host, a host that fails IDNA mapping (refused),
    wildcard depth, the apex refused by a wildcard, an IP host against name entries (refused) and
    against IP entries (`::ffff:` forms after `Unmap`);
  - nil means off, an empty non-nil matcher refuses all;
  - a refused host is never resolved (the `Resolve` hook is not called) and never dialed; the `dial`
    check refuses on its own when `checkURL` is bypassed;
  - a listed host resolving to a private or metadata address is refused as today; a listed host
    with its private address in `egress.allow` passes, either alone does not; a non-443 port on a
    public address is still refused;
  - through a fake `egress.proxy`, a refused host sends no `CONNECT`;
  - `doErr` and `Download` keep `*HostError`; `HostAllowed` agrees with the fetch path.
- **config**: the validation messages; `hosts_enforce` with `hosts` missing or `null` refused;
  `hosts` without `hosts_enforce` refused; a bootstrap issuer or upstream (each kind) off the list
  refused; `egress.hosts` and `hosts_enforce` are file-only.
- **app**: `Egress` applies the list; `hosts_enforce: true` with `hosts: []` builds a client that
  refuses every host (the empty list does not become nil in `egressWith`); `ServerEgress`, credential
  posters, sinks and the community sync's client are not narrowed (a server issuer whose host is
  not listed still verifies tokens).
- **upstream**:
  - add refused with `egress-host` for a repository prefix, a core and a community upstream; a
    bundle-mode upstream with an unlisted prefix is added, and the start-time pass does not warn
    about it;
  - a run with a refused prefix fails before its cells, changes no cell record, records the class,
    is scheduled as a failed run (not interrupted), and runs normally once the host is listed;
  - a lazy ask records the cell failed and answers `404`; a pull-through miss answers as usual and
    records nothing;
  - a bundle export's `.well-known` read of a source on an unlisted host is refused with the host;
  - the relay and the usage forwarder refuse before `cred.Token` (a credential fake counts token
    requests); the forwarder's reason is `egress-host`.
- **tenants and auth**: issuer add refused for its URL, an explicit `jwks_uri`, and a discovered
  `jwks_uri` on an unlisted host (named); the verifier's `key()` returns the refusal as the cause
  and a token fails with the reason `issuer host not allowed`.
- **bootstrap**: an issuer whose discovered `jwks_uri` is on an unlisted host makes the item
  `refused`, naming the host, without retries; `transient()` is false for `ErrHostNotAllowed`.
- **attachment**: feed add refused; a bundle-mode feed with an unlisted URL is added; a feed run's
  class.
- **api**: the `egress-host` problem and its `host` member on each add; the mapping wins over the
  generic answers; each refusal is recorded as an audited refusal with reason `egress-host`.
- **e2e**: the existing cases unchanged (the list off). The lazy-mirror case's upstream is
  `http://localhost:<port>` (profile dev, loopback http), so with `egress.Config.Hosts` set
  directly to a matcher holding `localhost` it installs as before, and with a matcher not holding it
  the DuckDB client gets `404`.

## Decisions

The owner, 2026-11-05:

1. **Absent means no narrowing, as today; the narrowing is turned on explicitly** with
   `egress.hosts_enforce: true`, so it can never fail open by accident: with it on, an empty list
   refuses everything and a missing or `null` list is a configuration error. Self-hosters change
   nothing; Enterest turns it on.
2. **No never-list of own names.** kista keeps only its refusal of its own `serve.public_url` host.
   A server-issuer-host rule (in the first draft) is dropped: Enterest's token service is every
   tenant's issuer and must be on the list, and Keycloak is not kista's issuer. kista's requests
   carry no credentials and the operator's public endpoints are public anyway; the real risk,
   source-address-restricted hosts trusting kista's egress address, is a deployment rule (Security).
3. **Probing the server-wide list is accepted.** Every refused add is an audited refusal with the
   reason `egress-host`. Per-tenant entries are a follow-up.
4. **Changes apply at restart.** No reloaded hosts file.

## Alternatives considered

- **Filtering in a proxy or the NetworkPolicy.** kista CONNECTs to checked addresses, and a
  NetworkPolicy has no names. Sending `CONNECT host:port` instead would let the proxy resolve, and so
  undo the address check that stops rebinding.
- **One list for every client, with the configuration's own hosts added implicitly.** The server
  issuers' hosts would then be reachable by tenants too, and a server issuer's discovered JWKS host
  is not in the file.
- **The list's presence as the switch (absent: off, present: on).** An empty list that a template or
  a merge drops, or that code turns into nil, would silently mean "no narrowing". An explicit switch
  makes that a configuration error (Decision 1).
- **Ports in host entries.** Two places would decide ports; the address rules already do.
- **A never-list of own names** (the public URL's host and the server issuers' hosts). Wrong for
  Enterest, whose token service must be listed, and no protection the address-trust rule does not
  give (Decision 2).
- **A per-tenant list.** Finer, and no probing across tenants, but it needs a data model, an API
  and an approval flow; the decision is a server-wide list. A follow-up.
- **An API or CLI to edit the list.** What kista may reach is decided by the file, like the rest of
  egress; an API would let a server administrator's token open egress.
- **Host names in `egress.allow`.** That list admits private addresses; mixing names into it would
  make a name a way past the address rules (spec 0006's rejected alternative).
- **Implicitly allowing `extensions.duckdb.org` and `community-extensions.duckdb.org`.** A hidden
  rule; an operator who turns the list on lists them, and the bootstrap check catches a missing one.
- **A `refused_host` field on issuers, upstreams and feeds.** Dropped as more than needed: the add
  answer, the run's error and the start-time warning cover it.
- **A reloaded hosts file.** Not needed: changes apply at restart (Decision 4).

## Delivery

One PR on `0044-egress-hosts-impl`: egress, config, app, upstream, tenants and auth, bootstrap,
attachment, licenses, usage, api, store (the two new reads), and the website pages below.

**Docs** (`website/`): `configuration.md` (`egress.hosts_enforce` and `egress.hosts` in the egress
table, the switch's errors, the bootstrap check), `upstreams.md` (the Egress section: both rules,
ports with `egress.allow`, the built-in hosts, the add and run errors, the lazy and pull-through
answers), `access.md` (issuers: the issuer's and its JWKS's hosts, the Google and Entra examples,
the refusal reason), `attachments.md` (feed hosts, bundle-mode feeds exempt), `licensing.md` (the
relay and usage reasons), `security.md` (the egress guard, DNS of listed names, source-restricted
hosts, the threat table), `observability.md` (the class, the reason, the audited refusal, the
warnings), `api.md` (`egress-host`), and `deployment/kubernetes.md` (the NetworkPolicy cannot filter
by name; `egress.hosts` does it in kista; egress IPs never trusted by source-restricted hosts).

### As built

- **egress.** `internal/egress/hosts.go`: `NewHosts` (never nil), `CheckHostEntry` (the entry
  rules, for the configuration's messages), `(*Hosts).Check(url)` (nil on a nil matcher),
  `ErrHostNotAllowed` and `*HostError`. `Config` gains `Hosts` and `Log`; `checkURL` and `dial`
  both call one `refusedHost`, which logs the refusal at warn with the host; `doErr` (now also
  `Download`'s mapping) returns the `*HostError` unchanged. `Client.HostAllowed` logs nothing;
  `egress.CheckHost(checker, url, log, args...)` is the callers' pre-check (nil checker: off),
  logging with the caller's tenant and record, so a refusal is logged once either way.
  `Client.HostsOn()` is the start log's.
- **config.** `Egress.HostsEnforce` and `Egress.Hosts`; `Config.EgressHosts()` is the matcher (nil
  when off). `hosts: []` decodes as an empty, non-nil list, so `[]` and `null` stay apart. The
  bootstrap check names the field: `bootstrap.issuer.url`, `bootstrap.issuer.jwks_uri`,
  `bootstrap.upstreams[i]`. DuckDB's two URLs moved to `upspec` (`CoreURL`, `CommunityURL`,
  `BuiltIn(kind)`), which the configuration already imports; `upstream.CoreURL` and `CommunityURL`
  are the same constants.
- **app.** `egressWith` takes the matcher: `Egress` passes `cfg.EgressHosts()`; `ServerEgress`, the
  credential posters, the sinks and the new `CommunityEgress` pass nil. The services' `Hosts` is the
  tenants' client itself (`app.NewServices`; `serve` for the forwarder).
- **Issuers.** `tenants.AuthAdmin.IssuerJWKS` checks the URL's host and an explicit `jwks_uri`'s,
  discovers, and checks the discovered one's; issuer add and the bootstrap's plan both use it. In the
  plan, a refused host is the issuer item `refused` (its detail the `HostError`), so `apply` answers
  `ErrRefused` without retrying; `transient()` is false for `ErrHostNotAllowed` before the other
  classes (it may be wrapped in `ErrIssuerFetch`), and `classify` makes a bootstrap upstream refused
  by host a refused item.
- **Upstreams.** The check is in `CheckSource` (so `CheckAddAll` and the bootstrap have it), at the
  start of `run` (wrapping `errFailed`: `last_run.error` reads `the run failed: egress: refused:
  <host> …`, as other run failures carry that prefix), in `Puller.fetch` once the cell's lease is
  held (where the keys' failure is), and before a bundle export's `.well-known` read. A refused
  lazy or pull-through fetch is the outcome `failed` to the puller, so the cell is not offered again
  for a minute (`failedTTL`) and a miss storm logs no line per request.
- **Feeds, relay, usage.** `attachment.Feeds.Add` checks an online feed's URL before its
  credential; `run` checks before the credential's token. `licenses.Service.fetch` checks before
  `Credentials.For`; the forwarder before any day, and `post` maps a `*HostError` from egress to the
  same reason `egress-host: <host>`.
- **Tokens.** `jwksEntry` and `discoEntry` keep `lastErr`; `key()` returns it with a missing key,
  and `VerifyIdentity` fails with `auth.ReasonIssuerHost` (`issuer host not allowed`, the log
  line's) when it is `ErrHostNotAllowed`. In the `auth.failure` event the reason is
  `issuer_host_not_allowed` (`auth.EventReason`): the event's reasons are one token, as
  `stale_token` and `unnamed_writer` are.
- **API.** `serviceErr` maps a `*HostError` first (after the limit problems): `400 egress-host` with
  `host`, noted as a refusal with `reason: egress-host` and `host`; `authz.refused` gains the field
  `host` in the events catalogue.
- **Start.** `serve` logs `egress: host list on` (`entries`) or `egress: host list off`, and
  `warnEgressHosts` one warning per host off the list (`host`, `records`, `tenants`) from
  `Store.IssuerURLs` (now with `jwks_uri`), `Store.UpstreamPrefixes` and `Store.FeedURLs`, skipping
  bundle-mode records.
- **e2e.** `TestUpstreamLazy`'s mirror runs with the list on (`localhost` listed). The refusal is a
  case of its own, `TestUpstreamLazyHostOff`, on a fresh pair: every cell of the first case's
  mirror is fetched by its end. DuckDB's first request is answered `503` (the ask), its retry `404`.
- **Refusals coalesce.** The event writer merges an actor's refusals with one reason into a first
  event and a count (as for `publisher-taken`), so a run of probes records the first one's `host`
  and how many followed.

**Implemented 2026-11-12.** The code (egress, config, app, upstreams, issuers and the verifier,
the bootstrap, feeds, the relay, usage, the API, the store reads, the start-time warning), every
test the Testing section lists, the e2e lazy-mirror cases, and the website pages. The full
`make test` passes with SQLite, PostgreSQL and SQL Server all required (`KISTA_TEST_REQUIRE_DBS=1`),
`make lint` is clean over the whole tree, and the full `make e2e` passes on DuckDB at
`e2e/DUCKDB_PIN`.

## Follow-ups

- Per-tenant entries narrowing the server-wide list, which would also end the probing noted under
  Security.
- Feeding the list to an FQDN-aware network policy (Cilium, Calico) as a second layer, spec 0025's
  follow-up.
