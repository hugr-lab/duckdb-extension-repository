package bundle

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
)

// Statements in a bundle (spec 0014 phase 1b): a plan's feeds, a manifest's feeds and their entries.

// Limits on feeds in a plan and a bundle.
const (
	MaxFeeds       = 50
	MaxFeedBodies  = 100000 // across a plan's feeds
	MaxFeedEntries = 100000 // batches, and documents, per feed
	MaxKeysFile    = 64 << 10
	MaxBatch       = 1 << 20
	MaxDocument    = attachment.MaxConfigurableSize
)

// PlanFeed is one of a site's bundle-mode feeds (merged by URL): what it pins and accepts, the bodies
// it offers with the last seq received, and the plan's sources whose new bodies it may be offered.
type PlanFeed struct {
	URL     string     `json:"url"`
	Keys    []string   `json:"keys"`
	Kinds   []string   `json:"kinds"` // none: every kind
	Sources []int      `json:"sources"`
	Bodies  []PlanBody `json:"bodies"`
}

// PlanBody is a body a feed offers, with the last seq received (0: none).
type PlanBody struct {
	BodyHash string `json:"body_hash"`
	Seq      int64  `json:"seq"`
}

// Feed is a feed's part of a bundle: its entries in the order the tar carries them (the keys file,
// then each batch followed by the documents it first names), and per body the range the batches
// cover.
type Feed struct {
	URL     string     `json:"url"`
	Entries []FileRef  `json:"entries"`
	Bodies  []FeedBody `json:"bodies"`
}

// FeedBody is the range (after_seq, seq] of a body's statements a bundle covers.
type FeedBody struct {
	BodyHash string `json:"body_hash"`
	AfterSeq int64  `json:"after_seq"`
	Seq      int64  `json:"seq"`
}

// BatchEntry is a statement in a batch, as a feed's answer carries it, with the exporting side's
// unsigned note when the bundle carries no content for it (the gateway's refusal or removal).
type BatchEntry struct {
	Statement string `json:"statement"`
	Signature string `json:"signature"`
	Note      string `json:"note,omitempty"`
}

var (
	feedTenantRE = regexp.MustCompile(`/api/v1/tenants/[a-z0-9][a-z0-9-]{0,62}$`)
	feedEntryRE  = regexp.MustCompile(`^feeds/([1-9][0-9]?)/(attestation-keys\.json|batch-([1-9][0-9]{0,5})\.json|documents/([0-9a-f]{64}))$`)
)

// FeedPath is the n-th feed's (1-based) entry name: "attestation-keys.json", "batch-<k>.json" or
// "documents/<sha256>".
func FeedPath(n int, name string) string { return "feeds/" + strconv.Itoa(n) + "/" + name }

// ValidFeedURL checks a feed URL is in kista's normalised form (spec 0011): a normalised https
// prefix ending in /api/v1/tenants/<tenant>.
func ValidFeedURL(u string) error {
	if err := ValidPrefix(u); err != nil || !feedTenantRE.MatchString(u) {
		return fmt.Errorf("%w: feed URL %q is not a normalised …/api/v1/tenants/<tenant>", ErrInvalid, u)
	}
	return nil
}

func validFeedKinds(kinds []string) error {
	for i, k := range kinds {
		if !slices.Contains(attachment.Kinds, k) || i > 0 && kinds[i-1] >= k {
			return fmt.Errorf("%w: feed kinds %v: known kinds, sorted, each once", ErrInvalid, kinds)
		}
	}
	return nil
}

// validatePlanFeeds checks a plan's feeds against its n sources.
func validatePlanFeeds(feeds []PlanFeed, sources int) error {
	if len(feeds) > MaxFeeds {
		return fmt.Errorf("%w: over %d feeds", ErrInvalid, MaxFeeds)
	}
	bodies := 0
	for i, f := range feeds {
		if err := ValidFeedURL(f.URL); err != nil {
			return err
		}
		if i > 0 && feeds[i-1].URL >= f.URL {
			return fmt.Errorf("%w: feeds out of canonical order, or a URL twice", ErrInvalid)
		}
		if len(f.Keys) == 0 || len(f.Keys) > 10 {
			return fmt.Errorf("%w: feed %s pins 1 to 10 keys", ErrInvalid, f.URL)
		}
		for j, k := range f.Keys {
			if !fpRE.MatchString(k) || j > 0 && f.Keys[j-1] >= k {
				return fmt.Errorf("%w: feed %s: keys sorted, each once", ErrInvalid, f.URL)
			}
		}
		if err := validFeedKinds(f.Kinds); err != nil {
			return err
		}
		for j, s := range f.Sources {
			if s < 0 || s >= sources || j > 0 && f.Sources[j-1] >= s {
				return fmt.Errorf("%w: feed %s: sources are the plan's, sorted, each once", ErrInvalid, f.URL)
			}
		}
		for j, b := range f.Bodies {
			if bodies++; bodies > MaxFeedBodies {
				return fmt.Errorf("%w: over %d feed bodies", ErrInvalid, MaxFeedBodies)
			}
			if !hexRE.MatchString(b.BodyHash) || b.Seq < 0 || j > 0 && f.Bodies[j-1].BodyHash >= b.BodyHash {
				return fmt.Errorf("%w: feed %s: bodies sorted, each once, seq at least 0", ErrInvalid, f.URL)
			}
		}
	}
	return nil
}

// FeedEntry tells what a feed entry's name is: its feed (1-based), and for a batch its number, for
// a document its content hash.
type FeedEntry struct {
	Feed     int
	Keys     bool
	Batch    int
	Document string
}

// ParseFeedEntry parses a feed entry's name.
func ParseFeedEntry(name string) (FeedEntry, bool) {
	m := feedEntryRE.FindStringSubmatch(name)
	if m == nil {
		return FeedEntry{}, false
	}
	n, _ := strconv.Atoi(m[1])
	e := FeedEntry{Feed: n, Keys: m[2] == "attestation-keys.json", Document: m[4]}
	if m[3] != "" {
		e.Batch, _ = strconv.Atoi(m[3])
	}
	return e, true
}

// validateFeeds checks a manifest's feeds: each URL once in order, its entries named for its place
// (the keys file first, batches numbered from 1, a document after a batch, by its content hash,
// once), the sizes within their limits, and its bodies' ranges.
func validateFeeds(feeds []Feed) error {
	if len(feeds) > MaxFeeds {
		return fmt.Errorf("%w: over %d feeds", ErrInvalid, MaxFeeds)
	}
	bodies := 0
	for i, f := range feeds {
		n := i + 1
		if err := ValidFeedURL(f.URL); err != nil {
			return err
		}
		if i > 0 && feeds[i-1].URL >= f.URL {
			return fmt.Errorf("%w: feeds out of canonical order, or a URL twice", ErrInvalid)
		}
		if len(f.Entries) == 0 {
			return fmt.Errorf("%w: feed %s carries no keys file", ErrInvalid, f.URL)
		}
		batches, docs := 0, map[string]bool{}
		for j, e := range f.Entries {
			fe, ok := ParseFeedEntry(e.Path)
			if !ok || fe.Feed != n || !hexRE.MatchString(e.SHA256) || e.Size <= 0 {
				return fmt.Errorf("%w: feed %s: entry %q", ErrInvalid, f.URL, Text(e.Path, 256))
			}
			switch {
			case j == 0:
				if !fe.Keys || e.Size > MaxKeysFile {
					return fmt.Errorf("%w: feed %s: the first entry is its keys file, at most %d bytes", ErrInvalid, f.URL, MaxKeysFile)
				}
			case fe.Keys:
				return fmt.Errorf("%w: feed %s: a second keys file", ErrInvalid, f.URL)
			case fe.Batch > 0:
				if batches++; fe.Batch != batches || batches > MaxFeedEntries || e.Size > MaxBatch {
					return fmt.Errorf("%w: feed %s: batches numbered from 1, at most %d bytes each", ErrInvalid, f.URL, MaxBatch)
				}
			default:
				if batches == 0 || docs[fe.Document] || fe.Document != e.SHA256 || len(docs) >= MaxFeedEntries || e.Size > MaxDocument {
					return fmt.Errorf("%w: feed %s: document %s: after a batch, once, named by its hash", ErrInvalid, f.URL, fe.Document)
				}
				docs[fe.Document] = true
			}
		}
		for j, b := range f.Bodies {
			if bodies++; bodies > MaxFeedBodies {
				return fmt.Errorf("%w: over %d feed bodies", ErrInvalid, MaxFeedBodies)
			}
			if !hexRE.MatchString(b.BodyHash) || b.AfterSeq < 0 || b.Seq < b.AfterSeq || j > 0 && f.Bodies[j-1].BodyHash >= b.BodyHash {
				return fmt.Errorf("%w: feed %s: bodies sorted, each once, 0 <= after_seq <= seq", ErrInvalid, f.URL)
			}
		}
	}
	return nil
}

// ParseBatch decodes a batch: its statements, each base64 as a feed's answer carries it. The
// statements themselves are checked by their reader, as a feed client checks them.
func ParseBatch(data []byte) ([]BatchEntry, error) {
	if len(data) > MaxBatch {
		return nil, fmt.Errorf("%w: a batch over %d bytes", ErrInvalid, MaxBatch)
	}
	var out []BatchEntry
	if err := DecodeStrict(data, &out); err != nil {
		return nil, err
	}
	for _, e := range out {
		_, err1 := base64.StdEncoding.DecodeString(e.Statement)
		_, err2 := base64.StdEncoding.DecodeString(e.Signature)
		if err1 != nil || err2 != nil || len(e.Note) > MaxDetail || Text(e.Note, MaxDetail) != e.Note {
			return nil, fmt.Errorf("%w: a batch entry's encoding or note", ErrInvalid)
		}
	}
	return out, nil
}

// MarshalBatch encodes a batch as the export writes it.
func MarshalBatch(entries []BatchEntry) ([]byte, error) {
	if entries == nil {
		entries = []BatchEntry{}
	}
	return json.Marshal(entries)
}
