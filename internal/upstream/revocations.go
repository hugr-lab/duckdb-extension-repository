package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/credential"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0039: revocations from a kista remote.
const (
	// maxRevocationRows is the most rows a pass reads (and a bundle carries): more is a problem, and
	// the rest is not read.
	maxRevocationRows = 10000
	// maxRevocationPages is the most pages a read asks for: maxRevocationRows at kista's 1,000 a page,
	// and one more to learn there are more.
	maxRevocationPages = maxRevocationRows/1000 + 1
	// maxRevocationsPage bounds one page's answer (1,000 rows of short fields).
	maxRevocationsPage = 4 << 20
	// a run's counts of the pass
	countRevoked         = "revoked"           // revocations applied
	countRevokedOnSource = "revoked_on_source" // stored, the switch off: shown, not applied
	outcomeRevocations   = "revocations"       // a problem of the pass itself
)

// remoteRevocations is what a pass read of a remote's -/revocations route.
type remoteRevocations struct {
	kista       bool   // the remote answered kista's shape
	notModified bool   // the first page answered 304: nothing more was read
	etag        string // the first page's
	complete    bool   // every page was read, within maxRevocationRows
	rows        []release.RevokedRow
}

// errNotKista is a remote whose -/revocations is a missing path, or answers another shape.
var errNotKista = errors.New("the source answers no revocations: not a kista, suspended or gone")

// readRevocations reads a repository upstream's -/revocations, every page, with its credential; the
// first page is conditional on etag. A 429 or 503 with a short Retry-After is pacing (spec 0032's).
func (s *Service) readRevocations(ctx context.Context, tenant string, u store.Upstream, etag string) (remoteRevocations, error) {
	var out remoteRevocations
	cred, tok, err := s.token(ctx, tenant, u)
	if err != nil {
		return out, err
	}
	base := s.prefix(u) + "/-/revocations"
	cursor, seen := "", map[string]bool{"": true}
	for page := 0; ; page++ {
		if page == maxRevocationPages {
			return out, fmt.Errorf("the source's revocations run over %d pages: the rest is not read", maxRevocationPages)
		}
		url := base
		if cursor != "" {
			url += "/" + cursor
		}
		cond := ""
		if page == 0 {
			cond = etag
		}
		var buf bytes.Buffer
		var d egress.Download
		d, tok, err = s.getPage(ctx, cred, tok, url, cond, &buf)
		switch {
		case errors.Is(err, egress.ErrNotFound):
			return out, errNotKista
		case err != nil:
			return out, err
		case d.NotModified && cond == "":
			return out, errors.New("a 304 to a request that was not conditional")
		case d.NotModified:
			out.kista, out.notModified, out.etag = true, true, etag
			return out, nil
		}
		var ans struct {
			API  *int                  `json:"api"`
			Rows *[]release.RevokedRow `json:"rows"`
			Next string                `json:"next"`
		}
		if json.Unmarshal(buf.Bytes(), &ans) != nil || ans.API == nil || ans.Rows == nil {
			return out, errNotKista
		}
		out.kista = true
		if page == 0 {
			out.etag = d.ETag
		}
		out.rows = append(out.rows, *ans.Rows...)
		if len(out.rows) > maxRevocationRows {
			out.rows = out.rows[:maxRevocationRows]
			return out, fmt.Errorf("the source lists over %d revocations: the rest is not read", maxRevocationRows)
		}
		if ans.Next == "" {
			out.complete = true
			return out, nil
		}
		if len(*ans.Rows) == 0 {
			return out, errors.New("the source's revocations page on from an empty page")
		}
		// each page moves on: a cursor seen in this read already is a loop
		if seen[ans.Next] || !cursorRe.MatchString(ans.Next) {
			return out, errors.New("the source's revocations page to a cursor that is not one")
		}
		seen[ans.Next] = true
		cursor = ans.Next
	}
}

var cursorRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`)

// getPage fetches one page into w: a 401 forgets the token once (as download does), a 429 or 503
// with a short Retry-After is asked again.
func (s *Service) getPage(ctx context.Context, cred *credential.Credential, tok, url, etag string, w *bytes.Buffer) (egress.Download, string, error) {
	timeout := s.Config.FetchTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	o := egress.DownloadOptions{ETag: etag, MaxBytes: maxRevocationsPage, Timeout: timeout, MinRate: 1}
	refreshed := false
	for attempt := 0; ; attempt++ {
		w.Reset()
		if cred != nil {
			o.Header = http.Header{"Authorization": {"Bearer " + tok}}
		}
		d, err := s.Fetch.Download(ctx, url, o, w)
		var ae *egress.AuthError
		if cred != nil && !refreshed && errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			refreshed = true
			cred.Invalidate(tok)
			if tok, err = cred.Token(ctx); err != nil {
				return d, tok, err
			}
			continue
		}
		var re *egress.RetryError
		if !errors.As(err, &re) || !re.Given || re.After > maxRetryAfter || attempt >= fetchRetries {
			return d, tok, err
		}
		select {
		case <-ctx.Done():
			return d, tok, ctx.Err()
		case <-time.After(re.After):
		}
	}
}

// revocationPass is a mirror's revocation pass (spec 0039), first in each run whatever follows: it
// reads the remote's revocations, matches them by build to this upstream's builds and stores them,
// then applies those due when the switch is on (counted and summed up in one problem when it is
// off). Its failures are the run's problems; the run goes on. A dry run only reads: it stores,
// applies and counts nothing.
func (s *Service) revocationPass(ctx context.Context, sc store.ServeChannel, u store.Upstream, a authz.Actor, res *Result) {
	if u.Kind != store.UpstreamRepo || u.Mode != store.ModeMirror {
		return
	}
	problem := func(name, platform, detail string) {
		if len(res.Problems) < maxProblems {
			res.Problems = append(res.Problems, CellNote{Platform: platform, Name: name, Outcome: outcomeRevocations, Detail: detail})
		}
	}
	etag := u.RevocationsETag
	got, err := s.readRevocations(ctx, sc.Tenant.Name, u, etag)
	var ae *egress.AuthError
	switch {
	case errors.Is(err, credential.ErrClient):
		problem("", "", "the identity provider refuses kista's own client")
		return
	case errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) && !u.Revocations && !u.RemoteKista:
		return // a remote that refuses the route is no kista known to this upstream: nothing to say every run
	case errors.Is(err, errNotKista):
		if u.Revocations { // kept: a suspended or unreachable kista is still the one switched on
			problem("", "", err.Error())
		} else if u.RemoteKista && !res.DryRun {
			if err := s.Store.InTenantTx(ctx, u.TenantID, nil, func(tx *store.Tx) error { return tx.SetUpstreamRemote(ctx, u.ID, false, "") }); err != nil {
				problem("", "", err.Error())
			}
		}
		return
	case err != nil && !got.kista:
		problem("", "", "reading the source's revocations: "+err.Error())
		return
	case err != nil: // cut over a cap: what was read is stored, nothing unlisted is forgotten
		problem("", "", err.Error())
	}
	if res.DryRun {
		return
	}
	builds, err := s.Store.UpstreamBuilds(ctx, u.TenantID, u.ID)
	if err != nil {
		problem("", "", err.Error())
		return
	}
	if !got.notModified {
		if err := s.storeRevocations(ctx, u, builds, got); err != nil {
			problem("", "", "storing the source's revocations: "+err.Error())
			return
		}
	} else if err := s.Store.InTenantTx(ctx, u.TenantID, nil, func(tx *store.Tx) error {
		return tx.SetUpstreamRemote(ctx, u.ID, true, got.etag)
	}); err != nil {
		problem("", "", err.Error())
		return
	}
	s.applyRevocations(ctx, u, a, res, problem)
}

// storeRevocations stores the rows matching this upstream's builds (state and reason as last seen).
// A stored row the remote no longer lists (after a complete read) or whose build is gone is
// forgotten: deleted when nothing was applied or lifted of it, otherwise kept, unlisted, so it is
// still lifted and a remote dropping it for a run undoes no lift.
func (s *Service) storeRevocations(ctx context.Context, u store.Upstream, builds map[[4]string]string, got remoteRevocations) error {
	stored, err := s.Store.UpstreamRevocations(ctx, u.ID)
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	for _, id := range builds {
		ids[id] = true
	}
	listed := map[string]bool{}
	return s.Store.InTenantTx(ctx, u.TenantID, nil, func(tx *store.Tx) error {
		for _, row := range got.rows {
			if store.RevocationRank(row.State) == 0 || !bodyHashRe.MatchString(row.BodyHash) {
				continue
			}
			id, ok := builds[[4]string{row.Name, row.Version, row.Platform, row.BodyHash}]
			if !ok || listed[id] {
				continue // nothing revoked ahead of time; a row listed twice taken once
			}
			listed[id] = true
			if err := tx.PutUpstreamRevocation(ctx, store.UpstreamRevocation{UpstreamID: u.ID, BuildID: id, TenantID: u.TenantID,
				BodyHash: row.BodyHash, Name: row.Name, PackageVersion: row.Version, Platform: row.Platform, State: row.State,
				Reason: store.CleanReason(row.Reason)}); err != nil {
				return err
			}
		}
		for _, r := range stored {
			if !ids[r.BuildID] || got.complete && !listed[r.BuildID] {
				if err := tx.ForgetUpstreamRevocation(ctx, r); err != nil {
					return err
				}
			}
		}
		if u.Mode != store.ModeMirror { // a bundle's rows: no remote read
			return nil
		}
		etag := ""
		if got.complete { // a cut read is read again whole next time
			etag = got.etag
		}
		return tx.SetUpstreamRemote(ctx, u.ID, true, etag)
	})
}

var bodyHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// applyRevocations applies an upstream's stored revocations that are due (spec 0039), or, the switch
// off, counts them and says so in one problem (the cells' problems keep their room).
func (s *Service) applyRevocations(ctx context.Context, u store.Upstream, a authz.Actor, res *Result, problem func(name, platform, detail string)) {
	stored, err := s.Store.UpstreamRevocations(ctx, u.ID)
	if err != nil {
		problem("", "", err.Error())
		return
	}
	off := 0
	for _, r := range stored {
		if !r.Due() {
			continue
		}
		if !u.Revocations {
			off++
			continue
		}
		applied, err := s.applyRevocation(ctx, u, a, r)
		if err != nil {
			problem(r.Name, r.Platform, "applying the source's revocation: "+err.Error())
			continue
		}
		if applied {
			res.Counts[countRevoked]++
		}
	}
	if off > 0 {
		res.Counts[countRevokedOnSource] += off
		problem("", "", fmt.Sprintf("%d of the source's revocations of this upstream's builds are not applied: its revocations are switched off", off))
	}
}

// applyRevocation applies one stored revocation: a withdrawal or block as the upstream's block of the
// body, a yank as a yank of the build's releases in the home channel. In the same transaction, under
// the tenant's upstream lock (so a lift and an apply never interleave), the row is read again and
// applied only while still due, then marked applied, with its event.
func (s *Service) applyRevocation(ctx context.Context, u store.Upstream, a authz.Actor, r store.UpstreamRevocation) (bool, error) {
	action := "blocked"
	if r.State == store.RevokeYanked {
		action = "yanked"
	}
	h := release.RevocationHooks{
		Check: func(tx *store.Tx) (bool, error) {
			cur, err := tx.GetUpstreamRevocation(ctx, u.ID, r.BuildID)
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			if err != nil || !cur.Due() || cur.State != r.State {
				return false, err // changed meanwhile: the next run applies what it is now
			}
			return true, nil
		},
		Done: func(tx *store.Tx, n int) error {
			if err := tx.MarkRevocationApplied(ctx, u.ID, r.BuildID, r.State); err != nil {
				return err
			}
			return tx.Event(ctx, u.TenantID, a.String(), "upstream.revocation", "upstream:"+u.Name, map[string]any{"upstream": u.Name,
				"upstream_id": u.ID, "name": r.Name, "version": r.PackageVersion, "platform": r.Platform, "body_hash": r.BodyHash,
				"action": action, "state": r.State, "reason": r.Reason, "yanked": n})
		},
	}
	var done bool
	var err error
	if r.State == store.RevokeYanked {
		_, done, err = s.Releases.UpstreamYank(ctx, a, u.TenantID, u.ID, u.ChannelID, r.BuildID, r.BodyHash, h)
	} else {
		_, done, err = s.Releases.UpstreamBlock(ctx, a, u.TenantID, u.ID, r.BodyHash, r.State, r.Reason, h)
	}
	return done, err
}

// importRevocations takes a bundle source's revocations into a bundle-mode upstream (spec 0039):
// matched by build among its own builds and stored as a pass stores rows, never forgetting one
// (absence is no signal in a bundle), then, at every import, the stored ones due are applied when its
// switch is on (a switch turned on since the last bundle applies what is stored).
func (s *Service) importRevocations(ctx context.Context, t store.Tenant, tg *target) {
	res := &tg.res
	problem := func(name, platform, detail string) {
		if len(res.Problems) < maxProblems {
			res.Problems = append(res.Problems, CellNote{Platform: platform, Name: name, Outcome: outcomeRevocations, Detail: detail})
		}
	}
	if len(tg.src.Revocations) > 0 {
		builds, err := s.Store.UpstreamBuilds(ctx, tg.u.TenantID, tg.u.ID)
		if err != nil {
			problem("", "", err.Error())
			return
		}
		got := remoteRevocations{kista: true}
		for _, r := range tg.src.Revocations {
			got.rows = append(got.rows, release.RevokedRow{Name: r.Name, Version: r.Version, Platform: r.Platform, BodyHash: r.BodyHash,
				State: r.State, Reason: r.Reason})
		}
		if err := s.storeRevocations(ctx, tg.u, builds, got); err != nil {
			problem("", "", "storing the bundle's revocations: "+err.Error())
			return
		}
	}
	a := authz.Actor{Kind: authz.ActorSystem, ID: "upstream:" + t.Name + "/" + tg.u.Name}
	s.applyRevocations(ctx, tg.u, a, res, problem)
}

// SetRevocations switches applying the remote's revocations on or off (spec 0039; expected: the
// upstream's version, 0 for any): a repository mirror whose remote is a kista (read now when it is
// not known yet), or a bundle-mode upstream. Switching on applies the stored revocations at the next
// run, which it makes due.
func (s *Service) SetRevocations(ctx context.Context, a authz.Actor, tenant, name string, on bool, expected int64) (store.Upstream, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return store.Upstream{}, err
	}
	u, err := s.Get(ctx, a, tenant, name)
	if err != nil {
		return u, err
	}
	kista := u.RemoteKista
	if on && !kista && u.Mode != store.ModeBundle {
		if kista, err = s.probeKista(ctx, tenant, u); err != nil {
			return u, err
		}
	}
	err = s.Store.InTenantTxForUpdate(ctx, t.ID, []string{store.TenantUpstreamLock(t.ID)}, func(tx *store.Tx) error {
		if u, err = tx.GetUpstream(ctx, t.ID, name); err != nil {
			return err
		}
		if expected != 0 && expected != u.Version {
			return store.ErrConflict
		}
		if on {
			if err := revocationsAllowed(u, kista); err != nil {
				return err
			}
			if kista && !u.RemoteKista {
				if err := tx.SetUpstreamRemote(ctx, u.ID, true, u.RevocationsETag); err != nil {
					return err
				}
			}
		}
		if u.Revocations == on {
			return nil
		}
		if err := tx.SetUpstreamRevocations(ctx, &u, on, 0); err != nil {
			return err
		}
		pending := 0
		stored, err := tx.UpstreamRevocations(ctx, u.ID)
		if err != nil {
			return err
		}
		for _, r := range stored {
			if r.Due() {
				pending++
			}
		}
		if on && u.Mode == store.ModeMirror {
			if err := tx.MakeDue(ctx, u.ID, ""); err != nil {
				return err
			}
		}
		return tx.Event(ctx, t.ID, a.String(), "upstream.revocations", "upstream:"+u.Name,
			map[string]any{"upstream": u.Name, "upstream_id": u.ID, "revocations": on, "pending": pending})
	})
	if err != nil {
		return store.Upstream{}, err
	}
	return s.Get(ctx, a, tenant, name)
}

// revocationsAllowed refuses the switch on an upstream that cannot apply revocations (spec 0039): a
// repository mirror whose remote is a kista, or a bundle-mode upstream, takes it.
func revocationsAllowed(u store.Upstream, kista bool) error {
	switch {
	case u.Mode == store.ModeBundle:
		return nil
	case u.Kind != store.UpstreamRepo || u.Mode != store.ModeMirror:
		return fmt.Errorf("%w: revocations are applied by a repository mirror of a kista, or a bundle-mode upstream", store.ErrInvalid)
	case !kista:
		return fmt.Errorf("%w: the source is not a kista (its -/revocations answers no revocations)", store.ErrInvalid)
	}
	return nil
}

// probeKista reads a repository upstream's -/revocations once: whether its remote is a kista. A
// failure to read it is ErrSourceFetch (the API's 400 still).
func (s *Service) probeKista(ctx context.Context, tenant string, u store.Upstream) (bool, error) {
	if u.Kind != store.UpstreamRepo || u.Mode == store.ModeBundle {
		return false, nil
	}
	got, err := s.readRevocations(ctx, tenant, u, "")
	switch {
	case errors.Is(err, errNotKista):
		return false, nil
	case err != nil && !got.kista:
		return false, fmt.Errorf("%w: %w: reading the source's revocations: %w", ErrSourceFetch, store.ErrInvalid, err)
	}
	return true, nil
}

// RevocationsOf lists an upstream's stored revocations, with what was applied and lifted.
func (s *Service) RevocationsOf(ctx context.Context, a authz.Actor, tenant, name string) ([]store.UpstreamRevocation, error) {
	u, err := s.Get(ctx, a, tenant, name)
	if err != nil {
		return nil, err
	}
	return s.Store.UpstreamRevocations(ctx, u.ID)
}

// LiftRevocation lifts an upstream's revocation of a build (spec 0039; expected: the upstream's
// version, 0 for any): its block of the body, if any, is removed and what it yanked is restored. The
// stored row is marked lifted at the stronger of its stored and applied states, so the pass applies
// it again only when the remote's state becomes stronger still. A row nothing applied of is left to
// lift (never applied, or lifted already) is ErrNotLiftable. The mark, the restore, the upstream's
// version and the event are one transaction, under the tenant's upstream lock (a run's apply waits).
// A tenant administrator's.
func (s *Service) LiftRevocation(ctx context.Context, a authz.Actor, tenant, name, buildID string, expected int64) (int, error) {
	t, err := s.tenant(ctx, a, tenant)
	if err != nil {
		return 0, err
	}
	u, err := s.Get(ctx, a, tenant, name)
	if err != nil {
		return 0, err
	}
	var r store.UpstreamRevocation
	var state string
	n, _, err := s.Releases.LiftUpstream(ctx, a, t.ID, u.ID, buildID, release.RevocationHooks{
		Check: func(tx *store.Tx) (bool, error) {
			var err error
			if u, err = tx.GetUpstream(ctx, t.ID, name); err != nil {
				return false, err
			}
			if expected != 0 && expected != u.Version {
				return false, store.ErrConflict
			}
			if r, err = tx.GetUpstreamRevocation(ctx, u.ID, buildID); err != nil {
				return false, err
			}
			if !r.Liftable() {
				return false, ErrNotLiftable
			}
			state = r.AppliedState
			if store.RevocationRank(r.State) > store.RevocationRank(state) {
				state = r.State
			}
			if err := tx.MarkRevocationLifted(ctx, u.ID, buildID, state); err != nil {
				return false, err
			}
			return true, tx.BumpUpstream(ctx, &u)
		},
		Done: func(tx *store.Tx, n int) error {
			return tx.Event(ctx, t.ID, a.String(), "upstream.revocation.lift", "upstream:"+u.Name, map[string]any{"upstream": u.Name,
				"upstream_id": u.ID, "build": buildID, "body_hash": r.BodyHash, "state": state, "restored": n})
		},
	})
	return n, err
}
