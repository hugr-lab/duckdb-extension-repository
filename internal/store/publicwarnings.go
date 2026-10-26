package store

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
)

// Spec 0035 phase 2: an advisory warning when a catalogue entry that holds a licence is served
// publicly, that is, has an active or deprecated public release in a signed channel whose rule
// selects it, its build not withdrawn and its body not blocked. The release writes (InsertRelease,
// SetReleaseVisibility, TakeReleases), a rule's switch (SetChannelRule) and the licence writes
// (PutLicense, SetLicenseVisibility to public) note what they touched. Right after the transaction
// commits, a short transaction of its own reads which of those are such entries and writes one
// license.public_warning event per (act, source, name, channel): best effort, a failure is logged
// and dropped, never refusing or undoing the act. The warning is not complete: a change that
// reaches no hook (a grant, a block lifted) warns nothing.

// LicenseWarningKind is the warning's event kind.
const LicenseWarningKind audit.Kind = "license.public_warning"

// LicenseWarning is one warning: an entry (Source, as answers show it) of a tenant (TenantName)
// that holds a licence for a package (PackageType, Name) and has a public release in Channel (its
// name).
type LicenseWarning struct {
	TenantID, TenantName string
	PackageType, Name    string
	Source, Channel      string
	Message              string
}

type warningKey struct{ tenantID, source, name, channel string }

// licenseWarnings collects an act's warnings across its transactions (a request, a command, an
// upstream's run): a (source, name, channel) whose event was written is not written again, and the
// written warnings are kept for the act's answer. Two transactions of one act checking at the same
// moment may each write one: a rare duplicate event, accepted for an advisory warning.
type licenseWarnings struct {
	mu     sync.Mutex
	warned map[warningKey]bool
	list   []LicenseWarning
}

func (c *licenseWarnings) seen(k warningKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.warned[k]
}

// add records warnings written, each with its key (its source as stored).
func (c *licenseWarnings) add(keys []warningKey, ws []LicenseWarning) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, w := range ws {
		if k := keys[i]; !c.warned[k] {
			c.warned[k] = true
			c.list = append(c.list, w)
		}
	}
}

type licenseWarningsKey struct{}

// WithLicenseWarnings returns a context whose transactions collect their licence warnings into
// one act (LicenseWarnings reads them); a context that collects already is returned as it is.
func WithLicenseWarnings(ctx context.Context) context.Context {
	if _, ok := ctx.Value(licenseWarningsKey{}).(*licenseWarnings); ok {
		return ctx
	}
	return context.WithValue(ctx, licenseWarningsKey{}, &licenseWarnings{warned: map[warningKey]bool{}})
}

// LicenseWarnings are the warnings written for the context's act, in the order they were written
// (none without WithLicenseWarnings).
func LicenseWarnings(ctx context.Context) []LicenseWarning {
	c, ok := ctx.Value(licenseWarningsKey{}).(*licenseWarnings)
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.list)
}

// AddLicenseWarnings records warnings as written in the context's act; for tests only.
func AddLicenseWarnings(ctx context.Context, ws []LicenseWarning) {
	if c, ok := ctx.Value(licenseWarningsKey{}).(*licenseWarnings); ok {
		keys := make([]warningKey, len(ws))
		for i, w := range ws {
			keys[i] = warningKey{w.TenantID, w.Source, w.Name, w.Channel}
		}
		c.add(keys, ws)
	}
}

// txWarnings is what a transaction's writes touched, checked once it commits.
type txWarnings struct {
	releases []string    // release ids written public or made public
	rules    [][3]string // (channel id, name, source): a rule that selects a source now, or releases a successor took
	entries  [][4]string // (tenant id, type, name, source): a licence stored or made public
	// the act: the one the service names (SetWarningAct), else the first hook's; the actor is the
	// first hook's
	act, hookAct, hookActor string
}

func (w *txWarnings) noted() bool { return len(w.releases)+len(w.rules)+len(w.entries) > 0 }

func (w *txWarnings) hook(act, actor string) {
	if w.hookAct == "" {
		w.hookAct = act
	}
	if w.hookActor == "" {
		w.hookActor = actor
	}
}

// SetWarningAct names the act of this transaction's licence warnings (the kind of its event:
// release.place, license.import, …); without it, the first hook's default names it.
func (t *Tx) SetWarningAct(act audit.Kind) { t.w.act = string(act) }

func (t *Tx) warnRelease(id, act, actor string) {
	t.w.releases = append(t.w.releases, id)
	t.w.hook(act, actor)
}

func (t *Tx) warnRule(channelID, name, source, act, actor string) {
	if r := [3]string{channelID, name, source}; !slices.Contains(t.w.rules, r) {
		t.w.rules = append(t.w.rules, r)
	}
	t.w.hook(act, actor)
}

func (t *Tx) warnEntry(l *License, act string) {
	if e := [4]string{l.TenantID, l.PackageType, l.Name, l.Entry}; !slices.Contains(t.w.entries, e) {
		t.w.entries = append(t.w.entries, e)
	}
	t.w.hook(act, l.Actor)
}

// publicLicensed selects the (tenant, its name, type, name, source, channel) of active or
// deprecated public releases in signed channels whose rule selects the release's source, their
// build not withdrawn and their body not blocked, of entries holding a licence.
const publicLicensed = `SELECT DISTINCT r.tenant_id, tn.name, c.package_type, r.name, r.source, c.name FROM releases r
JOIN channels c ON c.id = r.channel_id
JOIN tenants tn ON tn.id = r.tenant_id
JOIN channel_sources cs ON cs.channel_id = r.channel_id AND cs.name = r.name AND cs.source = r.source
JOIN builds b ON b.id = r.build_id
JOIN licenses l ON l.tenant_id = r.tenant_id AND l.package_type = c.package_type AND l.name = r.name AND l.entry_source = r.source
WHERE r.state IN ('active', 'deprecated') AND r.visibility = 'public' AND c.kind = 'signed' AND b.withdrawn_at IS NULL
AND NOT EXISTS (SELECT 1 FROM blocks x WHERE x.tenant_id = r.tenant_id AND x.body_hash = b.body_hash) AND `

// warningFault fails the warnings' check; for tests only.
var warningFault atomic.Pointer[func() error]

// SetLicenseWarningFault makes the licence warnings' check fail with f's error (nil: clears it);
// for tests only.
func SetLicenseWarningFault(f func() error) {
	if f == nil {
		warningFault.Store(nil)
		return
	}
	warningFault.Store(&f)
}

// warningTimeout bounds the warnings' check after an act's commit.
const warningTimeout = 2 * time.Second

// writeLicenseWarnings checks what a committed transaction noted and writes its warnings' events
// (spec 0035 phase 2: best effort). The check is a read outside any transaction; only when it finds
// a warning the act has not written yet does one attempt of a short transaction write the events,
// so a transaction noting nothing to warn about takes no second write lock (SQLite). A key written
// by another transaction of the act between the read and the write may rarely be written twice. A
// failure, a timeout (warningTimeout) or a panic is logged and dropped: the act stands. The act's
// collector learns the warnings once they are written.
func (s *Store) writeLicenseWarnings(ctx context.Context, w *txWarnings) {
	log := cmp.Or(s.Log, slog.Default())
	defer func() {
		if p := recover(); p != nil {
			log.Error("store: a licence warning's check failed (advisory: the change stands)", "kind", string(LicenseWarningKind), "panic", p)
		}
	}()
	// the act committed: its warning is written even if its caller left, within a bound
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), warningTimeout)
	defer cancel()
	c, _ := ctx.Value(licenseWarningsKey{}).(*licenseWarnings)
	keys, ws, err := s.licenseWarnings(ctx, w, c)
	if err != nil {
		log.Warn("store: a licence warning was not written (advisory: the change stands)", "kind", string(LicenseWarningKind), "error", err)
		return
	}
	if c != nil {
		c.add(keys, ws)
	}
}

func (s *Store) licenseWarnings(ctx context.Context, w *txWarnings, c *licenseWarnings) ([]warningKey, []LicenseWarning, error) {
	if f := warningFault.Load(); f != nil {
		if err := (*f)(); err != nil {
			return nil, nil, err
		}
	}
	found, err := publicLicensedOf(ctx, s.query, w)
	if err != nil {
		return nil, nil, err
	}
	found = slices.DeleteFunc(found, func(x LicenseWarning) bool {
		return c != nil && c.seen(warningKey{x.TenantID, x.Source, x.Name, x.Channel})
	})
	if len(found) == 0 {
		return nil, nil, nil
	}
	act := cmp.Or(w.act, w.hookAct)
	actor := cmp.Or(w.hookActor, "system:kista")
	var ws []LicenseWarning
	var keys []warningKey
	err = s.once(ctx, nil, func(tx *Tx) error {
		ws, keys = nil, nil
		for _, x := range found {
			k := warningKey{x.TenantID, x.Source, x.Name, x.Channel}
			label, err := tx.SourceLabel(ctx, x.Source)
			if err != nil {
				return err
			}
			fields := map[string]any{"name": x.Name, "source": label, "channel": x.Channel, "act": act}
			if x.PackageType != pkgtype.Default {
				fields["package_type"] = x.PackageType
			}
			e, err := s.newEvent(ctx, x.TenantID, actor, LicenseWarningKind, audit.OK, "channel:"+x.Channel+"/ext:"+x.Name, fields)
			if err == nil {
				err = tx.insertEvent(ctx, e)
			}
			if err != nil {
				return err
			}
			x.Message = fmt.Sprintf("%s holds a licence for %s and has a public release in channel %s: serve a licensed extension "+
				"publicly only where its distributor allows it (advisory: nothing was refused)", label, x.Name, x.Channel)
			x.Source = label
			ws, keys = append(ws, x), append(keys, k)
		}
		return nil
	})
	return keys, ws, err
}

// publicLicensed reads which of the noted releases, rules and entries are entries holding a
// licence served publicly, each (tenant, source as stored, name, channel) once, in the order found.
func publicLicensedOf(ctx context.Context, query queryFn, w *txWarnings) ([]LicenseWarning, error) {
	var out []LicenseWarning
	seen := map[warningKey]bool{}
	read := func(cond string, args ...any) error {
		rows, err := query(ctx, publicLicensed+"("+cond+")", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x LicenseWarning
			if err := rows.Scan(&x.TenantID, &x.TenantName, &x.PackageType, &x.Name, &x.Source, &x.Channel); err != nil {
				return err
			}
			if k := (warningKey{x.TenantID, x.Source, x.Name, x.Channel}); !seen[k] {
				seen[k] = true
				out = append(out, x)
			}
		}
		return rows.Err()
	}
	// SQL Server takes at most 2100 parameters: a few hundred per query
	ids := slices.Compact(slices.Sorted(slices.Values(w.releases)))
	for len(ids) > 0 {
		n := min(len(ids), 500)
		args := make([]any, n)
		for i, id := range ids[:n] {
			args[i] = id
		}
		if err := read("r.id IN ("+strings.TrimSuffix(strings.Repeat("?, ", n), ", ")+")", args...); err != nil {
			return nil, err
		}
		ids = ids[n:]
	}
	for rules := w.rules; len(rules) > 0; {
		n := min(len(rules), 200)
		var conds []string
		var args []any
		for _, r := range rules[:n] {
			conds, args = append(conds, "r.channel_id = ? AND r.name = ? AND r.source = ?"), append(args, r[0], r[1], r[2])
		}
		if err := read("("+strings.Join(conds, ") OR (")+")", args...); err != nil {
			return nil, err
		}
		rules = rules[n:]
	}
	// the entries by tenant and type: through the tenant's channels, their rules and releases
	byPackage := map[[2]string][][2]string{}
	var keys [][2]string
	for _, e := range w.entries {
		k := [2]string{e[0], e[1]}
		if _, ok := byPackage[k]; !ok {
			keys = append(keys, k)
		}
		byPackage[k] = append(byPackage[k], [2]string{e[2], e[3]})
	}
	for _, k := range keys {
		for ns := byPackage[k]; len(ns) > 0; {
			n := min(len(ns), 300)
			conds, args := []string{}, []any{k[0], k[1]}
			for _, x := range ns[:n] {
				conds, args = append(conds, "cs.name = ? AND cs.source = ?"), append(args, x[0], x[1])
			}
			if err := read("c.tenant_id = ? AND c.package_type = ? AND (("+strings.Join(conds, ") OR (")+"))", args...); err != nil {
				return nil, err
			}
			ns = ns[n:]
		}
	}
	return out, nil
}
