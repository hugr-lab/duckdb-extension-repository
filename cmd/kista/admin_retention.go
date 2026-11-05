package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0042: version retention in channels: the rules, pins, the line act, the plan and the pass.

// ruleFlags applies -keep and -grace to a rule read: a flag not given keeps its field; at a channel
// "default" leaves a field out (the tenant's) and keep "none" is no retention there; at a tenant
// "none" leaves a field out (no default) and "default" is no word. A keep is a whole number from 1,
// as the API takes it.
func ruleFlags(r store.RetentionRule, keep, grace *string, given map[string]bool, channel bool) (store.RetentionRule, error) {
	if given["keep"] {
		switch k := *keep; {
		case channel && k == "default", !channel && k == "none":
			r.Keep = nil
		case channel && k == "none":
			none := 0
			r.Keep = &none
		default:
			n, err := strconv.Atoi(k)
			if err != nil || n < 1 {
				if channel {
					return r, fmt.Errorf("%w: -keep is a whole number from 1, none (no retention here) or default (the tenant's)", errUsage)
				}
				return r, fmt.Errorf("%w: -keep is a whole number from 1, or none (no default)", errUsage)
			}
			r.Keep = &n
		}
	}
	if given["grace"] {
		switch g := *grace; {
		case channel && g == "default", !channel && g == "none":
			r.Grace = nil
		default:
			d, err := parseHold(g)
			if err != nil || g == "none" || g == "default" {
				if channel {
					return r, fmt.Errorf("%w: -grace is a duration such as 720h or 30d, 0 to purge at the next pass, or default (the tenant's)", errUsage)
				}
				return r, fmt.Errorf("%w: -grace is a duration such as 720h or 30d, 0 to purge at the next pass, or none (no default)", errUsage)
			}
			r.Grace = &d
		}
	}
	return r, nil
}

// ruleText is a stored rule: a channel's field not set is the tenant's ("default"), a tenant's
// "unset".
func ruleText(r store.RetentionRule, channel bool) string {
	f := release.RuleFields(r)
	k, g := "unset", "unset"
	if channel {
		k, g = "default", "default"
	}
	if v, ok := f["keep"]; ok {
		k = fmt.Sprint(v)
	}
	if v, ok := f["grace"]; ok {
		g = fmt.Sprint(v)
	}
	return "keep " + k + ", grace " + g
}

func inForceText(r release.RuleInForce) string {
	if r.Passthrough {
		return "none (a passthrough channel)"
	}
	if !r.Active() {
		if r.None {
			return "none (the channel's)"
		}
		return "none (missing: " + strings.Join(r.Missing(), ", ") + ")"
	}
	capped := ""
	if r.Capped {
		capped = ", capped by limits.ceilings.retention_grace"
	}
	return fmt.Sprintf("keep %d (%s), grace %s (%s%s)", r.Keep, r.KeepFrom, audit.Duration(r.Grace), r.GraceFrom, capped)
}

func (a *adminCmd) tenantRetention(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tenant retention", flag.ContinueOnError)
	keep, grace := fs.String("keep", "", ""), fs.String("grace", "", "")
	pos, err := flags(fs, args)
	if err != nil || len(pos) != 1 {
		return errUsage
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	r, err := a.svc.Releases.TenantRetention(ctx, a.actor, pos[0])
	if err != nil {
		return err
	}
	if len(given) > 0 {
		next, err := ruleFlags(r, keep, grace, given, false)
		if err != nil {
			return err
		}
		if r, err = a.svc.Releases.SetTenantRetention(ctx, a.actor, pos[0], next, release.RetentionTag(r)); err != nil {
			return err
		}
		a.logf("tenant retention %s %s", pos[0], ruleText(r, false))
	}
	fmt.Fprintln(a.out, ruleText(r, false))
	return nil
}

// channelRetention runs the channel's retention commands: retention, retention-plan, pin, unpin and
// retire-line.
func (a *adminCmd) channelRetention(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("channel "+sub, flag.ContinueOnError)
	keep, grace := fs.String("keep", "", ""), fs.String("grace", "", "")
	line, reason, name := fs.String("duckdb-version", "", ""), fs.String("reason", "", ""), fs.String("name", "", "")
	pos, err := flags(fs, args)
	if err != nil || len(pos) == 0 {
		return errUsage
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	t, c, err := splitPath(pos[0])
	if err != nil {
		return err
	}
	switch {
	case sub == "retention" && len(pos) == 1 && !given["duckdb-version"] && !given["reason"] && !given["name"]:
		v, err := a.svc.Releases.ChannelRetention(ctx, a.actor, t, c)
		if err != nil {
			return err
		}
		if given["keep"] || given["grace"] {
			next, err := ruleFlags(v.Rule, keep, grace, given, true)
			if err != nil {
				return err
			}
			if v, err = a.svc.Releases.SetChannelRetention(ctx, a.actor, t, c, next, release.RetentionTag(v.Rule)); err != nil {
				return err
			}
			a.logf("channel retention %s/%s %s", t, c, ruleText(v.Rule, true))
		}
		fmt.Fprintf(a.out, "rule: %s\nin force: %s\n", ruleText(v.Rule, true), inForceText(v.InForce))
		if !v.CheckedAt.IsZero() {
			fmt.Fprintf(a.out, "checked: %s\n", ts(v.CheckedAt))
		}
		pins, err := a.svc.Releases.Pins(ctx, a.actor, t, c, "", "", 0)
		if err != nil {
			return err
		}
		if len(pins) > 0 {
			a.table("PIN\tNAME\tVERSION\tDUCKDB\tREASON\tPINNED", func(w io.Writer) {
				for _, p := range pins {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s by %s\n", p.ID, p.Name, p.PackageVersion, orAll(p.DuckDBVersion), p.Reason,
						ts(p.PinnedAt), p.PinnedBy)
				}
			})
		}
	case sub == "retention-plan" && len(pos) == 1 && len(given) == 0:
		plan, err := a.svc.Releases.RetentionPlan(ctx, a.actor, t, c, "", 0)
		if err != nil {
			return err
		}
		a.printPlan(t+"/"+c, plan)
	case sub == "pin" && len(pos) == 3 && given["reason"]:
		p, existed, err := a.svc.Releases.Pin(ctx, a.actor, t, c, store.RetentionPin{Name: pos[1], PackageVersion: pos[2],
			DuckDBVersion: *line, Reason: *reason})
		if err != nil {
			return err
		}
		if existed {
			fmt.Fprintln(a.errw, "the version is pinned already")
		} else {
			a.logf("channel pin %s/%s %s %s %s", t, c, p.Name, p.PackageVersion, orAll(p.DuckDBVersion))
		}
		fmt.Fprintln(a.out, p.ID)
	case sub == "unpin" && len(pos) == 2 && len(given) == 0:
		if err := a.svc.Releases.Unpin(ctx, a.actor, t, c, pos[1]); err != nil {
			return err
		}
		a.logf("channel unpin %s/%s %s", t, c, pos[1])
	case sub == "retire-line" && len(pos) == 2 && !given["keep"] && !given["grace"] && !given["reason"] && !given["duckdb-version"]:
		res, err := a.svc.Releases.RetireLine(ctx, a.actor, t, c, pos[1], *name)
		if err != nil {
			return err
		}
		a.logf("channel retire-line %s/%s %s: %d retired, %d kept by another line, %d withdrawn skipped, %d pins removed", t, c, pos[1],
			len(res.Retired), len(res.Kept), res.Withdrawn, res.PinsRemoved)
		a.table("RELEASE\tNAME\tVERSION\tPLATFORM\tOUTCOME", func(w io.Writer) {
			for _, r := range res.Retired {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\tretired\n", r.ID, r.Name, r.Version, r.Platform)
			}
			for _, r := range res.Kept {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\tkept by %s\n", r.ID, r.Name, r.Version, r.Platform, strings.Join(r.Lines, ", "))
			}
		})
	default:
		return errUsage
	}
	return nil
}

func orAll(line string) string {
	if line == "" {
		return "every line"
	}
	return line
}

func (a *adminCmd) printPlan(channel string, plan release.RetentionPlan) {
	fmt.Fprintf(a.out, "%s: in force: %s\n", channel, inForceText(plan.InForce))
	if plan.PurgeOff {
		fmt.Fprintln(a.out, "purging is off until gc.interval is set: the pass retires but purges nothing")
	}
	a.table("ACTION\tNAME\tVERSION\tRELEASE\tPLATFORM\tBYTES\tWHEN", func(w io.Writer) {
		for _, x := range plan.Retire {
			for _, r := range x.Releases {
				fmt.Fprintf(w, "retire\t%s\t%s\t%s\t%s\t%d\t%s\n", x.Name, x.Version, r.ID, r.Platform, r.Bytes, strings.Join(x.Lines, ","))
			}
		}
		for _, x := range plan.Purge {
			action := "purge"
			if x.Pinned {
				action = "pinned"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", action, x.Name, x.Version, x.Release.ID, x.Release.Platform, x.Release.Bytes, ts(x.PurgeAt))
		}
		for _, x := range plan.Retired {
			when := "-"
			if !x.PurgeAt.IsZero() {
				when = ts(x.PurgeAt)
			}
			fmt.Fprintf(w, "retired\t%s\t%s\t%s\t%s\t%d\t%s\n", x.Name, x.Version, x.Release.ID, x.Release.Platform, x.Release.Bytes, when)
		}
	})
}

// retentionRun runs the pass now in every channel (of a tenant, or one channel): a dry run printing
// each channel's plan unless -apply.
func (a *adminCmd) retentionRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("retention run", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "")
	pos, err := flags(fs, args)
	if err != nil || len(pos) > 1 {
		return errUsage
	}
	chs, err := a.svc.Store.RetentionChannels(ctx)
	if err != nil {
		return err
	}
	tenant, channel := "", ""
	if len(pos) == 1 {
		tenant, channel, _ = strings.Cut(pos[0], "/")
	}
	host, _ := os.Hostname()
	r := &release.Retainer{Service: a.svc.Releases, Holder: "admin:" + host + "/" + store.NewID(), Log: slog.New(slog.DiscardHandler)}
	for _, c := range chs {
		if tenant != "" && c.Tenant != tenant || channel != "" && c.Channel != channel {
			continue
		}
		if !*apply {
			plan, err := a.svc.Releases.RetentionPlan(ctx, a.actor, c.Tenant, c.Channel, "", 0)
			if err != nil {
				return err
			}
			a.printPlan(c.Tenant+"/"+c.Channel, plan)
			continue
		}
		res, held, err := r.Channel(ctx, c.ChannelID)
		switch {
		case err != nil:
			return fmt.Errorf("%s/%s: %w", c.Tenant, c.Channel, err)
		case !held:
			fmt.Fprintf(a.out, "%s/%s: another replica runs its pass now\n", c.Tenant, c.Channel)
		default:
			fmt.Fprintf(a.out, "%s/%s: %d retired, %d purged, %d failed%s\n", c.Tenant, c.Channel, res.Retired, res.Purged, res.Failed,
				failureText(res.Failure))
		}
	}
	return nil
}

func failureText(f string) string {
	if f == "" {
		return ""
	}
	return " (" + f + ")"
}
