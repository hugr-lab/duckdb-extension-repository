package bundle

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// ErrUnsigned is a bundle without a signature: exported with -unsigned and not yet signed.
var ErrUnsigned = errors.New("bundle: the bundle is not signed (kista bundle sign signs it)")

// Head is a bundle's first entries: the manifest, and when signed its signature and key.
type Head struct {
	Manifest      *Manifest
	ManifestBytes []byte
	ManifestSHA   string
	Signed        bool
	Sig           []byte
	Pub           ed25519.PublicKey
	Fingerprint   string // the key's
}

// Check verifies the signature with the bundle's key, which pinned must accept by fingerprint.
func (h *Head) Check(pinned func(fingerprint string) bool) error {
	if !h.Signed {
		return ErrUnsigned
	}
	if !pinned(h.Fingerprint) {
		return fmt.Errorf("bundle: the bundle key %s is not pinned", h.Fingerprint)
	}
	return Verify(h.Pub, h.ManifestBytes, h.Sig)
}

// Item is a used entry after the head: a source's .well-known file, an extension file, or a feed's
// entry (phase 1b).
type Item struct {
	WellKnown *Source // the source whose .well-known file this is: Data, its hash checked
	Data      []byte
	File      string // files/<sha256>.gz: R, its size checked
	Size      int64
	R         io.Reader // the entry's bytes; read it before Next

	// a feed's entry: Feed's Entries[Entry], its bytes in Data when its size and hash match the
	// manifest; Damaged says why the feed's target fails (an entry missing, out of order, or
	// differing from the manifest), once and for every later entry of the feed
	Feed      *Feed
	FeedIndex int
	Entry     int
	Damaged   string
}

type feedState struct {
	next    int    // the next entry expected
	damaged string // why the feed's entries cannot be trusted whole
}

// Bundle reads a bundle once, front to back: the head, then the items in order. Unnamed entries,
// other entry types and second copies are read through, counted, and capped at MaxSkipped.
type Bundle struct {
	Head
	t        *Reader
	pending  *RawEntry
	sources  map[string]*Source // by .well-known path
	files    map[string]int64   // by path: the size
	order    map[string]int     // by path: canonical position
	lastFile int
	seen     map[string]bool
	inFiles  bool
	// feeds (phase 1b): each entry's feed and place, and each feed's progress
	feedAt  map[string][2]int
	feeds   []feedState
	inFeeds bool
	curFeed int
}

// Open reads a bundle's head and validates its manifest; now is the importing host's clock (zero:
// no future-skew rule). With pinned, the key's pin and the signature are checked before the
// manifest is parsed, so an unsigned or foreign bundle costs no parsing (the import); without it,
// Head.Check checks them later (inspect, sign).
func Open(r io.Reader, now time.Time, pinned func(fingerprint string) bool) (*Bundle, error) {
	t := NewReader(r)
	b := &Bundle{t: t, sources: map[string]*Source{}, files: map[string]int64{}, order: map[string]int{}, seen: map[string]bool{},
		lastFile: -1, feedAt: map[string][2]int{}}
	e, err := t.Next()
	if err != nil {
		return nil, headErr(err)
	}
	if !e.Regular || e.Name != ManifestEntry || e.Size > MaxManifest {
		return nil, fmt.Errorf("%w: the first entry is not %s (at most %d bytes)", ErrMalformed, ManifestEntry, MaxManifest)
	}
	if b.ManifestBytes, err = io.ReadAll(t); err != nil {
		return nil, headErr(err)
	}
	sum := sha256.Sum256(b.ManifestBytes)
	b.ManifestSHA = hex.EncodeToString(sum[:])
	e, err = t.Next()
	if err != nil && err != io.EOF {
		return nil, headErr(err)
	}
	if err == nil && e.Name == SigEntry {
		if !e.Regular || e.Size != ed25519.SignatureSize {
			return nil, fmt.Errorf("%w: %s is %d raw bytes", ErrMalformed, SigEntry, ed25519.SignatureSize)
		}
		if b.Sig, err = io.ReadAll(t); err != nil {
			return nil, headErr(err)
		}
		if e, err = t.Next(); err != nil {
			return nil, headErr(err)
		}
		if !e.Regular || e.Name != PubEntry || e.Size > maxHead {
			return nil, fmt.Errorf("%w: the third entry is not %s", ErrMalformed, PubEntry)
		}
		pemBytes, err := io.ReadAll(t)
		if err != nil {
			return nil, headErr(err)
		}
		if b.Pub, err = ParsePublicKey(pemBytes); err != nil {
			return nil, err
		}
		b.Signed, b.Fingerprint = true, Fingerprint(b.Pub)
	} else if err == nil {
		if !e.Regular { // among the first three entries, anything but a plain file refuses the bundle
			return nil, fmt.Errorf("%w: %s among the first entries", ErrMalformed, e.Why)
		}
		if e.Name == PubEntry {
			return nil, fmt.Errorf("%w: %s without %s", ErrMalformed, PubEntry, SigEntry)
		}
		b.pending = &e
	} else {
		b.pending = nil // io.EOF: a bundle of no files
	}
	if pinned != nil {
		if err := b.Check(pinned); err != nil {
			return nil, err
		}
	}
	var m Manifest
	if err := DecodeStrict(b.ManifestBytes, &m); err != nil {
		return nil, err
	}
	if err := m.Validate(now); err != nil {
		return nil, err
	}
	b.Manifest = &m
	for i := range m.Sources {
		if w := m.Sources[i].WellKnown; w != nil {
			b.sources[w.Path] = &m.Sources[i]
		}
	}
	for i, p := range m.FileOrder() {
		b.order[p] = i
	}
	for _, s := range m.Sources {
		for _, c := range s.Cells {
			if c.Outcome == Fetched {
				b.files[c.File] = c.Size
			}
		}
	}
	b.feeds = make([]feedState, len(m.Feeds))
	for fi, f := range m.Feeds {
		for ei, e := range f.Entries {
			b.feedAt[e.Path] = [2]int{fi, ei}
		}
	}
	return b, nil
}

func headErr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: the bundle ends in its head", ErrMalformed)
	}
	return err
}

// Next returns the next used entry, or io.EOF at the end of archive. A truncated input is
// io.ErrUnexpectedEOF; what came before it stays valid.
func (b *Bundle) Next() (Item, error) {
	for {
		var e RawEntry
		var err error
		if b.pending != nil {
			e, b.pending = *b.pending, nil
		} else if e, err = b.t.Next(); err != nil {
			return Item{}, err
		}
		if b.t.Skipped()+b.skippable(e) > MaxSkipped {
			return Item{}, fmt.Errorf("%w: over %d bytes of entries read through", ErrMalformed, MaxSkipped)
		}
		if strings.HasPrefix(e.Name, "sources/") && b.inFiles {
			return Item{}, fmt.Errorf("%w: %q after the files", ErrMalformed, Text(e.Name, 256))
		}
		if (strings.HasPrefix(e.Name, "sources/") || strings.HasPrefix(e.Name, "files/")) && b.inFeeds {
			return Item{}, fmt.Errorf("%w: %q after the feeds", ErrMalformed, Text(e.Name, 256))
		}
		if strings.HasPrefix(e.Name, "files/") {
			b.inFiles = true
		}
		if !e.Regular || b.seen[e.Name] || e.Name == ManifestEntry || e.Name == SigEntry || e.Name == PubEntry {
			if err := b.t.Skip(e); err != nil {
				return Item{}, err
			}
			continue
		}
		if src, ok := b.sources[e.Name]; ok {
			if e.Size != src.WellKnown.Size {
				return Item{}, fmt.Errorf("%w: %s is %d bytes, the manifest says %d", ErrMalformed, e.Name, e.Size, src.WellKnown.Size)
			}
			data, err := io.ReadAll(b.t)
			if err != nil {
				return Item{}, err
			}
			if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != src.WellKnown.SHA256 {
				return Item{}, fmt.Errorf("%w: %s differs from the manifest's hash", ErrMalformed, e.Name)
			}
			b.seen[e.Name] = true
			return Item{WellKnown: src, Data: data, Size: e.Size}, nil
		}
		if size, ok := b.files[e.Name]; ok {
			if e.Size != size {
				return Item{}, fmt.Errorf("%w: %s is %d bytes, the manifest says %d", ErrMalformed, e.Name, e.Size, size)
			}
			if pos := b.order[e.Name]; pos < b.lastFile {
				return Item{}, fmt.Errorf("%w: %s out of canonical order", ErrMalformed, e.Name)
			} else {
				b.lastFile = pos
			}
			b.seen[e.Name] = true
			return Item{File: e.Name, Size: e.Size, R: b.t}, nil
		}
		if at, ok := b.feedAt[e.Name]; ok {
			return b.feedItem(e, at[0], at[1])
		}
		if err := b.t.Skip(e); err != nil { // unnamed: read through
			return Item{}, err
		}
	}
}

// feedItem reads a feed's entry: in its place, its bytes matching the manifest, or the feed marked
// damaged (spec 0014: a damaged feed fails its target only).
func (b *Bundle) feedItem(e RawEntry, fi, ei int) (Item, error) {
	b.inFeeds = true
	f, st := &b.Manifest.Feeds[fi], &b.feeds[fi]
	ref := f.Entries[ei]
	b.seen[e.Name] = true
	switch {
	case st.damaged != "":
	case fi < b.curFeed:
		st.damaged = fmt.Sprintf("%s after a later feed's entries", ref.Path)
	case ei > st.next:
		st.damaged = f.Entries[st.next].Path + " is missing"
	}
	b.curFeed = max(b.curFeed, fi)
	st.next = max(st.next, ei+1)
	it := Item{Feed: f, FeedIndex: fi, Entry: ei, Size: e.Size}
	if e.Size > MaxDocument { // past any feed entry's limit: not damage, a bundle to refuse
		return Item{}, fmt.Errorf("%w: %s is %d bytes", ErrMalformed, ref.Path, e.Size)
	}
	if e.Size != ref.Size {
		// read through without counting toward MaxSkipped: a damaged feed fails its target only,
		// and the size cap above bounds each entry
		if _, err := io.Copy(io.Discard, b.t); err != nil {
			return Item{}, err
		}
		if st.damaged == "" {
			st.damaged = fmt.Sprintf("%s is %d bytes, the manifest says %d", ref.Path, e.Size, ref.Size)
		}
	} else {
		data, err := io.ReadAll(b.t)
		if err != nil {
			return Item{}, err
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != ref.SHA256 {
			if st.damaged == "" {
				st.damaged = ref.Path + " differs from the manifest's hash"
			}
		} else {
			it.Data = data
		}
	}
	it.Damaged = st.damaged
	return it, nil
}

// FeedWhole reports whether every entry of the manifest's fi-th feed was read, in order, as the
// manifest says: only then may its target's cursors move.
func (b *Bundle) FeedWhole(fi int) bool {
	st := b.feeds[fi]
	if st.damaged != "" || st.next != len(b.Manifest.Feeds[fi].Entries) {
		return false
	}
	for _, e := range b.Manifest.Feeds[fi].Entries {
		if !b.seen[e.Path] {
			return false
		}
	}
	return true
}

func (b *Bundle) skippable(e RawEntry) int64 {
	_, feed := b.feedAt[e.Name]
	if e.Regular && !b.seen[e.Name] && (b.sources[e.Name] != nil || b.files[e.Name] != 0 || feed) {
		return 0
	}
	return e.Size + e.Overhead
}

// Missing lists the entries the manifest names that were not read: complete only after io.EOF.
func (b *Bundle) Missing() []string {
	var out []string
	for p := range b.sources {
		if !b.seen[p] {
			out = append(out, p)
		}
	}
	for p := range b.files {
		if !b.seen[p] {
			out = append(out, p)
		}
	}
	for p := range b.feedAt {
		if !b.seen[p] {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// SHA256 is the SHA-256 of every byte read through the end-of-archive blocks: the bundle's
// input_sha256, once Next returned io.EOF.
func (b *Bundle) SHA256() string { return b.t.SHA256() }

// Ended reports whether the end of archive was read.
func (b *Bundle) Ended() bool { return b.t.Ended() }

// Skipped is the size of the entries read through.
func (b *Bundle) Skipped() int64 { return b.t.Skipped() }

// WriteHead writes a bundle's head: the manifest and, with a key, its signature and public key.
func WriteHead(w *Writer, manifest []byte, priv ed25519.PrivateKey) error {
	if err := w.Entry(ManifestEntry, int64(len(manifest)), strings.NewReader(string(manifest))); err != nil {
		return err
	}
	if priv == nil {
		return nil
	}
	sig := Sign(priv, manifest)
	if err := w.Entry(SigEntry, int64(len(sig)), strings.NewReader(string(sig))); err != nil {
		return err
	}
	pub := MarshalPublicKey(priv.Public().(ed25519.PublicKey))
	return w.Entry(PubEntry, int64(len(pub)), strings.NewReader(string(pub)))
}
