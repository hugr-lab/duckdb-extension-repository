package bundle_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
)

const feedURL = "https://gw.example.org/api/v1/tenants/reviews"

// feedFixture adds a feed to the fixture: its keys file, two batches, a document after each.
func feedFixture(t *testing.T) (*fixture, map[string][]byte) {
	t.Helper()
	f := newFixture(t)
	data := map[string][]byte{}
	ref := func(path string, b []byte) bundle.FileRef {
		data[path] = b
		return bundle.FileRef{Path: path, SHA256: sha(b), Size: int64(len(b))}
	}
	batch := func(n int) []byte {
		b, err := bundle.MarshalBatch([]bundle.BatchEntry{{Statement: "e30=", Signature: "c2ln", Note: map[bool]string{true: "kind on the exporting kista"}[n == 2]}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	doc1, doc2 := []byte(`{"verdict":"approved"}`), []byte(`{"bomFormat":"CycloneDX"}`)
	f.m.Feeds = []bundle.Feed{{URL: feedURL, Entries: []bundle.FileRef{
		ref("feeds/1/attestation-keys.json", []byte(`{"keys":[]}`)),
		ref("feeds/1/batch-1.json", batch(1)),
		ref("feeds/1/documents/"+sha(doc1), doc1),
		ref("feeds/1/batch-2.json", batch(2)),
		ref("feeds/1/documents/"+sha(doc2), doc2),
	}, Bodies: []bundle.FeedBody{{BodyHash: strings.Repeat("a", 64), AfterSeq: 3, Seq: 9}}}}
	return f, data
}

// writeFeeds writes the fixture's bundle with its feed entries in the order given.
func writeFeeds(t *testing.T, f *fixture, data map[string][]byte, priv ed25519.PrivateKey, order []string) []byte {
	t.Helper()
	var out bytes.Buffer
	w := bundle.NewWriter(&out)
	if err := bundle.WriteHead(w, f.manifest(t), priv); err != nil {
		t.Fatal(err)
	}
	if err := w.Entry("sources/1/well-known.json", int64(len(f.wk)), bytes.NewReader(f.wk)); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.m.FileOrder() {
		if err := w.Entry(p, int64(len(f.files[p])), bytes.NewReader(f.files[p])); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range order {
		b := data[p]
		if b == nil {
			b = f.files[p]
		}
		if err := w.Entry(p, int64(len(b)), bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type feedRead struct {
	paths   []string
	damaged string
	whole   bool
}

func readFeeds(t *testing.T, raw []byte) (feedRead, []string, error) {
	t.Helper()
	b, err := bundle.Open(bytes.NewReader(raw), t0, nil)
	if err != nil {
		return feedRead{}, nil, err
	}
	var r feedRead
	for {
		it, err := b.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return r, nil, err
		}
		if it.R != nil {
			io.Copy(io.Discard, it.R)
		}
		if it.Feed != nil {
			r.paths = append(r.paths, it.Feed.Entries[it.Entry].Path)
			if it.Damaged != "" && r.damaged == "" {
				r.damaged = it.Damaged
			}
			if it.Damaged == "" && it.Data == nil {
				t.Fatalf("%s: no data", it.Feed.Entries[it.Entry].Path)
			}
		}
	}
	r.whole = b.FeedWhole(0)
	return r, b.Missing(), nil
}

// Spec 0014 phase 1b: a feed's entries come after the files, in the manifest's order; a missing,
// misplaced or altered entry fails that feed only; a file or source after a feed entry, or an entry
// past every limit, refuses the bundle.
func TestFeedEntries(t *testing.T) {
	f, data := feedFixture(t)
	if err := f.m.Validate(t0); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, e := range f.m.Feeds[0].Entries {
		order = append(order, e.Path)
	}
	r, missing, err := readFeeds(t, writeFeeds(t, f, data, nil, order))
	if err != nil || r.damaged != "" || !r.whole || len(missing) != 0 || len(r.paths) != 5 {
		t.Fatalf("whole: %+v %v %v", r, missing, err)
	}
	batch, err := bundle.ParseBatch(data["feeds/1/batch-2.json"])
	if err != nil || len(batch) != 1 || batch[0].Note != "kind on the exporting kista" {
		t.Fatalf("batch: %+v %v", batch, err)
	}

	cases := map[string]struct {
		order []string
		alter func(map[string][]byte)
		want  string
	}{
		"a batch missing": {order: []string{order[0], order[2], order[3], order[4]}, want: "batch-1.json is missing"},
		"swapped":         {order: []string{order[0], order[3], order[1], order[2], order[4]}, want: "batch-1.json is missing"},
		"a document altered": {order: order, alter: func(d map[string][]byte) {
			p := order[2]
			d[p] = bytes.ToUpper(d[p])
		}, want: "differs from the manifest's hash"},
		"the keys file shorter": {order: order, alter: func(d map[string][]byte) { d[order[0]] = []byte(`{}`) }, want: "the manifest says"},
	}
	for name, c := range cases {
		d := map[string][]byte{}
		for k, v := range data {
			d[k] = bytes.Clone(v)
		}
		if c.alter != nil {
			c.alter(d)
		}
		r, _, err := readFeeds(t, writeFeeds(t, f, d, nil, c.order))
		if err != nil || !strings.Contains(r.damaged, c.want) || r.whole {
			t.Errorf("%s: %+v %v", name, r, err)
		}
	}
	// a feed cut short is not whole, and its entries are missing
	r, missing, err = readFeeds(t, writeFeeds(t, f, data, nil, order[:3]))
	if err != nil || r.damaged != "" || r.whole || len(missing) != 2 {
		t.Fatalf("cut short: %+v %v %v", r, missing, err)
	}
	// a file after a feed entry refuses the bundle
	last := f.m.FileOrder()[0]
	if _, _, err := readFeeds(t, writeFeeds(t, f, data, nil, append(slicesClone(order), last))); !errors.Is(err, bundle.ErrMalformed) {
		t.Fatalf("a file after the feeds: %v", err)
	}
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

// A manifest's and a plan's feeds are checked as strictly as their sources.
func TestFeedValidate(t *testing.T) {
	f, _ := feedFixture(t)
	good := f.m
	mod := func(fn func(*bundle.Feed)) bundle.Manifest {
		m := good
		fd := good.Feeds[0]
		fd.Entries = append([]bundle.FileRef(nil), fd.Entries...)
		fd.Bodies = append([]bundle.FeedBody(nil), fd.Bodies...)
		fn(&fd)
		m.Feeds = []bundle.Feed{fd}
		return m
	}
	bad := map[string]bundle.Manifest{
		"an http URL":          mod(func(f *bundle.Feed) { f.URL = "http://gw.example.org/api/v1/tenants/reviews" }),
		"not a tenant URL":     mod(func(f *bundle.Feed) { f.URL = "https://gw.example.org/acme/prod" }),
		"no keys file first":   mod(func(f *bundle.Feed) { f.Entries = f.Entries[1:] }),
		"batches out of order": mod(func(f *bundle.Feed) { f.Entries[1], f.Entries[3] = f.Entries[3], f.Entries[1] }),
		"a document first":     mod(func(f *bundle.Feed) { f.Entries[1], f.Entries[2] = f.Entries[2], f.Entries[1] }),
		"a document misnamed":  mod(func(f *bundle.Feed) { f.Entries[2].SHA256 = strings.Repeat("0", 64) }),
		"another feed's entry": mod(func(f *bundle.Feed) { f.Entries[1].Path = "feeds/2/batch-1.json" }),
		"a batch over 1 MiB":   mod(func(f *bundle.Feed) { f.Entries[1].Size = bundle.MaxBatch + 1 }),
		"seq below after_seq":  mod(func(f *bundle.Feed) { f.Bodies[0].Seq = 2 }),
		"a body twice":         mod(func(f *bundle.Feed) { f.Bodies = append(f.Bodies, f.Bodies[0]) }),
		"an issuer id as name": mod(func(f *bundle.Feed) { f.Entries[2].Path = "feeds/1/documents/../../x" }),
	}
	for name, m := range bad {
		if err := m.Validate(t0); !errors.Is(err, bundle.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	two := good
	two.Feeds = []bundle.Feed{good.Feeds[0], good.Feeds[0]}
	if err := two.Validate(t0); !errors.Is(err, bundle.ErrInvalid) {
		t.Errorf("a URL twice: %v", err)
	}

	p := bundle.Plan{Format: bundle.PlanFormat, CreatedAt: t0, Tenant: "acme", BundleKeys: []string{"sha256:" + strings.Repeat("1", 64)},
		Sources: []bundle.PlanSource{{Kind: "duckdb-core", Cells: []bundle.PlanCell{}}},
		Feeds: []bundle.PlanFeed{{URL: feedURL, Keys: []string{"sha256:" + strings.Repeat("2", 64)}, Kinds: []string{"review", "sbom"},
			Sources: []int{0}, Bodies: []bundle.PlanBody{{BodyHash: strings.Repeat("a", 64), Seq: 4}}}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	var back bundle.Plan
	if err := bundle.DecodeStrict(b, &back); err != nil || back.Validate() != nil || len(back.Feeds) != 1 {
		t.Fatalf("round trip: %v", err)
	}
	pmod := func(fn func(*bundle.PlanFeed)) bundle.Plan {
		q := p
		fd := p.Feeds[0]
		fd.Bodies = append([]bundle.PlanBody(nil), fd.Bodies...)
		fn(&fd)
		q.Feeds = []bundle.PlanFeed{fd}
		return q
	}
	for name, q := range map[string]bundle.Plan{
		"no key":          pmod(func(f *bundle.PlanFeed) { f.Keys = nil }),
		"an unknown kind": pmod(func(f *bundle.PlanFeed) { f.Kinds = []string{"gossip"} }),
		"kinds unsorted":  pmod(func(f *bundle.PlanFeed) { f.Kinds = []string{"sbom", "review"} }),
		"a source past":   pmod(func(f *bundle.PlanFeed) { f.Sources = []int{1} }),
		"a negative seq":  pmod(func(f *bundle.PlanFeed) { f.Bodies[0].Seq = -1 }),
	} {
		if err := q.Validate(); !errors.Is(err, bundle.ErrInvalid) {
			t.Errorf("plan %s: %v", name, err)
		}
	}
}

// Two feeds: an earlier feed's entry after a later feed's damages the earlier one only; entries of
// the wrong size, however many, damage their feed and never refuse the bundle; an entry over 16 MiB
// refuses it.
func TestFeedEntriesAcrossFeeds(t *testing.T) {
	f, data := feedFixture(t)
	second := bundle.Feed{URL: "https://gw.example.org/api/v1/tenants/scans", Bodies: []bundle.FeedBody{}}
	for _, e := range f.m.Feeds[0].Entries[:2] {
		p := strings.Replace(e.Path, "feeds/1/", "feeds/2/", 1)
		data[p] = data[e.Path]
		second.Entries = append(second.Entries, bundle.FileRef{Path: p, SHA256: e.SHA256, Size: e.Size})
	}
	f.m.Feeds = append(f.m.Feeds, second)
	if err := f.m.Validate(t0); err != nil {
		t.Fatal(err)
	}
	read := func(order []string, d map[string][]byte) ([]string, []bool, error) {
		t.Helper()
		b, err := bundle.Open(bytes.NewReader(writeFeeds(t, f, d, nil, order)), t0, nil)
		if err != nil {
			return nil, nil, err
		}
		damaged := map[int]string{}
		for {
			it, err := b.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, nil, err
			}
			if it.R != nil {
				io.Copy(io.Discard, it.R)
			}
			if it.Feed != nil && it.Damaged != "" && damaged[it.FeedIndex] == "" {
				damaged[it.FeedIndex] = it.Damaged
			}
		}
		return []string{damaged[0], damaged[1]}, []bool{b.FeedWhole(0), b.FeedWhole(1)}, nil
	}
	one, two := f.m.Feeds[0].Entries, f.m.Feeds[1].Entries
	paths := func(es ...bundle.FileRef) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.Path)
		}
		return out
	}
	// in order: both whole
	order := append(paths(one...), paths(two...)...)
	if dmg, whole, err := read(order, data); err != nil || dmg[0] != "" || dmg[1] != "" || !whole[0] || !whole[1] {
		t.Fatalf("in order: %v %v %v", dmg, whole, err)
	}
	// the first feed's last entry after the second feed's: the first only is damaged
	order = append(append(paths(one[:4]...), paths(two...)...), one[4].Path)
	if dmg, whole, err := read(order, data); err != nil || !strings.Contains(dmg[0], "after a later feed") || dmg[1] != "" || whole[0] || !whole[1] {
		t.Fatalf("interleaved: %v %v %v", dmg, whole, err)
	}
	// every entry of the first feed a byte short: damaged, the bundle still read
	short := map[string][]byte{}
	for k, v := range data {
		short[k] = v
		if strings.HasPrefix(k, "feeds/1/") {
			short[k] = v[:len(v)-1]
		}
	}
	order = append(paths(one...), paths(two...)...)
	if dmg, whole, err := read(order, short); err != nil || !strings.Contains(dmg[0], "the manifest says") || !whole[1] {
		t.Fatalf("short entries: %v %v %v", dmg, whole, err)
	}
	// an entry over 16 MiB refuses the bundle
	big := map[string][]byte{}
	for k, v := range data {
		big[k] = v
	}
	big[one[2].Path] = make([]byte, bundle.MaxDocument+1)
	if _, _, err := read(order, big); !errors.Is(err, bundle.ErrMalformed) {
		t.Fatalf("an entry over 16 MiB: %v", err)
	}
}
