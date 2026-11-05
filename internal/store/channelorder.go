package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Spec 0043: the channels' order, for display only. A signed channel may name its next channel, of
// the same tenant and package type: a strict chain (a channel is the next of at most one channel,
// and following next never comes back), which nothing on the server's paths reads.

// OrderEdge is one edge of a tenant's order: a channel and its next, by name.
type OrderEdge struct {
	Channel string `json:"channel"`
	Next    string `json:"next"`
}

// ChannelOrder reads the order of a tenant's channels of a package type, by channel name.
func (s *Store) ChannelOrder(ctx context.Context, tenantID, packageType string) ([]OrderEdge, error) {
	var out []OrderEdge
	err := s.eachRow(ctx, `SELECT c.name, n.name FROM channel_next o JOIN channels c ON c.id = o.channel_id JOIN channels n ON n.id = o.next_channel_id
WHERE c.tenant_id = ? AND c.package_type = ?`, []any{tenantID, packageType}, func(sc func(...any) error) error {
		var e OrderEdge
		if err := sc(&e.Channel, &e.Next); err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	SortOrder(out)
	return out, err
}

// SetChannelOrder replaces the order of a tenant's channels of a package type with edges (none
// clears it), checking every channel named is the tenant's, signed and of the type, that no channel
// is named twice as a channel nor twice as a next, that none is its own next, and that the edges
// make no cycle: ErrInvalid naming the first problem. It answers the order it replaced and whether
// anything changed. The caller holds the tenant's row (its channels are created under it).
func (t *Tx) SetChannelOrder(ctx context.Context, tenantID, packageType string, edges []OrderEdge, by string) (before []OrderEdge, changed bool, err error) {
	type ch struct{ id, kind, packageType string }
	chs := map[string]ch{}
	rows, err := t.query(ctx, "SELECT id, name, kind, package_type FROM channels WHERE tenant_id = ?", tenantID)
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		var c ch
		var name string
		if err := rows.Scan(&c.id, &name, &c.kind, &c.packageType); err != nil {
			rows.Close()
			return nil, false, err
		}
		chs[name] = c
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	check := func(name string) error {
		c, ok := chs[name]
		switch {
		case !ok:
			return fmt.Errorf("%w: %q is not a channel of the tenant", ErrInvalid, name)
		case c.kind != ChannelSigned:
			return fmt.Errorf("%w: %s is a passthrough channel, which takes no place in the order", ErrInvalid, name)
		case c.packageType != packageType:
			return fmt.Errorf("%w: %s serves %s, not %s", ErrInvalid, name, c.packageType, packageType)
		}
		return nil
	}
	next := map[string]string{}
	prev := map[string]string{}
	for _, e := range edges {
		if err := check(e.Channel); err != nil {
			return nil, false, err
		}
		if err := check(e.Next); err != nil {
			return nil, false, err
		}
		switch {
		case e.Channel == e.Next:
			return nil, false, fmt.Errorf("%w: %s is named as its own next", ErrInvalid, e.Channel)
		case next[e.Channel] != "":
			return nil, false, fmt.Errorf("%w: %s is named twice as a channel", ErrInvalid, e.Channel)
		case prev[e.Next] != "":
			return nil, false, fmt.Errorf("%w: %s is named twice as a next (after %s and %s)", ErrInvalid, e.Next, prev[e.Next], e.Channel)
		}
		next[e.Channel], prev[e.Next] = e.Next, e.Channel
	}
	// each channel has at most one next: following it from any channel either ends or comes back
	for _, e := range edges {
		seen := map[string]bool{e.Channel: true}
		for c := next[e.Channel]; c != ""; c = next[c] {
			if seen[c] {
				return nil, false, fmt.Errorf("%w: the order makes a cycle through %s", ErrInvalid, e.Channel)
			}
			seen[c] = true
		}
	}
	if before, err = t.ChannelOrder(ctx, tenantID, packageType); err != nil {
		return nil, false, err
	}
	after := slices.Clone(edges)
	SortOrder(after)
	if slices.Equal(before, after) {
		return before, false, nil
	}
	if _, err := t.exec(ctx, `DELETE FROM channel_next WHERE channel_id IN (SELECT id FROM channels WHERE tenant_id = ? AND package_type = ?)`,
		tenantID, packageType); err != nil {
		return nil, false, err
	}
	at := t.s.d.timeArg(t.Now())
	for _, e := range after {
		if _, err := t.exec(ctx, "INSERT INTO channel_next (channel_id, next_channel_id, set_by, set_at) VALUES (?, ?, ?, ?)",
			chs[e.Channel].id, chs[e.Next].id, by, at); err != nil {
			return nil, false, t.s.mapErr(err, "channel order")
		}
	}
	return before, true, nil
}

// ChannelOrder is the store's ChannelOrder in a transaction.
func (t *Tx) ChannelOrder(ctx context.Context, tenantID, packageType string) ([]OrderEdge, error) {
	rows, err := t.query(ctx, `SELECT c.name, n.name FROM channel_next o JOIN channels c ON c.id = o.channel_id JOIN channels n ON n.id = o.next_channel_id
WHERE c.tenant_id = ? AND c.package_type = ?`, tenantID, packageType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrderEdge
	for rows.Next() {
		var e OrderEdge
		if err := rows.Scan(&e.Channel, &e.Next); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	SortOrder(out)
	return out, rows.Err()
}

// SortOrder sorts an order's edges by channel name, bytewise (as answers list them).
func SortOrder(edges []OrderEdge) {
	slices.SortFunc(edges, func(a, b OrderEdge) int { return strings.Compare(a.Channel, b.Channel) })
}
