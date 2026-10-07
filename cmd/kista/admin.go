package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/app"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

const adminUsage = `usage: kista admin -config <file> <command> ...

  migrate                                         apply pending migrations
  check                                           verify the schema without changing it
  backup <file>                                   consistent copy of a SQLite store
  tenant create <name> [-display-name …]
  tenant list
  tenant suspend|resume <name>
  version add <name> -kind release|dev [-c-api v1.2.0]
  version list
  channel create <tenant>/<channel> -kind signed|passthrough
  channel versions <tenant>/<channel> [-add v]... [-remove v]...
  channel list <tenant>
  key add <tenant>/<channel> -signer <ref> [-active]
  key list <tenant>/<channel>
  key activate <tenant>/<channel> <key-id|fingerprint> [-force]
  key retire <tenant>/<channel> <key-id|fingerprint> [-force]
  key events <tenant>/<channel>
  wellknown <tenant>/<channel>
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
	a := &adminCmd{svc: app.NewServices(cfg, s, authz.ServerAdmin{}), actor: authz.OSActor(), out: stdout, errw: stderr}
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
	svc   *app.Services
	actor authz.Actor
	out   io.Writer
	errw  io.Writer
}

// logf prints the one-line record of an admin action (spec 0009 moves these to the audit log).
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
	}
	return errUsage
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
	pos, err := flags(fs, args)
	if err != nil {
		return err
	}
	switch {
	case sub == "create" && len(pos) == 1:
		t, err := a.svc.Tenants.CreateTenant(ctx, a.actor, pos[0], *display)
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
		a.table("NAME\tSTATE\tDISPLAY NAME\tCREATED", func(w io.Writer) {
			for _, t := range ts_ {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.Name, t.State, t.DisplayName, ts(t.CreatedAt))
			}
		})
	case (sub == "suspend" || sub == "resume") && len(pos) == 1:
		state := map[string]string{"suspend": store.TenantSuspended, "resume": store.TenantActive}[sub]
		if _, err := a.svc.Tenants.SetTenantState(ctx, a.actor, pos[0], state); err != nil {
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
	capi := fs.String("c-api", "", "")
	pos, err := flags(fs, args)
	if err != nil {
		return err
	}
	switch {
	case sub == "add" && len(pos) == 1:
		v, err := a.svc.Tenants.AddVersion(ctx, a.actor, pos[0], *kind, *capi)
		if err != nil {
			return err
		}
		a.logf("version add %s", v.Name)
	case sub == "list" && len(pos) == 0:
		vs, err := a.svc.Tenants.ListVersions(ctx, a.actor)
		if err != nil {
			return err
		}
		a.table("NAME\tKIND\tC API", func(w io.Writer) {
			for _, v := range vs {
				fmt.Fprintf(w, "%s\t%s\t%s\n", v.Name, v.Kind, v.CAPIVersion)
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
	if err != nil || len(pos) < 1 {
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
