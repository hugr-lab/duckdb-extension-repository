package sinks_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/audit/sinks"
	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/egress"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

var ctx = context.Background()

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

func tenant(t *testing.T, st *store.Store, name string) store.Tenant {
	t.Helper()
	tn := store.Tenant{Name: name, DisplayName: name}
	if err := st.InTx(ctx, "", func(tx *store.Tx) error { return tx.CreateTenant(ctx, &tn) }); err != nil {
		t.Fatal(err)
	}
	return tn
}

func event(t *testing.T, st *store.Store, tenantID, subject string) string {
	t.Helper()
	var id string
	if err := st.InTx(ctx, "", func(tx *store.Tx) error {
		if err := tx.Event(ctx, tenantID, "os:1:t", "grant.add", subject, map[string]any{"principal": "p"}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	evs, err := st.ListEvents(ctx, tenantID, store.EventFilter{Subject: subject, Limit: 1})
	if err != nil || len(evs) != 1 {
		t.Fatalf("the event: %v %v", evs, err)
	}
	id = evs[0].ID
	return id
}

// collector is a fake OTLP endpoint: it fails while fail > 0, then records event ids per tenant.
type collector struct {
	mu       sync.Mutex
	fail     int
	status   int
	received map[string][]string // tenant id → event ids
	header   http.Header
	bodies   [][]byte
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail > 0 {
		c.fail--
		w.WriteHeader(c.status)
		return
	}
	b, _ := io.ReadAll(r.Body)
	c.bodies, c.header = append(c.bodies, b), r.Header.Clone()
	var req struct {
		ResourceLogs []struct {
			Resource struct {
				Attributes []struct {
					Key   string `json:"key"`
					Value struct {
						S string `json:"stringValue"`
					} `json:"value"`
				} `json:"attributes"`
			} `json:"resource"`
			ScopeLogs []struct {
				LogRecords []struct {
					Attributes []struct {
						Key   string `json:"key"`
						Value struct {
							S string `json:"stringValue"`
						} `json:"value"`
					} `json:"attributes"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := json.Unmarshal(b, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for _, rl := range req.ResourceLogs {
		tid := ""
		for _, a := range rl.Resource.Attributes {
			if a.Key == "kista.tenant.id" {
				tid = a.Value.S
			}
		}
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				for _, a := range lr.Attributes {
					if a.Key == "kista.event.id" {
						c.received[tid] = append(c.received[tid], a.Value.S)
					}
				}
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (c *collector) ids() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]bool{}
	for _, ids := range c.received {
		for _, id := range ids {
			out[id] = true
		}
	}
	return out
}

func loopbackClient(t *testing.T, srv *httptest.Server) *egress.Client {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	c, err := egress.New(egress.Config{AllowLoopbackHTTP: true,
		Allow: []egress.Allow{{Prefix: netip.MustParsePrefix("127.0.0.1/32"), Ports: []uint16{uint16(port)}}}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func jsonlIDs(t *testing.T, path string) map[string]string {
	t.Helper()
	out := map[string]string{} // id → tenant id
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r struct {
			ID       string `json:"id"`
			TenantID string `json:"tenant_id"`
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue // a line being written
		}
		out[r.ID] = r.TenantID
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDelivery(t *testing.T) {
	st := openStore(t)
	acme, beta := tenant(t, st, "acme"), tenant(t, st, "beta")
	col := &collector{fail: 2, status: http.StatusServiceUnavailable, received: map[string][]string{}}
	srv := httptest.NewServer(col)
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "events.jsonl")
	cfg := []config.Sink{
		{Name: "collector", Kind: "otlp", Tenants: []string{"acme"}, Server: true},
		{Name: "file", Kind: "jsonl", Tenants: []string{"*"}},
	}
	resolver, err := sinks.Attach(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// events written before the manager runs wait in the buffer
	acmeEv, betaEv := event(t, st, acme.ID, "grant:1"), event(t, st, beta.ID, "grant:2")
	serverEv := event(t, st, audit.ServerTenant, "grant:3")
	// a tenant created after the resolver loaded: its bits are set for every sink naming tenants
	gamma := tenant(t, st, "gamma")
	gammaEv := event(t, st, gamma.ID, "grant:4")
	if e, _ := st.GetEvent(ctx, gamma.ID, gammaEv); e.Pending != 3 {
		t.Fatalf("an unknown tenant's bits: %+v", e)
	}
	run := func() (stop func()) {
		otlp := &sinks.OTLP{URL: srv.URL + "/v1/logs", Client: loopbackClient(t, srv), Header: http.Header{"Authorization": {"Bearer s"}},
			Resource: map[string]string{"deployment.environment.name": "test", "service.name": "spoofed"}, Version: "v-test", Instance: "i"}
		m := &sinks.Manager{Store: st, Resolver: resolver, Holder: "test", Log: slog.New(slog.DiscardHandler),
			Every: 5 * time.Millisecond, Refresh: 20 * time.Millisecond, MaxBackoff: 20 * time.Millisecond, Report: time.Millisecond,
			Sinks: []sinks.Sink{
				{Name: "collector", Selection: sinks.SelectionOf(cfg[0]), Sender: otlp},
				{Name: "file", Selection: sinks.SelectionOf(cfg[1]), Sender: &sinks.JSONL{Path: file, Keep: 2}},
			}}
		c, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { m.Run(c); close(done) }()
		return func() { cancel(); <-done }
	}
	stop := run()
	waitFor(t, "delivery", func() bool {
		got := col.ids()
		return got[acmeEv] && got[serverEv] && len(jsonlIDs(t, file)) >= 3
	})
	pending := func() int {
		evs, err := st.PendingEvents(ctx, 3, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(evs)
	}
	// the failures and the recovery are server events
	waitFor(t, "audit.sink", func() bool {
		evs, _ := st.ListEvents(ctx, audit.ServerTenant, store.EventFilter{Kind: "audit.sink", Ascending: true})
		return len(evs) == 2 && evs[0].Outcome == audit.Failed && strings.Contains(evs[0].Data, `"class":"egress.status"`) && strings.Contains(evs[0].Data, `"status":503`) &&
			evs[1].Outcome == audit.OK
	})
	waitFor(t, "every bit cleared", func() bool { return pending() == 0 })
	stop()
	got := col.ids()
	if got[betaEv] || got[gammaEv] {
		t.Fatalf("the collector received a tenant it does not take: %v", got)
	}
	lines := jsonlIDs(t, file)
	if _, ok := lines[serverEv]; ok {
		t.Fatal("the file received the server's events without server: true")
	}
	for _, id := range []string{acmeEv, betaEv, gammaEv} {
		if _, ok := lines[id]; !ok {
			t.Fatalf("the file lacks %s: %v", id, lines)
		}
	}
	if col.header.Get("Authorization") != "Bearer s" || col.header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers: %v", col.header)
	}
	body := string(col.bodies[0])
	for _, want := range []string{`"service.name","value":{"stringValue":"kista"}`, `"deployment.environment.name"`, `"eventName":"kista.grant.add"`,
		`"kista.tenant.name","value":{"stringValue":"acme"}`} {
		if !strings.Contains(body, want) {
			t.Fatalf("the OTLP body lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, "spoofed") {
		t.Fatalf("events.resource overrode service.name: %s", body)
	}
	// a restart sends nothing again: delivered events lost their bits
	acme2 := event(t, st, acme.ID, "grant:5")
	stop = run()
	waitFor(t, "the next event", func() bool { return col.ids()[acme2] })
	stop()
	col.mu.Lock()
	defer col.mu.Unlock()
	if n := len(col.received[acme.ID]); n != 2 {
		t.Fatalf("acme's events sent once each: %v", col.received[acme.ID])
	}
}

func TestRemovedSink(t *testing.T) {
	st := openStore(t)
	acme := tenant(t, st, "acme")
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	st.Now = func() time.Time { return now }
	old := []config.Sink{{Name: "old", Kind: "jsonl", Path: "-", Tenants: []string{"*"}}}
	if _, err := sinks.Attach(ctx, st, old); err != nil {
		t.Fatal(err)
	}
	ev := event(t, st, acme.ID, "grant:1")
	// the configuration drops it; another sink comes
	cfg := []config.Sink{{Name: "new", Kind: "jsonl", Path: "-", Tenants: []string{"*"}}}
	resolver, err := sinks.Attach(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := st.GetEvent(ctx, acme.ID, ev); e.Pending != 1 {
		t.Fatalf("old's bit: %+v", e)
	}
	run := func() {
		var out strings.Builder
		m := &sinks.Manager{Store: st, Resolver: resolver, Holder: "test", Log: slog.New(slog.DiscardHandler),
			Every: 5 * time.Millisecond, Refresh: 5 * time.Millisecond,
			Sinks: []sinks.Sink{{Name: "new", Selection: sinks.SelectionOf(cfg[0]), Sender: &sinks.JSONL{Path: "-", Stdout: &out}}}}
		c, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		m.Run(c)
	}
	run() // listed a minute ago: kept
	if e, _ := st.GetEvent(ctx, acme.ID, ev); e.Pending != 1 {
		t.Fatalf("a sink removed recently keeps its bit: %+v", e)
	}
	now = now.Add(11 * time.Minute)
	run()
	if e, _ := st.GetEvent(ctx, acme.ID, ev); e.Pending != 0 {
		t.Fatalf("a sink no replica lists loses its bit: %+v", e)
	}
	reg, err := st.RegisterSinks(ctx, []string{"new"})
	if err != nil || len(reg) != 1 || reg[0].Name != "new" {
		t.Fatalf("old is forgotten: %+v %v", reg, err)
	}
}

func TestOTLPRefusesRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	o := &sinks.OTLP{URL: srv.URL, Client: loopbackClient(t, srv)}
	err := o.Send(ctx, []sinks.Record{{ID: "x", TenantID: audit.ServerTenant, Kind: "server.start", Outcome: audit.OK, Data: json.RawMessage(`{}`)}})
	var se *sinks.StatusError
	if !errors.As(err, &se) || se.Status != http.StatusTemporaryRedirect {
		t.Fatalf("a redirect: %v", err)
	}
}

func TestJSONLRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.jsonl")
	j := &sinks.JSONL{Path: path, MaxSize: 300, Keep: 2}
	defer j.Close()
	for i := range 20 {
		if err := j.Send(ctx, []sinks.Record{{ID: strconv.Itoa(i), Kind: "grant.add", Data: json.RawMessage(`{}`)}}); err != nil {
			t.Fatal(err)
		}
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 || st.Size() > 300 {
		t.Fatalf("the file: %v %v", st, err)
	}
	for _, p := range []string{path + ".1", path + ".2"} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("kept: %v", err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("beyond keep: %v", err)
	}
	if _, ok := jsonlIDs(t, path)["19"]; !ok {
		t.Fatal("the newest line is in the file")
	}
}

// fakeSender records batches and refuses those larger than max with 413, and any holding refuse.
type fakeSender struct {
	mu     sync.Mutex
	max    int
	refuse string // an event id refused alone with 400
	got    []sinks.Record
}

func (f *fakeSender) Send(_ context.Context, rs []sinks.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(rs) > f.max {
		return &sinks.StatusError{Status: http.StatusRequestEntityTooLarge}
	}
	for _, r := range rs {
		if r.ID == f.refuse {
			return &sinks.StatusError{Status: http.StatusBadRequest}
		}
	}
	f.got = append(f.got, rs...)
	return nil
}

func (f *fakeSender) Close() error { return nil }

func (f *fakeSender) records() []sinks.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

// A collector that refuses large batches gets smaller ones; an event it refuses alone is dropped
// and counted, never blocking the sink. Server identities on tenants' events are masked.
func TestRefusedBatchesAndMasking(t *testing.T) {
	st := openStore(t)
	acme := tenant(t, st, "acme")
	cfg := []config.Sink{{Name: "s", Kind: "jsonl", Path: "-", Tenants: []string{"acme"}}}
	resolver, err := sinks.Attach(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := range 20 {
		ids = append(ids, event(t, st, acme.ID, "grant:"+strconv.Itoa(i)))
	}
	if err := st.InTx(ctx, "", func(tx *store.Tx) error {
		return tx.Event(audit.WithRequest(ctx, audit.Request{Client: "198.51.100.7", ActorName: "Root"}), acme.ID,
			"server:corp|sub:root", "grant.add", "grant:admin", map[string]any{"principal": "p"})
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakeSender{max: 3, refuse: ids[7]}
	m := &sinks.Manager{Store: st, Resolver: resolver, Holder: "test", Log: slog.New(slog.DiscardHandler), Every: time.Millisecond,
		MaxBackoff: time.Millisecond, Sinks: []sinks.Sink{{Name: "s", Selection: sinks.SelectionOf(cfg[0]), Sender: f}}}
	c, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { m.Run(c); close(done) }()
	// 20 events; the refused one's audit.dropped is never sent to the sink that refused it
	waitFor(t, "delivery", func() bool { return len(f.records()) == 20 })
	cancel()
	<-done
	got := map[string]sinks.Record{}
	for _, r := range f.records() {
		got[r.ID] = r
	}
	if _, ok := got[ids[7]]; ok || len(got) != 20 {
		t.Fatalf("the refused event: %d records", len(got))
	}
	drops, _ := st.ListEvents(ctx, acme.ID, store.EventFilter{Kind: "audit.dropped"})
	if len(drops) != 1 || !strings.Contains(drops[0].Data, `"sink":"s"`) {
		t.Fatalf("audit.dropped: %+v", drops)
	}
	for _, r := range got {
		if r.Subject == "grant:admin" && (r.Actor != "server" || r.ActorName != "" || r.Client != "") {
			t.Fatalf("a server identity reached a tenant's sink: %+v", r)
		}
	}
}

// A bit that passes from a removed sink to a new one carries stale events of tenants the new sink
// does not take: they never reach it. A late commit (an earlier at, written after delivery passed
// it) is delivered, not skipped.
func TestReusedBitAndLateCommit(t *testing.T) {
	st := openStore(t)
	acme, beta := tenant(t, st, "acme"), tenant(t, st, "beta")
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	st.Now = func() time.Time { return now }
	if _, err := sinks.Attach(ctx, st, []config.Sink{{Name: "a", Kind: "jsonl", Path: "-", Tenants: []string{"acme"}}}); err != nil {
		t.Fatal(err)
	}
	stale := event(t, st, acme.ID, "grant:stale")
	// a's bit freed without its events cleared (the worst case), then taken by b
	if ok, err := st.RetireSink(ctx, "a", now.Add(time.Second)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := st.DropSink(ctx, "~removing:0"); err != nil {
		t.Fatal(err)
	}
	cfg := []config.Sink{{Name: "b", Kind: "jsonl", Path: "-", Tenants: []string{"beta"}}}
	resolver, err := sinks.Attach(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if reg, _ := st.RegisterSinks(ctx, []string{"b"}); len(reg) != 1 || reg[0].Bit != 0 {
		t.Fatalf("b takes bit 0: %+v", reg)
	}
	now = now.Add(time.Minute)
	betaEv := event(t, st, beta.ID, "grant:b1")
	f := &fakeSender{max: 500}
	m := &sinks.Manager{Store: st, Resolver: resolver, Holder: "test", Log: slog.New(slog.DiscardHandler), Every: time.Millisecond,
		Sinks: []sinks.Sink{{Name: "b", Selection: sinks.SelectionOf(cfg[0]), Sender: f}}}
	c, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { m.Run(c); close(done) }()
	defer func() { cancel(); <-done }()
	has := func(id string) bool {
		return slices.ContainsFunc(f.records(), func(r sinks.Record) bool { return r.ID == id })
	}
	waitFor(t, "beta's event", func() bool { return has(betaEv) })
	// a transaction that commits late: its at is before the delivered event's
	now = now.Add(-30 * time.Second)
	late := event(t, st, beta.ID, "grant:late")
	waitFor(t, "the late commit", func() bool { return has(late) })
	if has(stale) {
		t.Fatal("a stale bit carried acme's event to a sink that takes only beta")
	}
	waitFor(t, "the stale bit cleared", func() bool {
		e, err := st.GetEvent(ctx, acme.ID, stale)
		return err == nil && e.Pending == 0
	})
}

// An endpoint that refuses everything as malformed is failing: a few events are dropped, then the
// sink backs off and reports it; drops never feed themselves.
func TestRefusingEverythingIsFailing(t *testing.T) {
	st := openStore(t)
	acme := tenant(t, st, "acme")
	cfg := []config.Sink{{Name: "s", Kind: "jsonl", Path: "-", Tenants: []string{"acme"}}}
	resolver, err := sinks.Attach(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		event(t, st, acme.ID, "grant:"+strconv.Itoa(i))
	}
	f := &fakeSender{max: 0} // every batch: 413
	m := &sinks.Manager{Store: st, Resolver: resolver, Holder: "test", Log: slog.New(slog.DiscardHandler), Every: time.Millisecond,
		MaxBackoff: 50 * time.Millisecond, Report: time.Millisecond,
		Sinks: []sinks.Sink{{Name: "s", Selection: sinks.SelectionOf(cfg[0]), Sender: f}}}
	c, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { m.Run(c); close(done) }()
	waitFor(t, "audit.sink", func() bool {
		evs, _ := st.ListEvents(ctx, audit.ServerTenant, store.EventFilter{Kind: "audit.sink"})
		return len(evs) > 0 && evs[0].Outcome == audit.Failed
	})
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	drops, _ := st.ListEvents(ctx, acme.ID, store.EventFilter{Kind: "audit.dropped"})
	if len(drops) != 3 {
		t.Fatalf("drops: %d", len(drops))
	}
	for _, d := range drops {
		if d.Pending != 0 {
			t.Fatalf("a drop record carries the refusing sink's bit: %+v", d)
		}
	}
	// the endpoint is fixed and serve restarted: the new process reports the recovery
	f.mu.Lock()
	f.max = 500
	f.mu.Unlock()
	m2 := &sinks.Manager{Store: st, Resolver: resolver, Holder: "test2", Log: slog.New(slog.DiscardHandler), Every: time.Millisecond,
		Report: time.Millisecond, Sinks: []sinks.Sink{{Name: "s", Selection: sinks.SelectionOf(cfg[0]), Sender: f}}}
	c, cancel = context.WithCancel(ctx)
	done = make(chan struct{})
	go func() { m2.Run(c); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "the recovery", func() bool {
		evs, _ := st.ListEvents(ctx, audit.ServerTenant, store.EventFilter{Kind: "audit.sink"})
		return len(evs) > 0 && evs[0].Outcome == audit.OK
	})
	if n := len(f.records()); n != 7 {
		t.Fatalf("the events that waited: %d", n)
	}
}
