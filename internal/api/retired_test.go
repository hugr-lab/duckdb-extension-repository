package api_test

import (
	"bytes"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// retire retires a release of a channel as the retention pass does (spec 0042).
func (en *env) retire(t *testing.T, channel, id string) {
	t.Helper()
	ch, err := en.st.GetChannel(ctx, "acme", channel)
	if err != nil {
		t.Fatal(err)
	}
	if err := en.st.InTx(ctx, "kista/channel/"+ch.ID, func(tx *store.Tx) error {
		r, err := tx.GetRelease(ctx, ch.ID, id)
		if err != nil {
			return err
		}
		if err := tx.RetireRelease(ctx, &r, "system:release-retention"); err != nil {
			return err
		}
		return tx.BumpReleaseVersion(ctx, ch.ID)
	}); err != nil {
		t.Fatal(err)
	}
}

// Spec 0042: a retired release is DuckDB's missing answer, the same bytes as a version that never
// existed; the index shows it as retired (with the DuckDB versions it would serve) and answers the
// version's status retired; ?state=retired lists it; its extension's administrators restore it; an
// insert into its slot is 409 retired.
func TestRetiredIndexAndRoutes(t *testing.T) {
	m := newMgmt(t)
	const E = "/api/v1/tenants/acme/channels/prod/extensions/"
	id := m.ids["tresor-1.0"]
	m.retire(t, "prod", id)

	missing := m.get(t, "/acme/prod/tresor/9.9/v2.0.0/linux_amd64/tresor.duckdb_extension.gz", "")
	for _, p := range []string{"tresor/1.0/v2.0.0/linux_amd64/tresor.duckdb_extension.gz", "v2.0.0/linux_amd64/tresor.duckdb_extension.gz"} {
		r := m.get(t, "/acme/prod/"+p, "")
		if r.status != missing.status || !bytes.Equal(r.body, missing.body) || r.status == 200 {
			t.Fatalf("%s: %d %q, the missing answer is %d %q", p, r.status, r.body, missing.status, missing.body)
		}
	}
	item := m.get(t, E+"tresor/versions/1.0?duckdb_version=v2.0.0&platform=linux_amd64", "").json(t)
	if item["status"] != store.ReleaseRetired || item["current"] != false || len(item["yanked"].([]any)) != 0 {
		t.Fatalf("the version's status: %v", item)
	}
	// a yank beside it says more: yanked comes first
	if s := m.get(t, E+"tresor/versions/0.9?duckdb_version=v2.0.0&platform=linux_amd64", "").json(t)["status"]; s != "yanked" {
		t.Fatalf("a yanked version: %v", s)
	}
	var row map[string]any
	for _, x := range m.get(t, E+"tresor", "").json(t)["releases"].([]any) {
		if r := x.(map[string]any); r["version"] == "1.0" {
			row = r
		}
	}
	if row["state"] != store.ReleaseRetired || len(row["serves"].([]any)) != 0 || len(row["current_for"].([]any)) != 0 ||
		len(row["would_serve"].([]any)) != 1 || row["would_serve"].([]any)[0] != "v2.0.0" {
		t.Fatalf("the retired row: %v", row)
	}

	carl, eve := m.toks["channel adm"], m.toks["ext admin"]
	rs := m.call(t, "GET", "/api/v1/tenants/acme/channels/prod/releases?state=retired", carl, "").json(t)["releases"].([]any)
	if len(rs) != 1 || rs[0].(map[string]any)["id"] != id || rs[0].(map[string]any)["retired_from"] != store.ReleaseActive ||
		rs[0].(map[string]any)["retired_at"] == "" {
		t.Fatalf("?state=retired: %v", rs)
	}
	// a restore: by the extension's administrators, not an installer; If-Match as every release change
	if r := m.call(t, "POST", E+"tresor/releases/"+id+"/restore", m.toks["install"], "", "If-Match", "*"); r.status != 404 {
		t.Fatalf("an installer's restore: %d", r.status)
	}
	if r := m.call(t, "POST", E+"tresor/releases/"+id+"/restore", eve, ""); r.status != 428 {
		t.Fatalf("a restore without If-Match: %d", r.status)
	}
	r := m.call(t, "POST", E+"tresor/releases/"+id+"/restore", eve, "", "If-Match", "*")
	if r.status != 200 || r.json(t)["state"] != store.ReleaseActive || r.json(t)["retired_from"] != nil {
		t.Fatalf("restore: %d %s", r.status, r.body)
	}
	if r := m.call(t, "POST", E+"tresor/releases/"+id+"/restore", eve, "", "If-Match", "*"); r.status != 409 {
		t.Fatalf("restored twice: %d", r.status)
	}
	if r := m.get(t, "/acme/prod/tresor/1.0/v2.0.0/linux_amd64/tresor.duckdb_extension.gz", ""); r.status != 200 {
		t.Fatalf("the restored release is not served: %d", r.status)
	}
	if ev := m.lastEvent(t, "release.restore"); ev.Subject != "channel:prod/ext:tresor/release:"+id {
		t.Fatalf("release.restore: %+v", ev)
	}

	// an insert into a retired slot: a promotion of the same body
	if err := store.ExecRaw(ctx, m.st, "UPDATE builds SET checked = ? WHERE name = ?", true, "acl"); err != nil {
		t.Fatal(err)
	}
	m.retire(t, "prod", m.ids["acl"])
	r = m.call(t, "POST", E+"acl/releases/promote", m.toks["server admin"], `{"from_channel":"staging","version":"1.0"}`)
	if r.status != 409 || r.json(t)["type"] != "urn:kista:problem:retired" {
		t.Fatalf("a promotion into a retired slot: %d %s", r.status, r.body)
	}
}

// lastEvent is the tenant's newest event of a kind.
func (m *mgmt) lastEvent(t *testing.T, kind string) store.Event {
	t.Helper()
	tn, err := m.st.GetTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := m.st.ListEvents(ctx, tn.ID, store.EventFilter{Kind: kind, Limit: 1})
	if err != nil || len(evs) == 0 {
		t.Fatalf("no %s event: %v", kind, err)
	}
	return evs[0]
}
