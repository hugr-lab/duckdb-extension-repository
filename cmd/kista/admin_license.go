package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/license"
	"github.com/hugr-lab/duckdb-extension-repository/internal/licenses"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// license manages a tenant's licences (spec 0012): kista stores what verifies against a key its
// extension's live builds declare (spec 0021), and issues nothing.
func (a *adminCmd) license(ctx context.Context, sub string, args []string) error {
	fs := flag.NewFlagSet("license", flag.ContinueOnError)
	replace := fs.Bool("replace", false, "")
	keyFile := fs.String("key", "", "")
	ptype := fs.String("type", "", "") // spec 0034: the licence's package type ("": the default)
	pos, err := flags(fs, args)
	if err != nil {
		return err
	}
	ls := a.svc.Licenses
	show := func(v licenses.View) {
		fmt.Fprintf(a.out, "name: %s\nid: %s\nissuer: %s\nholder: %s %s\nplan: %s\nstatus: %s\n", v.Name, v.ID, v.Issuer, v.HolderID,
			v.Holder, v.Plan, v.Status)
		fmt.Fprintf(a.out, "issued: %s\nexpires: %s\ngrace until: %s\nkey: %s\nsource: %s\n", v.IssuedAt, v.ExpiresAt, v.GraceUntil, v.Key,
			v.Source)
		if v.Upstream != "" {
			fmt.Fprintf(a.out, "upstream: %s\n", v.Upstream)
		}
		fmt.Fprintf(a.out, "visibility: %s\n", v.Visibility)
		if v.Versions != nil {
			fmt.Fprintf(a.out, "versions: min %q below %q\n", v.Versions.Min, v.Versions.Below)
		}
		for _, l := range v.Limits {
			b, _ := json.Marshal(l.Value)
			fmt.Fprintf(a.out, "limit: %s = %s\n", l.Name, b)
		}
	}
	read := func(path string) (string, error) {
		var b []byte
		var err error
		if path == "-" {
			b, err = io.ReadAll(io.LimitReader(a.in, license.MaxSize+1))
		} else {
			b, err = os.ReadFile(path)
		}
		return strings.TrimSpace(string(b)), err
	}
	switch {
	case sub == "keys" && (len(pos) == 1 || len(pos) == 2):
		name := ""
		if len(pos) == 2 {
			name = pos[1]
		}
		nks, err := ls.LicenseKeys(ctx, a.actor, pos[0], *ptype, name)
		if err != nil {
			return err
		}
		a.table("NAME\tKEY\tBUILDS\tSOURCES", func(w io.Writer) {
			for _, nk := range nks {
				for _, k := range nk.Keys {
					srcs := make([]string, 0, len(k.Sources))
					for _, s := range k.Sources {
						srcs = append(srcs, s.Kind+":"+s.Name)
					}
					fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", nk.Name, k.Kid, k.Builds, strings.Join(srcs, ","))
				}
			}
		})
	case sub == "list" && len(pos) == 1:
		vs, err := ls.List(ctx, a.actor, pos[0], *ptype)
		if err != nil {
			return err
		}
		a.table("NAME\tPLAN\tHOLDER\tSTATUS\tEXPIRES\tSOURCE\tVISIBILITY", func(w io.Writer) {
			for _, v := range vs {
				src := v.Source
				if v.Upstream != "" {
					src += ":" + v.Upstream
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.Name, v.Plan, v.HolderID, v.Status, v.ExpiresAt, src, v.Visibility)
			}
		})
	case sub == "show" && len(pos) == 2:
		v, err := ls.Get(ctx, a.actor, pos[0], *ptype, pos[1])
		if err != nil {
			return err
		}
		show(v)
	case sub == "token" && len(pos) == 2:
		tok, err := ls.Token(ctx, a.actor, pos[0], *ptype, pos[1])
		if err != nil {
			return err
		}
		fmt.Fprintln(a.out, tok)
	case sub == "import" && len(pos) == 2:
		tok, err := read(pos[1])
		if err != nil {
			return err
		}
		expected := int64(licenses.Create)
		if *replace {
			expected = 0
		}
		v, err := ls.Import(ctx, a.actor, pos[0], *ptype, tok, expected)
		if err != nil {
			// the rule, as the API names it (spec 0028: holder_mismatch)
			var le *license.Error
			if errors.Is(err, licenses.ErrRefused) && errors.As(err, &le) {
				return fmt.Errorf("the licence is refused (%s): %s", le.Rule, strings.TrimPrefix(le.Error(), "license: "))
			}
			return err
		}
		a.logf("license import %s %s (%s, plan %s, until %s)", pos[0], v.Name, v.ID, v.Plan, v.ExpiresAt)
	case sub == "remove" && len(pos) == 2:
		if err := ls.Remove(ctx, a.actor, pos[0], *ptype, pos[1], 0); err != nil {
			return err
		}
		a.logf("license remove %s %s", pos[0], pos[1])
	case (sub == "public" || sub == "private") && len(pos) == 2:
		if _, err := ls.SetVisibility(ctx, a.actor, pos[0], *ptype, pos[1], map[string]string{"public": store.Public, "private": store.Private}[sub], 0); err != nil {
			return err
		}
		a.logf("license %s %s %s", sub, pos[0], pos[1])
	case sub == "verify" && len(pos) == 1:
		tok, err := read(pos[0])
		if err != nil {
			return err
		}
		if *keyFile == "" {
			return fmt.Errorf("license verify: -key <pem> names the public key to verify with")
		}
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		keys, err := license.ParseKeys([]string{string(b)})
		if err != nil {
			return err
		}
		t, err := license.Parse(tok)
		if err != nil {
			return err
		}
		if err := t.Verify(keys); err != nil {
			return err
		}
		claims, _ := json.MarshalIndent(t.Claims, "", "  ")
		fmt.Fprintf(a.out, "%s\nstatus: %s\n", claims, t.StatusAt(time.Now()))
	default:
		return errUsage
	}
	return nil
}
