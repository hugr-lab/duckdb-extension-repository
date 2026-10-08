package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/app"
	"github.com/hugr-lab/duckdb-extension-repository/internal/auth"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

const adminUsage = `usage: kista admin -config <file> <command> ...

  migrate                                         apply pending migrations
  check                                           verify the schema without changing it
  backup <file>                                   consistent copy of a SQLite store
  tenant create <name> [-display-name …] [-domain <storage domain>]
  tenant list
  tenant suspend|resume <name>
  tenant audience add|remove <tenant> <audience>   assign a token audience (server-wide)
  tenant audience list <tenant>
  issuer add <tenant> -name <n> -url <issuer> [-jwks-uri u] [-alg A]... [-require claim=value]...
         [-roles-claim path] [-groups-claim path] [-client-claim path] [-max-lifetime 24h]
         a claim path is keys joined by dots (realm_access.roles), or a JSON array of keys when a
         key has dots of its own: '["https://example.com/roles"]' 
  issuer list <tenant>
  issuer remove <tenant> <name>                   removes its grants too
  grant add <tenant> -principal kind:issuer|value -verb install|admin... [-channel c] [-extension x]
  grant list <tenant>
  grant remove <tenant> <grant-id>
  version add <name> -kind release|dev [-c-api v1.5.6]...   one maximum C API per major
  version c-api <name> -c-api v2.0.0                       add a major's maximum C API
  version list
  channel create <tenant>/<channel> -kind signed|passthrough
  channel versions <tenant>/<channel> [-add v]... [-remove v]...
  channel list <tenant>
  key check -signer <ref>                         open a key, check it, sign a probe; stores nothing
  key add <tenant>/<channel> -signer <ref> [-active]
  key list <tenant>/<channel>
  key activate <tenant>/<channel> <key-id|fingerprint> [-force]
  key retire <tenant>/<channel> <key-id|fingerprint> [-force]
  key events <tenant>/<channel>
  key resign <tenant>/<channel>                   sign releases with the active key; move the serving key
  release add <tenant>/<channel> <file|-> -name <name> [-private] [-not-current]
  release list <tenant>/<channel> [-name <name>]
  release yank|deprecate|activate|current|public|private <tenant>/<channel> <release-id>
  wellknown <tenant>/<channel>
  blob check                                      pin and check the storage domains, list tenants without one
`

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// admin runs `kista admin`. It is the server administrator's CLI: it runs as the service's OS user,
// with the service's config, and calls the same services the HTTP API will.
func admin(args, env []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kista admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file")
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
	svc, err := app.NewServices(cfg, s, authz.ServerAdmin{})
	if err != nil {
		fmt.Fprintln(stderr, "kista admin:", err)
		return 1
	}
	a := &adminCmd{in: os.Stdin, cfg: cfg, svc: svc, actor: authz.OSActor(), out: stdout, errw: stderr}
	if err := a.dispatch(ctx, rest); err != nil {
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
	case "tenant":
		return a.tenant(ctx, sub, args[min(2, len(args)):])
	case "version":
		return a.version(ctx, sub, args[min(2, len(args)):])
	case "channel":
		return a.channel(ctx, sub, args[min(2, len(args)):])
	case "key":
		return a.key(ctx, sub, args[min(2, len(args)):])
	case "release":
		return a.release(ctx, sub, args[min(2, len(args)):])
	case "issuer":
		return a.issuer(ctx, sub, args[min(2, len(args)):])
	case "grant":
		return a.grant(ctx, sub, args[min(2, len(args)):])
	case "blob":
		if sub != "check" || len(args) != 2 {
			return errUsage
		}
		return a.blobCheck(ctx)
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
	pos, err := flags(fs, args)
	if err != nil {
		return err
	}
	switch {
	case sub == "create" && len(pos) == 1:
		t, err := a.svc.Tenants.CreateTenant(ctx, a.actor, pos[0], *display, *domain)
		if err != nil {
			return err
		}
		a.logf("tenant create %s", t.Name)
		fmt.Fprintln(a.out, t.ID)
	case sub == "list" && len(pos) == 0:
		ts_, err := a.svc.Tenants.ListTenants(ctx, a.actor)
		if err != nil {
			return err
		}
		a.table("NAME\tSTATE\tDOMAIN\tDISPLAY NAME\tCREATED", func(w io.Writer) {
			for _, t := range ts_ {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.State, t.StorageDomain, t.DisplayName, ts(t.CreatedAt))
			}
		})
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
	pos, err := flags(fs, args)
	if err != nil {
		return err
	}
	switch {
	case sub == "add" && len(pos) == 1:
		v, err := a.svc.Tenants.AddVersion(ctx, a.actor, pos[0], *kind, capis)
		if err != nil {
			return err
		}
		a.logf("version add %s", v.Name)
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
	var add, remove multi
	fs.Var(&add, "add", "")
	fs.Var(&remove, "remove", "")
	pos, err := flags(fs, args)
	if err != nil {
		return err
	}
	switch {
	case sub == "create" && len(pos) == 1:
		t, c, err := splitPath(pos[0])
		if err != nil {
			return err
		}
		ch, err := a.svc.Tenants.CreateChannel(ctx, a.actor, t, c, *kind)
		if err != nil {
			return err
		}
		a.logf("channel create %s/%s (%s)", t, ch.Name, ch.Kind)
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
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", c.Name, c.Kind, c.Version, ts(c.CreatedAt))
			}
		})
	default:
		return errUsage
	}
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
		fmt.Fprintln(a.out, sg.ID(), extfile.Fingerprint(sg.Public()))
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
		a.table("ID\tFINGERPRINT\tSTATE\tSIGNER\tTRUSTED SINCE\tSTATE CHANGED", func(w io.Writer) {
			for _, k := range ks {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", k.ID, k.Fingerprint, k.State, k.SignerRef, ts(k.TrustedSince), ts(k.StateChangedAt))
			}
		})
	case (sub == "activate" || sub == "retire") && len(pos) == 2:
		do := a.svc.Keys.Activate
		if sub == "retire" {
			do = a.svc.Keys.Retire
		}
		k, err := do(ctx, a.actor, t, c, pos[1], *force)
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
	pos, err := flags(fs, args)
	if err != nil || len(pos) == 0 {
		return errUsage
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
		r, existed, err := a.svc.Releases.Add(ctx, a.actor, t, c, in, release.AddOptions{Name: *name, Private: *private, NotCurrent: *notCurrent})
		if err != nil {
			return err
		}
		if existed {
			fmt.Fprintln(a.errw, "the release already exists")
		} else {
			a.logf("release add %s/%s %s %s %s %s", t, c, r.Name, r.ExtVersion, r.Platform, r.Slot)
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
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Name, r.ExtVersion, r.Platform, r.Slot,
					r.State, r.Visibility, seq, ts(r.CreatedAt))
			}
		})
	case len(pos) == 2 && slices.Contains([]string{"yank", "deprecate", "activate", "current", "public", "private"}, sub):
		r, err := a.svc.Releases.Apply(ctx, a.actor, t, c, pos[1], release.Change(sub))
		if err != nil {
			return err
		}
		a.logf("release %s %s/%s %s (%s %s, %s)", sub, t, c, r.ID, r.Name, r.ExtVersion, r.State)
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
	pos, err := flags(fs, args)
	if err != nil || len(pos) == 0 {
		return errUsage
	}
	switch {
	case sub == "add" && len(pos) == 1 && *principal != "" && len(verbs) > 0:
		g, err := a.svc.Auth.AddGrant(ctx, a.actor, pos[0], *principal, verbs, *channel, *extension)
		if err != nil {
			return err
		}
		a.logf("grant add %s %s %s", pos[0], g.Principal(), strings.Join(g.Verbs, ","))
		fmt.Fprintln(a.out, g.ID)
	case sub == "list" && len(pos) == 1:
		gs, err := a.svc.Auth.ListGrants(ctx, a.actor, pos[0])
		if err != nil {
			return err
		}
		a.table("ID\tPRINCIPAL\tVERBS\tCHANNEL\tEXTENSION", func(w io.Writer) {
			for _, g := range gs {
				ch, ext := g.ChannelName, g.Extension
				if ch == "" {
					ch = "*"
				}
				if ext == "" {
					ext = "*"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", g.ID, g.Principal(), strings.Join(g.Verbs, ","), ch, ext)
			}
		})
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
