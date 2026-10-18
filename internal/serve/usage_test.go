package serve

import (
	"bytes"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

type fakeUsage struct {
	mu     sync.Mutex
	counts map[store.UsageKey]int64
	full   bool
	drops  int
}

func (f *fakeUsage) Drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drops++
}

func (f *fakeUsage) AddAll(m map[store.UsageKey]int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.full {
		return false
	}
	for k, n := range m {
		f.counts[k] += n
	}
	return true
}

// get sums the counts of a name's version and event (counter "" for the event's own count) by
// whether the reporter was authenticated.
func (f *fakeUsage) get(version, event, counter string, authenticated bool) (n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, c := range f.counts {
		if k.ExtVersion == version && k.Event == event && k.Counter == counter && k.Authenticated == authenticated {
			n += c
		}
	}
	return n
}

func (en *env) post(t *testing.T, path string, hdr map[string]string, body string) answer {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, en.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	resp.Header.Del("Date")
	return answer{resp.StatusCode, resp.Header, b}
}

func report(version string) string {
	return `{"version": "` + version + `", "platform": "linux_amd64", "duckdb_version": "v2.0.0", "license": "lic-1", "status": "valid",
		"events": [{"name": "load", "count": 1}, {"name": "attach", "count": 2, "counters": {"sources": 5}}]}`
}

// Spec 0012 phase 1b: who may report is decided from the path as the licence route decides; a
// report is counted per key, and a principal holding install makes one extension.use a day.
func TestUsageReports(t *testing.T) {
	en, idp, adm := authEnv(t, "https")
	en.add(t, ext(t, 2000, 1, cpp("1.0")), release.AddOptions{Unchecked: true, Name: "tresor"})
	en.add(t, ext(t, 2000, 2, cpp("1.1")), release.AddOptions{Unchecked: true, Name: "tresor", Private: true})
	en.add(t, ext(t, 2000, 3, cpp("1.0")), release.AddOptions{Unchecked: true, Name: "secret", Private: true})
	evs, u := &fakeEvents{}, &fakeUsage{counts: map[store.UsageKey]int64{}}
	en.h.o.Events, en.h.o.Usage = evs, u
	tok := idp.token(t, nil)
	const (
		binMiss = "/acme/prod/tresor/9.9/v2.0.0/linux_amd64/tresor.duckdb_extension.gz"
		tresor  = "/acme/prod/-/usage/tresor"
		secret  = "/acme/prod/-/usage/secret"
	)
	same := func(name string, a, b answer) {
		t.Helper()
		if a.status != b.status || !bytes.Equal(a.body, b.body) || !sameHeaders(a.header, b.header) {
			t.Fatalf("%s: %d %q %v, want %d %q %v", name, a.status, a.body, a.header, b.status, b.body, b.header)
		}
	}
	noContent := func(name string, a answer) {
		t.Helper()
		if a.status != 204 || len(a.body) != 0 || a.header.Get("Cache-Control") != "private, no-store" {
			t.Fatalf("%s: %d %q %v", name, a.status, a.body, a.header)
		}
	}

	// a private-only name: as a missing path for each caller, before the body is read
	same("anonymous, a private name", en.post(t, secret, nil, report("1.0")), en.do(t, "GET", binMiss, nil))
	same("a token without install, a private name", en.post(t, secret, bearerHdr(tok), "not json"), en.do(t, "GET", binMiss, bearerHdr(tok)))
	if !slices.ContainsFunc(evs.events, func(e string) bool {
		return strings.HasPrefix(e, "authz.refused ") && strings.HasSuffix(e, " route:usage")
	}) {
		t.Fatalf("no refusal recorded: %q", evs.events)
	}
	same("anonymous, an unknown name", en.post(t, "/acme/prod/-/usage/nothing", nil, report("1.0")), en.do(t, "GET", binMiss, nil))
	if u.get("1.0", "load", "", false)+u.get("1.0", "load", "", true) != 0 {
		t.Fatal("a refused report counted")
	}

	// a public release: anyone reports; anonymous and a token without install count, make no event
	noContent("anonymous, public", en.post(t, tresor, nil, report("1.0")))
	noContent("a token without install, public", en.post(t, tresor, bearerHdr(tok), report("1.0")))
	if u.get("1.0", "load", "", false) != 1 || u.get("1.0", "load", "", true) != 1 || u.get("1.0", "attach", "sources", false) != 5 {
		t.Fatalf("counts: %v", u.counts)
	}
	// a version the caller is not served (1.1 is private), an unknown version, a bad platform: dropped
	noContent("anonymous, a private version", en.post(t, tresor, nil, report("1.1")))
	noContent("an unknown version", en.post(t, tresor, nil, report("7.0")))
	noContent("a bad platform", en.post(t, tresor, nil, strings.Replace(report("1.0"), "linux_amd64", "Linux AMD64", 1)))
	if u.get("1.1", "load", "", false) != 0 || u.get("7.0", "load", "", false) != 0 || u.get("1.0", "load", "", false) != 1 {
		t.Fatalf("dropped reports counted: %v", u.counts)
	}
	for _, e := range evs.events {
		if strings.HasPrefix(e, "extension.use") {
			t.Fatalf("an event without install: %q", evs.events)
		}
	}

	// install on tresor: every version; one event a (principal, version, day)
	if _, err := adm.AddGrant(ctx, admin, "acme", "subject:corp|alice", []string{"install"}, tenants.GrantOn{Extension: "tresor"}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"1.1", "1.1", "1.0"} {
		noContent("install, "+v, en.post(t, tresor, bearerHdr(tok), report(v)))
	}
	if u.get("1.1", "load", "", true) != 2 {
		t.Fatalf("counts: %v", u.counts)
	}
	var uses []map[string]any
	for i, e := range evs.events {
		if strings.HasPrefix(e, "extension.use ") {
			if !strings.HasSuffix(e, " channel:prod/ext:tresor") {
				t.Fatalf("subject: %q", e)
			}
			uses = append(uses, evs.data[i])
		}
	}
	if len(uses) != 2 || uses[0]["version"] != "1.1" || uses[1]["version"] != "1.0" || uses[0]["license"] != "lic-1" ||
		uses[0]["status"] != "valid" || strings.Join(uses[0]["events"].([]string), ",") != "attach,load" {
		t.Fatalf("extension.use: %v", uses)
	}

	// the next day: an event again
	en.st.Now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	noContent("install, the next day", en.post(t, tresor, bearerHdr(tok), report("1.0")))
	en.st.Now = time.Now
	if n := len(slices.DeleteFunc(slices.Clone(evs.events), func(e string) bool { return !strings.HasPrefix(e, "extension.use ") })); n != 3 {
		t.Fatalf("extension.use the next day: %d", n)
	}

	// the body's rules, after authorisation
	for _, c := range []struct {
		name, ctype, body string
		status            int
		rule              string
	}{
		{"not JSON", "application/json", "{", 400, "json"},
		{"a bad event name", "application/json", strings.Replace(report("1.0"), `"load"`, `"Load"`, 1), 400, "name"},
		{"text", "text/plain", report("1.0"), 415, "content"},
		{"over 64 KiB", "application/json", report("1.0") + strings.Repeat(" ", 64<<10), 413, "size"},
	} {
		a := en.post(t, tresor, map[string]string{"Content-Type": c.ctype}, c.body)
		if a.status != c.status || !strings.HasPrefix(string(a.body), c.rule+":") {
			t.Errorf("%s: %d %q", c.name, a.status, a.body)
		}
	}
	// a version's distinct keys a day are bounded: a report inventing more is dropped, and counted
	en.h.usageKeys.Max = 5 // load, attach, attach.sources (valid) are in
	noContent("new names over the keys' budget", en.post(t, tresor, nil, strings.Replace(report("1.0"), `"load"`, `"x1"`, 1)+""))
	noContent("new names over the keys' budget", en.post(t, tresor, nil, strings.NewReplacer(`"load"`, `"x2"`, `"attach"`, `"x3"`).Replace(report("1.0"))))
	if u.get("1.0", "x2", "", false) != 0 || u.get("1.0", "x1", "", false) != 1 || u.drops != 1 {
		t.Fatalf("the keys' budget: %v drops %d", u.counts, u.drops)
	}
	// a report over usage's budget is dropped, and answered the same
	u.full = true
	noContent("over the budget", en.post(t, tresor, nil, report("1.0")))
	u.full = false

	// methods: the usage route takes POST only; the others never POST
	if a := en.do(t, "GET", tresor, nil); a.status != 405 || a.header.Get("Allow") != "POST" {
		t.Fatalf("GET: %d %v", a.status, a.header)
	}
	if a := en.post(t, "/acme/prod/-/licenses/tresor", nil, "{}"); a.status != 405 || a.header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST a licence: %d %v", a.status, a.header)
	}
	// a passthrough channel takes no reports; a bad name is no route
	if a := en.post(t, "/acme/mirror/-/usage/tresor", nil, report("1.0")); a.status != 404 {
		t.Fatalf("passthrough: %d", a.status)
	}
	if a := en.post(t, "/acme/prod/-/usage/Bad", nil, report("1.0")); a.status != 404 {
		t.Fatalf("a bad name: %d", a.status)
	}

	// statistics.usage_accept false: answered, nothing kept, no event
	en.h.o.Usage = nil
	n := len(evs.events)
	noContent("not accepted", en.post(t, tresor, bearerHdr(tok), `{"anything": true}`))
	if len(evs.events) != n {
		t.Fatal("an event while usage is not accepted")
	}
}

// Over plain http a token is ignored: only public releases' reports.
func TestUsageOverHTTP(t *testing.T) {
	en, idp, adm := authEnv(t, "http")
	en.add(t, ext(t, 2000, 1, cpp("1.0")), release.AddOptions{Unchecked: true, Name: "secret", Private: true})
	en.h.o.Usage = &fakeUsage{counts: map[store.UsageKey]int64{}}
	if _, err := adm.AddGrant(ctx, admin, "acme", "subject:corp|alice", []string{"install"}, tenants.GrantOn{Extension: "secret"}); err != nil {
		t.Fatal(err)
	}
	if a := en.post(t, "/acme/prod/-/usage/secret", bearerHdr(idp.token(t, nil)), report("1.0")); a.status != 401 {
		t.Fatalf("a private name over http with a token: %d", a.status)
	}
}
