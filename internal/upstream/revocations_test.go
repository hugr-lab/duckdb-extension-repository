package upstream_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// fakeKista answers <prefix>/-/revocations as a kista does (spec 0039): its rows, page rows a
// page (cursor "p<n>"), an ETag over every row, 304 to it; busy answers 429 that many times first.
type fakeKista struct {
	mu    sync.Mutex
	rows  []release.RevokedRow
	page  int
	busy  int
	reads int // 200 answers
	conds int // 304 answers
}

func (k *fakeKista) set(rows ...release.RevokedRow) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rows = rows
}

func (k *fakeKista) serve(w http.ResponseWriter, q *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.busy > 0 {
		k.busy--
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	b, _ := json.Marshal(k.rows)
	sum := sha256.Sum256(b)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	start := 0
	if c, ok := strings.CutPrefix(q.URL.Path, "/-/revocations/p"); ok {
		start, _ = strconv.Atoi(c)
		etag = ""
	}
	if etag != "" && q.Header.Get("If-None-Match") == etag {
		k.conds++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	k.reads++
	size := k.page
	if size <= 0 {
		size = 1000
	}
	end := min(start+size, len(k.rows))
	ans := map[string]any{"api": 1, "rows": append([]release.RevokedRow{}, k.rows[start:end]...)}
	if end < len(k.rows) {
		ans["next"] = "p" + strconv.Itoa(end)
	}
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	_ = json.NewEncoder(w).Encode(ans)
}

func (en *env) kista() *fakeKista {
	k := &fakeKista{}
	en.repo.mu.Lock()
	en.repo.kista = k.serve
	en.repo.mu.Unlock()
	return k
}

// row is a revoked row of a release the downstream holds.
func row(c store.Candidate, state, reason string) release.RevokedRow {
	return release.RevokedRow{Name: c.Name, Version: c.PackageVersion, Platform: c.Platform, BodyHash: c.BodyHash, State: state, Reason: reason}
}

func (en *env) stored(t *testing.T) map[string]store.UpstreamRevocation {
	t.Helper()
	rs, err := en.up.RevocationsOf(ctx, admin, "acme", "acme-repo")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.UpstreamRevocation{}
	for _, r := range rs {
		out[r.PackageVersion] = r
	}
	return out
}

func states(t *testing.T, en *env, channel, name string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, r := range en.releasesIn(t, channel, name) {
		out[r.PackageVersion] = r.State
	}
	return out
}

// Spec 0039: a mirror of a kista reads its revocations first in each run, matches them by build and
// stores them; switched off it shows them and applies nothing; switched on it applies them: a
// withdrawal as the upstream's block (its builds' releases yanked in the home channel and in the
// channels they were promoted to, the body refused from it), a yank as a yank in the home channel
// only. A stronger state is applied again; a lift restores what it yanked and is not applied again
// unless stronger; an administrator's block takes over what an upstream yanked.
func TestRevocationPass(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		k := en.kista()
		for i, v := range []string{"1.0", "1.1"} {
			en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp(v, "v2.0.0"), i+1, "tresor_duckdb_cpp_init"))
			if i == 0 {
				u, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"}))
				if err != nil || !u.RemoteKista || u.Revocations {
					t.Fatalf("add: %+v %v", u, err)
				}
			}
			if r := en.sync(t, false); r.Counts[store.CellReleased] != 1 {
				t.Fatalf("run %d: %+v", i, r)
			}
		}
		rels := map[string]store.Candidate{}
		for _, r := range en.releases(t, "tresor") {
			rels[r.PackageVersion] = r
			if _, _, err := en.rel.Promote(ctx, admin, "acme", "other", "tresor", release.PromoteOptions{From: "prod", Release: r.ID}); err != nil {
				t.Fatal(err)
			}
		}
		v10, v11 := rels["1.0"], rels["1.1"]

		// switched off: stored, shown, nothing applied; a row of a version not held revokes nothing ahead
		ahead := row(v10, store.RevokeWithdrawn, "x")
		ahead.Version = "9.9"
		k.set(row(v10, store.RevokeWithdrawn, "CVE\x01-2026"), ahead)
		r := en.sync(t, false)
		if r.Counts["revoked_on_source"] != 1 || r.Counts["revoked"] != 0 || !strings.Contains(fmt.Sprint(r.Problems), "withdrawn on the source: CVE -2026") {
			t.Fatalf("switched off: %+v", r)
		}
		if s := en.stored(t); len(s) != 1 || s["1.0"].State != store.RevokeWithdrawn || s["1.0"].Reason != "CVE -2026" || s["1.0"].AppliedState != "" {
			t.Fatalf("stored: %+v", s)
		}
		if st := states(t, en, "prod", "tresor"); st["1.0"] != store.ReleaseActive {
			t.Fatalf("switched off, applied: %+v", st)
		}

		// switched on: the stored rows are applied at the next run, though the remote answers 304
		if _, err := en.up.SetRevocations(ctx, admin, "acme", "acme-repo", true, 0); err != nil {
			t.Fatal(err)
		}
		if evs := en.events(t, "upstream.revocations"); len(evs) != 1 || !strings.Contains(evs[0].Data, `"pending":1`) {
			t.Fatalf("the switch's event: %+v", evs)
		}
		conds := k.conds
		if r := en.sync(t, false); r.Counts["revoked"] != 1 || k.conds != conds+1 {
			t.Fatalf("switched on: %+v (304s %d)", r, k.conds-conds)
		}
		for _, ch := range []string{"prod", "other"} {
			if st := states(t, en, ch, "tresor"); st["1.0"] != store.ReleaseYanked || st["1.1"] != store.ReleaseActive {
				t.Fatalf("%s after the block: %+v", ch, st)
			}
		}
		if evs := en.events(t, "upstream.revocation"); len(evs) != 1 || !strings.Contains(evs[0].Data, `"action":"blocked"`) {
			t.Fatalf("the revocation's event: %+v", evs)
		}
		// applied once: the next run applies nothing again
		if r := en.sync(t, false); r.Counts["revoked"] != 0 {
			t.Fatalf("applied again: %+v", r)
		}
		// the body is refused from this upstream: its file fetched again is blocked
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		if r := en.sync(t, false); r.Counts[store.CellBlocked] != 1 {
			t.Fatalf("the blocked body fetched again: %+v", r)
		}

		// a yank: the home channel only; then a withdrawal of the same build, applied again as a block
		k.set(row(v10, store.RevokeWithdrawn, "CVE-2026"), row(v11, store.RevokeYanked, ""))
		if r := en.sync(t, false); r.Counts["revoked"] != 1 {
			t.Fatalf("a yank: %+v", r)
		}
		if p, o := states(t, en, "prod", "tresor"), states(t, en, "other", "tresor"); p["1.1"] != store.ReleaseYanked || o["1.1"] != store.ReleaseActive {
			t.Fatalf("a yank in the home channel only: %+v %+v", p, o)
		}
		k.set(row(v10, store.RevokeWithdrawn, "CVE-2026"), row(v11, store.RevokeBlocked, "malware"))
		if r := en.sync(t, false); r.Counts["revoked"] != 1 {
			t.Fatalf("a stronger state: %+v", r)
		}
		if o := states(t, en, "other", "tresor"); o["1.1"] != store.ReleaseYanked {
			t.Fatalf("the block swept the copy: %+v", o)
		}
		// a weaker state changes nothing applied
		k.set(row(v10, store.RevokeYanked, ""), row(v11, store.RevokeYanked, ""))
		if r := en.sync(t, false); r.Counts["revoked"] != 0 || states(t, en, "other", "tresor")["1.1"] != store.ReleaseYanked {
			t.Fatalf("a weaker state: %+v", r)
		}

		// a lift restores what the upstream yanked, in both channels, and is not applied again
		n, err := en.up.LiftRevocation(ctx, admin, "acme", "acme-repo", v11.BuildID, 0)
		if err != nil || n != 2 {
			t.Fatalf("lift: %d %v", n, err)
		}
		for _, ch := range []string{"prod", "other"} {
			if st := states(t, en, ch, "tresor"); st["1.1"] != store.ReleaseActive {
				t.Fatalf("%s after the lift: %+v", ch, st)
			}
		}
		if s := en.stored(t)["1.1"]; s.LiftedState != store.RevokeBlocked {
			t.Fatalf("lifted: %+v", s)
		}
		if r := en.sync(t, false); r.Counts["revoked"] != 0 || states(t, en, "prod", "tresor")["1.1"] != store.ReleaseActive {
			t.Fatalf("a lifted revocation applied again: %+v", r)
		}
		if evs := en.events(t, "upstream.revocation.lift"); len(evs) != 1 || !strings.Contains(evs[0].Data, `"restored":2`) {
			t.Fatalf("the lift's event: %+v", evs)
		}
		// stronger than lifted: applied again
		k.set(row(v10, store.RevokeWithdrawn, "CVE-2026"), row(v11, store.RevokeWithdrawn, "CVE-2027"))
		if r := en.sync(t, false); r.Counts["revoked"] != 1 || states(t, en, "prod", "tresor")["1.1"] != store.ReleaseYanked {
			t.Fatalf("stronger than lifted: %+v", r)
		}

		// an administrator's block takes over: a later lift restores nothing of that body
		if _, _, err := en.rel.Block(ctx, admin, "acme", v11.BodyHash, "ours"); err != nil {
			t.Fatal(err)
		}
		if n, err := en.up.LiftRevocation(ctx, admin, "acme", "acme-repo", v11.BuildID, 0); err != nil || n != 0 {
			t.Fatalf("lift under an administrator's block: %d %v", n, err)
		}
		if st := states(t, en, "prod", "tresor"); st["1.1"] != store.ReleaseYanked {
			t.Fatalf("an administrator's block lifted: %+v", st)
		}
		// a stored row whose remote row goes away is forgotten; what it applied stays
		k.set(row(v11, store.RevokeWithdrawn, "CVE-2027"))
		en.sync(t, false)
		if s := en.stored(t); len(s) != 1 || states(t, en, "prod", "tresor")["1.0"] != store.ReleaseYanked {
			t.Fatalf("a row gone: %+v", s)
		}
	})
}

// Spec 0039: the pass pages, paces a 429, stores nothing on a dry run, and runs before the cells
// whatever follows (a tenant at its bytes limit); a remote that is no kista is refused the switch,
// and one that stops answering while switched on is a problem that keeps its state.
func TestRevocationRemote(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		en.repo.put("v2.0.0", "linux_amd64", "tresor", signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "tresor_duckdb_cpp_init"))
		if _, err := en.up.Add(ctx, admin, "acme", en.spec(store.UpstreamEntry{Name: "tresor"})); err != nil {
			t.Fatal(err)
		}
		en.sync(t, false)
		// not a kista: no switch
		if _, err := en.up.SetRevocations(ctx, admin, "acme", "acme-repo", true, 0); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("the switch on a static repository: %v", err)
		}
		k := en.kista()
		k.page, k.busy = 1, 2
		c := en.releases(t, "tresor")[0]
		other := row(c, store.RevokeYanked, "")
		other.Name = "aaa"
		k.set(other, row(c, store.RevokeYanked, "old"))
		// a dry run reads and stores nothing
		if r := en.sync(t, true); len(en.stored(t)) != 0 || r.Error != "" {
			t.Fatalf("a dry run stored: %+v", en.stored(t))
		}
		u, err := en.up.SetRevocations(ctx, admin, "acme", "acme-repo", true, 0)
		if err != nil || !u.RemoteKista || !u.Revocations {
			t.Fatalf("the switch: %+v %v", u, err)
		}
		// at the tenant's bytes limit the pass still runs first
		if _, _, err := en.ten.SetTenantLimits(ctx, admin, "acme", map[string]int64{store.LimitBytes: 0}, ""); err != nil {
			t.Fatal(err)
		}
		reads := k.reads
		if r := en.sync(t, false); r.Counts["revoked"] != 1 || k.reads != reads+2 {
			t.Fatalf("paged and paced: %+v (reads %d)", r, k.reads-reads)
		}
		if st := states(t, en, "prod", "tresor"); st["1.0"] != store.ReleaseYanked {
			t.Fatalf("the yank: %+v", st)
		}
		// the remote stops answering: a problem; the switch and what is stored stay
		en.repo.mu.Lock()
		en.repo.kista = nil
		en.repo.mu.Unlock()
		r := en.sync(t, false)
		if !strings.Contains(fmt.Sprint(r.Problems), "not a kista") || len(en.stored(t)) != 1 {
			t.Fatalf("a remote gone: %+v %+v", r, en.stored(t))
		}
		if u, _ := en.up.Get(ctx, admin, "acme", "acme-repo"); !u.RemoteKista || !u.Revocations {
			t.Fatalf("its state: %+v", u)
		}
	})
}

// Spec 0039: an export for a plan that accepts revocations carries the exporting channel's revoked
// rows of the plan's names in a v2 manifest (private ones only with -private), and the site applies
// them when its switch is on; a plan that does not accept them gets a v1 manifest without any.
func TestExportRevocations(t *testing.T) {
	for _, e := range storetest.Engines(t) {
		t.Run(e.Name, func(t *testing.T) {
			g := newGateway(t, e)
			g.put(t, "tresor", "1.0", 1)
			if _, err := g.up.Add(ctx, admin, "acme", g.spec(store.UpstreamEntry{Name: "tresor"})); err != nil {
				t.Fatal(err)
			}
			g.sync(t, false)
			g.air(t, "tresor")
			plan := g.plan(t)
			if !plan.AcceptsRevocations {
				t.Fatal("a plan of this version accepts revocations")
			}
			prefix := plan.Sources[0].Prefix
			_, data := g.export(t, plan, upstream.ExportOptions{Private: []string{prefix}})
			if m, _, _ := g.open(t, data); m.Format != bundle.ManifestFormat || len(m.Sources[0].Revocations) != 0 {
				t.Fatalf("nothing revoked: %+v", m)
			}
			g.importSite(t, data)
			if _, err := g.up.SetRevocations(ctx, admin, "site", "air", true, 0); err != nil {
				t.Fatal(err)
			}
			// yanked at the exporting kista: carried, private, with -private only
			rel := g.releases(t, "tresor")[0]
			if _, err := g.rel.Apply(ctx, admin, "acme", "prod", "tresor", rel.ID, release.Yank, 0); err != nil {
				t.Fatal(err)
			}
			res, data := g.export(t, plan, upstream.ExportOptions{})
			if m, _, _ := g.open(t, data); len(m.Sources[0].Revocations) != 0 || outcomes(res)["revocations"] != 0 {
				t.Fatalf("a private row without -private: %+v", m.Sources[0].Revocations)
			}
			res, data = g.export(t, plan, upstream.ExportOptions{Private: []string{prefix}})
			m, _, _ := g.open(t, data)
			if m.Format != bundle.ManifestFormatV2 || len(m.Sources[0].Revocations) != 1 || m.Sources[0].Revocations[0].State != store.RevokeYanked ||
				m.Sources[0].Revocations[0].BodyHash != rel.BodyHash || outcomes(res)["revocations"] != 1 {
				t.Fatalf("revocations: %+v", m)
			}
			ir := g.importSite(t, data)
			site, err := g.rel.List(ctx, admin, "site", "prod", "tresor")
			if err != nil || len(site) != 1 || site[0].State != store.ReleaseYanked || target(t, ir, "air").Result.Counts["revoked"] != 1 {
				t.Fatalf("the site's release: %+v %v %+v", site, err, ir)
			}
			// a plan that does not accept them: a v1 manifest without them
			plan.AcceptsRevocations = false
			_, data = g.export(t, plan, upstream.ExportOptions{Private: []string{prefix}})
			if m, _, _ := g.open(t, data); m.Format != bundle.ManifestFormat || len(m.Sources[0].Revocations) != 0 {
				t.Fatalf("an older plan: %+v", m)
			}
		})
	}
}

// Spec 0039: a bundle-mode upstream takes the switch; a bundle's revocations are stored by build
// among its own builds and applied when it is on; an absent row deletes nothing.
func TestRevocationsImported(t *testing.T) {
	each(t, func(t *testing.T, en *env) {
		bu := en.bundler(t)
		en.bundleUpstream(t, "air", "x")
		x1 := signed(t, en.key, cpp("1.0", "v2.0.0"), 1, "x_duckdb_cpp_init")
		data, _ := bu.make([]bcell{{v: "v2.0.0", name: "x", file: x1}}, nil)
		en.importB(t, bu, data, upstream.ImportOptions{})
		rs := en.releases(t, "x")
		if len(rs) != 1 {
			t.Fatalf("imported: %+v", rs)
		}
		rev := func(m *bundle.Manifest) {
			m.Format = bundle.ManifestFormatV2
			m.Sources[0].Revocations = []bundle.Revocation{{Name: "x", Version: "1.0", Platform: "linux_amd64", BodyHash: rs[0].BodyHash,
				State: store.RevokeWithdrawn, Reason: "CVE"}}
		}
		data, _ = bu.make([]bcell{{v: "v2.0.0", name: "x", outcome: bundle.Unchanged, bodyHash: rs[0].BodyHash}}, rev)
		res := en.importB(t, bu, data, upstream.ImportOptions{})
		if tr := target(t, res, "air"); tr.Result.Counts["revoked_on_source"] != 1 || en.releases(t, "x")[0].State != store.ReleaseActive {
			t.Fatalf("switched off: %+v", tr)
		}
		if _, err := en.up.SetRevocations(ctx, admin, "acme", "air", true, 0); err != nil {
			t.Fatal(err)
		}
		data, _ = bu.make([]bcell{{v: "v2.0.0", name: "x", outcome: bundle.Unchanged, bodyHash: rs[0].BodyHash}}, rev)
		res = en.importB(t, bu, data, upstream.ImportOptions{})
		if tr := target(t, res, "air"); tr.Result.Counts["revoked"] != 1 || en.releases(t, "x")[0].State != store.ReleaseYanked {
			t.Fatalf("switched on: %+v", tr)
		}
		// absence is no signal: a later bundle without it keeps the stored row
		data, _ = bu.make([]bcell{{v: "v2.0.0", name: "x", outcome: bundle.Unchanged, bodyHash: rs[0].BodyHash}}, nil)
		en.importB(t, bu, data, upstream.ImportOptions{})
		stored, err := en.up.RevocationsOf(ctx, admin, "acme", "air")
		if err != nil || len(stored) != 1 {
			t.Fatalf("a bundle without the row: %+v %v", stored, err)
		}
	})
}
