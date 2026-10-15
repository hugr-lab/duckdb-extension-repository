package bundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Formats and limits (spec 0014).
const (
	PlanFormat     = "kista-bundle-plan/v1"
	ManifestFormat = "kista-bundle/v1"

	MaxManifest  = 64 << 20 // bytes, also a plan's
	MaxCells     = 100000
	MaxFiles     = 50000
	MaxWellKnown = 64 << 10
	MaxETag      = 256
	MaxDetail    = 1000
	MaxExporter  = 256
	// FutureSkew is how far past the importing host's clock a manifest's created_at may be.
	FutureSkew = 10 * time.Minute
)

// Cell outcomes in a manifest.
const (
	Fetched   = "fetched"   // the cell's file is in the bundle
	Unchanged = "unchanged" // the plan's body_hash: no file
	Missing   = "missing"
	Rejected  = "rejected"
)

// ErrInvalid is a plan or manifest that breaks the format.
var ErrInvalid = errors.New("bundle: invalid")

// Plan is what a site asks for (spec 0014, The plan): its bundle-mode upstreams' sources and cells.
type Plan struct {
	Format     string       `json:"format"`
	CreatedAt  time.Time    `json:"created_at"`
	Tenant     string       `json:"tenant"`
	BundleKeys []string     `json:"bundle_keys"`
	Sources    []PlanSource `json:"sources"`
}

// PlanSource is one source: a kind, and for a repository its normalised prefix and pinned keys.
type PlanSource struct {
	Kind   string     `json:"kind"`
	Prefix string     `json:"prefix,omitempty"`
	Keys   []string   `json:"keys,omitempty"`
	Cells  []PlanCell `json:"cells"`
}

// PlanCell is a cell the site wants, with the extension versions it accepts (none: any) and, when
// it holds the cell, what it holds.
type PlanCell struct {
	DuckDBVersion string   `json:"duckdb_version"`
	Platform      string   `json:"platform"`
	Name          string   `json:"name"`
	Versions      []string `json:"versions"`
	ETag          string   `json:"etag,omitempty"`
	BodyHash      string   `json:"body_hash,omitempty"`
}

// Manifest is a bundle's kista-bundle.json.
type Manifest struct {
	Format    string    `json:"format"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Exporter  Exporter  `json:"exporter"`
	Plan      PlanRef   `json:"plan"`
	Sources   []Source  `json:"sources"`
}

// Exporter names the exporting kista, for the record only.
type Exporter struct {
	Version   string `json:"version"`
	PublicURL string `json:"public_url"`
}

// PlanRef is the plan a bundle answers.
type PlanRef struct {
	Tenant    string    `json:"tenant"`
	CreatedAt time.Time `json:"created_at"`
	SHA256    string    `json:"sha256"`
}

// Source is one source's cells, and for a repository its .well-known file.
type Source struct {
	Kind      string   `json:"kind"`
	Prefix    string   `json:"prefix,omitempty"`
	WellKnown *FileRef `json:"well_known,omitempty"`
	Cells     []Cell   `json:"cells"`
}

// FileRef names an entry by path, hash and size.
type FileRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Cell is one cell's outcome on the exporting side.
type Cell struct {
	DuckDBVersion string    `json:"duckdb_version"`
	Platform      string    `json:"platform"`
	Name          string    `json:"name"`
	Outcome       string    `json:"outcome"`
	File          string    `json:"file,omitempty"`          // fetched: files/<sha256>.gz
	SHA256        string    `json:"sha256,omitempty"`        // fetched: of the inflated file (body ‖ signature)
	Size          int64     `json:"size,omitempty"`          // fetched: the gzip entry's size
	InflatedSize  int64     `json:"inflated_size,omitempty"` // fetched
	BodyHash      string    `json:"body_hash,omitempty"`     // fetched, unchanged: spec 0002's composite hash
	ETag          string    `json:"etag,omitempty"`
	FetchedAt     time.Time `json:"fetched_at,omitzero"` // fetched, unchanged: last confirmed at the source
	Authenticated bool      `json:"authenticated,omitempty"`
	Detail        string    `json:"detail,omitempty"` // missing, rejected
}

var (
	hexRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	fpRE     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	uuid7RE  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	filePath = regexp.MustCompile(`^files/[0-9a-f]{64}\.gz$`)
	wkPath   = regexp.MustCompile(`^sources/[1-9][0-9]{0,4}/well-known\.json$`)
)

// DecodeStrict decodes JSON into v refusing unknown fields, duplicate keys and trailing data.
func DecodeStrict(data []byte, v any) error {
	if len(data) > MaxManifest {
		return fmt.Errorf("%w: over %d bytes", ErrInvalid, MaxManifest)
	}
	if err := noDuplicateKeys(data); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("%w: data after the document", ErrInvalid)
	}
	return nil
}

// noDuplicateKeys walks the document's tokens and refuses an object with a key twice: Go's decoder
// keeps the last silently, and a signed document must mean one thing.
func noDuplicateKeys(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	type frame struct {
		obj  bool
		keys map[string]bool
		key  bool // the next token in this object is a key
	}
	var stack []*frame
	for {
		t, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if _, delim := t.(json.Delim); !delim && top != nil && top.obj && top.key {
			k, _ := t.(string)
			if top.keys[k] {
				return fmt.Errorf("%w: the key %q twice in one object", ErrInvalid, k)
			}
			top.keys[k] = true
			top.key = false
			continue
		}
		switch t {
		case json.Delim('{'):
			stack = append(stack, &frame{obj: true, keys: map[string]bool{}, key: true})
			continue
		case json.Delim('['):
			stack = append(stack, &frame{})
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 && stack[len(stack)-1].obj {
			stack[len(stack)-1].key = true // a value ended: a key comes next
		}
	}
}

// Text strips control and format characters and bounds s to n bytes: export reasons and the
// exporter are shown and stored as data.
func Text(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// ValidPrefix checks a repository source's prefix is in kista's normalised form: https, lower-case
// scheme and host, no default port, no user info, query, fragment, dot segments or trailing slash.
func ValidPrefix(p string) error {
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		u.RawPath != "" || strings.Contains(p, "#") || strings.ToLower(u.Host) != u.Host || u.Port() == "443" ||
		strings.HasSuffix(p, "/") || strings.Contains("/"+u.Path+"/", "/./") || strings.Contains("/"+u.Path+"/", "/../") ||
		strings.Contains(u.Path, "//") {
		return fmt.Errorf("%w: prefix %q is not a normalised https prefix", ErrInvalid, p)
	}
	return nil
}

func validKind(k string) bool {
	return k == store.UpstreamCore || k == store.UpstreamCommunity || k == store.UpstreamRepo
}

// sourceKey orders and identifies sources: kind, then prefix.
func sourceKey(kind, prefix string) string { return kind + "\x00" + prefix }

func checkSource(kind, prefix string, keys []string) error {
	if !validKind(kind) {
		return fmt.Errorf("%w: source kind %q", ErrInvalid, kind)
	}
	if kind == store.UpstreamRepo {
		if err := ValidPrefix(prefix); err != nil {
			return err
		}
	} else if prefix != "" || len(keys) > 0 {
		return fmt.Errorf("%w: a %s source takes no prefix or keys", ErrInvalid, kind)
	}
	for _, k := range keys {
		if !fpRE.MatchString(k) {
			return fmt.Errorf("%w: key fingerprint %q", ErrInvalid, k)
		}
	}
	return nil
}

func checkCellKey(v, p, n string) error {
	if err := release.ValidDuckDBVersion(v); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := release.ValidPlatform(p); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := release.ValidName(n); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

// Validate checks a plan's grammar and limits (spec 0009's grammars): a plan is input, whoever
// wrote it.
func (p *Plan) Validate() error {
	if p.Format != PlanFormat {
		return fmt.Errorf("%w: plan format %q", ErrInvalid, p.Format)
	}
	if p.CreatedAt.IsZero() {
		return fmt.Errorf("%w: the plan has no created_at", ErrInvalid)
	}
	if err := store.ValidName(p.Tenant); err != nil {
		return fmt.Errorf("%w: tenant: %w", ErrInvalid, err)
	}
	if len(p.BundleKeys) == 0 || len(p.BundleKeys) > 10 {
		return fmt.Errorf("%w: a plan lists 1 to 10 bundle keys", ErrInvalid)
	}
	for _, k := range p.BundleKeys {
		if !fpRE.MatchString(k) {
			return fmt.Errorf("%w: bundle key %q", ErrInvalid, k)
		}
	}
	seen, cells := map[string]bool{}, 0
	for i, s := range p.Sources {
		if err := checkSource(s.Kind, s.Prefix, s.Keys); err != nil {
			return err
		}
		if s.Kind == store.UpstreamRepo && len(s.Keys) == 0 {
			return fmt.Errorf("%w: repository source %s pins no key", ErrInvalid, s.Prefix)
		}
		k := sourceKey(s.Kind, s.Prefix)
		if seen[k] {
			return fmt.Errorf("%w: source %s %s twice", ErrInvalid, s.Kind, s.Prefix)
		}
		if i > 0 && k < sourceKey(p.Sources[i-1].Kind, p.Sources[i-1].Prefix) {
			return fmt.Errorf("%w: sources out of canonical order", ErrInvalid)
		}
		seen[k] = true
		cellSeen := map[[3]string]bool{}
		for _, c := range s.Cells {
			if cells++; cells > MaxCells {
				return fmt.Errorf("%w: over %d cells", ErrInvalid, MaxCells)
			}
			if err := checkCellKey(c.DuckDBVersion, c.Platform, c.Name); err != nil {
				return err
			}
			ck := [3]string{c.DuckDBVersion, c.Platform, c.Name}
			if cellSeen[ck] {
				return fmt.Errorf("%w: cell %v twice in a source", ErrInvalid, ck)
			}
			cellSeen[ck] = true
			for _, v := range c.Versions {
				if err := release.ValidExtVersion(v); err != nil {
					return fmt.Errorf("%w: %w", ErrInvalid, err)
				}
			}
			if len(c.ETag) > MaxETag || c.BodyHash != "" && !hexRE.MatchString(c.BodyHash) {
				return fmt.Errorf("%w: cell %v: etag or body_hash", ErrInvalid, ck)
			}
		}
	}
	return nil
}

// Validate checks a manifest before anything else (spec 0014, The bundle). now is the importing
// host's clock, for the future-skew rule; zero skips it (the export, inspect without a clock).
func (m *Manifest) Validate(now time.Time) error {
	if m.Format != ManifestFormat {
		return fmt.Errorf("%w: manifest format %q", ErrInvalid, m.Format)
	}
	if !uuid7RE.MatchString(m.ID) {
		return fmt.Errorf("%w: id %q is not a UUIDv7", ErrInvalid, m.ID)
	}
	if m.CreatedAt.IsZero() || m.Plan.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created_at", ErrInvalid)
	}
	if !now.IsZero() && m.CreatedAt.After(now.Add(FutureSkew)) {
		return fmt.Errorf("%w: created_at %s is in the future", ErrInvalid, m.CreatedAt.Format(time.RFC3339))
	}
	if len(m.Exporter.Version) > MaxExporter || len(m.Exporter.PublicURL) > MaxExporter ||
		Text(m.Exporter.Version, MaxExporter) != m.Exporter.Version || Text(m.Exporter.PublicURL, MaxExporter) != m.Exporter.PublicURL {
		return fmt.Errorf("%w: exporter: at most %d bytes of printable text each", ErrInvalid, MaxExporter)
	}
	if err := store.ValidName(m.Plan.Tenant); err != nil {
		return fmt.Errorf("%w: plan tenant: %w", ErrInvalid, err)
	}
	if !hexRE.MatchString(m.Plan.SHA256) {
		return fmt.Errorf("%w: plan sha256", ErrInvalid)
	}
	seen, cells := map[string]bool{}, 0
	files := map[string]Cell{}
	wk := map[string]bool{}
	for i, s := range m.Sources {
		if err := checkSource(s.Kind, s.Prefix, nil); err != nil {
			return err
		}
		k := sourceKey(s.Kind, s.Prefix)
		if seen[k] {
			return fmt.Errorf("%w: source %s %s twice", ErrInvalid, s.Kind, s.Prefix)
		}
		if i > 0 && k < sourceKey(m.Sources[i-1].Kind, m.Sources[i-1].Prefix) {
			return fmt.Errorf("%w: sources out of canonical order", ErrInvalid)
		}
		seen[k] = true
		switch {
		case s.Kind == store.UpstreamRepo && s.WellKnown == nil:
			return fmt.Errorf("%w: repository source %s carries no .well-known file", ErrInvalid, s.Prefix)
		case s.Kind != store.UpstreamRepo && s.WellKnown != nil:
			return fmt.Errorf("%w: a %s source carries no .well-known file", ErrInvalid, s.Kind)
		case s.WellKnown != nil:
			w := s.WellKnown
			if !wkPath.MatchString(w.Path) || wk[w.Path] || !hexRE.MatchString(w.SHA256) || w.Size <= 0 || w.Size > MaxWellKnown {
				return fmt.Errorf("%w: well_known of %s", ErrInvalid, s.Prefix)
			}
			wk[w.Path] = true
		}
		cellSeen := map[[3]string]bool{}
		for _, c := range s.Cells {
			if cells++; cells > MaxCells {
				return fmt.Errorf("%w: over %d cells", ErrInvalid, MaxCells)
			}
			if err := checkCellKey(c.DuckDBVersion, c.Platform, c.Name); err != nil {
				return err
			}
			ck := [3]string{c.DuckDBVersion, c.Platform, c.Name}
			if cellSeen[ck] {
				return fmt.Errorf("%w: cell %v twice in a source", ErrInvalid, ck)
			}
			cellSeen[ck] = true
			if err := c.validate(); err != nil {
				return fmt.Errorf("%w: cell %v: %w", ErrInvalid, ck, err)
			}
			if c.Outcome == Fetched {
				if prev, ok := files[c.File]; ok && (prev.Size != c.Size || prev.InflatedSize != c.InflatedSize || prev.BodyHash != c.BodyHash) {
					return fmt.Errorf("%w: file %s referenced with different sizes", ErrInvalid, c.File)
				}
				files[c.File] = c
			}
		}
	}
	if len(files) > MaxFiles {
		return fmt.Errorf("%w: over %d files", ErrInvalid, MaxFiles)
	}
	return nil
}

func (c Cell) validate() error {
	if len(c.ETag) > MaxETag || len(c.Detail) > MaxDetail || Text(c.Detail, MaxDetail) != c.Detail {
		return errors.New("etag or detail")
	}
	if c.BodyHash != "" && !hexRE.MatchString(c.BodyHash) {
		return errors.New("body_hash")
	}
	hasFile := c.File != "" || c.SHA256 != "" || c.Size != 0 || c.InflatedSize != 0
	switch c.Outcome {
	case Fetched:
		if !filePath.MatchString(c.File) || c.File != "files/"+c.SHA256+".gz" || c.Size <= 0 || c.InflatedSize <= 256 ||
			c.BodyHash == "" || c.FetchedAt.IsZero() {
			return errors.New("a fetched cell names its file, hash, sizes, body_hash and fetched_at")
		}
	case Unchanged:
		if hasFile || c.BodyHash == "" || c.FetchedAt.IsZero() {
			return errors.New("an unchanged cell carries its body_hash and fetched_at, and no file")
		}
	case Missing, Rejected:
		if hasFile || c.BodyHash != "" || c.Authenticated {
			return errors.New("a missing or rejected cell carries no file")
		}
	default:
		return fmt.Errorf("outcome %q", c.Outcome)
	}
	return nil
}

// fileKey orders files: by source, then name, platform and the lowest DuckDB version among the
// cells each file fills.
type fileKey struct {
	source         int
	name, platform string
	version        string
	path           string
}

// FileOrder returns the manifest's files in canonical order, each once.
func (m *Manifest) FileOrder() []string {
	keys := map[string]*fileKey{}
	for si, s := range m.Sources {
		for _, c := range s.Cells {
			if c.Outcome != Fetched {
				continue
			}
			k, ok := keys[c.File]
			if !ok {
				keys[c.File] = &fileKey{si, c.Name, c.Platform, c.DuckDBVersion, c.File}
				continue
			}
			if si < k.source || si == k.source && release.CompareVersions(c.DuckDBVersion, k.version) < 0 {
				k.source, k.name, k.platform, k.version = si, c.Name, c.Platform, c.DuckDBVersion
			}
		}
	}
	all := make([]*fileKey, 0, len(keys))
	for _, k := range keys {
		all = append(all, k)
	}
	slices.SortFunc(all, func(a, b *fileKey) int {
		switch {
		case a.source != b.source:
			return a.source - b.source
		case a.name != b.name:
			return strings.Compare(a.name, b.name)
		case a.platform != b.platform:
			return strings.Compare(a.platform, b.platform)
		case a.version != b.version:
			return release.CompareVersions(a.version, b.version)
		}
		return strings.Compare(a.path, b.path)
	})
	out := make([]string, len(all))
	for i, k := range all {
		out[i] = k.path
	}
	return out
}

// Canonical sorts sources by (kind, prefix) and cells by (name, platform, DuckDB version), the
// order the export writes and validation expects.
func Canonical(sources []Source) {
	slices.SortFunc(sources, func(a, b Source) int {
		return strings.Compare(sourceKey(a.Kind, a.Prefix), sourceKey(b.Kind, b.Prefix))
	})
	for _, s := range sources {
		slices.SortFunc(s.Cells, func(a, b Cell) int {
			if c := strings.Compare(a.Name, b.Name); c != 0 {
				return c
			}
			if c := strings.Compare(a.Platform, b.Platform); c != 0 {
				return c
			}
			return release.CompareVersions(a.DuckDBVersion, b.DuckDBVersion)
		})
	}
}
