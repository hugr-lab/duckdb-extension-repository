package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
)

// Spec 0035 phase 2: an advisory warning when a catalogue entry that holds a licence is served
// publicly, that is, has an active or deprecated public release in a signed channel whose rule
// selects it. The release writes (InsertRelease, SetReleaseVisibility, TakeReleases), a rule's
// switch (SetChannelRule) and the licence writes (PutLicense, SetLicenseVisibility to public) note
// what they touched; before the transaction commits, the store reads which of those are such
// entries and writes one license.public_warning event per (act, source, name, channel). Nothing is
// refused. The warning is not complete: a change that reaches no hook (a grant, a block lifted, a
// release activated again) warns nothing.

// LicenseWarningKind is the warning's event kind.
const LicenseWarningKind audit.Kind = "license.public_warning"

// LicenseWarning is one warning: an entry (Source, as answers show it) that holds a licence for a
// package (PackageType, Name) and has a public release in Channel (its name).
type LicenseWarning struct {
	TenantID, PackageType, Name string
	Source, Channel             string
	Message                     string
}

type warningKey struct{ tenantID, source, name, channel string }

// licenseWarnings collects an act's warnings across its transactions (a request, a command, an
// upstream's run): each (source, name, channel) is written once, and the warnings of the
// transactions that committed are kept for the act's answer.
type licenseWarnings struct {
	mu      sync.Mutex
	claimed map[warningKey]bool
	list    []LicenseWarning
}

type licenseWarningsKey struct{}

// WithLicenseWarnings returns a context whose transactions collect their licence warnings into
// one act (LicenseWarnings reads them); a context that collects already is returned as it is.
func WithLicenseWarnings(ctx context.Context) context.Context {
	if _, ok := ctx.Value(licenseWarningsKey{}).(*licenseWarnings); ok {
		return ctx
	}
	return context.WithValue(ctx, licenseWarningsKey{}, &licenseWarnings{claimed: map[warningKey]bool{}})
}

// LicenseWarnings are the warnings of the committed transactions of the context's act, in the
// order they were written (none without WithLicenseWarnings).
func LicenseWarnings(ctx context.Context) []LicenseWarning {
	c, ok := ctx.Value(licenseWarningsKey{}).(*licenseWarnings)
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.list)
}

// txWarnings is what a transaction's writes touched, read before it commits.
type txWarnings struct {
	releases []string    // release ids written public, made public or moved to a successor
	rules    [][3]string // (channel id, name, source): a rule that selects a source now
	entries  [][4]string // (tenant id, type, name, source): a licence stored or made public
	// the act: the one a service names (SetWarningAct), else the last event the transaction wrote,
	// else the first hook's; the actor likewise (the event's, else the hook's)
	act, lastKind, lastActor, hookAct, hookActor string
}

func (w *txWarnings) hook(act, actor string) {
	if w.hookAct == "" {
		w.hookAct = act
	}
	if w.hookActor == "" {
		w.hookActor = actor
	}
}

// SetWarningAct names the act of this transaction's licence warnings, for a transaction whose own
// events do not name it (a relay's or a bundle's licences).
func (t *Tx) SetWarningAct(act audit.Kind) { t.w.act = string(act) }

func (t *Tx) warnRelease(id, act, actor string) {
	t.w.releases = append(t.w.releases, id)
	t.w.hook(act, actor)
}

func (t *Tx) warnRule(channelID, name, source, actor string) {
	if r := [3]string{channelID, name, source}; !slices.Contains(t.w.rules, r) {
		t.w.rules = append(t.w.rules, r)
	}
	t.w.hook("source.set", actor)
}

func (t *Tx) warnEntry(l *License, act string) {
	if e := [4]string{l.TenantID, l.PackageType, l.Name, l.Entry}; !slices.Contains(t.w.entries, e) {
		t.w.entries = append(t.w.entries, e)
	}
	t.w.hook(act, l.Actor)
}

// publicLicensed selects the (tenant, type, name, source, channel) of active or deprecated public
// releases in signed channels whose rule selects the release's source, of entries holding a licence.
const publicLicensed = `SELECT DISTINCT r.tenant_id, c.package_type, r.name, r.source, c.name FROM releases r
JOIN channels c ON c.id = r.channel_id
JOIN channel_sources cs ON cs.channel_id = r.channel_id AND cs.name = r.name AND cs.source = r.source
JOIN licenses l ON l.tenant_id = r.tenant_id AND l.package_type = c.package_type AND l.name = r.name AND l.entry_source = r.source
WHERE r.state IN ('active', 'deprecated') AND r.visibility = 'public' AND c.kind = 'signed' AND `

// pendingWarnings are a transaction's warnings, written before its commit and claimed in its act's
// collector: kept once it commits, given back if it does not.
type pendingWarnings struct {
	c    *licenseWarnings
	keys []warningKey
	list []LicenseWarning
}

func (p *pendingWarnings) release() {
	if p == nil || p.c == nil {
		return
	}
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	for _, k := range p.keys {
		delete(p.c.claimed, k)
	}
}

func (p *pendingWarnings) keep() {
	if p == nil || p.c == nil || len(p.list) == 0 {
		return
	}
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	p.c.list = append(p.c.list, p.list...)
}

// licenseWarnings writes the transaction's warnings (one event per source, name and channel not
// written yet in its act) and returns them pending its commit.
func (t *Tx) licenseWarnings(ctx context.Context) (*pendingWarnings, error) {
	w := t.w
	if len(w.releases) == 0 && len(w.rules) == 0 && len(w.entries) == 0 {
		return nil, nil
	}
	found := map[warningKey]string{} // the key's package type
	var order []warningKey
	read := func(cond string, args ...any) error {
		rows, err := t.query(ctx, publicLicensed+cond, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k warningKey
			var typ string
			if err := rows.Scan(&k.tenantID, &typ, &k.name, &k.source, &k.channel); err != nil {
				return err
			}
			if _, ok := found[k]; !ok {
				found[k] = typ
				order = append(order, k)
			}
		}
		return rows.Err()
	}
	ids := slices.Compact(slices.Sorted(slices.Values(w.releases)))
	for len(ids) > 0 {
		n := min(len(ids), 500) // SQL Server takes at most 2100 parameters
		args := make([]any, n)
		for i, id := range ids[:n] {
			args[i] = id
		}
		if err := read("r.id IN ("+strings.TrimSuffix(strings.Repeat("?, ", n), ", ")+")", args...); err != nil {
			return nil, err
		}
		ids = ids[n:]
	}
	for _, r := range w.rules {
		if err := read("r.channel_id = ? AND r.name = ? AND r.source = ?", r[0], r[1], r[2]); err != nil {
			return nil, err
		}
	}
	for _, e := range w.entries {
		if err := read("r.tenant_id = ? AND c.package_type = ? AND r.name = ? AND r.source = ?", e[0], e[1], e[2], e[3]); err != nil {
			return nil, err
		}
	}
	if len(order) == 0 {
		return nil, nil
	}
	p := &pendingWarnings{}
	p.c, _ = ctx.Value(licenseWarningsKey{}).(*licenseWarnings)
	if p.c != nil {
		p.c.mu.Lock()
		for _, k := range order {
			if !p.c.claimed[k] {
				p.c.claimed[k] = true
				p.keys = append(p.keys, k)
			}
		}
		p.c.mu.Unlock()
	} else {
		p.keys = order
	}
	act := cmp.Or(w.act, w.lastKind, w.hookAct)
	actor := cmp.Or(w.lastActor, w.hookActor, "system:kista")
	for _, k := range p.keys {
		label, err := t.SourceLabel(ctx, k.source)
		if err != nil {
			p.release()
			return nil, err
		}
		typ := found[k]
		fields := map[string]any{"name": k.name, "source": label, "channel": k.channel, "act": act}
		if typ != pkgtype.Default {
			fields["package_type"] = typ
		}
		e, err := t.s.newEvent(ctx, k.tenantID, actor, LicenseWarningKind, audit.OK, "channel:"+k.channel+"/ext:"+k.name, fields)
		if err == nil {
			err = t.insertEvent(ctx, e)
		}
		if err != nil {
			p.release()
			return nil, err
		}
		p.list = append(p.list, LicenseWarning{TenantID: k.tenantID, PackageType: typ, Name: k.name, Source: label, Channel: k.channel,
			Message: fmt.Sprintf("%s holds a licence for %s and has a public release in channel %s: serve a licensed extension "+
				"publicly only where its distributor allows it (advisory: nothing was refused)", label, k.name, k.channel)})
	}
	return p, nil
}
