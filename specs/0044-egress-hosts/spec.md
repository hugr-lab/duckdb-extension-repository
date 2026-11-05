# Spec 0044: Egress hosts, a server-wide host allowlist for what kista fetches for tenants

- **Status**: draft
- **Date**: 2026-11-05
- **Author**: vgsml, Claude

## Summary

kista's egress client (spec 0006) decides by address: any public address on 443 passes, a private
one only when `egress.allow` names its CIDR. So a tenant's administrator can point an upstream, an
issuer or a feed at any host on the internet, and kista fetches from it. This spec adds a second,
narrower gate: `egress.hosts`, a file-only list of host names (exact, or a wildcard over
subdomains, with an optional port). When it is set, the tenants' egress client fetches only from a
URL whose host is on the list, and the address rules still apply to every address dialed: both must
pass. The check is on the URL's name, before any DNS resolution or connection, in kista itself,
because kista dials (and asks a proxy to `CONNECT` to) checked addresses, never names. A refused
host gets its own answer that names it (`400 egress-host` on an add, the run's error, a field on the
record), so a tenant knows to ask the server's operator. Operator-configured destinations (server
issuers, credential token endpoints, event sinks, the community sync) are not narrowed: their URLs
are already in the same file. No list keeps today's behaviour; Enterest sets one.

This records Enterest's decision of 2026-11-05 (Enterest spec 0017): kista's own outbound traffic
is narrowed by host name through a server-wide allowlist; a customer's mirror or identity provider
works only after an operator adds its host.

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
bandwidth and IP reputation, as a probe of third-party hosts, and as a client of the operator's own
public services (Enterest's web and API, its Keycloak) from inside the trust boundary. Enterest
wants to know, and approve, every host its kista talks to on a customer's behalf.

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

Bundle-mode upstreams (spec 0014) contact nothing and are not affected.

### The rule

**`egress.hosts` narrows the tenants' client only.** `app.Egress` (the tenant class) applies the
list; `ServerEgress`, the credential posters, the OTLP sinks and the community sync do not.
Operator destinations are fixed in the same file-only configuration, so a list would only repeat
them; and keeping them off the list is what lets the server's own identity provider (Enterest's
Keycloak) work for server administrators while no tenant can reach it (below). The community sync
moves to a client of its own (`app.CommunityEgress`: egress.allow, no host list), so its host
(`codeload.github.com` by default) need not be on a list every tenant can use.

**Shape.** In the existing file-only `egress` block:

```yaml
egress:
  hosts:                              # absent: no host narrowing; present: only these hosts
    - extensions.duckdb.org           # duckdb-core upstreams
    - community-extensions.duckdb.org # duckdb-community upstreams
    - login.microsoftonline.com       # Entra issuers (discovery and JWKS)
    - "*.okta.com"                    # any subdomain, at any depth; not okta.com itself
    - repo.acme.example:8443          # a port other than 443
    - 203.0.113.10                    # an IP literal, matched exactly
  allow:
    - cidr: 10.20.0.0/16              # unchanged: private networks that may be reached
```

An entry is `host` or `host:port` (an IPv6 literal in brackets with a port):

- a **host name**, matched exactly after normalisation: IDNA-mapped (`idna.Lookup`, as egress
  already normalises the own host), lowercase, a trailing dot dropped;
- a **wildcard** `*.<domain>`: any name ending in `.<domain>`, at any depth, never `<domain>`
  itself. `*` is only the whole leftmost label; `<domain>` has at least two labels (`*.com` and `*`
  are refused as typos, not as policy: the list is trusted operator input);
- an **IP literal**, matched exactly against a URL that names that address (no wildcard, no CIDR:
  ranges are `egress.allow`'s);
- a **port**: without one, the entry allows 443 only. An entry with a port allows that port only;
  list a host twice for two ports.

A URL matches when its normalised host matches an entry and its port (443 when absent) is the
entry's. The port in a host entry never widens the address rules: a public address is still dialed
on 443 only, and another port needs an `egress.allow` CIDR entry with that port, as today.

**Both must pass.** The host rule is checked on the URL's name before anything else happens; then,
for an allowed host, egress resolves the name and checks every address and the dialed one exactly as
today (special-purpose ranges, the never-list, `egress.allow`, the own host, https only, no
redirects). A host on the list that resolves to a private or metadata address is refused as today;
a private IdP needs both its name on `egress.hosts` and its network in `egress.allow`. Spec 0006
rejected "allowlisting egress by host name" because a name can resolve to a metadata address: this
list never lets an address past the address rules, it only removes names.

**Where it is checked.** In `egress.Client.checkURL`, which every method (`Get`, `Post`,
`PostForm`, `PostJSON`, `Download`) calls before resolving or dialing, and again in `dial` on the
name the transport dials (the same name, since no redirect is followed; the second check guards a
future path that skips `checkURL`). A refused host is never resolved, never dialed, and never sent
to `egress.proxy`. TLS verification and SNI stay on the name, so an allowed name still needs a valid
certificate for it.

**Absent and empty.** `egress.hosts` absent (or `null`) means no host narrowing: today's behaviour,
for self-hosters who rely on the address rules. Present and empty (`hosts: []`) means the tenants'
client reaches no host at all (a site that takes bundles only). kista logs which it is at start
(`egress: host list off` or `egress: host list on, N entries`). Whether absent should instead fail
closed is open question 1.

**Never on the list.** Configuration refuses an entry that matches, exactly or by wildcard, the host
of `serve.public_url` (egress refuses it anyway, so the entry is a mistake) or the host of an
`auth.server_issuers` URL: tenants must not reach the server's own identity provider (Enterest's
Keycloak). Other own services that kista does not know about (Enterest's web and API) are the
operator's to keep off; the documentation says so, and says never to use a wildcard over one's own
domain.

**The configuration's own tenant records.** With a list set, configuration refuses a bootstrap issuer
(URL, explicit `jwks_uri`) or a bootstrap upstream (its prefix, or the built-in host of its kind)
whose host is not on it, so the mistake stops the start with the entry named rather than as a
refused fetch at bootstrap.

**Changes take effect at restart**, as for the rest of the file (`configuration.md`: "the file is
read at start"). Replicas of one deployment read one file; during a rolling update a host may be
allowed on one replica and not yet on another. Whether a separately reloaded hosts file is needed is
open question 2.

**Code.** `egress.Config` gains `Hosts []HostRule` (nil: off) and a logger; `egress.Client` gains
`HostAllowed(raw string) error` (nil, or a `*HostError`) for the callers' pre-checks and the record
fields below. The matcher is a set of exact names and a set of wildcard domains; a host is checked
by walking its parent domains, so the cost does not grow with the list and no limit on its length is
needed. `config.Egress` gains `Hosts []string` with the validation above.

### What a tenant sees

A refused host is an error of its own, distinct from "cannot be reached": it is decided on the name
alone, before DNS, so naming it tells the tenant nothing about internal names.

- **The error.** `egress.ErrHostNotAllowed` wraps `egress.ErrRefused` (existing classifications
  still see a refusal); a `*egress.HostError` carries the host and port and says
  `egress: refused: <host>[:<port>] is not on this server's egress host list`.
- **API.** A new problem type `egress-host`, status `400`, with a `host` member (the host, with the
  port when it is not 443), and the detail `<host> is not among the hosts this server may reach
  (egress.hosts); the server's operator adds it`. It answers:
  - **upstream add** (`POST …/upstreams`): the prefix's host, or a `duckdb-core` /
    `duckdb-community` upstream's built-in host, checked first, before any fetch;
  - **issuer add** (`POST …/issuers`): the issuer URL's host and an explicit `jwks_uri`'s host,
    checked first; a discovered `jwks_uri` on a host that is not on the list answers the same, naming
    that host (the Google case);
  - **feed add** (`POST …/feeds`): the feed URL's host.

  The mapping is checked before `ErrIssuerFetch`, `ErrSourceFetch` and `ErrInvalid`, which would
  otherwise turn it into the generic answers.
- **Runs.** An upstream run checks its prefix once at its start: a refused host fails the run before
  its cells (`last_run.error` is the error above, the `upstream.run` event's `error` class is
  `egress-host`), touches no cell record, and the next run is scheduled as after any failed run, so
  adding the host and restarting is enough. A lazy or pull-through fetch that meets it records the
  cell `failed` with that detail and answers the client `404`, as spec 0032 does for a fetch that
  cannot start. A feed run fails the same way (`feed.run` class `egress-host`). The licence relay
  and the usage forwarder record it in their results (the usage result on
  `GET …/upstreams/{name}/usage`) with the same words.
- **Issuer refresh.** While serving, a refused refresh leaves the cached JWKS in use until it expires
  (plus spec 0006's stale grace), as for any failed refresh; after that the issuer's tokens fail,
  and the verifier (which keeps each JWKS URL's last refresh error) gives the failure the reason
  `issuer host not allowed` (in the token-failure log line and the `auth.failure` event, spec 0010)
  instead of `no key`.
- **Records.** Issuers, upstreams and feeds in the API (and so the console) carry `refused_host`
  when a stored URL's host is not on the list (an issuer's URL or explicit `jwks_uri`; an
  upstream's prefix or its kind's built-in host; a feed's URL): a function of the configuration and
  the record, computed on read, no fetch. It covers records made before the list was set, or before a
  host was removed. The console shows it on the record's page, with the detail above.

### Operator interface

Configuration only, file-only (the `egress` block is file-only already): no API, no CLI command,
no data model, no migration. `kista admin -config <file> check` validates the entries. A server
administrator token can never widen what kista reaches.

### Audit and events

- No new event kind. The `upstream.run` and `feed.run` events gain the class `egress-host`; an
  `auth.failure` gains the reason above. An add refused for its host is a `400`, which spec 0010
  does not record (nothing happened).
- The operator's signal that a tenant needs a host: egress logs a warning
  `egress: a host not on egress.hosts` with the host and port, at most once per host per minute per
  replica. At start, `serve` logs one warning per distinct stored host that is not on the list, with
  the number of records and tenants that name it (the startup pass reads issuer, upstream and feed
  URLs, as it already reads issuer URLs for spec 0023's warning).
- The list's own history is the configuration's (GitOps, the Helm values); kista logs its state at
  start.

### Changes to other specs

- **0006**: egress gains the host rule; "`egress.allow` never names hosts" stays true, and
  `egress.hosts` is a separate, narrowing list.
- **0009, 0011, 0024, 0032, 0039**: the add answers and run failures above; the bootstrap check.
- **0010**: the class `egress-host` and the token-failure reason.
- **0015**: the console shows `refused_host`.
- **0030**: the community sync has its own client.

## Security

- **Fail-closed when on.** With a list, a tenant destination is fetched only when its name and port
  are listed and every address passes the address rules. The check runs before resolution and before
  any connection or proxy request, in the same function every egress method calls. An empty list
  refuses everything.
- **No new reach.** The list never admits an address the address rules refuse, a port they refuse,
  `http`, a redirect, or the own host. Operator clients are unchanged.
- **The server's own services.** Configuration refuses entries covering `serve.public_url`'s host and
  the server issuers' hosts, so no tenant issuer, upstream or feed can target the server's own
  identity provider. Other own hosts are a documented operator duty.
- **What a tenant learns.** The `egress-host` answer says a name is not on the list. That is a fact
  about the configuration, not about DNS or the network: it is decided before resolution, so the
  spec 0006 rule (a name that does not resolve and a name that resolves inside answer alike) holds.
  It does let a tenant probe whether a guessed host is on a server-wide list, which on a hosted
  service may hint that another customer uses it. Accepted: it is what makes the refusal clear, a
  wildcard entry reveals nothing about any one name, and a per-tenant list (follow-up) would remove
  it.
- **Name matching.** Names are compared in the form resolution uses (IDNA-mapped, lowercase, no
  trailing dot), on both sides, so `EXAMPLE.com.` and a Unicode spelling cannot slip past an entry or
  around a refusal. A wildcard never matches its apex. TLS still verifies the allowed name.
- **Trusted input.** The list is the operator's, file-only; no API or environment variable sets it.

## Testing

- **egress (unit)**: entry parsing (exact, wildcard, IP literals, ports, IDNA, a trailing dot) and
  refusals (`*`, `*.com`, `a.*.b`, `*x.com`, a scheme, a path, a CIDR, port 0, an empty entry);
  matching (case, trailing dot, a Unicode URL host, wildcard depth, the apex refused by a wildcard,
  a port mismatch, the default 443); absent means off, `[]` refuses all; a refused host is never
  resolved (the `Resolve` hook is not called) and never dialed; a listed host resolving to a private
  or metadata address is refused as today; a host on the list and its private address in
  `egress.allow` passes, either alone does not; a listed port on a public address is still refused;
  through a fake `egress.proxy`, a refused host sends no `CONNECT`; the own host is refused under a
  covering wildcard; `HostAllowed` agrees with the fetch path; the warning is rate-limited.
- **config**: the validation messages; entries covering `serve.public_url` or a server issuer's host
  refused; a bootstrap issuer or upstream (each kind) off the list refused; `egress.hosts` is
  file-only.
- **app**: `Egress` applies the list; `ServerEgress`, credential posters, sinks and the community
  sync's client do not (a server issuer whose host is not listed still verifies tokens).
- **upstream**: add refused with `egress-host` for a repository prefix, a core and a community
  upstream; a run with a refused prefix fails before its cells, changes no cell record, records the
  class, and runs normally once the host is listed; a lazy ask records the cell failed and answers
  `404`; the relay and the usage forwarder record the error; `refused_host` on the record.
- **tenants and auth**: issuer add refused for its URL, an explicit `jwks_uri`, and a discovered
  `jwks_uri` on an unlisted host (named); a refresh refused after the list changes keeps the cached
  keys until they expire, then fails with the new reason; `refused_host` on the issuer.
- **attachment**: feed add refused; a feed run's class.
- **api**: the `egress-host` problem and its `host` member on each add; the mapping wins over the
  generic answers.
- **e2e**: the existing cases unchanged (no list); one lazy-mirror case with `egress.hosts` naming the
  test upstream's host (`127.0.0.1`) installs as before, and with the list not naming it answers the
  DuckDB client `404`.

## Alternatives considered

- **Filtering in a proxy or the NetworkPolicy.** kista CONNECTs to checked addresses, and a
  NetworkPolicy has no names. Sending `CONNECT host:port` instead would let the proxy resolve, and so
  undo the address check that stops rebinding.
- **One list for every client, with the configuration's own hosts added implicitly.** The server
  issuers' hosts would then be reachable by tenants too, which is what Enterest forbids; and a server
  issuer's discovered JWKS host is not in the file.
- **A per-tenant list.** Finer, and no probing across tenants, but it needs a data model, an API
  and an approval flow; the decision is a server-wide list. A follow-up if needed.
- **An API or CLI to edit the list.** What kista may reach is decided by the file, like the rest of
  egress; an API would let a server administrator's token open egress.
- **Host names in `egress.allow`.** That list admits private addresses; mixing names into it would
  make a name a way past the address rules (spec 0006's rejected alternative).
- **Implicitly allowing `extensions.duckdb.org` and `community-extensions.duckdb.org`.** A hidden
  rule; an operator who sets a list lists them, and the bootstrap check catches a missing one.

## Open questions

1. **Absent list: today's behaviour, or fail closed?** Recommended: absent keeps today's behaviour
   (no host narrowing), so self-hosters who rely on the address rules change nothing, and Enterest
   sets a list in its deployment. The alternative refuses every tenant destination until hosts are
   listed: safer by default, but every self-hosted upstream and issuer breaks until the operator
   lists `extensions.duckdb.org` and its IdP. (kista is unreleased, so either costs no migration.)
2. **Restart to apply, or a reloaded hosts file?** Recommended: restart, as for the rest of the file;
   adding a customer's host is a configuration change and a rolling update. The alternative is
   `egress.hosts_file`, re-read when it changes (as TLS certificates are, at most once a minute),
   which lets Enterest add a host without a rollout at the cost of a second source of the list. It
   can follow if onboarding needs it.

## Delivery

One PR on `0044-egress-hosts-impl`: egress, config, app, upstream, tenants and auth, attachment,
api, the console's `refused_host`, and the website pages below.

**Docs** (`website/`): `configuration.md` (`egress.hosts` in the egress table, the never-listed hosts,
the bootstrap check), `upstreams.md` (the Egress section: both rules, the built-in hosts, the
run and add errors), `access.md` (issuers: the issuer's and its JWKS's hosts, the Google and Entra
examples), `attachments.md` (feed hosts), `security.md` (the egress guard and the threat table),
`observability.md` (the class and the reason, the warnings), `api.md` (`egress-host`,
`refused_host`), `console.md`, and `deployment/kubernetes.md` (the NetworkPolicy cannot filter by
name; `egress.hosts` does it in kista).

## Follow-ups

- A per-tenant list narrowing the server-wide one, which would also end the probing noted under
  Security.
- A reloaded hosts file (open question 2).
- Feeding the list to an FQDN-aware network policy (Cilium, Calico) as a second layer, spec 0025's
  follow-up.
