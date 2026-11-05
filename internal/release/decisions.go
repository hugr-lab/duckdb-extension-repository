package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0041: the administrators' decisions on the decision queue (Skip and Dismiss), kept in kista
// so every administrator sees the same queue. A decision is advisory: it changes no placement, rule,
// follow, grant or serving answer.

// DecisionInput is a decision as a tenant administrator writes it: a skip of a channel's waiting
// version, a dismissal of a channel's follow skip (Situation: its reason, a JSON string), or a
// dismissal of the tenant's (Situation: the channels whose rules take the name, a JSON array).
type DecisionInput struct {
	Kind, Channel string
	Source, Name  string // the source as answers name it
	Version       string
	Situation     json.RawMessage
	PackageType   string // the tenant's decision's type ("": the default); a channel's is the channel's
}

// maxSkipReason bounds a follow skip's reason a dismissal names: the bytes follow_skips keeps.
const maxSkipReason = 400

// DecisionView is a decision with its source as answers name it.
type DecisionView struct {
	store.Decision
	SourceLabel string
}

// SetDecision writes the decision of (scope, kind, source, name), or replaces the one there, and
// writes decision.set (with before when it replaced one): the tenant's administrators only. The
// source is one of the tenant's live sources, the name has entries of it in the catalogue (of the
// channel's type), a skip's version is one of them in a signed channel, and a dismissal's situation
// is of its kind's JSON type, stored canonically. created reports a new decision.
func (s *Service) SetDecision(ctx context.Context, a authz.Actor, tenant string, in DecisionInput) (DecisionView, bool, error) {
	t, err := s.mayAdminTenant(ctx, a, tenant)
	if err != nil {
		return DecisionView{}, false, err
	}
	d := store.Decision{TenantID: t.ID, Kind: in.Kind, Name: in.Name, DecidedBy: a.String(), PackageType: pkgtype.Default}
	if in.PackageType != "" {
		d.PackageType = in.PackageType
	}
	if err := ValidName(in.Name); err != nil {
		return DecisionView{}, false, err
	}
	var ch store.Channel
	if in.Channel != "" {
		if ch, err = s.Store.GetChannel(ctx, tenant, in.Channel); err != nil {
			return DecisionView{}, false, err
		}
		if ch.Kind != store.ChannelSigned {
			return DecisionView{}, false, fmt.Errorf("%w: a decision is on a signed channel's rule", store.ErrInvalid)
		}
		if in.PackageType != "" && in.PackageType != ch.PackageType {
			return DecisionView{}, false, fmt.Errorf("%w: the channel %s serves %s, not %s", store.ErrInvalid, ch.Name, ch.PackageType, in.PackageType)
		}
		d.ChannelID, d.Channel, d.PackageType = ch.ID, ch.Name, ch.PackageType
	}
	switch in.Kind {
	case store.DecisionSkip:
		if ch.ID == "" || in.Version == "" || len(in.Situation) > 0 {
			return DecisionView{}, false, fmt.Errorf("%w: a skip names a channel and a version, and no situation", store.ErrInvalid)
		}
		d.PackageVersion = in.Version
	case store.DecisionDismiss:
		if in.Version != "" || len(in.Situation) == 0 {
			return DecisionView{}, false, fmt.Errorf("%w: a dismissal names its situation, and no version", store.ErrInvalid)
		}
		if d.Situation, err = s.situation(ctx, tenant, ch.ID != "", in.Situation); err != nil {
			return DecisionView{}, false, err
		}
	default:
		return DecisionView{}, false, fmt.Errorf("%w: a decision's kind is skip or dismiss", store.ErrInvalid)
	}
	src, err := s.Store.ResolveSource(ctx, t.ID, in.Source)
	if errors.Is(err, store.ErrNoSource) {
		return DecisionView{}, false, fmt.Errorf("%w: %q is not a source of the tenant (tenant, upstream:<an upstream>, publisher:<a publisher>)",
			store.ErrInvalid, in.Source)
	}
	if err != nil {
		return DecisionView{}, false, err
	}
	d.Source = src
	var before store.Decision
	var had bool
	err = s.Store.InTx(ctx, store.DecisionLock(t.ID), func(tx *store.Tx) error {
		if err := tx.WritableTenant(ctx, t.ID, false); err != nil { // spec 0019: a deleting tenant's refuses
			return err
		}
		if ch.ID != "" { // the channel read outside: still there
			if _, err := tx.GetChannel(ctx, tenant, ch.Name); err != nil {
				return err
			}
		}
		// the source read outside: still there (a source removed since takes its decisions with it)
		if label, err := tx.SourceLabel(ctx, src); err != nil {
			return err
		} else if strings.HasSuffix(label, "(removed)") {
			return fmt.Errorf("%w: %q was removed", store.ErrInvalid, in.Source)
		}
		// the entry, read in the transaction: a decision is written only while it is there (its last
		// build collected or purged takes its decisions with it)
		if vs, err := tx.EntryVersions(ctx, t.ID, d.PackageType, src, in.Name); err != nil {
			return err
		} else if len(vs) == 0 || d.Kind == store.DecisionSkip && !slices.Contains(vs, in.Version) {
			return fmt.Errorf("%w: no entry of %s %s from %s in the catalogue", store.ErrNotFound, in.Name, in.Version, in.Source)
		}
		if before, had, err = tx.SetDecision(ctx, &d); err != nil {
			return err
		}
		f := decisionFields(d, in.Source)
		if had {
			f["before"] = decided(before)
		}
		return tx.Event(ctx, t.ID, a.String(), "decision.set", decisionSubject(d), f)
	})
	if err != nil {
		return DecisionView{}, false, err
	}
	return DecisionView{Decision: d, SourceLabel: in.Source}, !had, nil
}

// decided is what a decision is keyed to: a skip's version, or a dismissal's situation.
func decided(d store.Decision) any {
	if d.Kind == store.DecisionSkip {
		return d.PackageVersion
	}
	return json.RawMessage(d.Situation)
}

// situation checks a dismissal's situation and answers its canonical JSON text: for a channel's (a
// follow's skip) a JSON string of at most maxSkipReason bytes; for the tenant's, a JSON array of the
// tenant's channel names, sorted bytewise and without duplicates.
func (s *Service) situation(ctx context.Context, tenant string, channel bool, raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if channel {
		var reason string
		if err := dec.Decode(&reason); err != nil || dec.More() || reason == "" || len(reason) > maxSkipReason {
			return "", fmt.Errorf("%w: a channel's dismissal's situation is the follow's skip reason, a JSON string of 1 to %d bytes",
				store.ErrInvalid, maxSkipReason)
		}
		b, err := json.Marshal(reason)
		return string(b), err
	}
	var names []string
	if err := dec.Decode(&names); err != nil || dec.More() || names == nil {
		return "", fmt.Errorf("%w: the tenant's dismissal's situation is the channels taking the name, a JSON array of channel names", store.ErrInvalid)
	}
	chs, err := s.Store.ListChannels(ctx, tenant)
	if err != nil {
		return "", err
	}
	for _, n := range names {
		if !slices.ContainsFunc(chs, func(c store.Channel) bool { return c.Name == n }) {
			return "", fmt.Errorf("%w: %q is not a channel of the tenant", store.ErrInvalid, n)
		}
	}
	slices.Sort(names)
	b, err := json.Marshal(slices.Compact(names))
	return string(b), err
}

// decisionFields are a decision's event data.
func decisionFields(d store.Decision, source string) map[string]any {
	f := map[string]any{"id": d.ID, "kind": d.Kind, "source": source}
	if d.Kind == store.DecisionSkip {
		f["version"] = d.PackageVersion
	} else {
		f["situation"] = json.RawMessage(d.Situation)
	}
	if d.PackageType != pkgtype.Default {
		f["package_type"] = d.PackageType
	}
	return f
}

// decisionSubject is a channel's decision's subject, or the tenant's.
func decisionSubject(d store.Decision) string {
	if d.Channel != "" {
		return "channel:" + d.Channel + "/ext:" + d.Name
	}
	return "ext:" + d.Name
}

// ClearDecision removes a decision (an administrator's Undo) at its row version (0: any), writing
// decision.clear: the tenant's administrators only. A decision of another tenant is not found;
// one changed since, store.ErrConflict.
func (s *Service) ClearDecision(ctx context.Context, a authz.Actor, tenant, id string, version int64) error {
	t, err := s.mayAdminTenant(ctx, a, tenant)
	if err != nil {
		return err
	}
	labels, err := s.Store.SourceLabels(ctx, t.ID)
	if err != nil {
		return err
	}
	return s.Store.InTx(ctx, store.DecisionLock(t.ID), func(tx *store.Tx) error {
		if err := tx.WritableTenant(ctx, t.ID, false); err != nil { // spec 0019: a deleting tenant's refuses
			return err
		}
		d, err := tx.DeleteDecision(ctx, t.ID, id, version)
		if err != nil {
			return err
		}
		return tx.Event(ctx, t.ID, a.String(), "decision.clear", decisionSubject(d), decisionFields(d, store.Label(labels, d.Source)))
	})
}

// Decisions lists a tenant's decisions by id (of one package type, "" every one; of one channel,
// "" every scope), paged after an id: the tenant's administrators and its auditors.
func (s *Service) Decisions(ctx context.Context, a authz.Actor, tenant, packageType, channel, after string, limit int) ([]DecisionView, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAudit, authz.Resource{Tenant: tenant}); err != nil {
		return nil, err
	}
	t, err := s.Store.GetTenant(ctx, tenant)
	if err != nil {
		return nil, err
	}
	f := store.DecisionFilter{PackageType: packageType, After: after, Limit: limit}
	if channel != "" {
		ch, err := s.Store.GetChannel(ctx, tenant, channel)
		if err != nil {
			return nil, err
		}
		f.ChannelID = ch.ID
	}
	return s.decisions(ctx, t.ID, f)
}

// ChannelDecisions lists a channel's decisions by id, paged after an id: its administrators, and
// the tenant's administrators and auditors.
func (s *Service) ChannelDecisions(ctx context.Context, a authz.Actor, tenant, channel, after string, limit int) ([]DecisionView, error) {
	err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, Channel: channel})
	if errors.Is(err, authz.ErrDenied) {
		err = s.Authz.Allow(ctx, a, authz.VerbAudit, authz.Resource{Tenant: tenant})
	}
	if err != nil {
		return nil, err
	}
	ch, err := s.Store.GetChannel(ctx, tenant, channel)
	if err != nil {
		return nil, err
	}
	return s.decisions(ctx, ch.TenantID, store.DecisionFilter{ChannelID: ch.ID, After: after, Limit: limit})
}

func (s *Service) decisions(ctx context.Context, tenantID string, f store.DecisionFilter) ([]DecisionView, error) {
	ds, err := s.Store.ListDecisions(ctx, tenantID, f)
	if err != nil || len(ds) == 0 {
		return nil, err
	}
	labels, err := s.Store.SourceLabels(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]DecisionView, len(ds))
	for i, d := range ds {
		out[i] = DecisionView{Decision: d, SourceLabel: store.Label(labels, d.Source)}
	}
	return out, nil
}
