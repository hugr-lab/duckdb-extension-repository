package bundle_test

import (
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/bundle"
)

var t0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func gz(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write(data)
	w.Close()
	return b.Bytes()
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type fixture struct {
	m     bundle.Manifest
	wk    []byte
	files map[string][]byte // path: the gzip entry
}

// newFixture is a manifest with a repository source carrying two files and a core source with an
// unchanged and a rejected cell.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{wk: []byte(`{"signature_keys":[]}`), files: map[string][]byte{}}
	cell := func(v, n string, body []byte) bundle.Cell {
		z := gz(t, body)
		p := "files/" + sha(body) + ".gz"
		f.files[p] = z
		return bundle.Cell{DuckDBVersion: v, Platform: "linux_amd64", Name: n, Outcome: bundle.Fetched, File: p, SHA256: sha(body),
			Size: int64(len(z)), InflatedSize: int64(len(body)), BodyHash: strings.Repeat("a", 64), FetchedAt: t0}
	}
	f.m = bundle.Manifest{Format: bundle.ManifestFormat, ID: "0192f0c4-1b2a-7c3d-8e4f-0123456789ab", CreatedAt: t0,
		Exporter: bundle.Exporter{Version: "kista v0.3.0", PublicURL: "https://gw.example.org"},
		Plan:     bundle.PlanRef{Tenant: "acme", CreatedAt: t0.Add(-time.Hour), SHA256: strings.Repeat("b", 64)},
		Sources: []bundle.Source{
			{Kind: "duckdb-core", Cells: []bundle.Cell{
				{DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "httpfs", Outcome: bundle.Unchanged, BodyHash: strings.Repeat("c", 64), FetchedAt: t0},
				{DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "json", Outcome: bundle.Rejected, Detail: "withdrawn on the exporting kista"},
			}},
			{Kind: "repository", Prefix: "https://src.example.org/acme/prod",
				WellKnown: &bundle.FileRef{Path: "sources/1/well-known.json", SHA256: sha(f.wk), Size: int64(len(f.wk))},
				Cells:     []bundle.Cell{cell("v2.0.0", "acl", bytes.Repeat([]byte("A"), 600)), cell("v2.0.0", "tresor", bytes.Repeat([]byte("T"), 700))}},
		}}
	return f
}

func (f *fixture) manifest(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(f.m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// write writes the bundle, signed with priv (nil: unsigned); extra entries are written after the
// files.
func (f *fixture) write(t *testing.T, priv ed25519.PrivateKey) ([]byte, string) {
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
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), w.SHA256()
}

func key(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	p, fp, err := bundle.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := bundle.ParsePrivateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	return priv, fp
}

func readAll(t *testing.T, b *bundle.Bundle) (wk, files []string, err error) {
	t.Helper()
	for {
		it, err := b.Next()
		if err != nil {
			return wk, files, err
		}
		if it.R != nil {
			if _, err := io.Copy(io.Discard, it.R); err != nil {
				return wk, files, err
			}
		}
		if it.WellKnown != nil {
			wk = append(wk, it.WellKnown.Prefix)
		} else {
			files = append(files, it.File)
		}
	}
}

func TestKeysAndSignature(t *testing.T) {
	priv, fp := key(t)
	pub := priv.Public().(ed25519.PublicKey)
	if bundle.Fingerprint(pub) != fp || !strings.HasPrefix(fp, "sha256:") {
		t.Fatalf("fingerprint %s", fp)
	}
	pemPub := bundle.MarshalPublicKey(pub)
	if p, err := bundle.ParsePublicKey(append([]byte("\n"), pemPub...)); err != nil || bundle.Fingerprint(p) != fp {
		t.Fatalf("parse: %v", err)
	}
	// the fingerprint is the parsed key's: a PEM with other line breaks gives the same one
	reflowed := strings.ReplaceAll(string(pemPub), "\n", "\r\n")
	if p, err := bundle.ParsePublicKey([]byte(reflowed)); err != nil || bundle.Fingerprint(p) != fp {
		t.Fatalf("reflowed PEM: %v", err)
	}
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&rk.PublicKey)
	if _, err := bundle.ParsePublicKey(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})); err == nil {
		t.Error("an RSA public key was accepted")
	}
	if _, err := bundle.ParsePublicKey(append(pemPub, []byte("junk")...)); err == nil {
		t.Error("trailing data after the key was accepted")
	}
	m := []byte(`{"x":1}`)
	sig := bundle.Sign(priv, m)
	if len(sig) != 64 || bundle.Verify(pub, m, sig) != nil {
		t.Fatal("verify")
	}
	if bundle.Verify(pub, []byte(`{"x":2}`), sig) == nil || bundle.Verify(pub, m, sig[:63]) == nil {
		t.Error("a bad signature verified")
	}
	// the context prefix: a plain Ed25519 signature of the bytes does not verify
	if bundle.Verify(pub, m, ed25519.Sign(priv, m)) == nil {
		t.Error("a signature without the context verified")
	}
}

func TestDecodeStrict(t *testing.T) {
	var v struct {
		A int `json:"a"`
		B struct {
			C int `json:"c"`
		} `json:"b"`
	}
	for doc, ok := range map[string]bool{
		`{"a":1,"b":{"c":2}}`:            true,
		`{"a":1,"b":{"c":2},"z":3}`:      false, // unknown field
		`{"a":1,"a":2}`:                  false,
		`{"a":1,"b":{"c":2,"c":3}}`:      false, // nested duplicate
		`{"a":1,"b":{"c":2}} {}`:         false, // trailing data
		`[{"a":1},{"a":1}]`:              false, // an array: not the struct
		`{"a":1,"b":{"c":2}}` + "\n\t  ": true,
	} {
		if err := bundle.DecodeStrict([]byte(doc), &v); (err == nil) != ok {
			t.Errorf("%s: %v", doc, err)
		}
	}
	// keys equal in two sibling objects are not duplicates
	var w []map[string]int
	if err := bundle.DecodeStrict([]byte(`[{"a":1},{"a":2}]`), &w); err != nil {
		t.Error(err)
	}
}

func TestManifestValidate(t *testing.T) {
	f := newFixture(t)
	if err := f.m.Validate(t0); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(m *bundle.Manifest){
		"a format":                 func(m *bundle.Manifest) { m.Format = "kista-bundle/v2" },
		"a UUIDv4":                 func(m *bundle.Manifest) { m.ID = "0192f0c4-1b2a-4c3d-8e4f-0123456789ab" },
		"in the future":            func(m *bundle.Manifest) { m.CreatedAt = t0.Add(11 * time.Minute) },
		"a control character":      func(m *bundle.Manifest) { m.Exporter.Version = "kista\x1b[2J" },
		"a long exporter":          func(m *bundle.Manifest) { m.Exporter.PublicURL = strings.Repeat("x", 257) },
		"a bad plan hash":          func(m *bundle.Manifest) { m.Plan.SHA256 = "x" },
		"sources out of order":     func(m *bundle.Manifest) { m.Sources[0], m.Sources[1] = m.Sources[1], m.Sources[0] },
		"a source twice":           func(m *bundle.Manifest) { m.Sources[1].Kind, m.Sources[1].Prefix = "duckdb-core", "" },
		"an http prefix":           func(m *bundle.Manifest) { m.Sources[1].Prefix = "http://src.example.org/acme/prod" },
		"a trailing slash":         func(m *bundle.Manifest) { m.Sources[1].Prefix += "/" },
		"no .well-known":           func(m *bundle.Manifest) { m.Sources[1].WellKnown = nil },
		"a core .well-known":       func(m *bundle.Manifest) { m.Sources[0].WellKnown = m.Sources[1].WellKnown },
		"a .well-known path":       func(m *bundle.Manifest) { m.Sources[1].WellKnown.Path = "sources/../x/well-known.json" },
		"a cell twice":             func(m *bundle.Manifest) { m.Sources[0].Cells[1].Name = "httpfs" },
		"an unknown outcome":       func(m *bundle.Manifest) { m.Sources[0].Cells[1].Outcome = "lost" },
		"a file on a missing cell": func(m *bundle.Manifest) { m.Sources[0].Cells[1].File = m.Sources[1].Cells[0].File },
		"a file path":              func(m *bundle.Manifest) { m.Sources[1].Cells[0].File = "files/../x.gz" },
		"a file not its hash":      func(m *bundle.Manifest) { m.Sources[1].Cells[0].SHA256 = strings.Repeat("e", 64) },
		"no fetched_at":            func(m *bundle.Manifest) { m.Sources[1].Cells[0].FetchedAt = time.Time{} },
		"fetched after the bundle": func(m *bundle.Manifest) { m.Sources[1].Cells[0].FetchedAt = m.CreatedAt.Add(time.Second) },
		"a padded gzip entry": func(m *bundle.Manifest) {
			m.Sources[1].Cells[0].Size = bundle.MaxGzip(m.Sources[1].Cells[0].InflatedSize) + 1
		},
		"an authenticated core":  func(m *bundle.Manifest) { m.Sources[0].Cells[0].Authenticated = true },
		"unchanged with a file":  func(m *bundle.Manifest) { m.Sources[0].Cells[0].File = m.Sources[1].Cells[0].File },
		"unchanged without hash": func(m *bundle.Manifest) { m.Sources[0].Cells[0].BodyHash = "" },
		"a long detail":          func(m *bundle.Manifest) { m.Sources[0].Cells[1].Detail = strings.Repeat("d", 1001) },
		"a bad platform":         func(m *bundle.Manifest) { m.Sources[0].Cells[1].Platform = "../x" },
		"one file, two sizes": func(m *bundle.Manifest) {
			c := m.Sources[1].Cells[0]
			c.DuckDBVersion, c.Size = "v2.1.0", c.Size+1
			m.Sources[1].Cells = append(m.Sources[1].Cells, c)
		},
	} {
		g := newFixture(t)
		g.m.Sources[1].WellKnown = &bundle.FileRef{Path: "sources/1/well-known.json", SHA256: sha(g.wk), Size: int64(len(g.wk))}
		change(&g.m)
		if err := g.m.Validate(t0); !errors.Is(err, bundle.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a file shared by two cells with the same sizes (a C_STRUCT build) is one file
	g := newFixture(t)
	c := g.m.Sources[1].Cells[0]
	c.DuckDBVersion = "v2.1.0"
	g.m.Sources[1].Cells = append(g.m.Sources[1].Cells, c)
	if err := g.m.Validate(t0); err != nil || len(g.m.FileOrder()) != 2 {
		t.Fatalf("a shared file: %v %v", err, g.m.FileOrder())
	}
}

func TestPlanValidate(t *testing.T) {
	p := bundle.Plan{Format: bundle.PlanFormat, CreatedAt: t0, Tenant: "acme", BundleKeys: []string{"sha256:" + strings.Repeat("1", 64)},
		Sources: []bundle.PlanSource{
			{Kind: "duckdb-core", Cells: []bundle.PlanCell{{DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "httpfs"}}},
			{Kind: "repository", Prefix: "https://src.example.org/acme/prod", Keys: []string{"sha256:" + strings.Repeat("2", 64)},
				Cells: []bundle.PlanCell{{DuckDBVersion: "v2.0.0", Platform: "linux_amd64", Name: "acl", Versions: []string{"1.0.0"},
					BodyHash: strings.Repeat("a", 64)}}},
		}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(p *bundle.Plan){
		"no bundle key":       func(p *bundle.Plan) { p.BundleKeys = nil },
		"a bad key":           func(p *bundle.Plan) { p.BundleKeys = []string{"sha256:XYZ"} },
		"a repository no key": func(p *bundle.Plan) { p.Sources[1].Keys = nil },
		"a core prefix":       func(p *bundle.Plan) { p.Sources[0].Prefix = "https://x.example.org" },
		"a bad tenant":        func(p *bundle.Plan) { p.Tenant = "Acme!" },
		"a bad version":       func(p *bundle.Plan) { p.Sources[1].Cells[0].Versions = []string{".."} },
		"out of order":        func(p *bundle.Plan) { p.Sources[0], p.Sources[1] = p.Sources[1], p.Sources[0] },
	} {
		q := p
		q.Sources = []bundle.PlanSource{p.Sources[0], p.Sources[1]}
		q.Sources[1].Cells = []bundle.PlanCell{p.Sources[1].Cells[0]}
		change(&q)
		if err := q.Validate(); !errors.Is(err, bundle.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	f := newFixture(t)
	priv, fp := key(t)
	data, wsum := f.write(t, priv)
	b, err := bundle.Open(bytes.NewReader(data), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Signed || b.Fingerprint != fp || b.Check(func(s string) bool { return s == fp }) != nil {
		t.Fatalf("head: %+v", b.Head)
	}
	if b.Check(func(string) bool { return false }) == nil {
		t.Error("an unpinned key was accepted")
	}
	wk, files, err := readAll(t, b)
	if err != io.EOF || len(wk) != 1 || len(files) != 2 || !b.Ended() || b.Skipped() != 0 {
		t.Fatalf("items %v %v: %v", wk, files, err)
	}
	if b.SHA256() != wsum || b.SHA256() != sha(data) {
		t.Errorf("input_sha256 %s, the writer's %s, the bytes' %s", b.SHA256(), wsum, sha(data))
	}
	// unsigned: refused by Check; trailing bytes after the end of archive are never read
	data, _ = f.write(t, nil)
	b, err = bundle.Open(io.MultiReader(bytes.NewReader(data), errReader{}), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Check(func(string) bool { return true }); !errors.Is(err, bundle.ErrUnsigned) {
		t.Errorf("unsigned: %v", err)
	}
	if _, _, err := readAll(t, b); err != io.EOF {
		t.Errorf("read past the end of archive: %v", err)
	}
	// a manifest edited after signing does not verify
	data, _ = f.write(t, priv)
	edited := bytes.Replace(data, []byte(`"tenant":"acme"`), []byte(`"tenant":"acmf"`), 1)
	edited = fixChecksum(t, edited, 0)
	if b, err := bundle.Open(bytes.NewReader(edited), t0, nil); err != nil || b.Check(func(string) bool { return true }) == nil {
		t.Errorf("an edited manifest: %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read after the end of archive") }

// header builds a raw ustar header block.
func header(name string, size int64, typ byte, magic string) []byte {
	h := make([]byte, 512)
	copy(h, name)
	copy(h[100:], "0000644\x00")
	copy(h[124:], fmt.Sprintf("%011o\x00", size))
	h[156] = typ
	copy(h[257:], magic)
	copy(h[148:], "        ")
	var s int
	for _, b := range h {
		s += int(b)
	}
	copy(h[148:], fmt.Sprintf("%06o\x00 ", s))
	return h
}

func entry(name string, typ byte, data []byte) []byte {
	b := append(header(name, int64(len(data)), typ, "ustar\x0000"), data...)
	return append(b, make([]byte, (512-len(data)%512)%512)...)
}

func pax(records map[string]string) []byte {
	var b []byte
	for k, v := range records {
		rec := " " + k + "=" + v + "\n"
		n := len(rec)
		for len(fmt.Sprint(n+len(fmt.Sprint(n)))) != len(fmt.Sprint(n)) {
			n++
		}
		n += len(fmt.Sprint(n))
		b = append(b, fmt.Sprintf("%d%s", n, rec)...)
	}
	return b
}

// fixChecksum recomputes the checksum of the header at offset off after an edit of its entry.
func fixChecksum(t *testing.T, data []byte, off int) []byte {
	t.Helper()
	h := data[off : off+512]
	copy(h[148:], "        ")
	var s int
	for _, b := range h {
		s += int(b)
	}
	copy(h[148:], fmt.Sprintf("%06o\x00 ", s))
	return data
}

// head returns the signed head of a fixture's bundle, and the rest (sources, files, end of archive).
func split(t *testing.T, f *fixture, priv ed25519.PrivateKey) (head, sources, files []byte) {
	t.Helper()
	var hb bytes.Buffer
	w := bundle.NewWriter(&hb)
	if err := bundle.WriteHead(w, f.manifest(t), priv); err != nil {
		t.Fatal(err)
	}
	src := entry("sources/1/well-known.json", '0', f.wk)
	var fl []byte
	for _, p := range f.m.FileOrder() {
		fl = append(fl, entry(p, '0', f.files[p])...)
	}
	return hb.Bytes(), src, fl
}

func TestReaderRefusals(t *testing.T) {
	f := newFixture(t)
	priv, _ := key(t)
	head, src, files := split(t, f, priv)
	eoa := make([]byte, 1024)
	order := f.m.FileOrder()
	first := entry(order[0], '0', f.files[order[0]])
	second := entry(order[1], '0', f.files[order[1]])
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	// read through, counted, never used: names outside the grammar, links, PAX globals, GNU long
	// names, sparse files, a second copy
	for name, data := range map[string][]byte{
		"a dot-dot name":   cat(head, src, entry("../etc/passwd", '0', []byte("x")), files, eoa),
		"an absolute name": cat(head, src, entry("/etc/passwd", '0', []byte("x")), files, eoa),
		"a symlink":        cat(head, src, entry(order[0], '2', nil), files, eoa), // a link named as a file
		"a PAX global":     cat(head, src, entry("g", 'g', pax(map[string]string{"path": order[0]})), files, eoa),
		"a GNU long name":  cat(head, src, entry("././@LongLink", 'L', []byte(order[0])), entry("x", '0', f.files[order[0]]), files, eoa),
		"a sparse file":    cat(head, src, entry("p", 'x', pax(map[string]string{"GNU.sparse.size": "10"})), first, second, eoa),
		"a second copy":    cat(head, src, first, first, second, eoa),
	} {
		b, err := bundle.Open(bytes.NewReader(data), t0, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, got, err := readAll(t, b)
		want := 2
		if name == "a sparse file" {
			want = 1 // the sparse entry is the first file: never used
		}
		if err != io.EOF || len(got) != want {
			t.Errorf("%s: %v %v", name, got, err)
		}
		if b.Skipped() == 0 {
			t.Errorf("%s: nothing counted as skipped", name)
		}
	}
	// refused
	big := entry("junk", '0', make([]byte, bundle.MaxSkipped+1))
	for name, data := range map[string][]byte{
		"sources after files": cat(head, first, src, second, eoa),
		"files out of order":  cat(head, src, second, first, eoa),
		"a zero block alone":  cat(head, src, files, make([]byte, 512), header("x", 0, '0', "ustar\x0000")),
		"over the skip cap":   cat(head, src, big, files, eoa),
		"a GNU header": cat(head, src, append(append(header(order[0], int64(len(f.files[order[0]])), '0', "ustar  \x00"), f.files[order[0]]...),
			make([]byte, (512-len(f.files[order[0]])%512)%512)...), second, eoa),
		"a PAX chain": cat(head, src, entry("p", 'x', pax(map[string]string{"comment": "a"})), entry("q", 'x', pax(map[string]string{"comment": "b"})),
			files, eoa),
	} {
		b, err := bundle.Open(bytes.NewReader(data), t0, nil)
		if err != nil {
			t.Fatalf("%s: open: %v", name, err)
		}
		if _, _, err := readAll(t, b); !errors.Is(err, bundle.ErrMalformed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// the skip cap counts headers and padding: zero-size entries and PAX headers are not free
	for name, unit := range map[string][]byte{
		"zero-size entries": entry("junk", '0', nil),
		"PAX headers":       cat(entry("p", 'x', pax(map[string]string{"comment": strings.Repeat("c", 60000)})), entry("junk", '0', nil)),
	} {
		b, err := bundle.Open(io.MultiReader(bytes.NewReader(cat(head, src)), &repeat{unit: unit}), t0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := readAll(t, b); !errors.Is(err, bundle.ErrMalformed) || !strings.Contains(err.Error(), "read through") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a truncated tar: what came before stays, then io.ErrUnexpectedEOF
	b, _ := bundle.Open(bytes.NewReader(cat(head, src, first, second[:520])), t0, nil)
	if _, got, err := readAll(t, b); !errors.Is(err, io.ErrUnexpectedEOF) || len(got) != 1 {
		t.Errorf("truncated: %v %v", got, err)
	}
	// bad heads
	sig := entry(bundle.SigEntry, '0', make([]byte, 64))
	for name, data := range map[string][]byte{
		"no manifest first":      cat(src, head, eoa),
		"a PAX global first":     cat(entry("g", 'g', pax(map[string]string{"comment": "x"})), head, eoa),
		"a short signature":      cat(entry(bundle.ManifestEntry, '0', f.manifest(t)), entry(bundle.SigEntry, '0', make([]byte, 63)), eoa),
		"a signature, no key":    cat(entry(bundle.ManifestEntry, '0', f.manifest(t)), sig, src, eoa),
		"a key, no signature":    cat(entry(bundle.ManifestEntry, '0', f.manifest(t)), entry(bundle.PubEntry, '0', []byte("x")), eoa),
		"a manifest not strict":  cat(entry(bundle.ManifestEntry, '0', append(f.manifest(t)[:len(f.manifest(t))-1], []byte(`,"x":1}`)...)), eoa),
		"a symlink manifest":     cat(entry(bundle.ManifestEntry, '2', nil), eoa),
		"a truncated head":       head[:600],
		"a bad header checksum":  cat(func() []byte { h := entry(bundle.ManifestEntry, '0', f.manifest(t)); h[0] = 'K'; return h }(), eoa),
		"an RSA key as the .pub": cat(entry(bundle.ManifestEntry, '0', f.manifest(t)), sig, entry(bundle.PubEntry, '0', rsaPEM(t)), eoa),
		"a link second":          cat(entry(bundle.ManifestEntry, '0', f.manifest(t)), entry("x", '2', nil), src, eoa),
		"a PAX global second":    cat(entry(bundle.ManifestEntry, '0', f.manifest(t)), entry("g", 'g', nil), src, eoa),
	} {
		if _, err := bundle.Open(bytes.NewReader(data), t0, nil); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
}

func rsaPEM(t *testing.T) []byte {
	t.Helper()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func TestInflate(t *testing.T) {
	body := bytes.Repeat([]byte("k"), 4096)
	z := gz(t, body)
	var out bytes.Buffer
	if n, s, err := bundle.Inflate(bytes.NewReader(z), 4096, &out); err != nil || n != 4096 || s != sha(body) {
		t.Fatalf("inflate: %d %s %v", n, s, err)
	}
	for name, in := range map[string][]byte{
		"over the cap":      z, // with max 4095 below
		"trailing data":     append(append([]byte{}, z...), 0),
		"a second member":   append(append([]byte{}, z...), gz(t, []byte("x"))...),
		"not gzip":          []byte("plain"),
		"a truncated gzip":  z[:len(z)-5],
		"a gzip bomb":       gz(t, make([]byte, 10<<20)),
		"empty":             nil,
		"a corrupt trailer": append(append([]byte{}, z[:len(z)-4]...), 1, 2, 3, 4),
	} {
		max := int64(4096)
		if name == "over the cap" {
			max = 4095
		}
		if _, _, err := bundle.Inflate(bytes.NewReader(in), max, io.Discard); err == nil {
			t.Errorf("%s: inflated", name)
		}
	}
}

func TestCanonical(t *testing.T) {
	srcs := []bundle.Source{
		{Kind: "repository", Prefix: "https://b.example.org", Cells: []bundle.Cell{{Name: "z", Platform: "linux_amd64", DuckDBVersion: "v2.10.0"},
			{Name: "z", Platform: "linux_amd64", DuckDBVersion: "v2.9.0"}, {Name: "a", Platform: "osx_arm64", DuckDBVersion: "v2.0.0"}}},
		{Kind: "repository", Prefix: "https://a.example.org"},
		{Kind: "duckdb-core"},
	}
	bundle.Canonical(srcs)
	if srcs[0].Kind != "duckdb-core" || srcs[1].Prefix != "https://a.example.org" {
		t.Fatalf("sources: %+v", srcs)
	}
	c := srcs[2].Cells
	if c[0].Name != "a" || c[1].DuckDBVersion != "v2.9.0" || c[2].DuckDBVersion != "v2.10.0" {
		t.Fatalf("cells: %+v", c)
	}
}

func TestText(t *testing.T) {
	if got := bundle.Text("a\x1b[2Jb\u202ec", 100); got != "a[2Jbc" {
		t.Errorf("%q", got)
	}
	if got := bundle.Text("ééé", 5); got != "éé" {
		t.Errorf("%q", got)
	}
}

// repeat yields unit over and over: an input with no end.
type repeat struct {
	unit []byte
	off  int
}

func (r *repeat) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.unit[r.off:])
		n += c
		r.off = (r.off + c) % len(r.unit)
	}
	return n, nil
}

// With pins, the key and the signature are checked before the manifest is parsed.
func TestOpenPinned(t *testing.T) {
	f := newFixture(t)
	priv, fp := key(t)
	data, _ := f.write(t, priv)
	if _, err := bundle.Open(bytes.NewReader(data), t0, func(s string) bool { return s == fp }); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.Open(bytes.NewReader(data), t0, func(string) bool { return false }); err == nil {
		t.Error("an unpinned key")
	}
	// an unsigned bundle whose manifest would not even parse: refused as unsigned, never parsed
	bad := append(entry(bundle.ManifestEntry, '0', []byte("{not json")), make([]byte, 1024)...)
	if _, err := bundle.Open(bytes.NewReader(bad), t0, func(string) bool { return true }); !errors.Is(err, bundle.ErrUnsigned) {
		t.Errorf("unsigned: %v", err)
	}
	if _, err := bundle.Open(bytes.NewReader(bad), t0, nil); !errors.Is(err, bundle.ErrInvalid) {
		t.Errorf("without pins the manifest is parsed: %v", err)
	}
}

func TestStrictKeys(t *testing.T) {
	var v struct {
		Outcome string `json:"outcome"`
	}
	for _, doc := range []string{`{"outcome":"missing","OUTCOME":"fetched"}`, `{"Outcome":"x"}`, `{"\u017fources":1}`, `{"outcome\u0000":"x"}`} {
		if err := bundle.DecodeStrict([]byte(doc), &v); !errors.Is(err, bundle.ErrInvalid) {
			t.Errorf("%s: %v", doc, err)
		}
	}
}

func TestManifestLimits(t *testing.T) {
	for name, change := range map[string]func(m *bundle.Manifest){
		"a .well-known over 64 KiB": func(m *bundle.Manifest) { m.Sources[1].WellKnown.Size = 64<<10 + 1 },
		"sources/0":                 func(m *bundle.Manifest) { m.Sources[1].WellKnown.Path = "sources/0/well-known.json" },
		"sources/123456":            func(m *bundle.Manifest) { m.Sources[1].WellKnown.Path = "sources/123456/well-known.json" },
		"upper-case hex": func(m *bundle.Manifest) {
			c := &m.Sources[1].Cells[0]
			c.SHA256 = strings.ToUpper(c.SHA256)
			c.File = "files/" + c.SHA256 + ".gz"
		},
		"an etag with a control": func(m *bundle.Manifest) { m.Sources[1].Cells[0].ETag = "\"x\x07\"" },
		"an etag over 256":       func(m *bundle.Manifest) { m.Sources[1].Cells[0].ETag = strings.Repeat("e", 257) },
		"a detail with a control": func(m *bundle.Manifest) {
			m.Sources[0].Cells[1].Detail = "a\x1b[2Jb"
		},
		"over 100,000 cells": func(m *bundle.Manifest) {
			for i := range 100001 {
				m.Sources[0].Cells = append(m.Sources[0].Cells, bundle.Cell{DuckDBVersion: "v2.0.0", Platform: "linux_amd64",
					Name: fmt.Sprintf("e%06d", i), Outcome: bundle.Missing})
			}
		},
	} {
		g := newFixture(t)
		change(&g.m)
		if err := g.m.Validate(t0); !errors.Is(err, bundle.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a file shared by two cells is ordered by the lowest DuckDB version among them, v2.9 before v2.10
	g := newFixture(t)
	acl, tresor := g.m.Sources[1].Cells[0], g.m.Sources[1].Cells[1]
	acl.DuckDBVersion, tresor.DuckDBVersion = "v2.10.0", "v2.10.0"
	early := tresor
	early.DuckDBVersion, early.Name = "v2.9.0", "acl" // tresor's file also fills acl at v2.9.0
	g.m.Sources[1].Cells = []bundle.Cell{acl, early, tresor}
	if order := g.m.FileOrder(); len(order) != 2 || order[0] != tresor.File {
		t.Errorf("order: %v (want %s first)", order, tresor.File)
	}
}
