package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/app"
	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bootstrap"
	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/icon"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
	"github.com/hugr-lab/duckdb-extension-repository/internal/usage"
)

const adminUsage = `usage: kista admin -config <file|env:NAME> <command> ...

  migrate                                         apply pending migrations
  check                                           verify the schema without changing it
  backup <file>                                   consistent copy of a SQLite store
  bootstrap [-dry-run] [-adopt] [-accept] [-rearm]   apply the config's bootstrap section once (spec 0020):
                                                  -dry-run prints the plan (reading signers, issuers and upstreams);
                                                  -adopt creates in an existing tenant;
                                                  -accept takes a changed section as applied; -rearm forgets it was applied
  tenant create <name> [-display-name …] [-domain <storage domain>] [-holder-id <id>]
                                                  -holder-id: the holder its licences must name (spec 0028)
  tenant list [-state active,suspended,deleting,deleted]   every state but deleted by default
  tenant show <name>                              its record; a deleting one's progress and keys to retire
  tenant suspend|resume <name>
  tenant holder <name> <id>|-clear                set or clear the holder its licences must name (the licences' sub)
  tenant limits <name> [-bytes …] [-channels …] [-upstreams …] [-publishers …]
                                                  the tenant's limits (spec 0038): what is set, in force and used;
                                                  each flag given sets one: a number (bytes: 50GiB), 0 (none),
                                                  unlimited, or default (the configuration's limits:)
  tenant distributor-hold <name> [<duration>]     the minimum hold for distributors' publishers (spec 0037): their
                                                  entries are held at least this long (24h, 2d; 0 for none;
                                                  one day by default); without a duration, printed
  tenant delete <name> -confirm <name>            a tenant suspended for tenants.min_suspended (spec 0019)
  tenant reap <name> [-force]                     remove a deleting tenant's rows here, after the settle period;
                                                  -force: now, when no kista serve or other command runs
  tenant audience add|remove <tenant> <audience>   assign a token audience (server-wide)
  tenant audience list <tenant>
  issuer add <tenant> -name <n> -url <issuer> [-jwks-uri u] [-alg A]... [-require claim=value]...
         [-roles-claim path] [-groups-claim path] [-client-claim path] [-max-lifetime 24h]
         a claim path is keys joined by dots (realm_access.roles), or a JSON array of keys when a
         key has dots of its own: '["https://example.com/roles"]' 
  issuer list <tenant>
  issuer remove <tenant> <name>                   removes its grants too
  issuer console <tenant> <name> (-client-id <id> [-scopes a,b] [-audience a] [-audience-parameter p] | -remove)
                  the administration console's public client at the issuer (spec 0015)
  grant add <tenant> -principal kind:issuer|value -verb install|admin|publish|promote|audit|feed... [-channel c]
            [-extension x | -extension-group g] [-type t]   a package type (spec 0034): a name's defaults to
                                                  duckdb_extension, a grant without one to * (every type)
  grant list <tenant> [-extension-group g] [-extension x [-type t]]
  grant remove <tenant> <grant-id>
  attachment add <tenant> <extension> <file|-> -body-hash <h> -kind <k> -media-type <t> -title <t> [-supersedes <id>]
                  a document about a body (spec 0011): sbom, vulnerabilities, review, provenance, notes
  attachment list <tenant> <extension> [-body-hash <h>]
  attachment show <tenant> <extension> <id> [-content]
  attachment readers|administrators|remove <tenant> <extension> <id>
  attachment feedable|unfeedable <tenant> <extension> <id>   offered by the tenant's feed, or withdrawn
                                                  every attachment command takes [-type t]: the name's package type
  attestation-key add <tenant> <signer-ref> [-active]   a key that signs the feed's statements (spec 0011)
  attestation-key list <tenant>
  attestation-key activate|retire <tenant> <id|fingerprint> [-force]
  feed add <tenant> <name> -url https://<host>/api/v1/tenants/<t> -credential <c> -key <fingerprint,...>
           [-kinds <k,...>] [-readers] [-channels <c,...>] [-export]   a subscription to another repository's feed
  feed add <tenant> <name> -mode bundle -url … -key <fingerprint,...> [-kinds …] [-readers] [-channels …]
           for statements in bundles (spec 0014; not carried yet): no credential, never run
  feed list <tenant>
  feed show|sync|pause|resume <tenant> <name>    a running kista serve takes a requested run
  feed set <tenant> <name> [-credential <c>] [-key <fp,...>] [-kinds <k,...|->] [-readers=true|false]
           [-export=true|false]                  keep what a bundle needs: =true reads the history again, =false drops it
  feed remove <tenant> <name> [-documents]       its documents stay unless -documents
  extension-group add <tenant> <group> [-description …]   a named set of extension names (spec 0017)
  extension-group list <tenant>
  extension-group show <tenant> <group>          the group, its members, its grants
  extension-group description <tenant> <group> <text>
  extension-group remove <tenant> <group>        refused while a grant names it
  extension-group members <tenant> <group> [-add a,b,…] [-remove c,…] [-type t]   lists them without -add/-remove;
                                                  members are (type, name): -type names the names' type
  version add <name> -kind release|dev [-c-api v1.5.6]... -core-extensions <file|-> [-force]
                                                           one maximum C API per major; the version's
                                                           core extensions (kista duckdb core-extensions);
                                                           -force when tenants have releases under them
  version c-api <name> -c-api v2.0.0                       add a major's maximum C API
  version core-extensions <name> -core-extensions <file|-> [-force]
                                                           reserve more of the version's core extensions
  version list
  reserved add <name>... -kind core|community [-force] [-type t]   reserve extension names server-wide (spec 0030);
                                                           -force when tenants have releases under them
  reserved list [-name <name>] [-type t]                   the reserved names and their sources
  reserved remove <name> [-type t]                         a name administrators alone reserved
  channel create <tenant>/<channel> -kind signed|passthrough [-type t]
                                                  -type: the package type it serves, never changed (spec 0034;
                                                  default duckdb_extension, a passthrough channel's)
  channel versions <tenant>/<channel> [-add v]... [-remove v]...
  channel source <tenant>/<channel> [<name> [-set <source>] [-mode auto|manual] | -clear]
                                                  a name's source in the channel (spec 0031): tenant,
                                                  upstream:<name> or publisher:<name>; only it is served
  channel follow <tenant>/<channel> [<source>]  follow a whole source (spec 0033): its names the channel has
                                                  no rule for are taken automatically; without one, the follows
  channel unfollow <tenant>/<channel> <source>   stop following it; what it placed stays
  channel hold <tenant>/<channel> [<duration>]   the channel's hold (spec 0033): an automatic placement waits
                                                  until an entry's arrival plus it (24h, 2d; 0 for none)
  channel list <tenant>
  key check -signer <ref>                         open a key, check it, sign a probe; stores nothing
  key add <tenant>/<channel> -signer <ref> [-active]
  key list <tenant>/<channel>
  key activate <tenant>/<channel> <key-id|fingerprint> [-force]
  key retire <tenant>/<channel> <key-id|fingerprint> [-force]
  key events <tenant>/<channel>
  key resign <tenant>/<channel>                   sign releases with the active key; move the serving key
  release add <tenant>/<channel> <file|-> -name <name> [-private] [-not-current] [-unchecked]
  release list <tenant>/<channel> [-name <name>]
  release yank|deprecate|activate|current|public|private <tenant>/<channel> <release-id>
  release purge <tenant>/<channel> <release-id> [-force]
                  delete a yanked release for good; its slot stays taken (-force while gc.interval is 0)
  release promote <tenant>/<channel> -name <name> -from <channel> (-release <id> | -version <v>)
                  [-private] [-not-current]               release Builds of another channel here
  release place <tenant>/<channel> -name <name> -source <source> -version <v> [-private] [-not-current] [-type t] [-early]
                  place a version's builds from the catalogue (spec 0033); later builds of it follow
                  (-type: the builds' package type, the channel's when left out; another is refused;
                  -early: a held version, a channel administrator's choice, audited)
  catalogue list <tenant> [-source <source>] [-name <name>] [-type t]
                                                  the tenant's catalogue: each source's names and versions,
                                                  and the channels whose rule takes them (spec 0033)
  catalogue show <tenant> <source> <name> [-type t]   an entry's versions, builds and where each is released
  catalogue add <tenant> <file|-> -name <name> [-private] [-type t]
                                                  put a build into the catalogue only, placed nowhere
  catalogue purge <tenant> <build-id>             delete a build no release holds; its slot is free again
  catalogue withdraw <tenant> publisher:<name>|publisher-id:<id> <name> <version> -reason <text> [-type t]
                                                  withdraw a version of a distributor's publisher (spec 0033):
                                                  served nowhere in the tenant from then on, for good, and no
                                                  build of it arrives again from that source
  catalogue meta <tenant> <source> <name> [-summary <text>] [-icon <file.png>] [-clear-summary] [-clear-icon] [-type t]
                                                  an entry's icon and summary (spec 0036): without a flag, printed;
                                                  a field not named keeps its value
  publisher add|remove <tenant> <name>          an identity that only publishes and promotes
  publisher add <tenant> <name> -distributor <distributor>   a distributor's publisher (spec 0033), marked at
                                                  its creation: it publishes into the catalogue only, without
                                                  grants; its keys, hold, purges and withdrawals are the
                                                  distributor's
  publisher list <tenant>
  publisher show <tenant> <name>                a publisher, with its id (spec 0037)
  publisher license-keys <tenant> <name> on|off  mark it a source of licence keys (spec 0021): its publications
                   record the builds' keys; its credentials and grants are then a server administrator's
  publisher key add <tenant> <name> <file.pub|-> [-expires <365d|RFC 3339>]
                   register a public SSH key (ssh-ed25519 or ecdsa-sha2-nistp256): the publisher signs its
                   requests with it (spec 0023)
  publisher key list <tenant> <name>
  publisher key remove <tenant> <name> <key-id> [-purge-held]
                                                  -purge-held: purge the entries it published that are
                                                  still held (a revoked key; spec 0033)
  publisher hold <tenant> <name> <duration>       the publisher's hold (spec 0033): its entries are held this
                                                  long and it publishes into the catalogue only (24h, 2d; 0 for none);
                                                  a distributor's publisher's is the distributor's, and holds
                                                  only above the tenant's distributor-hold
  publisher distributor <tenant> <name> <distributor>   rename a distributor's publisher's distributor (an
                                                  unused one's; an existing publisher is never marked one)
  upstream add <tenant> <name> -kind duckdb-core|duckdb-community|repository -channel <channel>
               -platforms <p,...> [-extensions <name,...|*>] [-prefix <https://...> -keys <sha256:...,...>] [-public]
               [-mode mirror|pull-through|bundle] [-fetch eager|lazy] [-credential <name>]
  upstream fetch <tenant> <name> eager|lazy       a mirror's fetch mode (spec 0032): lazy fetches the cells asked for
                                                  and its eager entries; sync fetches the whole matrix
  upstream hold <tenant> <name> <duration>        an eager mirror's hold (spec 0033): its entries are held this long
                                                  before a channel takes them (24h, 2d; 0 for none)
  upstream credential <tenant> <name> <credential|->   a private upstream's credential (- clears it)
  upstream credentials                            the configured credentials (names, tenants, prefixes, who forwards usage)
  upstream list <tenant>
  upstream show|remove|pause|resume|public|private <tenant> <name>
  upstream sync <tenant> <name> [-dry-run]        a running kista serve takes the run
  upstream cells <tenant> <name> [-outcome <outcome>]   a lazy upstream's -outcome not_asked: offered, not fetched
  upstream licenses <tenant> <name> [-sync]        the upstream's last licence relay (spec 0012); -sync asks for one now
  upstream usage <tenant> <name> [forward|off]     the upstream's last usage forwarding (spec 0012); forward or off sets it
  upstream extension add <tenant> <name> <extension> [-versions <v,...>] [-allow-reserved] [-eager]
  upstream extension remove <tenant> <name> <extension>
  upstream platform|key add|remove <tenant> <name> <platform|fingerprint>
  license keys <tenant> [<name>] [-source s] [-type t]   the live licence keys of the tenant's builds, by name, each
                                                  with the entries it is live for and their builds declaring
                                                  it: what an import into each accepts (spec 0035)
  license list <tenant> [-source s] [-type t]
  license show|token <tenant> <name> [-source s] [-type t]   a licence's claims and state, or its token as
                                                  received; -source: the licence's entry, where the extension
                                                  has licences in several (spec 0035)
  license import <tenant> <file|-> -source s [-replace] [-type t]   store a licence (a compact JWS) under the
                                                  extension it names, in the catalogue entry -source names
                                                  (upstream:<name>, publisher:<name>, upstream-id:<id>,
                                                  publisher-id:<id>), verified against that entry's live keys;
                                                  -replace replaces the licence stored there; -type: its
                                                  package type (spec 0034), duckdb_extension by default
  license remove|public|private <tenant> <name> -source s [-type t]   -source: the licence's entry (a removed
                                                  source's id form for remove)
  license verify <file|-> -key <pem>              check a licence offline against a public key
  events list [-server] [-kind <k>] [-subject <prefix>] [-actor <a>] [-since <RFC 3339>] [-limit <n>]
              [-follow] [-format table|jsonl] [<tenant>]          the event buffer (spec 0010), newest first
  shadow list <tenant>                            core names the tenant replaced: passthrough channels do not serve them
  shadow remove <tenant> <name>                   serve DuckDB's build again from passthrough channels
  bundle plan <tenant> [-upstream <name,...>] [-feed <name,...>] [-licenses [-license-holder <sub>]] [-o <plan.json>]
           what the tenant's bundle-mode upstreams and feeds ask for (spec 0014); with only one of -upstream and
           -feed, the other kind is left out; -licenses asks for the licences of the names whose files come
  bundle import <tenant> <file|-|blob:<domain>/imports/<name>> [-upstream <name,...>] [-feed <name,...>] [-dry-run] [-any-tenant]
                                                              take a signed bundle into the bundle-mode upstreams and feeds
  bundle export <tenant> -plan <plan.json> (-key <key.pem> | -unsigned) -o <out> [-sync] [-private <prefix>]... [-licenses]
                                                              a bundle answering a site's plan, from this tenant's mirrors;
                                                              -licenses carries the licences the plan asks for
  bundle list <tenant> [-limit <n>]                           the tenant's bundle imports, the newest first
  bundle show <tenant> <import id>                            one import: its manifest, bundle key and counts
  bundle reset <tenant> <upstream>                            clear an upstream's bundle mark (it takes older bundles)
  bundle reset <tenant> -feed <name>                          clear a feed's bundle mark and reset its cursors
  block add <tenant> <body-hash> -reason <text>   ban a body tenant-wide: its releases are yanked
  block list <tenant>
  block remove <tenant> <body-hash>
  wellknown <tenant>/<channel>
  blob check                                      pin and check the storage domains, list tenants without one
  blob verify [-domain <d>] [-full [-mark] [-unmark]] [-concurrency <n>]
                                                  every stored body present and of its length (spec 0018);
                                                  -full reads every chunk; -mark marks what a reader would
                                                  (served 503 until the body is committed again); -unmark
                                                  clears the mark of a body found intact
  blob gc [-domain <d>] [-apply] [-force]         collect storage garbage (spec 0016): a dry run unless -apply;
                                                  -apply needs -force while gc.interval is 0 (replicas maybe older)
`

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// stdin is what "-" reads (tests replace it).
var stdin io.Reader = os.Stdin

// admin runs `kista admin`. It is the server administrator's CLI: it runs as the service's OS user,
// with the service's config, and calls the same services the HTTP API will.
func admin(args, env []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kista admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file, or env:NAME for a variable holding it")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 || rest[0] == "help" || *cfgPath == "" {
		fmt.Fprint(stderr, adminUsage)
		return 2
	}
	cfg, err := config.Load(*cfgPath, env)
	if err != nil {
		fmt.Fprintln(stderr, "kista admin:", err)
		return 1
	}
	if cfg.Profile == config.ProfileDev {
		fmt.Fprintln(stderr, "kista admin: WARNING: profile dev")
	}
	ctx := context.Background()
	if rest[0] == "migrate" || rest[0] == "check" {
		c := cfg
		c.Store.Migrate = map[string]string{"migrate": "auto", "check": "check"}[rest[0]]
		s, err := app.OpenStore(ctx, c)
		if err != nil {
			fmt.Fprintln(stderr, "kista admin:", err)
			return 1
		}
		s.Close()
		fmt.Fprintln(stdout, "schema ok")
		return 0
	}
	s, err := app.OpenStore(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, "kista admin:", err)
		return 1
	}
	defer s.Close()
	s.Log = slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc, err := app.NewServices(cfg, s, authz.ServerAdmin{})
	if err != nil {
		fmt.Fprintln(stderr, "kista admin:", err)
		return 1
	}
	a := &adminCmd{in: stdin, cfg: cfg, svc: svc, actor: authz.OSActor(), out: stdout, errw: stderr}
	// spec 0035 phase 2: the command is one act; its licence warnings go to stderr, and change no exit code
	ctx = store.WithLicenseWarnings(ctx)
	err = a.dispatch(ctx, rest)
	for _, w := range store.LicenseWarnings(ctx) {
		fmt.Fprintf(stderr, "kista admin: warning: %s: %s\n", w.TenantName, w.Message)
	}
	if err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprint(stderr, adminUsage)
			return 2
		}
		fmt.Fprintln(stderr, "kista admin:", err)
		return 1
	}
	return 0
}

var errUsage = errors.New("usage")

type adminCmd struct {
	in    io.Reader
	cfg   config.Config
	svc   *app.Services
	actor authz.Actor
	out   io.Writer
	errw  io.Writer
}

// logf prints the one-line record of an admin action (spec 0010 moves these to the audit log).
func (a *adminCmd) logf(format string, args ...any) {
	fmt.Fprintf(a.errw, "admin "+format+" by %s\n", append(args, a.actor)...)
}

func splitPath(p string) (string, string, error) {
	t, c, ok := strings.Cut(p, "/")
	if !ok || t == "" || c == "" {
		return "", "", fmt.Errorf("%w: %q is not <tenant>/<channel>", errUsage, p)
	}
	return t, c, nil
}

// flags parses flags that may come after positional arguments.
func flags(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, errUsage
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

func (a *adminCmd) dispatch(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errUsage
	}
	cmd, sub := args[0], ""
	if len(args) > 1 {
		sub = args[1]
	}
	switch cmd {
	case "backup":
		if len(args) != 2 {
			return errUsage
		}
		if err := a.svc.Store.Backup(ctx, args[1]); err != nil {
			return err
		}
		a.logf("backup %q", args[1])
		return nil
	case "wellknown":
		if len(args) != 2 {
			return errUsage
		}
		t, c, err := splitPath(args[1])
		if err != nil {
			return err
		}
		doc, _, err := a.svc.Keys.WellKnown(ctx, a.actor, t, c)
		if err != nil {
			return err
		}
		fmt.Fprintln(a.out, string(doc))
		return nil
	case "bootstrap":
		return a.bootstrap(ctx, args[1:])
	case "tenant":
		return a.tenant(ctx, sub, args[min(2, len(args)):])
	case "version":
		return a.version(ctx, sub, args[min(2, len(args)):])
	case "reserved":
		return a.reserved(ctx, sub, args[min(2, len(args)):])
	case "channel":
		return a.channel(ctx, sub, args[min(2, len(args)):])
	case "key":
		return a.key(ctx, sub, args[min(2, len(args)):])
	case "release":
		return a.release(ctx, sub, args[min(2, len(args)):])
	case "catalogue":
		return a.catalogue(ctx, sub, args[min(2, len(args)):])
	case "issuer":
		return a.issuer(ctx, sub, args[min(2, len(args)):])
	case "grant":
		return a.grant(ctx, sub, args[min(2, len(args)):])
	case "extension-group":
		return a.extensionGroup(ctx, sub, args[min(2, len(args)):])
	case "attachment":
		return a.attachment(ctx, sub, args[min(2, len(args)):])
	case "attestation-key":
		return a.attestationKey(ctx, sub, args[min(2, len(args)):])
	case "feed":
		return a.feed(ctx, sub, args[min(2, len(args)):])
	case "block":
		return a.block(ctx, sub, args[min(2, len(args)):])
	case "publisher":
		return a.publisher(ctx, sub, args[min(2, len(args)):])
	case "upstream":
		return a.upstream(ctx, sub, args[min(2, len(args)):])
	case "shadow":
		return a.shadow(ctx, sub, args[min(2, len(args)):])
	case "license":
		return a.license(ctx, sub, args[min(2, len(args)):])
	case "bundle":
		return a.bundle(ctx, sub, args[min(2, len(args)):])
	case "events":
		return a.events(ctx, sub, args[min(2, len(args)):])
	case "blob":
		switch {
		case sub == "check" && len(args) == 2:
			return a.blobCheck(ctx)
		case sub == "gc":
			return a.blobGC(ctx, args[2:])
		case sub == "verify":
			return a.blobVerify(ctx, args[2:])
		}
		return errUsage
	}
	return errUsage
}

// blobCheck runs the blob service's startup checks (spec 0005): every domain pinned to its store,
// marked for this deployment and not public; then it lists the domains and the tenants whose domain
// is not configured.
func (a *adminCmd) blobCheck(ctx context.Context) error {
	svc, err := app.BlobService(ctx, a.cfg, a.svc.Store, slog.New(slog.DiscardHandler))
	if err != nil {
		return err
	}
	defer func() { _ = svc.Close() }()
	missing, err := svc.MissingDomains(ctx)
	if err != nil {
		return err
	}
	for _, d := range svc.Domains() {
		fmt.Fprintf(a.out, "domain %s ok\n", d)
	}
	for _, t := range missing {
		fmt.Fprintf(a.out, "tenant %s: storage domain not configured; it is served nothing\n", t)
	}
	if len(missing) > 0 {
		return fmt.Errorf("%d tenant(s) without a configured storage domain", len(missing))
	}
	return nil
}

func (a *adminCmd) table(header string, rows func(w io.Writer)) {
	w := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, header)
	rows(w)
	w.Flush()
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func (a *adminCmd) tenant(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("tenant", flag.ContinueOnError)
	display := fs.String("display-name", "", "")
	domain := fs.String("domain", "", "")
	confirm := fs.String("confirm", "", "")
	force := fs.Bool("force", false, "")
	states := fs.String("state", "", "")
	holder := fs.String("holder-id", "", "")
	clearHolder := fs.Bool("clear", false, "")
	limits := map[string]*string{} // spec 0038
	for _, n := range store.LimitNames {
		limits[n] = fs.String(n, "", "")
	}
	pos, err := flags(fs, args)
	if err != nil {
		return err
	}
	switch {
	case sub == "create" && len(pos) == 1:
		t, err := a.svc.Tenants.CreateTenant(ctx, a.actor, pos[0], *display, *domain, *holder)
		if err != nil {
			return err
		}
		a.logf("tenant create %s", t.Name)
		fmt.Fprintln(a.out, t.ID)
	case sub == "holder" && (len(pos) == 2 && !*clearHolder || len(pos) == 1 && *clearHolder):
		id := ""
		if !*clearHolder {
			// an empty id (an unset variable) never clears it: -clear does
			if id = pos[1]; id == "" {
				return errors.New("tenant holder: the holder id is empty; -clear clears it")
			}
		}
		t, err := a.svc.Tenants.SetTenantHolder(ctx, a.actor, pos[0], id, 0)
		if err != nil {
			return err
		}
		if id == "" {
			a.logf("tenant holder %s cleared", t.Name)
		} else {
			a.logf("tenant holder %s %q", t.Name, id)
		}
	case sub == "limits" && len(pos) == 1:
		return a.tenantLimits(ctx, fs, pos[0], limits)
	case sub == "distributor-hold" && (len(pos) == 1 || len(pos) == 2):
		if len(pos) == 1 {
			d, err := a.svc.Auth.DistributorHold(ctx, a.actor, pos[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(a.out, d)
			return nil
		}
		d, err := parseHold(pos[1])
		if err != nil {
			return err
		}
		if _, err := a.svc.Auth.SetDistributorHold(ctx, a.actor, pos[0], d); err != nil {
			return err
		}
		a.logf("tenant distributor-hold %s %s", pos[0], d)
	case sub == "list" && len(pos) == 0:
		var phases []string
		if *states != "" {
			phases = strings.Split(*states, ",")
		}
		ts_, err := a.svc.Tenants.ListTenants(ctx, a.actor, phases...)
		if err != nil {
			return err
		}
		a.table("NAME\tSTATE\tDOMAIN\tDISPLAY NAME\tCREATED", func(w io.Writer) {
			for _, t := range ts_ {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.Phase(), t.StorageDomain, t.DisplayName, ts(t.CreatedAt))
			}
		})
	case sub == "delete" && len(pos) == 1:
		if *confirm != pos[0] {
			return fmt.Errorf("%w: give the tenant's name again: -confirm %s", errUsage, pos[0])
		}
		t, d, err := a.svc.Tenants.Delete(ctx, a.actor, pos[0], 0, a.cfg.Tenants.MinSuspended)
		if err != nil {
			return err
		}
		a.logf("tenant delete %s", t.Name)
		fmt.Fprintf(a.out, "tenant %s is being deleted; its rows go after %s (kista serve, or kista admin tenant reap %s)\n",
			t.Name, tenants.SettlePeriod, t.Name)
		a.deletionKeys(d)
		if a.cfg.GCSettings().Interval == 0 {
			fmt.Fprintln(a.out, "gc.interval is 0: its bodies stay in storage until kista admin blob gc -apply runs")
		}
	case sub == "reap" && len(pos) == 1:
		host, _ := os.Hostname()
		r := &tenants.Reaper{Store: a.svc.Tenants.Store, Holder: fmt.Sprintf("admin/%s/%d", host, os.Getpid()),
			Log: slog.New(slog.NewTextHandler(a.errw, nil))}
		t, err := r.Reap(ctx, pos[0], *force)
		if err != nil {
			return err
		}
		if t.Phase() != store.TenantDeleted {
			return fmt.Errorf("tenant %s: the deletion pass did not finish; tenant show %s says why", t.Name, t.Name)
		}
		a.logf("tenant reap %s", t.Name)
		fmt.Fprintf(a.out, "tenant %s deleted\n", t.Name)
	case sub == "show" && len(pos) == 1:
		t, d, err := a.svc.Tenants.Show(ctx, a.actor, pos[0])
		if err != nil {
			return err
		}
		line := func(k, v string) {
			if v != "" {
				fmt.Fprintf(a.out, "%-16s %s\n", k+":", v)
			}
		}
		line("name", t.Name)
		line("id", t.ID)
		line("state", t.Phase())
		line("display name", t.DisplayName)
		line("storage domain", t.StorageDomain)
		line("holder id", t.HolderID)
		line("distributor hold", holdText(t.DistributorHold)) // spec 0037: the minimum for distributors' publishers
		line("created", ts(t.CreatedAt))
		line("suspended", tsOpt(t.SuspendedAt))
		line("deletable from", tsOpt(tenants.DeletableAt(t, a.cfg.Tenants.MinSuspended)))
		if d != nil {
			line("deletion by", d.StartedBy)
			line("deleting since", ts(d.StartedAt))
			line("deleted", tsOpt(d.FinishedAt))
			line("passes", strconv.FormatInt(d.Passes, 10))
			line("rows deleted", strconv.FormatInt(d.DeletedRows, 10))
			line("reached", d.Table)
			line("last error", d.LastError)
			if d.FinishedAt.IsZero() {
				line("next attempt", tsOpt(d.NextAttemptAt))
			}
			a.deletionKeys(*d)
		}
	case sub == "audience" && len(pos) >= 2:
		switch {
		case pos[0] == "add" && len(pos) == 3:
			if err := a.svc.Auth.AddAudience(ctx, a.actor, pos[1], pos[2]); err != nil {
				return err
			}
			a.logf("tenant audience add %s %s", pos[1], pos[2])
		case pos[0] == "remove" && len(pos) == 3:
			if err := a.svc.Auth.RemoveAudience(ctx, a.actor, pos[1], pos[2]); err != nil {
				return err
			}
			a.logf("tenant audience remove %s %s", pos[1], pos[2])
		case pos[0] == "list" && len(pos) == 2:
			auds, err := a.svc.Auth.ListAudiences(ctx, a.actor, pos[1])
			if err != nil {
				return err
			}
			for _, x := range auds {
				fmt.Fprintln(a.out, x)
			}
		default:
			return errUsage
		}
	case (sub == "suspend" || sub == "resume") && len(pos) == 1:
		state := map[string]string{"suspend": store.TenantSuspended, "resume": store.TenantActive}[sub]
		if _, err := a.svc.Tenants.SetTenantState(ctx, a.actor, pos[0], state, 0); err != nil {
			return err
		}
		a.logf("tenant %s %s", sub, pos[0])
	default:
		return errUsage
	}
	return nil
}

func (a *adminCmd) version(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	kind := fs.String("kind", "", "")
	var capis multi
	fs.Var(&capis, "c-api", "")
	coreFile := fs.String("core-extensions", "", "")
	force := fs.Bool("force", false, "")
	pos, err := flags(fs, args)
	if err != nil {
		return err
	}
	core := tenants.CoreExtensions{Force: *force}
	if *coreFile != "" {
		if core.Names, core.Commit, err = readCoreExtensions(*coreFile, a.in); err != nil {
			return err
		}
	}
	switch {
	case sub == "add" && len(pos) == 1:
		v, added, err := a.svc.Tenants.AddVersion(ctx, a.actor, pos[0], *kind, capis, core)
		if err != nil {
			return err
		}
		a.logf("version add %s (core extensions newly reserved: %s)", v.Name, joinOrNone(added))
	case sub == "core-extensions" && len(pos) == 1 && *coreFile != "":
		added, err := a.svc.Tenants.AddVersionCore(ctx, a.actor, pos[0], core)
		if err != nil {
			return err
		}
		a.logf("version core-extensions %s (newly reserved: %s)", pos[0], joinOrNone(added))
	case sub == "c-api" && len(pos) == 1 && len(capis) == 1:
		v, err := a.svc.Tenants.AddVersionCAPI(ctx, a.actor, pos[0], capis[0])
		if err != nil {
			return err
		}
		a.logf("version c-api %s %s", v.Name, capis[0])
	case sub == "list" && len(pos) == 0:
		vs, err := a.svc.Tenants.ListVersions(ctx)
		if err != nil {
			return err
		}
		a.table("NAME\tKIND\tC API", func(w io.Writer) {
			for _, v := range vs {
				var cs []string
				for _, c := range v.CAPIs {
					cs = append(cs, c.String())
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", v.Name, v.Kind, strings.Join(cs, " "))
			}
		})
	default:
		return errUsage
	}
	return nil
}

func (a *adminCmd) channel(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("channel", flag.ContinueOnError)
	kind := fs.String("kind", "", "")
	ptype := fs.String("type", "", "") // spec 0034: the package type a new channel serves
	var add, remove multi
	fs.Var(&add, "add", "")
	fs.Var(&remove, "remove", "")
	set := fs.String("set", "", "")
	mode := fs.String("mode", "", "")
	clear := fs.Bool("clear", false, "")
	pos, err := flags(fs, args)
	if err != nil {
		return err
	}
	changes := *set != "" || *mode != ""
	switch {
	case sub == "source" && (len(pos) == 1 || len(pos) == 2) && (!changes || !*clear) && (len(pos) == 2 || !changes && !*clear):
		return a.channelSource(ctx, pos, *set, *mode, *clear)
	case (sub == "follow" || sub == "unfollow") && (len(pos) == 2 || len(pos) == 1 && sub == "follow"):
		return a.channelFollow(ctx, sub, pos)
	case sub == "hold" && (len(pos) == 1 || len(pos) == 2):
		t, c, err := splitPath(pos[0])
		if err != nil {
			return err
		}
		if len(pos) == 1 {
			d, err := a.svc.Releases.ChannelHold(ctx, a.actor, t, c)
			if err != nil {
				return err
			}
			fmt.Fprintln(a.out, d)
			return nil
		}
		d, err := parseHold(pos[1])
		if err != nil {
			return err
		}
		if _, err := a.svc.Releases.SetChannelHold(ctx, a.actor, t, c, d); err != nil {
			return err
		}
		a.logf("channel hold %s/%s %s", t, c, d)
	case sub == "create" && len(pos) == 1:
		t, c, err := splitPath(pos[0])
		if err != nil {
			return err
		}
		ch, err := a.svc.Tenants.CreateChannel(ctx, a.actor, t, c, *kind, *ptype)
		if err != nil {
			return err
		}
		a.logf("channel create %s/%s (%s)", t, ch.Name, typedKind(ch))
	case sub == "versions" && len(pos) == 1:
		t, c, err := splitPath(pos[0])
		if err != nil {
			return err
		}
		vs, err := a.svc.Tenants.SetChannelVersions(ctx, a.actor, t, c, add, remove)
		if err != nil {
			return err
		}
		if len(add)+len(remove) > 0 {
			a.logf("channel versions %s/%s +%v -%v", t, c, []string(add), []string(remove))
		}
		fmt.Fprintln(a.out, strings.Join(vs, "\n"))
	case sub == "list" && len(pos) == 1:
		cs, err := a.svc.Tenants.ListChannels(ctx, a.actor, pos[0])
		if err != nil {
			return err
		}
		a.table("NAME\tKIND\tVERSION\tCREATED", func(w io.Writer) {
			for _, c := range cs {
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", c.Name, typedKind(c), c.Version, ts(c.CreatedAt))
			}
		})
	default:
		return errUsage
	}
	return nil
}

// parseHold reads a hold: a duration (36h, 90m) or whole days (2d); 0 for none (spec 0033).
func parseHold(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 0 || days > 3650 { // beyond any hold; and no overflow
			return 0, fmt.Errorf("%w: a hold is a duration such as 24h or 2d, 0 for none", errUsage)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	if s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%w: a hold is a duration such as 24h or 2d, 0 for none", errUsage)
	}
	return d, nil
}

// effectiveHold is how long a publisher's entries are held: a distributor's publisher's, the larger
// of its hold and the tenant's minimum for distributors' publishers (spec 0037).
func effectiveHold(p store.Publisher, t store.Tenant) time.Duration {
	if p.Distributor == "" {
		return p.Hold
	}
	return max(p.Hold, t.DistributorHold)
}

// holdText shows a hold in a table or a show: "-" for none.
func holdText(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return d.String()
}

// typedKind is a channel's kind, with its package type when it is not the default (spec 0034).
func typedKind(c store.Channel) string {
	if c.PackageType != pkgtype.Default {
		return c.Kind + "/" + c.PackageType
	}
	return c.Kind
}

// channelSource lists a channel's sources, shows one name's, or sets or clears it (spec 0031), and
// its rule's mode (spec 0033).
func (a *adminCmd) channelSource(ctx context.Context, pos []string, set, mode string, clear bool) error {
	t, c, err := splitPath(pos[0])
	if err != nil {
		return err
	}
	var vs []release.SourceView
	switch {
	case len(pos) == 1:
		if vs, err = a.svc.Releases.Sources(ctx, a.actor, t, c); err != nil {
			return err
		}
	case clear:
		if err := a.svc.Releases.ClearSource(ctx, a.actor, t, c, pos[1]); err != nil {
			return err
		}
		a.logf("channel source %s/%s %s cleared", t, c, pos[1])
		return nil
	case set != "" || mode != "":
		if set == "" { // the mode alone: the rule's source stays
			cur, err := a.svc.Releases.Source(ctx, a.actor, t, c, pos[1])
			if err != nil {
				return err
			}
			if set = cur.Source; set == "" {
				return fmt.Errorf("%s has no source in the channel: -set one", pos[1])
			}
		}
		v, err := a.svc.Releases.SetSource(ctx, a.actor, t, c, pos[1], set, mode)
		if err != nil {
			return err
		}
		a.logf("channel source %s/%s %s %s %s", t, c, pos[1], set, v.Mode)
		vs = []release.SourceView{v}
	default:
		v, err := a.svc.Releases.Source(ctx, a.actor, t, c, pos[1])
		if err != nil {
			return err
		}
		vs = []release.SourceView{v}
	}
	a.table("NAME\tSOURCE\tMODE\tCURRENT\tNEWEST\tSET BY\tSET AT\tRELEASES BY SOURCE\tLAST OUTCOME", func(w io.Writer) {
		for _, v := range vs {
			src, by, at := v.Source, v.SetBy, ts(v.SetAt)
			if src == "" {
				src, by, at = "-", "-", "-"
			}
			var avail []string
			for s, n := range v.Available {
				avail = append(avail, fmt.Sprintf("%s=%d", s, n))
			}
			slices.Sort(avail)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.Name, src, dash(v.Mode), dash(v.Current), dash(v.Newest), by, at,
				strings.Join(avail, " "), dash(v.Outcome))
		}
	})
	return nil
}

func (a *adminCmd) key(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("key", flag.ContinueOnError)
	ref := fs.String("signer", "", "")
	active := fs.Bool("active", false, "")
	force := fs.Bool("force", false, "")
	pos, err := flags(fs, args)
	if err != nil {
		return errUsage
	}
	if sub == "check" {
		if len(pos) != 0 || *ref == "" {
			return errUsage
		}
		sg, err := a.svc.Sources.Open(ctx, *ref)
		if err != nil {
			return err
		}
		if err := keys.Probe(ctx, sg); err != nil {
			return err
		}
		fmt.Fprintln(a.out, sg.ID(), extfile.Fingerprint(sg.Public()), "protection="+a.svc.Sources.Protection(sg.ID()))
		return nil
	}
	if len(pos) < 1 {
		return errUsage
	}
	t, c, err := splitPath(pos[0])
	if err != nil {
		return err
	}
	switch {
	case sub == "add" && len(pos) == 1 && *ref != "":
		k, err := a.svc.Keys.Add(ctx, a.actor, t, c, *ref, *active)
		if err != nil {
			return err
		}
		a.logf("key add %s/%s %s (%s)", t, c, k.Fingerprint, k.State)
		fmt.Fprintln(a.out, k.ID, k.Fingerprint)
	case sub == "list" && len(pos) == 1:
		ks, err := a.svc.Keys.List(ctx, a.actor, t, c)
		if err != nil {
			return err
		}
		a.table("ID\tFINGERPRINT\tSTATE\tSIGNER\tPROTECTION\tTRUSTED SINCE\tSTATE CHANGED", func(w io.Writer) {
			for _, k := range ks {
				p := a.svc.Sources.Protection(k.SignerRef)
				if p == "" {
					p = "-" // a source no longer configured
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", k.ID, k.Fingerprint, k.State, k.SignerRef, p, ts(k.TrustedSince), ts(k.StateChangedAt))
			}
		})
	case (sub == "activate" || sub == "retire") && len(pos) == 2:
		do := a.svc.Keys.Activate
		if sub == "retire" {
			do = a.svc.Keys.Retire
		}
		k, err := do(ctx, a.actor, t, c, pos[1], 0, *force)
		if errors.Is(err, keys.ErrTooSoon) {
			return fmt.Errorf("%w (wait, or use -force)", err)
		}
		if err != nil {
			return err
		}
		forced := ""
		if *force {
			forced = " (forced)"
		}
		a.logf("key %s %s/%s %s%s", sub, t, c, k.Fingerprint, forced)
		if sub == "activate" {
			fmt.Fprintf(a.errw, "the key signs new releases now; existing ones are served with the old key's signatures "+
				"until a re-sign (kista serve with serve.resign, or kista admin key resign %s/%s) moves the serving key\n", t, c)
		}
	case sub == "resign" && len(pos) == 1:
		if err := a.svc.Tenants.Authz.Allow(ctx, a.actor, authz.VerbAdmin, authz.Resource{Tenant: t, Channel: c}); err != nil {
			return err
		}
		ch, err := a.svc.Store.GetChannel(ctx, t, c)
		if err != nil {
			return err
		}
		signed, moved, err := a.svc.Releases.Resign(ctx, ch.ID, nil)
		if err != nil {
			return err
		}
		a.logf("key resign %s/%s: %d signatures, serving key moved: %v", t, c, signed, moved)
	case sub == "events" && len(pos) == 1:
		evs, err := a.svc.Keys.Events(ctx, a.actor, t, c)
		if err != nil {
			return err
		}
		a.table("AT\tKEY\tFROM\tTO\tACTOR\tFORCED", func(w io.Writer) {
			for _, e := range evs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%v\n", ts(e.At), e.KeyID, e.From, e.To, e.Actor, e.Forced)
			}
		})
	default:
		return errUsage
	}
	return nil
}

func (a *adminCmd) release(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	name := fs.String("name", "", "")
	private := fs.Bool("private", false, "")
	notCurrent := fs.Bool("not-current", false, "")
	unchecked := fs.Bool("unchecked", false, "")
	from := fs.String("from", "", "")
	source := fs.String("source", "", "")
	relID := fs.String("release", "", "")
	version := fs.String("version", "", "")
	force := fs.Bool("force", false, "")
	ptype := fs.String("type", "", "")   // spec 0034: a placement's builds' package type ("": the channel's)
	early := fs.Bool("early", false, "") // spec 0033: place a held version
	pos, err := flags(fs, args)
	if err != nil || len(pos) == 0 || (*ptype != "" || *early) && sub != "place" {
		return errUsage
	}
	if *ptype != "" {
		if _, err := pkgtype.Parse(*ptype); err != nil {
			return err
		}
	}
	t, c, err := splitPath(pos[0])
	if err != nil {
		return err
	}
	switch {
	case sub == "add" && len(pos) == 2 && *name != "":
		in := a.in
		if pos[1] != "-" {
			f, err := os.Open(pos[1])
			if err != nil {
				return err
			}
			defer f.Close()
			in = f
		}
		svc, err := app.BlobService(ctx, a.cfg, a.svc.Store, slog.New(slog.NewTextHandler(a.errw, &slog.HandlerOptions{Level: slog.LevelWarn})))
		if err != nil {
			return err
		}
		defer func() { _ = svc.Close() }()
		a.svc.WithBlob(svc)
		r, existed, err := a.svc.Releases.Add(ctx, a.actor, t, c, in, release.AddOptions{Name: *name, Private: *private,
			NotCurrent: *notCurrent, Unchecked: *unchecked})
		if err != nil {
			return err
		}
		if existed {
			fmt.Fprintln(a.errw, "the release already exists")
		} else {
			a.logf("release add %s/%s %s %s %s %s", t, c, r.Name, r.PackageVersion, r.Platform, r.Slot)
		}
		fmt.Fprintln(a.out, r.ID)
	case sub == "list" && len(pos) == 1:
		rs, err := a.svc.Releases.List(ctx, a.actor, t, c, *name)
		if err != nil {
			return err
		}
		a.table("ID\tNAME\tVERSION\tPLATFORM\tSLOT\tSTATE\tVISIBILITY\tSEQ\tCREATED", func(w io.Writer) {
			for _, r := range rs {
				seq := "-"
				if r.Seq > 0 {
					seq = strconv.FormatInt(r.Seq, 10)
				}
				state := r.State
				if r.Withdrawn { // spec 0033 phase B: served nowhere
					state += " (withdrawn)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Name, r.PackageVersion, r.Platform, r.Slot,
					state, r.Visibility, seq, ts(r.CreatedAt))
			}
		})
	case sub == "promote" && len(pos) == 1 && *name != "" && *from != "":
		rs, existed, err := a.svc.Releases.Promote(ctx, a.actor, t, c, *name, release.PromoteOptions{From: *from,
			Release: *relID, Version: *version, Private: *private, NotCurrent: *notCurrent})
		if err != nil {
			return err
		}
		if existed {
			fmt.Fprintln(a.errw, "the releases already exist")
		}
		for _, r := range rs {
			a.logf("release promote %s/%s -> %s/%s %s %s %s %s", t, *from, t, c, r.Name, r.PackageVersion, r.Platform, r.Slot)
			fmt.Fprintln(a.out, r.ID)
		}
	case sub == "place" && len(pos) == 1 && *name != "" && *source != "" && *version != "":
		rs, existed, err := a.svc.Releases.Place(ctx, a.actor, t, c, *name, release.PlaceOptions{Source: *source, Version: *version,
			PackageType: *ptype, Private: *private, NotCurrent: *notCurrent, Early: *early})
		if err != nil {
			return err
		}
		if existed {
			fmt.Fprintln(a.errw, "the releases already exist")
		}
		for _, r := range rs {
			a.logf("release place %s %s/%s %s %s %s %s", *source, t, c, r.Name, r.PackageVersion, r.Platform, r.Slot)
			fmt.Fprintln(a.out, r.ID)
		}
	case len(pos) == 2 && slices.Contains([]string{"yank", "deprecate", "activate", "current", "public", "private"}, sub):
		r, err := a.svc.Releases.Apply(ctx, a.actor, t, c, "", pos[1], release.Change(sub), 0)
		if err != nil {
			return err
		}
		a.logf("release %s %s/%s %s (%s %s, %s)", sub, t, c, r.ID, r.Name, r.PackageVersion, r.State)
	case len(pos) == 2 && sub == "purge":
		if *force { // the operator vouches that every replica runs this version
			a.svc.Releases.PurgeOff = false
		}
		r, err := a.svc.Releases.Purge(ctx, a.actor, t, c, "", pos[1], 0)
		if err != nil {
			return err
		}
		a.logf("release purge %s/%s %s (%s %s %s %s)", t, c, r.ID, r.Name, r.PackageVersion, r.Platform, r.Slot)
	default:
		return errUsage
	}
	return nil
}

// claimPath parses a claim path: a JSON array of keys (for keys with dots), or keys joined by dots.
func claimPath(s string) ([]string, error) {
	p, err := auth.ParseClaimPath(s)
	if err != nil {
		return nil, fmt.Errorf("%w: claim path %q", errUsage, s)
	}
	return p, nil
}

func (a *adminCmd) issuer(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("issuer", flag.ContinueOnError)
	name := fs.String("name", "", "")
	issURL := fs.String("url", "", "")
	jwks := fs.String("jwks-uri", "", "")
	var algs, require multi
	fs.Var(&algs, "alg", "")
	fs.Var(&require, "require", "")
	roles := fs.String("roles-claim", "", "")
	groups := fs.String("groups-claim", "", "")
	client := fs.String("client-claim", "", "")
	life := fs.Duration("max-lifetime", 0, "")
	clientID := fs.String("client-id", "", "")
	scopes := fs.String("scopes", "", "")
	audience := fs.String("audience", "", "")
	audParam := fs.String("audience-parameter", "", "")
	remove := fs.Bool("remove", false, "")
	pos, err := flags(fs, args)
	if err != nil || len(pos) == 0 {
		return errUsage
	}
	switch {
	case sub == "add" && len(pos) == 1 && *name != "" && *issURL != "":
		is := store.Issuer{Name: *name, URL: *issURL, JWKSURI: *jwks, Algorithms: algs, MaxTokenLifetime: *life,
			RequiredClaims: map[string]string{}}
		for _, r := range require {
			k, v, ok := strings.Cut(r, "=")
			if !ok {
				return fmt.Errorf("%w: -require claim=value", errUsage)
			}
			is.RequiredClaims[k] = v
		}
		if is.RolesClaim, err = claimPath(*roles); err != nil {
			return err
		}
		if is.GroupsClaim, err = claimPath(*groups); err != nil {
			return err
		}
		if is.ClientClaim, err = claimPath(*client); err != nil {
			return err
		}
		out, err := a.svc.Auth.AddIssuer(ctx, a.actor, pos[0], is)
		if err != nil {
			return err
		}
		a.logf("issuer add %s %s %s", pos[0], out.Name, out.URL)
	case sub == "list" && len(pos) == 1:
		iss, err := a.svc.Auth.ListIssuers(ctx, a.actor, pos[0])
		if err != nil {
			return err
		}
		a.table("NAME\tURL\tJWKS\tALGORITHMS\tREQUIRED CLAIMS\tROLES\tGROUPS\tCLIENT\tMAX LIFETIME", func(w io.Writer) {
			for _, is := range iss {
				var req []string
				for k, v := range is.RequiredClaims {
					req = append(req, k+"="+v)
				}
				sort.Strings(req)
				jwks := is.JWKSURI
				if jwks == "" {
					jwks = "(discovered)"
				}
				path := func(p []string) string { b, _ := json.Marshal(p); return string(b) }
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", is.Name, is.URL, jwks, strings.Join(is.Algorithms, " "),
					strings.Join(req, ","), path(is.RolesClaim), path(is.GroupsClaim), path(is.ClientClaim), is.MaxTokenLifetime)
			}
		})
	case sub == "remove" && len(pos) == 2:
		if err := a.svc.Auth.RemoveIssuer(ctx, a.actor, pos[0], pos[1], ""); err != nil {
			return err
		}
		a.logf("issuer remove %s %s", pos[0], pos[1])
	case sub == "console" && len(pos) == 2 && *remove && *clientID == "" && *scopes == "" && *audience == "" && *audParam == "":
		if err := a.svc.Auth.RemoveConsoleClient(ctx, a.actor, pos[0], pos[1], ""); err != nil {
			return err
		}
		a.logf("issuer console %s %s removed", pos[0], pos[1])
	case sub == "console" && len(pos) == 2 && *clientID != "" && !*remove:
		sc := strings.FieldsFunc(*scopes, func(r rune) bool { return r == ',' || r == ' ' })
		if len(sc) == 0 {
			sc = config.DefaultConsoleScopes
		}
		c, err := a.svc.Auth.SetConsoleClient(ctx, a.actor, pos[0], pos[1], "", store.ConsoleClient{ClientID: *clientID,
			Scopes: sc, Audience: *audience, AudienceParameter: *audParam})
		if err != nil {
			return err
		}
		a.logf("issuer console %s %s %s", pos[0], pos[1], c.ClientID)
	default:
		return errUsage
	}
	return nil
}

func (a *adminCmd) grant(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("grant", flag.ContinueOnError)
	principal := fs.String("principal", "", "")
	var verbs multi
	fs.Var(&verbs, "verb", "")
	channel := fs.String("channel", "", "")
	extension := fs.String("extension", "", "")
	group := fs.String("extension-group", "", "")
	// spec 0034: a grant's package type ("": the default for its shape; "*" any type, without a name)
	ptype := fs.String("type", "", "")
	pos, err := flags(fs, args)
	if err != nil || len(pos) == 0 {
		return errUsage
	}
	switch {
	case sub == "add" && len(pos) == 1 && *principal != "" && len(verbs) > 0:
		g, err := a.svc.Auth.AddGrant(ctx, a.actor, pos[0], *principal, verbs,
			tenants.GrantOn{Channel: *channel, Package: *extension, PackageGroup: *group, PackageType: *ptype})
		if err != nil {
			return err
		}
		a.logf("grant add %s %s %s", pos[0], g.Principal(), strings.Join(g.Verbs, ","))
		fmt.Fprintln(a.out, g.ID)
	case sub == "list" && len(pos) == 1:
		if *group != "" {
			if err := store.ValidGroupName(*group); err != nil {
				return err
			}
		}
		if *extension != "" {
			if err := release.ValidName(*extension); err != nil {
				return err
			}
		}
		typ, err := pkgtype.Parse(*ptype)
		if err != nil {
			return err
		}
		gs, err := a.svc.Auth.ListGrants(ctx, a.actor, pos[0])
		if err != nil {
			return err
		}
		isReserved := false
		if *extension != "" {
			kind, err := a.svc.Store.ReservedFor(ctx, pos[0], typ, *extension) // spec 0030: DuckDB's, its upstreams'
			if err != nil {
				return err
			}
			isReserved = kind != ""
		}
		a.grantTable(gs, *group, typ, *extension, isReserved)
	case sub == "remove" && len(pos) == 2:
		if err := a.svc.Auth.RemoveGrant(ctx, a.actor, pos[0], pos[1]); err != nil {
			return err
		}
		a.logf("grant remove %s %s", pos[0], pos[1])
	default:
		return errUsage
	}
	return nil
}

// grantTable prints grants; with group, those on it; with extension, those reaching it (of
// packageType: spec 0034) (spec 0017). A grant's package type shows before its extension when it is
// not the default for its shape (type:name, type:*).
func (a *adminCmd) grantTable(gs []store.Grant, group, packageType, extension string, isReserved bool) {
	a.table("ID\tPRINCIPAL\tVERBS\tCHANNEL\tEXTENSION", func(w io.Writer) {
		for _, g := range gs {
			if group != "" && g.PackageGroupName != group {
				continue
			}
			if extension != "" && !reaches(g, packageType, extension, isReserved) {
				continue
			}
			ch, ext := g.ChannelName, g.Package
			if ch == "" {
				ch = "*"
			}
			switch {
			case g.PackageGroupName != "":
				ext = "extension-group:" + g.PackageGroupName
			case ext == "":
				ext = "*"
			}
			if t := g.TypeShown(); t != "" {
				ext = t + ":" + ext
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", g.ID, g.Principal(), strings.Join(g.Verbs, ","), ch, ext)
		}
	})
}

// reaches reports whether a grant gives some verb on a package, in its own channel scope.
func reaches(g store.Grant, packageType, extension string, isReserved bool) bool {
	_, verbs := authz.Reaching(g, "", packageType, extension, isReserved)
	return len(verbs) > 0
}

// attachment manages documents about bodies (spec 0011).
func (a *adminCmd) attachment(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("attachment", flag.ContinueOnError)
	bodyHash := fs.String("body-hash", "", "")
	kind := fs.String("kind", "", "")
	mediaType := fs.String("media-type", "", "")
	title := fs.String("title", "", "")
	supersedes := fs.String("supersedes", "", "")
	content := fs.Bool("content", false, "")
	ptype := fs.String("type", "", "") // spec 0034: the name's package type
	pos, err := flags(fs, args)
	if err != nil || len(pos) < 2 {
		return errUsage
	}
	typ, err := pkgtype.Parse(*ptype)
	if err != nil {
		return err
	}
	t, ext := pos[0], pos[1]
	pkg := pkgtype.Key{Type: typ, Name: ext}
	withBlob := func() (func(), error) {
		svc, err := app.BlobService(ctx, a.cfg, a.svc.Store, slog.New(slog.NewTextHandler(a.errw, &slog.HandlerOptions{Level: slog.LevelWarn})))
		if err != nil {
			return nil, err
		}
		a.svc.WithBlob(svc)
		return func() { _ = svc.Close() }, nil
	}
	switch {
	case sub == "add" && len(pos) == 3 && *bodyHash != "" && *kind != "" && *mediaType != "" && *title != "":
		var b []byte
		if pos[2] == "-" {
			b, err = io.ReadAll(io.LimitReader(a.in, attachment.MaxConfigurableSize+1))
		} else {
			b, err = os.ReadFile(pos[2])
		}
		if err != nil {
			return err
		}
		done, err := withBlob()
		if err != nil {
			return err
		}
		defer done()
		x, err := a.svc.Attachments.Attach(ctx, a.actor, t, pkg, attachment.AttachInput{BodyHash: *bodyHash, Kind: *kind,
			MediaType: *mediaType, Title: *title, Supersedes: *supersedes, Content: b})
		if err != nil {
			return err
		}
		a.logf("attachment add %s %s %s %s", t, ext, x.Kind, x.ID)
		fmt.Fprintln(a.out, x.ID)
	case sub == "list" && len(pos) == 2:
		all, err := a.svc.Attachments.List(ctx, a.actor, t, pkg)
		if err != nil {
			return err
		}
		heads := map[string]bool{}
		for _, x := range attachment.Heads(all) {
			heads[x.ID] = true
		}
		a.table("ID\tBODY\tKIND\tTITLE\tORIGIN\tVISIBILITY\tHEAD\tSUMMARY", func(w io.Writer) {
			for _, x := range all {
				if *bodyHash != "" && x.BodyHash != *bodyHash {
					continue
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%v\t%s\n", x.ID, x.BodyHash[:12], x.Kind, x.Title, x.Origin, x.Visibility,
					heads[x.ID], x.Summary)
			}
		})
	case sub == "show" && len(pos) == 3:
		if *content {
			done, err := withBlob()
			if err != nil {
				return err
			}
			defer done()
			return a.svc.Attachments.Content(ctx, a.actor, t, pkg, pos[2], func(_ store.Attachment, b []byte) error {
				_, err := a.out.Write(b)
				return err
			})
		}
		x, err := a.svc.Attachments.Get(ctx, a.actor, t, pkg, pos[2])
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "%s %s %q\nbody %s\nmedia type %s, %d bytes, sha256 %s\norigin %s (%s), %s\nsummary %s\n",
			x.ID, x.Kind, x.Title, x.BodyHash, x.MediaType, x.Size, x.ContentHash, x.Origin, x.AttachedBy, x.Visibility, x.Summary)
	case (sub == "readers" || sub == "administrators") && len(pos) == 3:
		v := sub
		if _, err := a.svc.Attachments.SetFlags(ctx, a.actor, t, pkg, pos[2], &v, nil, 0); err != nil {
			return err
		}
		a.logf("attachment %s %s %s %s", sub, t, ext, pos[2])
	case (sub == "feedable" || sub == "unfeedable") && len(pos) == 3:
		f := sub == "feedable"
		if _, err := a.svc.Attachments.SetFlags(ctx, a.actor, t, pkg, pos[2], nil, &f, 0); err != nil {
			return err
		}
		a.logf("attachment %s %s %s %s", sub, t, ext, pos[2])
	case sub == "remove" && len(pos) == 3:
		done, err := withBlob()
		if err != nil {
			return err
		}
		defer done()
		if err := a.svc.Attachments.Remove(ctx, a.actor, t, pkg, pos[2]); err != nil {
			return err
		}
		a.logf("attachment remove %s %s %s", t, ext, pos[2])
	default:
		return errUsage
	}
	return nil
}

func (a *adminCmd) extensionGroup(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("extension-group", flag.ContinueOnError)
	description := fs.String("description", "", "")
	add := fs.String("add", "", "")
	remove := fs.String("remove", "", "")
	ptype := fs.String("type", "", "") // spec 0034: the members' package type
	pos, err := flags(fs, args)
	if err != nil || len(pos) == 0 {
		return errUsage
	}
	split := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	}
	switch {
	case sub == "add" && len(pos) == 2:
		if _, err := a.svc.Auth.CreatePackageGroup(ctx, a.actor, pos[0], pos[1], *description); err != nil {
			return err
		}
		a.logf("extension-group add %s %s", pos[0], pos[1])
	case sub == "list" && len(pos) == 1:
		gs, _, err := a.svc.Auth.ListPackageGroups(ctx, a.actor, pos[0], "", "")
		if err != nil {
			return err
		}
		a.table("NAME\tMEMBERS\tGRANTS\tDESCRIPTION", func(w io.Writer) {
			for _, g := range gs {
				fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", g.Name, g.Members, g.Grants, g.Description)
			}
		})
	case sub == "show" && len(pos) == 2:
		g, _, err := a.svc.Auth.GetPackageGroup(ctx, a.actor, pos[0], pos[1])
		if err != nil {
			return err
		}
		ms, err := a.svc.Auth.PackageGroupMembers(ctx, a.actor, pos[0], pos[1])
		if err != nil {
			return err
		}
		gs, err := a.svc.Auth.ListGrants(ctx, a.actor, pos[0])
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "%s: %s\ncreated %s by %s\n\nmembers (%d):\n", g.Name, g.Description, ts(g.CreatedAt), g.CreatedBy, len(ms))
		for _, m := range ms {
			fmt.Fprintf(a.out, "  %s\n", memberName(m))
		}
		fmt.Fprintln(a.out, "\ngrants:")
		a.grantTable(gs, g.Name, "", "", false)
	case sub == "description" && len(pos) == 3:
		if _, err := a.svc.Auth.SetPackageGroupDescription(ctx, a.actor, pos[0], pos[1], pos[2], 0); err != nil {
			return err
		}
		a.logf("extension-group description %s %s", pos[0], pos[1])
	case sub == "remove" && len(pos) == 2:
		if err := a.svc.Auth.RemovePackageGroup(ctx, a.actor, pos[0], pos[1], 0); err != nil {
			return err
		}
		a.logf("extension-group remove %s %s", pos[0], pos[1])
	case sub == "members" && len(pos) == 2 && *add == "" && *remove == "":
		ms, err := a.svc.Auth.PackageGroupMembers(ctx, a.actor, pos[0], pos[1])
		if err != nil {
			return err
		}
		a.table("NAME\tADDED\tBY", func(w io.Writer) {
			for _, m := range ms {
				fmt.Fprintf(w, "%s\t%s\t%s\n", memberName(m), ts(m.AddedAt), m.AddedBy)
			}
		})
	case sub == "members" && len(pos) == 2:
		// every name checked first, then in batches of tenants.MaxMemberChange: removals first, so a
		// full group can take others
		for _, n := range slices.Concat(split(*add), split(*remove)) {
			if err := release.ValidName(n); err != nil {
				return err
			}
		}
		var ops [][2][]string
		for _, part := range []struct {
			names  []string
			remove bool
		}{{split(*remove), true}, {split(*add), false}} {
			for i := 0; i < len(part.names); i += tenants.MaxMemberChange {
				batch := part.names[i:min(i+tenants.MaxMemberChange, len(part.names))]
				if part.remove {
					ops = append(ops, [2][]string{nil, batch})
				} else {
					ops = append(ops, [2][]string{batch, nil})
				}
			}
		}
		var added, removed int
		for _, op := range ops {
			ad, rm, err := a.svc.Auth.ChangePackageGroupMembers(ctx, a.actor, pos[0], pos[1], *ptype, op[0], op[1])
			if err != nil {
				if added+removed > 0 {
					a.logf("extension-group members %s %s: %d added, %d removed before the error", pos[0], pos[1], added, removed)
				}
				return err
			}
			added, removed = added+len(ad), removed+len(rm)
		}
		a.logf("extension-group members %s %s: %d added, %d removed", pos[0], pos[1], added, removed)
	default:
		return errUsage
	}
	return nil
}

// memberName is a group member's name, with its package type when it is not the default (spec
// 0034: type:name).
func memberName(m store.GroupMember) string {
	if m.PackageType != pkgtype.Default {
		return m.PackageType + ":" + m.Name
	}
	return m.Name
}

func (a *adminCmd) block(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("block", flag.ContinueOnError)
	reason := fs.String("reason", "", "")
	pos, err := flags(fs, args)
	if err != nil || len(pos) == 0 {
		return errUsage
	}
	switch {
	case sub == "add" && len(pos) == 2 && *reason != "":
		b, existed, err := a.svc.Releases.Block(ctx, a.actor, pos[0], pos[1], *reason)
		if err != nil {
			return err
		}
		if existed {
			fmt.Fprintln(a.errw, "the body was blocked already; its releases are yanked")
		}
		a.logf("block add %s %s", pos[0], b.BodyHash)
	case sub == "list" && len(pos) == 1:
		bs, err := a.svc.Releases.ListBlocks(ctx, a.actor, pos[0])
		if err != nil {
			return err
		}
		a.table("BODY HASH\tREASON\tCREATED\tBY", func(w io.Writer) {
			for _, b := range bs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", b.BodyHash, b.Reason, ts(b.CreatedAt), b.CreatedBy)
			}
		})
	case sub == "remove" && len(pos) == 2:
		if err := a.svc.Releases.Unblock(ctx, a.actor, pos[0], pos[1]); err != nil {
			return err
		}
		a.logf("block remove %s %s", pos[0], pos[1])
	default:
		return errUsage
	}
	return nil
}

func (a *adminCmd) publisher(ctx context.Context, sub string, args []string) error {
	if sub == "key" {
		if len(args) == 0 {
			return errUsage
		}
		return a.publisherKey(ctx, args[0], args[1:])
	}
	fs := flag.NewFlagSet("publisher", flag.ContinueOnError)
	dist := fs.String("distributor", "", "") // spec 0033 phase B: a distributor's publisher, marked at its creation
	pos, err := flags(fs, args)
	if err != nil || *dist != "" && sub != "add" {
		return errUsage
	}
	switch {
	case sub == "add" && len(pos) == 2 && *dist != "":
		if _, err := a.svc.Auth.AddDistributorPublisher(ctx, a.actor, pos[0], pos[1], *dist); err != nil {
			return err
		}
		a.logf("publisher add %s %s -distributor %s", pos[0], pos[1], *dist)
	case sub == "add" && len(pos) == 2:
		if _, err := a.svc.Auth.AddPublisher(ctx, a.actor, pos[0], pos[1]); err != nil {
			return err
		}
		a.logf("publisher add %s %s", pos[0], pos[1])
	case sub == "remove" && len(pos) == 2:
		if _, err := a.svc.Auth.RemovePublisher(ctx, a.actor, pos[0], pos[1]); err != nil {
			return err
		}
		a.logf("publisher remove %s %s", pos[0], pos[1])
	case sub == "hold" && len(pos) == 3:
		d, err := parseHold(pos[2])
		if err != nil {
			return err
		}
		if _, err := a.svc.Auth.SetPublisherHold(ctx, a.actor, pos[0], pos[1], d); err != nil {
			return err
		}
		a.logf("publisher hold %s %s %s", pos[0], pos[1], d)
	case sub == "license-keys" && len(pos) == 3 && (pos[2] == "on" || pos[2] == "off"):
		if _, err := a.svc.Auth.SetPublisherLicenseKeys(ctx, a.actor, pos[0], pos[1], pos[2] == "on"); err != nil {
			return err
		}
		a.logf("publisher license-keys %s %s %s", pos[0], pos[1], pos[2])
	case sub == "distributor" && len(pos) == 3: // spec 0033 phase B: a distributor's publisher
		if _, err := a.svc.Auth.SetPublisherDistributor(ctx, a.actor, pos[0], pos[1], pos[2]); err != nil {
			return err
		}
		a.logf("publisher distributor %s %s %s", pos[0], pos[1], pos[2])
	case sub == "list" && len(pos) == 1:
		ps, err := a.svc.Auth.ListPublishers(ctx, a.actor, pos[0])
		if err != nil {
			return err
		}
		keys, err := a.svc.Auth.TenantPublisherKeys(ctx, a.actor, pos[0])
		if err != nil {
			return err
		}
		t, err := a.svc.Store.GetTenant(ctx, pos[0])
		if err != nil {
			return err
		}
		// HOLD is the publisher's own; a distributor's publisher's entries are held for the larger of
		// it and the tenant's minimum (spec 0037: HELD FOR)
		a.table("PUBLISHER\tID\tLICENSE KEYS\tKEY\tFINGERPRINT\tTITLE\tHOLD\tHELD FOR\tDISTRIBUTOR", func(w io.Writer) {
			for _, p := range ps {
				mark := "-"
				if p.LicenseKeys {
					mark = "yes"
				}
				held := holdText(effectiveHold(p, t))
				if len(keys[p.ID]) == 0 {
					fmt.Fprintf(w, "%s\t%s\t%s\t-\t\t\t%s\t%s\t%s\n", p.Name, p.ID, mark, holdText(p.Hold), held, dash(p.Distributor))
				}
				for _, k := range keys[p.ID] {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, p.ID, mark, k.ID, k.Fingerprint, k.Title, holdText(p.Hold),
						held, dash(p.Distributor))
				}
			}
		})
	case sub == "show" && len(pos) == 2: // spec 0037: with its id
		ps, err := a.svc.Auth.ListPublishers(ctx, a.actor, pos[0])
		if err != nil {
			return err
		}
		i := slices.IndexFunc(ps, func(p store.Publisher) bool { return p.Name == pos[1] })
		if i < 0 {
			return fmt.Errorf("%w: publisher %s", store.ErrNotFound, pos[1])
		}
		p := ps[i]
		keys, err := a.svc.Auth.ListPublisherKeys(ctx, a.actor, pos[0], p.Name)
		if err != nil {
			return err
		}
		line := func(k, v string) { fmt.Fprintf(a.out, "%-14s %s\n", k+":", v) }
		line("name", p.Name)
		line("id", p.ID)
		line("created", ts(p.CreatedAt))
		line("created by", p.CreatedBy)
		line("license keys", map[bool]string{true: "yes", false: "-"}[p.LicenseKeys])
		line("hold", holdText(p.Hold))
		if p.Distributor != "" { // spec 0037: its entries are held for the larger of its hold and the tenant's minimum
			t, err := a.svc.Store.GetTenant(ctx, pos[0])
			if err != nil {
				return err
			}
			line("held for", holdText(effectiveHold(p, t)))
		}
		line("distributor", dash(p.Distributor))
		line("keys", strconv.Itoa(len(keys)))
	default:
		return errUsage
	}
	return nil
}

func (a *adminCmd) publisherKey(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("publisher key", flag.ContinueOnError)
	expires := fs.String("expires", "", "")
	purgeHeld := fs.Bool("purge-held", false, "") // spec 0033: a revoked key's held entries
	pos, err := flags(fs, args)
	if err != nil || *purgeHeld && sub != "remove" {
		return errUsage
	}
	switch {
	case sub == "add" && len(pos) == 3:
		var at time.Time
		if *expires != "" {
			if d, ok := strings.CutSuffix(*expires, "d"); ok {
				n, err := strconv.Atoi(d)
				if err != nil || n <= 0 || n > 366 {
					return fmt.Errorf("%w: -expires <days>d or an RFC 3339 time", errUsage)
				}
				at = time.Now().Add(time.Duration(n) * 24 * time.Hour)
			} else if at, err = time.Parse(time.RFC3339, *expires); err != nil {
				return fmt.Errorf("%w: -expires <days>d or an RFC 3339 time", errUsage)
			}
		}
		in := a.in
		if pos[2] != "-" {
			f, err := os.Open(pos[2])
			if err != nil {
				return err
			}
			defer f.Close()
			in = f
		}
		line, err := io.ReadAll(io.LimitReader(in, 4096))
		if err != nil {
			return err
		}
		k, err := a.svc.Auth.AddPublisherKey(ctx, a.actor, pos[0], pos[1], string(line), at)
		if err != nil {
			return err
		}
		a.logf("publisher key add %s %s %s %s", pos[0], pos[1], k.ID, k.Fingerprint)
		fmt.Fprintln(a.out, k.ID)
	case *expires != "":
		return errUsage // -expires is add's
	case sub == "list" && len(pos) == 2:
		ks, err := a.svc.Auth.ListPublisherKeys(ctx, a.actor, pos[0], pos[1])
		if err != nil {
			return err
		}
		a.table("ID\tFINGERPRINT\tTYPE\tTITLE\tEXPIRES\tLAST USED\tCREATED\tBY", func(w io.Writer) {
			for _, k := range ks {
				exp, used := "-", "-"
				if !k.ExpiresAt.IsZero() {
					exp = ts(k.ExpiresAt)
				}
				if !k.LastUsedAt.IsZero() {
					used = ts(k.LastUsedAt)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", k.ID, k.Fingerprint, k.Type, k.Title, exp, used, ts(k.CreatedAt), k.CreatedBy)
			}
		})
	case sub == "remove" && len(pos) == 3:
		hp, err := a.svc.Auth.RemovePublisherKey(ctx, a.actor, pos[0], pos[1], pos[2], *purgeHeld)
		if err != nil {
			return err
		}
		a.logf("publisher key remove %s %s %s%s", pos[0], pos[1], pos[2], map[bool]string{true: " (held entries purged)"}[*purgeHeld])
		if *purgeHeld { // spec 0033: what the purge did; the entries placed early are kept, revoked
			fmt.Fprintf(a.out, "purged %d held entries\n", len(hp.Purged))
			if len(hp.Kept) > 0 {
				a.table("KEPT (REVOKED)\tNAME\tVERSION\tPLATFORM\tSLOT\tCHANNELS", func(w io.Writer) {
					for _, k := range hp.Kept {
						fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\n", k.ID, k.Name, k.PackageVersion, k.Platform, k.Slot(), k.Channels)
					}
				})
			}
		}
	default:
		return errUsage
	}
	return nil
}

func commaList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func (a *adminCmd) upstream(ctx context.Context, sub string, args []string) error {
	if sub == "extension" || sub == "platform" || sub == "key" {
		if len(args) == 0 {
			return errUsage
		}
		return a.upstreamItem(ctx, sub, args[0], args[1:])
	}
	fs := flag.NewFlagSet("upstream", flag.ContinueOnError)
	kind := fs.String("kind", "", "")
	channel := fs.String("channel", "", "")
	platforms := fs.String("platforms", "", "")
	extensions := fs.String("extensions", "", "")
	prefix := fs.String("prefix", "", "")
	keys := fs.String("keys", "", "")
	public := fs.Bool("public", false, "")
	dryRun := fs.Bool("dry-run", false, "")
	outcome := fs.String("outcome", "", "")
	mode := fs.String("mode", "", "")
	cred := fs.String("credential", "", "")
	fetch := fs.String("fetch", "", "")
	sync := fs.Bool("sync", false, "")
	pos, err := flags(fs, args)
	if err != nil {
		return err
	}
	show := func(u store.Upstream) {
		fmt.Fprintf(a.out, "name: %s\nkind: %s\nmode: %s\nstate: %s\nvisibility: %s\nplatforms: %s\n", u.Name, u.Kind, u.Mode, u.State,
			u.Visibility, strings.Join(u.Platforms, ","))
		if u.Mode == store.ModeMirror {
			fmt.Fprintf(a.out, "fetch: %s\n", u.FetchMode)
		}
		if u.Hold > 0 {
			fmt.Fprintf(a.out, "hold: %s\n", u.Hold)
		}
		if u.Prefix != "" {
			fmt.Fprintf(a.out, "prefix: %s\nkeys: %s\n", u.Prefix, strings.Join(u.Keys, ","))
		}
		if u.Credential != "" {
			fmt.Fprintf(a.out, "credential: %s\n", u.Credential)
		}
		if u.BundleID != "" {
			fmt.Fprintf(a.out, "last bundle: %s (made %s)\n", u.BundleID, ts(u.BundleCreatedAt))
		}
		for _, e := range u.Entries {
			fmt.Fprintf(a.out, "extension: %s %s%s%s\n", e.Name, strings.Join(e.Versions, ","), map[bool]string{true: " (allow reserved)"}[e.AllowReserved],
				map[bool]string{true: " (eager)"}[e.Eager])
		}
		if !u.RequestedAt.IsZero() {
			fmt.Fprintf(a.out, "run: requested %s\n", ts(u.RequestedAt))
		}
		if u.LastRun != "" {
			fmt.Fprintf(a.out, "last run: %s\n", u.LastRun)
		}
	}
	switch {
	case sub == "add" && len(pos) == 2:
		sp := upstream.Spec{Name: pos[1], Kind: *kind, Channel: *channel, Prefix: *prefix, Keys: commaList(*keys),
			Platforms: commaList(*platforms), Visibility: store.Private, Mode: *mode, Credential: *cred, FetchMode: *fetch}
		if *public {
			sp.Visibility = store.Public
		}
		for _, n := range commaList(*extensions) {
			sp.Entries = append(sp.Entries, store.UpstreamEntry{Name: n})
		}
		u, err := a.svc.Upstreams.Add(ctx, a.actor, pos[0], sp)
		if err != nil {
			return err
		}
		a.logf("upstream add %s %s (%s, channel %s)", pos[0], u.Name, u.Kind, *channel)
	case sub == "list" && len(pos) == 1:
		us, err := a.svc.Upstreams.List(ctx, a.actor, pos[0])
		if err != nil {
			return err
		}
		a.table("NAME\tKIND\tMODE\tSTATE\tVISIBILITY\tEXTENSIONS\tLAST RUN", func(w io.Writer) {
			for _, u := range us {
				last := "-"
				if !u.LastRunAt.IsZero() {
					last = ts(u.LastRunAt)
				}
				m := u.Mode
				if u.FetchMode == store.FetchLazy {
					m += " (lazy)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", u.Name, u.Kind, m, u.State, u.Visibility, len(u.Entries), last)
			}
		})
	case sub == "show" && len(pos) == 2:
		u, err := a.svc.Upstreams.Get(ctx, a.actor, pos[0], pos[1])
		if err != nil {
			return err
		}
		show(u)
	case sub == "remove" && len(pos) == 2:
		if err := a.svc.Upstreams.Remove(ctx, a.actor, pos[0], pos[1], 0); err != nil {
			return err
		}
		a.logf("upstream remove %s %s", pos[0], pos[1])
	case (sub == "pause" || sub == "resume" || sub == "public" || sub == "private") && len(pos) == 2:
		vis, state := map[string]string{"public": store.Public, "private": store.Private}[sub],
			map[string]string{"pause": store.UpstreamPaused, "resume": store.UpstreamActive}[sub]
		if _, err := a.svc.Upstreams.Set(ctx, a.actor, pos[0], pos[1], vis, state, 0); err != nil {
			return err
		}
		a.logf("upstream %s %s %s", sub, pos[0], pos[1])
	case sub == "credential" && len(pos) == 3:
		name := pos[2]
		if name == "-" {
			name = ""
		}
		if _, err := a.svc.Upstreams.SetCredential(ctx, a.actor, pos[0], pos[1], name, 0); err != nil {
			return err
		}
		a.logf("upstream credential %s %s %s", pos[0], pos[1], pos[2])
	case sub == "fetch" && len(pos) == 3:
		if _, err := a.svc.Upstreams.SetFetchMode(ctx, a.actor, pos[0], pos[1], pos[2], 0); err != nil {
			return err
		}
		a.logf("upstream fetch %s %s %s", pos[0], pos[1], pos[2])
	case sub == "hold" && len(pos) == 3:
		d, err := parseHold(pos[2])
		if err != nil {
			return err
		}
		if _, err := a.svc.Upstreams.SetHold(ctx, a.actor, pos[0], pos[1], d, 0); err != nil {
			return err
		}
		a.logf("upstream hold %s %s %s", pos[0], pos[1], d)
	case sub == "credentials" && len(pos) == 0:
		fws, err := a.svc.Upstreams.Store.UsageForwarders(ctx)
		if err != nil {
			return err
		}
		a.table("NAME\tKIND\tTENANTS\tPREFIXES\tPUBLIC\tFORWARDS USAGE", func(w io.Writer) {
			for _, i := range a.svc.Upstreams.Credentials.Infos() {
				var by []string
				for _, f := range fws {
					if f.Credential == i.Name {
						by = append(by, f.Tenant+"/"+f.Upstream)
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\t%s\n", i.Name, i.Kind, strings.Join(i.Tenants, ","), strings.Join(i.Prefixes, ","), i.AllowPublic,
					strings.Join(by, ","))
			}
		})
	case sub == "sync" && len(pos) == 2:
		if _, err := a.svc.Upstreams.Sync(ctx, a.actor, pos[0], pos[1], *dryRun); err != nil {
			return err
		}
		a.logf("upstream sync %s %s (dry run: %v): a running kista serve takes it", pos[0], pos[1], *dryRun)
	case sub == "licenses" && len(pos) == 2:
		if *sync {
			if err := a.svc.Licenses.Sync(ctx, a.actor, pos[0], pos[1]); err != nil {
				return err
			}
			a.logf("upstream licenses %s %s: a running kista serve takes the relay", pos[0], pos[1])
		}
		at, res, err := a.svc.Licenses.LastSync(ctx, a.actor, pos[0], pos[1])
		if err != nil {
			return err
		}
		label := "last relay"
		if *sync {
			label = "previous relay" // the one requested has not run yet
		}
		if res == nil {
			fmt.Fprintf(a.out, "%s: never\n", label)
			return nil
		}
		fmt.Fprintf(a.out, "%s: %s\n", label, ts(at))
		if res.Error != "" {
			fmt.Fprintf(a.out, "error: %s\n", res.Error)
		}
		for _, k := range slices.Sorted(maps.Keys(res.Counts)) {
			fmt.Fprintf(a.out, "%s: %d\n", k, res.Counts[k])
		}
		for _, r := range res.Refused {
			fmt.Fprintf(a.out, "refused: %s %s (%s): %s\n", r.Name, r.ID, r.Rule, r.Reason)
		}
	case sub == "usage" && (len(pos) == 2 || len(pos) == 3):
		if len(pos) == 3 {
			if _, err := a.svc.Upstreams.SetUsage(ctx, a.actor, pos[0], pos[1], pos[2], 0); err != nil {
				return err
			}
		}
		u, err := a.svc.Upstreams.Get(ctx, a.actor, pos[0], pos[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "usage: %s\n", u.Usage)
		at, res, err := a.svc.Upstreams.Store.GetUsageForward(ctx, u.ID)
		if err != nil {
			return err
		}
		if res == "" {
			fmt.Fprintln(a.out, "last forwarding: never")
			return nil
		}
		var r usage.ForwardResult
		if err := json.Unmarshal([]byte(res), &r); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "last forwarding: %s\nsent: %s\nskipped (the upstream takes no usage): %s\n", ts(at),
			strings.Join(r.Sent, " "), strings.Join(r.Skipped, " "))
		for _, f := range r.Failed {
			fmt.Fprintf(a.out, "failed: %s: %s\n", f.Day, f.Reason)
		}
	case sub == "cells" && len(pos) == 2:
		var after [3]string
		a.table("DUCKDB\tPLATFORM\tNAME\tOUTCOME\tFETCHED\tDETAIL", func(w io.Writer) {
			for {
				cs, err := a.svc.Upstreams.Cells(ctx, a.actor, pos[0], pos[1], *outcome, after, 500)
				if err != nil {
					fmt.Fprintf(w, "error: %v\n", err)
					return
				}
				for _, c := range cs {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", c.DuckDBVersion, c.Platform, c.Name, c.Outcome, ts(c.FetchedAt), c.Detail)
				}
				if len(cs) < 500 {
					return
				}
				l := cs[len(cs)-1]
				after = [3]string{l.DuckDBVersion, l.Platform, l.Name}
			}
		})
	default:
		return errUsage
	}
	return nil
}

func (a *adminCmd) upstreamItem(ctx context.Context, what, sub string, args []string) error {
	fs := flag.NewFlagSet("upstream "+what, flag.ContinueOnError)
	versions := fs.String("versions", "", "")
	allowReserved := fs.Bool("allow-reserved", false, "")
	eager := fs.Bool("eager", false, "")
	pos, err := flags(fs, args)
	if err != nil || len(pos) != 3 || (sub != "add" && sub != "remove") {
		return errUsage
	}
	if what != "extension" && (*versions != "" || *allowReserved || *eager) {
		return errUsage
	}
	t, name, item := pos[0], pos[1], pos[2]
	up := a.svc.Upstreams
	switch what + " " + sub {
	case "extension add":
		_, err = up.PutEntry(ctx, a.actor, t, name, store.UpstreamEntry{Name: item, Versions: commaList(*versions), AllowReserved: *allowReserved,
			Eager: *eager})
	case "extension remove":
		err = up.RemoveEntry(ctx, a.actor, t, name, item)
	case "platform add":
		err = up.AddPlatform(ctx, a.actor, t, name, item)
	case "platform remove":
		err = up.RemovePlatform(ctx, a.actor, t, name, item)
	case "key add":
		err = up.AddKey(ctx, a.actor, t, name, item)
	case "key remove":
		err = up.RemoveKey(ctx, a.actor, t, name, item)
	}
	if err != nil {
		return err
	}
	a.logf("upstream %s %s %s %s %s", what, sub, t, name, item)
	return nil
}

func (a *adminCmd) shadow(ctx context.Context, sub string, args []string) error {
	pos, err := flags(flag.NewFlagSet("shadow", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	switch {
	case sub == "list" && len(pos) == 1:
		xs, err := a.svc.Upstreams.Shadows(ctx, a.actor, pos[0])
		if err != nil {
			return err
		}
		a.table("NAME\tCREATED\tBY", func(w io.Writer) {
			for _, x := range xs {
				fmt.Fprintf(w, "%s\t%s\t%s\n", x.Name, ts(x.CreatedAt), x.CreatedBy)
			}
		})
	case sub == "remove" && len(pos) == 2:
		if err := a.svc.Upstreams.RemoveShadow(ctx, a.actor, pos[0], pos[1]); err != nil {
			return err
		}
		a.logf("shadow remove %s %s", pos[0], pos[1])
	default:
		return errUsage
	}
	return nil
}

func (a *adminCmd) events(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	server := fs.Bool("server", false, "")
	kind := fs.String("kind", "", "")
	subject := fs.String("subject", "", "")
	actor := fs.String("actor", "", "")
	since := fs.String("since", "", "")
	limit := fs.Int("limit", 100, "")
	follow := fs.Bool("follow", false, "")
	format := fs.String("format", "table", "")
	pos, err := flags(fs, args)
	if err != nil || sub != "list" || (*server) == (len(pos) == 1) || len(pos) > 1 || (*format != "table" && *format != "jsonl") {
		return errUsage
	}
	tenantID := audit.ServerTenant
	if !*server {
		t, _, err := a.svc.Tenants.Show(ctx, a.actor, pos[0]) // spec 0019: a tombstone's events too
		if err != nil {
			return err
		}
		tenantID = t.ID
	}
	f := store.EventFilter{Kind: *kind, Subject: *subject, Actor: *actor, Limit: *limit}
	if *since != "" {
		if f.Since, err = time.Parse(time.RFC3339Nano, *since); err != nil {
			return fmt.Errorf("%w: -since is an RFC 3339 time", errUsage)
		}
	}
	header := true
	print := func(evs []store.Event) {
		if *format == "jsonl" {
			for _, e := range evs {
				b, err := json.Marshal(map[string]any{"id": e.ID, "at": e.At.UTC().Format(time.RFC3339Nano), "kind": e.Kind, "v": e.V,
					"outcome": e.Outcome, "actor": e.Actor, "actor_name": e.ActorName, "subject": e.Subject,
					"data": json.RawMessage(e.Data), "client": e.Client, "request": e.Request})
				if err == nil {
					fmt.Fprintln(a.out, string(b))
				}
			}
			return
		}
		w := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
		if header {
			fmt.Fprintln(w, "AT\tKIND\tOUTCOME\tACTOR\tSUBJECT\tDATA")
			header = false
		}
		for _, e := range evs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", e.At.UTC().Format(time.RFC3339), e.Kind, e.Outcome, e.Actor, e.Subject, e.Data)
		}
		w.Flush()
	}
	if !*follow {
		evs, err := a.svc.Store.ListEvents(ctx, tenantID, f)
		if err != nil {
			return err
		}
		print(evs)
		return nil
	}
	// -follow: oldest first, polling the store; each poll looks back a window, so an event that
	// commits after a later one was printed is still printed (once: its id is remembered)
	const window = 30 * time.Second
	f.Ascending, f.Limit = true, 1000
	from := f.Since
	if from.IsZero() {
		from = time.Now().Add(-time.Minute)
	}
	seen := map[string]time.Time{}
	for {
		f.Since = from
		evs, err := a.svc.Store.ListEvents(ctx, tenantID, f)
		if err != nil {
			return err
		}
		var fresh []store.Event
		for _, e := range evs {
			if _, ok := seen[e.ID]; !ok {
				seen[e.ID] = e.At
				fresh = append(fresh, e)
			}
		}
		print(fresh)
		if len(evs) > 0 {
			if last := evs[len(evs)-1].At.Add(-window); last.After(from) {
				from = last
			}
		}
		for id, at := range seen {
			if at.Before(from) {
				delete(seen, id)
			}
		}
		if len(evs) == f.Limit && len(fresh) > 0 {
			continue // more waiting: no pause
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// blobVerify checks every record of the storage domains against its object (spec 0018).
func (a *adminCmd) blobVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("blob verify", flag.ContinueOnError)
	domain := fs.String("domain", "", "")
	full := fs.Bool("full", false, "")
	mark := fs.Bool("mark", false, "")
	unmark := fs.Bool("unmark", false, "")
	conc := fs.Int("concurrency", 4, "")
	if pos, err := flags(fs, args); err != nil || len(pos) != 0 || *conc < 1 || *conc > 32 || (*mark || *unmark) && !*full {
		return errUsage
	}
	svc, err := app.BlobService(ctx, a.cfg, a.svc.Store, slog.New(slog.DiscardHandler))
	if err != nil {
		return err
	}
	defer func() { _ = svc.Close() }()
	domains := svc.Domains()
	if *domain != "" {
		if !svc.HasDomain(*domain) {
			return fmt.Errorf("no storage domain %s in the configuration", *domain)
		}
		domains = []string{*domain}
	}
	problem := func(p blob.Problem) {
		note := ""
		switch {
		case p.Marked:
			note = " (marked)"
		case p.Unmarked:
			note = " (unmarked)"
		case p.WasMarked:
			note = " (marked before: served 503)"
		}
		fmt.Fprintf(a.out, "%s\t%s\t%s\t%s\t%s%s\n", p.Domain, p.BodyHash, p.StreamHash, p.Outcome, p.Detail, note)
	}
	var bad []string
	results := make([]blob.VerifyResult, 0, len(domains))
	for _, d := range domains {
		r, err := svc.Verify(ctx, d, blob.VerifyOptions{Full: *full, Mark: *mark, Unmark: *unmark, Concurrency: *conc, Problem: problem})
		if eerr := app.VerifyEvent(context.WithoutCancel(ctx), a.svc.Store, a.actor.String(), r, err); eerr != nil {
			fmt.Fprintf(a.errw, "domain %s: recording the pass: %v\n", d, eerr)
		}
		if err != nil {
			fmt.Fprintf(a.errw, "domain %s: %v\n", d, err)
		}
		if err != nil || !r.Clean() {
			bad = append(bad, d)
		}
		results = append(results, r)
	}
	a.table("DOMAIN\tCHECKED\tOK\tSKIPPED\tMARKED-OK\tMISSING\tSHORT\tLONG\tCORRUPT\tRECORD\tUNREADABLE\tMARKED\tUNMARKED\tBYTES", func(w io.Writer) {
		for _, r := range results {
			fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", r.Domain, r.Checked, r.OK, r.Skipped, r.MarkedOK,
				r.Missing, r.Short, r.Long, r.Corrupt, r.Record, r.Unreadable, r.Marked, r.Unmarked, r.Bytes)
		}
	})
	if len(bad) > 0 {
		return fmt.Errorf("storage domain(s) %s: not every body is present, intact and served", strings.Join(bad, ", "))
	}
	return nil
}

// blobGC runs a collection of the storage domains now (spec 0016), a dry run unless -apply.
func (a *adminCmd) blobGC(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("blob gc", flag.ContinueOnError)
	domain := fs.String("domain", "", "")
	apply := fs.Bool("apply", false, "")
	force := fs.Bool("force", false, "")
	if pos, err := flags(fs, args); err != nil || len(pos) != 0 {
		return errUsage
	}
	if *apply && a.cfg.GCSettings().Interval == 0 && !*force {
		return errors.New("gc.interval is 0: replicas may run a version that does not claim streams; " +
			"set gc.interval once every replica runs this one, or add -force")
	}
	svc, err := app.BlobService(ctx, a.cfg, a.svc.Store, slog.New(slog.DiscardHandler))
	if err != nil {
		return err
	}
	defer func() { _ = svc.Close() }()
	host, _ := os.Hostname()
	if len(host) > 64 {
		host = host[:64]
	}
	c := app.Collector(a.cfg, a.svc.Store, svc, "cli:"+host+"/"+store.NewID(), slog.New(slog.NewTextHandler(a.errw, nil)))
	domains := svc.Domains()
	if *domain != "" {
		domains = []string{*domain}
	}
	var failed []string
	a.table("DOMAIN\tBUILDS\tBODIES\tMARKED\tSTREAMS\tICONS\tTMP\tUPLOADS\tATTACHMENTS\tBYTES", func(w io.Writer) {
		for _, d := range domains {
			r, err := c.Pass(ctx, d, !*apply)
			switch {
			case r.Skipped:
				fmt.Fprintf(w, "%s\tskipped: another replica is collecting it\n", d)
			default:
				fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", d, r.Builds, r.Bodies, r.Marked, r.Streams, r.Icons, r.Tmp, r.Uploads, r.Attachments,
					r.Bytes)
			}
			if err != nil {
				failed = append(failed, d)
				fmt.Fprintf(a.errw, "domain %s: %v\n", d, err)
			}
		}
	})
	if len(failed) > 0 {
		return fmt.Errorf("the collection of %s stopped on an error", strings.Join(failed, ", "))
	}
	if !*apply {
		fmt.Fprintln(a.out, "a dry run: nothing was deleted (-apply to collect)")
	}
	return nil
}

// attestationKey manages a tenant's attestation keys (spec 0011 phase 1b).
func (a *adminCmd) attestationKey(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("attestation-key", flag.ContinueOnError)
	active := fs.Bool("active", false, "")
	force := fs.Bool("force", false, "")
	pos, err := flags(fs, args)
	if err != nil || len(pos) < 1 {
		return errUsage
	}
	switch {
	case sub == "add" && len(pos) == 2:
		k, err := a.svc.Keys.AddAttestation(ctx, a.actor, pos[0], pos[1], *active)
		if err != nil {
			return err
		}
		a.logf("attestation-key add %s %s (%s, %s)", pos[0], k.ID, k.Fingerprint, k.State)
		fmt.Fprintln(a.out, k.ID)
	case sub == "list" && len(pos) == 1:
		ks, err := a.svc.Keys.ListAttestation(ctx, a.actor, pos[0])
		if err != nil {
			return err
		}
		a.table("ID\tFINGERPRINT\tSTATE\tTRUSTED SINCE\tSIGNER", func(w io.Writer) {
			for _, k := range ks {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", k.ID, k.Fingerprint, k.State, ts(k.TrustedSince), k.SignerRef)
			}
		})
	case (sub == "activate" || sub == "retire") && len(pos) == 2:
		do := a.svc.Keys.RetireAttestation
		if sub == "activate" {
			do = a.svc.Keys.ActivateAttestation
		}
		k, err := do(ctx, a.actor, pos[0], pos[1], 0, *force)
		if err != nil {
			return err
		}
		a.logf("attestation-key %s %s %s (%s)", sub, pos[0], k.ID, k.Fingerprint)
	default:
		return errUsage
	}
	return nil
}

// optBool is a boolean flag that records whether it was given (-readers, -readers=false).
type optBool struct{ set, v bool }

func (b *optBool) String() string   { return strconv.FormatBool(b.v) }
func (b *optBool) IsBoolFlag() bool { return true }
func (b *optBool) Set(s string) error {
	v, err := strconv.ParseBool(s)
	b.set, b.v = true, v
	return err
}

func csv(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// feed manages a tenant's feeds (spec 0011 phase 1b).
func (a *adminCmd) feed(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("feed", flag.ContinueOnError)
	feedURL := fs.String("url", "", "")
	cred := fs.String("credential", "", "")
	keys := fs.String("key", "", "")
	kinds := fs.String("kinds", "", "")
	var readers, export optBool
	fs.Var(&readers, "readers", "")
	fs.Var(&export, "export", "")
	channels := fs.String("channels", "", "")
	documents := fs.Bool("documents", false, "")
	mode := fs.String("mode", "", "")
	pos, err := flags(fs, args)
	if err != nil || len(pos) < 1 {
		return errUsage
	}
	f := a.svc.Feeds
	switch {
	case sub == "add" && len(pos) == 2 && *feedURL != "":
		sp := attachment.FeedSpec{Name: pos[1], URL: *feedURL, Credential: *cred, Keys: csv(*keys), Kinds: csv(*kinds),
			Readers: readers.v, Channels: csv(*channels), Mode: *mode, Export: export.v}
		if len(sp.Channels) > 0 {
			sp.Offer = store.OfferChannels
		}
		fd, err := f.Add(ctx, a.actor, pos[0], sp)
		if err != nil {
			return err
		}
		a.logf("feed add %s %s (%s, %s)", pos[0], fd.Name, fd.URL, fd.Mode)
	case sub == "list" && len(pos) == 1:
		fds, err := f.List(ctx, a.actor, pos[0])
		if err != nil {
			return err
		}
		a.table("NAME\tURL\tMODE\tSTATE\tSTATUS\tLAST SUCCESS\tKINDS\tREADERS\tEXPORT", func(w io.Writer) {
			for _, fd := range fds {
				last := "-"
				if !fd.LastSuccessAt.IsZero() {
					last = ts(fd.LastSuccessAt)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%v\t%v\n", fd.Name, fd.URL, fd.Mode, fd.State, fd.Status, last,
					strings.Join(fd.Kinds, ","), fd.Readers, fd.Export)
			}
		})
	case sub == "show" && len(pos) == 2:
		fd, err := f.Get(ctx, a.actor, pos[0], pos[1])
		if err != nil {
			return err
		}
		last := "never"
		if !fd.LastSuccessAt.IsZero() {
			last = ts(fd.LastSuccessAt)
		}
		fmt.Fprintf(a.out, "%s %s\nmode %s, export %v\nstate %s, status %s %s\nlast success %s\ncredential %s\nkeys %s\nkinds %s, readers %v, offer %s\nlast run %s\n",
			fd.Name, fd.URL, fd.Mode, fd.Export, fd.State, fd.Status, fd.LastError, last, fd.Credential, strings.Join(fd.Keys, ","),
			strings.Join(fd.Kinds, ","), fd.Readers, fd.Offer, fd.LastRun)
		if !fd.KeysFileAt.IsZero() {
			fmt.Fprintf(a.out, "keys file kept %s\n", ts(fd.KeysFileAt))
		}
		if fd.BundleID != "" {
			fmt.Fprintf(a.out, "last bundle %s, created %s\n", fd.BundleID, ts(fd.BundleCreatedAt))
		}
	case sub == "sync" && len(pos) == 2:
		if _, err := f.Sync(ctx, a.actor, pos[0], pos[1]); err != nil {
			return err
		}
		a.logf("feed sync %s %s: a running kista serve takes it", pos[0], pos[1])
	case (sub == "pause" || sub == "resume") && len(pos) == 2:
		st := map[string]string{"pause": store.FeedPaused, "resume": store.FeedActive}[sub]
		if _, err := f.Change(ctx, a.actor, pos[0], pos[1], attachment.FeedChange{State: &st}, 0); err != nil {
			return err
		}
		a.logf("feed %s %s %s", sub, pos[0], pos[1])
	case sub == "set" && len(pos) == 2:
		var ch attachment.FeedChange
		if *cred != "" {
			ch.Credential = cred
		}
		if *keys != "" {
			k := csv(*keys)
			ch.Keys = &k
		}
		if *kinds != "" {
			k := csv(*kinds)
			if *kinds == "-" {
				k = []string{}
			}
			ch.Kinds = &k
		}
		if readers.set {
			ch.Readers = &readers.v
		}
		if export.set {
			ch.Export = &export.v
		}
		if _, err := f.Change(ctx, a.actor, pos[0], pos[1], ch, 0); err != nil {
			return err
		}
		a.logf("feed set %s %s", pos[0], pos[1])
	case sub == "remove" && len(pos) == 2:
		if err := f.Remove(ctx, a.actor, pos[0], pos[1], *documents, 0); err != nil {
			return err
		}
		a.logf("feed remove %s %s (documents removed: %v)", pos[0], pos[1], *documents)
	default:
		return errUsage
	}
	return nil
}

// deletionKeys prints the keys a deleted tenant leaves in its key sources, for the operator to
// retire there (spec 0019).
func (a *adminCmd) deletionKeys(d store.Deletion) {
	if len(d.Keys) == 0 {
		return
	}
	fmt.Fprintln(a.out, "keys to retire in their key sources:")
	a.table("KIND\tID\tFINGERPRINT\tSIGNER", func(w io.Writer) {
		for _, k := range d.Keys {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", k.Kind, k.ID, k.Fingerprint, k.SignerRef)
		}
	})
}

func tsOpt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return ts(t)
}

// bundle is spec 0014's site side.
func (a *adminCmd) bundle(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("bundle "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	names := fs.String("upstream", "", "")
	feedNames := fs.String("feed", "", "")
	out := fs.String("o", "", "")
	dryRun := fs.Bool("dry-run", false, "")
	anyTenant := fs.Bool("any-tenant", false, "")
	limit := fs.Int("limit", 50, "")
	planFile := fs.String("plan", "", "")
	keyFile := fs.String("key", "", "")
	unsigned := fs.Bool("unsigned", false, "")
	syncFirst := fs.Bool("sync", false, "")
	withLicenses := fs.Bool("licenses", false, "")
	holder := fs.String("license-holder", "", "")
	var private keyFlags
	fs.Var(&private, "private", "")
	pos, err := flags(fs, args)
	if err != nil {
		return err
	}
	if *holder != "" && sub != "plan" || *withLicenses && sub != "plan" && sub != "export" {
		return fmt.Errorf("%w: -licenses is for bundle plan and export, -license-holder for bundle plan", errUsage)
	}
	switch {
	case sub == "export" && len(pos) == 1:
		if *planFile == "" || *out == "" || (*keyFile == "") == !*unsigned {
			return fmt.Errorf("%w: bundle export needs -plan, -o, and -key or -unsigned", errUsage)
		}
		return a.bundleExport(ctx, pos[0], *planFile, *keyFile, *out, upstream.ExportOptions{Private: private, Sync: *syncFirst,
			Licenses: *withLicenses})
	case sub == "import" && len(pos) == 2:
		return a.bundleImport(ctx, pos[0], pos[1], upstream.ImportOptions{Upstreams: commaList(*names), Feeds: commaList(*feedNames),
			DryRun: *dryRun, AnyTenant: *anyTenant, BundleKeys: a.cfg.BundleKeysFor(pos[0])})
	case sub == "list" && len(pos) == 1:
		is, err := a.svc.Upstreams.Imports(ctx, a.actor, pos[0], *limit)
		if err != nil {
			return err
		}
		a.table("ID\tSTARTED\tSTATE\tDRY RUN\tMANIFEST\tCREATED", func(w io.Writer) {
			for _, i := range is {
				fmt.Fprintf(w, "%s\t%s\t%s\t%t\t%s\t%s\n", i.ID, ts(i.StartedAt), importState(i), i.DryRun, i.ManifestID, ts(i.ManifestCreatedAt))
			}
		})
	case sub == "show" && len(pos) == 2:
		i, err := a.svc.Upstreams.GetImport(ctx, a.actor, pos[0], pos[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "import       %s\nstate        %s\ndry run      %t\nmanifest     %s (created %s, sha256 %s)\n",
			i.ID, importState(i), i.DryRun, i.ManifestID, ts(i.ManifestCreatedAt), i.ManifestSHA256)
		fmt.Fprintf(a.out, "exporter     %s\nbundle key   %s\ninput sha256 %s\nstarted      %s by %s\n",
			i.Exporter, i.BundleKey, i.InputSHA256, ts(i.StartedAt), i.Actor)
		if !i.FinishedAt.IsZero() {
			fmt.Fprintf(a.out, "finished     %s\n", ts(i.FinishedAt))
		}
		var counts any
		if json.Unmarshal([]byte(i.Counts), &counts) == nil {
			b, _ := json.MarshalIndent(counts, "", "  ")
			fmt.Fprintf(a.out, "counts       %s\n", b)
		}
	case sub == "reset" && len(pos) == 1 && *feedNames != "" && !strings.Contains(*feedNames, ","):
		f, err := a.svc.Upstreams.ResetFeedBundle(ctx, a.actor, pos[0], *feedNames)
		if err != nil {
			return err
		}
		a.logf("bundle reset %s feed %s (it last imported %s; its cursors reset)", pos[0], f.Name, orNone(f.BundleID))
	case sub == "reset" && len(pos) == 2 && *feedNames == "":
		u, err := a.svc.Upstreams.ResetBundle(ctx, a.actor, pos[0], pos[1])
		if err != nil {
			return err
		}
		a.logf("bundle reset %s/%s (it last imported %s)", pos[0], u.Name, orNone(u.BundleID))
	case sub == "plan" && len(pos) == 1:
		if *holder != "" && !*withLicenses {
			return fmt.Errorf("%w: -license-holder needs -licenses", errUsage)
		}
		plan, err := a.svc.Upstreams.Plan(ctx, a.actor, pos[0], upstream.PlanTargets{Upstreams: commaList(*names), Feeds: commaList(*feedNames),
			Licenses: *withLicenses, LicenseHolder: *holder}, a.cfg.BundleKeysFor(pos[0]))
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if *out == "" {
			_, err = a.out.Write(data)
			return err
		}
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			return err
		}
		cells := 0
		for _, s := range plan.Sources {
			cells += len(s.Cells)
		}
		bodies := 0
		for _, f := range plan.Feeds {
			bodies += len(f.Bodies)
		}
		a.logf("bundle plan %s: %d sources, %d cells, %d feeds, %d feed bodies -> %s", pos[0], len(plan.Sources), cells, len(plan.Feeds), bodies, *out)
		return nil
	default:
		return errUsage
	}
	return nil
}

// errExportFailed ends an export some source of which failed as a whole.
var errExportFailed = errors.New("a source or a feed of the plan failed as a whole: see above")

// bundleExport writes a bundle answering a plan: to a temporary file beside the output, linked
// when complete, so an interrupted export leaves no output.
func (a *adminCmd) bundleExport(ctx context.Context, tenant, planFile, keyFile, out string, o upstream.ExportOptions) (err error) {
	if o.Plan, err = os.ReadFile(planFile); err != nil {
		return err
	}
	if keyFile != "" {
		kb, err := os.ReadFile(keyFile)
		if err != nil {
			return err
		}
		if o.Key, err = bundle.ParsePrivateKey(kb); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(out); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s exists, or cannot be checked: %v", out, err)
	}
	svc, err := app.BlobService(ctx, a.cfg, a.svc.Store, slog.New(slog.NewTextHandler(a.errw, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		return err
	}
	defer func() { _ = svc.Close() }()
	a.svc.WithBlob(svc)
	tmp, err := os.CreateTemp(filepath.Dir(out), ".kista-bundle-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	host, _ := os.Hostname()
	o.Holder = fmt.Sprintf("admin/%s/%d", host, os.Getpid())
	o.PublicURL, o.Version = a.cfg.Serve.PublicURL, "kista "+app.Version
	res, err := a.svc.Upstreams.Export(ctx, a.actor, tenant, tmp, o)
	if err != nil {
		return err
	}
	// the bundle stays the owner's (0600, as created): it is as sensitive as the site's configuration
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// a link never replaces a file that appeared at the output meanwhile
	if err := os.Link(tmp.Name(), out); err != nil {
		return err
	}
	signed := "unsigned: kista bundle sign signs it"
	if res.BundleKey != "" {
		signed = "signed with " + res.BundleKey
	}
	a.logf("bundle export %s: %s, %d files, %d bytes, %s -> %s", tenant, res.ID, res.Files, res.Bytes, signed, out)
	a.table("KIND\tPREFIX\tFROM\tCOUNTS\tBYTES\tERROR", func(w io.Writer) {
		for _, s := range res.Sources {
			why := ""
			if s.Error != "" {
				why = fmt.Sprintf("%q", s.Error) // it may quote a remote answer
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", s.Kind, orNone(s.Prefix), orNone(s.From), countsText(s.Counts), s.Bytes, why)
		}
	})
	for _, s := range res.Sources {
		for _, k := range s.Keys {
			fmt.Fprintf(a.out, "  %s lists key %s\n", s.Prefix, k)
		}
		for _, p := range s.Problems {
			fmt.Fprintf(a.out, "  %s %s %s %s: %s %q\n", orNone(s.Prefix), p.DuckDBVersion, p.Platform, p.Name, p.Outcome, p.Detail)
		}
		// the exporter sees what leaves: each licence's name and holder
		for _, l := range s.Licenses {
			fmt.Fprintf(a.out, "  %s licence %s, holder %q %q, expires %s\n", s.Prefix, l.Name, l.HolderID, l.Holder, ts(l.ExpiresAt))
		}
		if s.LicensesLeftOut > 0 {
			fmt.Fprintf(a.out, "  %s: %d licences over %d left out\n", s.Prefix, s.LicensesLeftOut, bundle.MaxLicenses)
		}
	}
	if res.LicenseNote != "" {
		fmt.Fprintln(a.out, res.LicenseNote)
	}
	if len(res.Feeds) > 0 {
		a.table("FEED\tFROM\tSTATUS\tLAST SUCCESS\tCOUNTS\tBYTES\tERROR", func(w io.Writer) {
			for _, f := range res.Feeds {
				last := "-"
				if !f.LastSuccess.IsZero() {
					last = ts(f.LastSuccess)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", f.URL, orNone(f.From), orNone(f.Status), last, countsText(f.Counts), f.Bytes, f.Error)
			}
		})
	}
	fmt.Fprintf(a.out, "plan sha256 %s\nmanifest sha256 %s\nsha256 %s\n", res.PlanSHA256, res.ManifestSHA256, res.SHA256)
	if res.Failed() {
		return errExportFailed
	}
	return nil
}

// errImportFailed ends an import some target of which failed, or whose bundle stopped early.
var errImportFailed = errors.New("the import did not complete: see above")

// bundleImport imports a bundle from a file, standard input ("-") or an object the operator
// uploaded to a domain's imports/ prefix ("blob:<domain>/imports/<name>").
func (a *adminCmd) bundleImport(ctx context.Context, tenant, src string, o upstream.ImportOptions) error {
	svc, err := app.BlobService(ctx, a.cfg, a.svc.Store, slog.New(slog.NewTextHandler(a.errw, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		return err
	}
	defer func() { _ = svc.Close() }()
	a.svc.WithBlob(svc)
	var in io.Reader
	switch {
	case src == "-":
		in = a.in
	case strings.HasPrefix(src, "blob:"):
		domain, name, ok := strings.Cut(strings.TrimPrefix(src, "blob:"), "/imports/")
		if !ok {
			return fmt.Errorf("%w: blob:<domain>/imports/<name>", errUsage)
		}
		rc, _, err := svc.OpenImport(ctx, domain, name)
		if err != nil {
			return err
		}
		defer rc.Close()
		in = rc
	default:
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	host, _ := os.Hostname()
	o.Holder = fmt.Sprintf("admin/%s/%d", host, os.Getpid())
	res, err := a.svc.Upstreams.Import(ctx, a.actor, tenant, in, o)
	if err != nil {
		return err
	}
	i := res.Import
	what := "bundle import"
	if i.DryRun {
		what = "bundle import (dry run)"
	}
	a.logf("%s %s: %s, manifest %s, %s", what, tenant, i.ID, i.ManifestID, i.State)
	a.table("TARGET\tSTATUS\tCOUNTS\tDETAIL", func(w io.Writer) {
		for _, t := range res.Targets {
			name, counts := t.Upstream, t.Result.Counts
			if t.Feed != "" {
				name, counts = "feed:"+t.Feed, nil
				if t.FeedRun != nil {
					counts = t.FeedRun.Counts
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, t.Status, countsText(counts), t.Detail)
		}
	})
	for _, t := range res.Targets {
		if t.FeedRun == nil {
			continue
		}
		for _, r := range t.FeedRun.Refused {
			fmt.Fprintf(a.out, "  feed:%s %s %s: %s\n", t.Feed, r.BodyHash, r.IssuerID, r.Reason)
		}
	}
	for _, t := range res.Targets {
		for _, p := range t.Result.Problems {
			fmt.Fprintf(a.out, "  %s %s %s %s: %s %s\n", t.Upstream, p.DuckDBVersion, p.Platform, p.Name, p.Outcome, p.Detail)
		}
	}
	if l := res.Licenses; l != nil {
		fmt.Fprintf(a.out, "licences: %s\n", countsText(l.Counts))
		for _, r := range l.Refused {
			fmt.Fprintf(a.out, "  licence %s %s: %s %q\n", r.Name, r.ID, r.Rule, r.Reason)
		}
		if l.Error != "" {
			fmt.Fprintf(a.out, "  licences not stored: %q\n", l.Error)
		}
	}
	if res.Unplanned > 0 {
		fmt.Fprintf(a.out, "%d cells of the bundle are in no target's matrix\n", res.Unplanned)
	}
	if res.Skipped > 0 {
		fmt.Fprintf(a.out, "%d bytes of entries were read through\n", res.Skipped)
	}
	if res.Feeds > 0 {
		fmt.Fprintf(a.out, "%d feeds of the bundle matched no feed target\n", res.Feeds)
	}
	if res.Gaps > 0 {
		fmt.Fprintf(a.out, "%d feed bodies took nothing: their range does not start where the feed is (write a new plan)\n", res.Gaps)
	}
	if res.Problem != "" {
		fmt.Fprintf(a.out, "the import stopped (%s): %q\n", res.Stop, res.Problem)
	}
	if i.InputSHA256 != "" {
		fmt.Fprintf(a.out, "input sha256 %s\n", i.InputSHA256)
	}
	if i.State != store.ImportDone || res.Gaps > 0 || res.Licenses != nil && res.Licenses.Error != "" {
		return errImportFailed
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func importState(i store.BundleImport) string {
	if i.Interrupted {
		return "failed (interrupted)"
	}
	return i.State
}

func countsText(c map[string]int) string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, c[k]))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

// bootstrap applies, plans, accepts or re-arms the configuration's bootstrap section (spec 0020
// phase 4a), as the OS user.
func (a *adminCmd) bootstrap(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dry := fs.Bool("dry-run", false, "")
	adopt := fs.Bool("adopt", false, "")
	accept := fs.Bool("accept", false, "")
	rearm := fs.Bool("rearm", false, "")
	pos, err := flags(fs, args)
	if err != nil || len(pos) != 0 || *accept && *rearm || (*accept || *rearm) && (*dry || *adopt) {
		return errUsage
	}
	d := app.BootstrapDeps(a.svc, slog.New(slog.NewTextHandler(a.errw, nil)))
	switch {
	case *rearm:
		if err := bootstrap.Rearm(ctx, d, a.actor); err != nil {
			return err
		}
		a.logf("bootstrap rearm")
		return nil
	case !a.cfg.Bootstrap.Set():
		return errors.New("the config has no bootstrap section")
	case *accept:
		if err := bootstrap.Accept(ctx, d, a.cfg.Bootstrap, a.actor); err != nil {
			return err
		}
		a.logf("bootstrap accept")
		return nil
	}
	host, _ := os.Hostname()
	res, err := bootstrap.Run(ctx, d, a.cfg.Bootstrap, bootstrap.Options{Actor: a.actor, DryRun: *dry, Adopt: *adopt,
		Source: a.cfg.Source, Version: app.Version, Host: host, Holder: host + "/" + store.NewID()})
	for _, i := range res.Plan.Items {
		fmt.Fprintln(a.out, i)
	}
	switch {
	case err != nil:
		return err
	case *dry && len(res.Plan.Refusals()) > 0:
		return fmt.Errorf("%w: %d objects", bootstrap.ErrRefused, len(res.Plan.Refusals()))
	case res.Already:
		fmt.Fprintln(a.out, "bootstrap: applied already")
	case res.Applied:
		a.logf("bootstrap %s", a.cfg.Bootstrap.Tenant.Name)
	}
	return nil
}

func (a *adminCmd) reserved(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("reserved", flag.ContinueOnError)
	kind := fs.String("kind", "", "")
	force := fs.Bool("force", false, "")
	name := fs.String("name", "", "")
	ptype := fs.String("type", "", "") // spec 0034: the names' package type
	pos, err := flags(fs, args)
	if err != nil {
		return err
	}
	switch {
	case sub == "add" && len(pos) > 0 && *name == "":
		res, err := a.svc.Tenants.Reserve(ctx, a.actor, *ptype, pos, *kind, *force)
		for n, ts := range res.Tenants {
			fmt.Fprintf(a.errw, "kista admin: %s has releases in %s\n", n, strings.Join(ts, ", "))
		}
		if err != nil {
			return err
		}
		a.logf("reserved add %s (newly reserved: %s)", *kind, joinOrNone(res.Added))
	case sub == "list" && len(pos) == 0:
		ns, err := a.svc.Tenants.ListReserved(ctx, a.actor, *ptype, *name)
		if err != nil {
			return err
		}
		a.table("NAME\tKIND\tADDED\tSOURCES", func(w io.Writer) {
			for _, n := range ns {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n.Name, n.Kind, n.AddedAt.UTC().Format(time.RFC3339), strings.Join(n.Sources, " "))
			}
		})
	case sub == "remove" && len(pos) == 1:
		if err := a.svc.Tenants.Unreserve(ctx, a.actor, *ptype, pos[0]); err != nil {
			return err
		}
		a.logf("reserved remove %s", pos[0])
	default:
		return errUsage
	}
	return nil
}

func joinOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, " ")
}

// dash is s, or "-" when it is empty: a table cell.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// catalogue lists a tenant's catalogue, shows one entry, adds a build or purges one (spec 0033).
func (a *adminCmd) catalogue(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("catalogue", flag.ContinueOnError)
	source := fs.String("source", "", "")
	name := fs.String("name", "", "")
	private := fs.Bool("private", false, "")
	ptype := fs.String("type", "", "")    // spec 0034: the catalogue's package type
	reason := fs.String("reason", "", "") // spec 0033 phase B: a withdrawal's
	// spec 0036: an entry's metadata
	summary, iconFile := fs.String("summary", "", ""), fs.String("icon", "", "")
	clearSummary, clearIcon := fs.Bool("clear-summary", false, ""), fs.Bool("clear-icon", false, "")
	pos, err := flags(fs, args)
	if err != nil {
		return err
	}
	typ, err := pkgtype.Parse(*ptype)
	if err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	metaFlags := set["summary"] || set["icon"] || *clearSummary || *clearIcon
	if *reason != "" && sub != "withdraw" || sub == "withdraw" && (*source != "" || *name != "" || *private) ||
		metaFlags && sub != "meta" || sub == "meta" && (*source != "" || *name != "" || *private || *reason != "") ||
		set["summary"] && *clearSummary || set["icon"] && (*clearIcon || *iconFile == "") {
		return errUsage // a flag another subcommand takes, or two that contradict
	}
	switch {
	case sub == "meta" && len(pos) == 3:
		return a.catalogueMeta(ctx, pos, typ, metaFlags, set["summary"], *summary, *iconFile, *clearSummary, *clearIcon)
	case sub == "withdraw" && len(pos) == 4 && *reason != "":
		w, err := a.svc.Releases.Withdraw(ctx, a.actor, pos[0], typ, pos[1], pos[2], pos[3], *reason)
		if err != nil {
			return err
		}
		if w.Already {
			fmt.Fprintln(a.errw, "the version was withdrawn already")
			return nil
		}
		a.logf("catalogue withdraw %s %s %s %s (%d entries)", pos[0], pos[1], pos[2], pos[3], len(w.Builds))
	case sub == "add" && len(pos) == 2 && *name != "":
		in := a.in
		if pos[1] != "-" {
			f, err := os.Open(pos[1])
			if err != nil {
				return err
			}
			defer f.Close()
			in = f
		}
		svc, err := app.BlobService(ctx, a.cfg, a.svc.Store, slog.New(slog.NewTextHandler(a.errw, &slog.HandlerOptions{Level: slog.LevelWarn})))
		if err != nil {
			return err
		}
		defer func() { _ = svc.Close() }()
		a.svc.WithBlob(svc)
		b, existed, err := a.svc.Releases.AddToCatalogue(ctx, a.actor, pos[0], in, release.AddOptions{Name: *name, PackageType: typ, Private: *private})
		if err != nil {
			return err
		}
		if existed {
			fmt.Fprintln(a.errw, "the catalogue already holds the build")
		} else {
			a.logf("catalogue add %s %s %s %s %s", pos[0], b.Name, b.PackageVersion, b.Platform, b.Slot())
		}
		fmt.Fprintln(a.out, b.ID)
	case sub == "purge" && len(pos) == 2:
		b, err := a.svc.Releases.PurgeEntry(ctx, a.actor, pos[0], pos[1])
		if err != nil {
			return err
		}
		a.logf("catalogue purge %s %s %s %s %s", pos[0], b.Name, b.PackageVersion, b.Platform, b.Slot())
	case sub == "list" && len(pos) == 1:
		es, err := a.svc.Releases.Catalogue(ctx, a.actor, pos[0], typ, *source, *name)
		if err != nil {
			return err
		}
		a.table("SOURCE\tNAME\tVERSIONS\tRULES\tHELD\tREVOKED\tWITHDRAWN", func(w io.Writer) {
			for _, e := range es {
				var rules []string
				for ch, mode := range e.Rules {
					rules = append(rules, ch+"="+mode)
				}
				slices.Sort(rules)
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Source, e.Name, strings.Join(e.Versions, " "), dash(strings.Join(rules, " ")),
					dash(strings.Join(e.Held, " ")), dash(strings.Join(e.Revoked, " ")), dash(strings.Join(e.Withdrawn, " ")))
			}
		})
	case sub == "show" && len(pos) == 3:
		vs, err := a.svc.Releases.CatalogueVersions(ctx, a.actor, pos[0], typ, pos[1], pos[2])
		if err != nil {
			return err
		}
		m, err := a.svc.Releases.EntryMeta(ctx, a.actor, pos[0], typ, pos[1], pos[2]) // spec 0036
		if err != nil {
			return err
		}
		now := a.svc.Store.Now()
		// the build's id last: what catalogue purge takes
		a.table("VERSION\tPLATFORM\tSLOT\tBODY\tVISIBILITY\tORIGIN\tCREATED\tRELEASED IN\tHELD UNTIL\tREVOKED\tWITHDRAWN\tID", func(w io.Writer) {
			for _, v := range vs {
				for _, b := range v.Builds {
					var in []string
					for _, p := range b.Placed {
						in = append(in, p.Channel+"("+p.State+")")
					}
					held, revoked, withdrawn := "-", "-", "-"
					if b.Held(now) {
						held = ts(b.AvailableAt)
					}
					if b.Revoked() {
						revoked = ts(b.RevokedAt)
					}
					if b.Withdrawn() {
						withdrawn = ts(b.WithdrawnAt) + " " + b.WithdrawnReason
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.Version, b.Platform, b.Slot(), b.BodyHash[:12], b.Visibility,
						b.Origin, ts(b.CreatedAt), dash(strings.Join(in, " ")), held, revoked, withdrawn, b.ID)
				}
			}
		})
		a.printMeta(m, false) // after the table, whose first line is its header
	default:
		return errUsage
	}
	return nil
}

// catalogueMeta prints an entry's metadata or, given a flag, writes it (spec 0036): it reads the
// current metadata and writes both fields, a field not named keeping its value (the icon by its
// hash).
func (a *adminCmd) catalogueMeta(ctx context.Context, pos []string, typ string, write, setSummary bool, summary, iconFile string,
	clearSummary, clearIcon bool) error {
	cur, err := a.svc.Releases.EntryMeta(ctx, a.actor, pos[0], typ, pos[1], pos[2])
	if err != nil {
		return err
	}
	if !write {
		a.printMeta(cur, true)
		return nil
	}
	w := release.MetaWrite{}
	switch {
	case setSummary:
		w.Summary = &summary
	case clearSummary:
	default:
		w.Summary = &cur.Summary
	}
	switch {
	case iconFile != "":
		f, err := os.Open(iconFile)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(f, icon.MaxBytes+1))
		f.Close()
		if err != nil {
			return err
		}
		svc, err := app.BlobService(ctx, a.cfg, a.svc.Store, slog.New(slog.NewTextHandler(a.errw, &slog.HandlerOptions{Level: slog.LevelWarn})))
		if err != nil {
			return err
		}
		defer func() { _ = svc.Close() }()
		a.svc.WithBlob(svc)
		w.Icon = &release.IconInput{ContentType: icon.ContentType, Data: data}
	case clearIcon:
	case cur.IconSHA256 != "":
		w.Icon = &release.IconInput{Keep: cur.IconSHA256}
	}
	v, err := a.svc.Releases.SetEntryMeta(ctx, a.actor, pos[0], typ, pos[1], pos[2], w)
	if err != nil {
		return err
	}
	a.logf("catalogue meta %s %s %s", pos[0], pos[1], pos[2])
	a.printMeta(v, true)
	return nil
}

// printMeta prints an entry's metadata: its summary and icon, and with who, who wrote it last.
func (a *adminCmd) printMeta(m release.EntryMetaView, who bool) {
	fmt.Fprintf(a.out, "summary: %s\n", dash(m.Summary))
	if m.IconSHA256 == "" {
		fmt.Fprintln(a.out, "icon: -")
	} else {
		fmt.Fprintf(a.out, "icon: %s %dx%d\n", m.IconSHA256, m.IconWidth, m.IconHeight)
	}
	if who && m.SetBy != "" {
		fmt.Fprintf(a.out, "set: %s by %s\n", ts(m.SetAt), m.SetBy)
	}
}

// channelFollow lists a channel's follows, follows a source, or stops following it (spec 0033).
func (a *adminCmd) channelFollow(ctx context.Context, sub string, pos []string) error {
	t, c, err := splitPath(pos[0])
	if err != nil {
		return err
	}
	var vs []release.FollowView
	switch {
	case sub == "unfollow":
		if err := a.svc.Releases.RemoveFollow(ctx, a.actor, t, c, pos[1]); err != nil {
			return err
		}
		a.logf("channel unfollow %s/%s %s", t, c, pos[1])
		return nil
	case len(pos) == 2:
		v, err := a.svc.Releases.SetFollow(ctx, a.actor, t, c, pos[1])
		if err != nil {
			return err
		}
		a.logf("channel follow %s/%s %s", t, c, pos[1])
		vs = []release.FollowView{v}
	default:
		if vs, err = a.svc.Releases.Follows(ctx, a.actor, t, c); err != nil {
			return err
		}
	}
	a.table("SOURCE\tSET BY\tSET AT\tSKIPPED", func(w io.Writer) {
		for _, v := range vs {
			var skips []string
			for n, r := range v.Skips {
				skips = append(skips, n+" ("+r+")")
			}
			slices.Sort(skips)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", v.Source, v.SetBy, ts(v.SetAt), dash(strings.Join(skips, "; ")))
		}
	})
	return nil
}

// tenantLimits prints a tenant's limits, or sets those whose flags are given (spec 0038): a flag left
// out changes nothing, an empty one is refused.
func (a *adminCmd) tenantLimits(ctx context.Context, fs *flag.FlagSet, tenant string, values map[string]*string) error {
	set := map[string]int64{}
	var bad error
	fs.Visit(func(f *flag.Flag) {
		v, ok := values[f.Name]
		if !ok || bad != nil {
			return
		}
		if *v == "" {
			bad = fmt.Errorf("%w: tenant limits: -%s is empty: give a number, unlimited or default", errUsage, f.Name)
			return
		}
		set[f.Name], bad = store.ParseLimit(f.Name, *v)
	})
	if bad != nil {
		return bad
	}
	_, l, err := a.svc.Tenants.TenantLimits(ctx, a.actor, tenant)
	if err != nil {
		return err
	}
	if len(set) > 0 {
		before := l.Set
		if _, l, err = a.svc.Tenants.SetTenantLimits(ctx, a.actor, tenant, set, ""); err != nil {
			return err
		}
		var changed []string // only what changed
		for _, n := range store.LimitNames {
			if v := l.Set.Get(n); v != before.Get(n) {
				changed = append(changed, fmt.Sprintf("%s %v", n, tenants.LimitValue(v)))
			}
		}
		if len(changed) > 0 {
			a.logf("tenant limits %s %s", tenant, strings.Join(changed, ", "))
		}
	}
	a.table("LIMIT\tSET\tIN FORCE\tUSED", func(w io.Writer) {
		for _, n := range store.LimitNames {
			in := tenants.LimitInForce(l.Limit.Get(n))
			if in == nil {
				in = "unlimited"
			}
			fmt.Fprintf(w, "%s\t%v\t%v\t%d\n", n, tenants.LimitValue(l.Set.Get(n)), in, l.Used.Get(n))
		}
	})
	return nil
}
