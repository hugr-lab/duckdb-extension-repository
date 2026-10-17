package attachment

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// DocSpool holds a bundle feed's documents on disk while the batches that name them are applied
// (spec 0014 phase 1b): one per feed of the bundle, shared by its targets, within a byte cap.
type DocSpool struct {
	dir   string
	max   int64
	bytes int64
	files map[string]string // content hash: its file
}

// MaxDocSpool is a bundle feed's documents' cap on disk unless the caller sets one.
const MaxDocSpool = 4 << 30

// ErrDocSpool is a feed's documents over the spool's cap: its targets fail.
var ErrDocSpool = errors.New("attachment: the bundle feed's documents are over the spool's cap")

var hashName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// NewDocSpool makes a spool under dir ("": the system's), capped at max bytes (0: MaxDocSpool).
func NewDocSpool(dir string, max int64) *DocSpool {
	if max <= 0 {
		max = MaxDocSpool
	}
	return &DocSpool{dir: dir, max: max, files: map[string]string{}}
}

// Put spools a document's content by its hash (checked by the bundle's reader).
func (d *DocSpool) Put(hash string, content []byte) error {
	if !hashName.MatchString(hash) {
		return fmt.Errorf("%w: a document name", ErrStatement)
	}
	if _, ok := d.files[hash]; ok {
		return nil
	}
	if d.bytes+int64(len(content)) > d.max {
		return fmt.Errorf("%w (%d bytes)", ErrDocSpool, d.max)
	}
	if d.files == nil {
		d.files = map[string]string{}
	}
	if len(d.files) == 0 {
		if d.dir != "" {
			if err := os.MkdirAll(d.dir, 0o700); err != nil {
				return errors.New("attachment: spooling a document failed")
			}
		}
		dir, err := os.MkdirTemp(d.dir, "kista-feed-import-*")
		if err != nil {
			return errors.New("attachment: spooling a document failed")
		}
		d.dir = dir
	}
	p := filepath.Join(d.dir, hash)
	if err := os.WriteFile(p, content, 0o600); err != nil {
		return errors.New("attachment: spooling a document failed")
	}
	d.files[hash], d.bytes = p, d.bytes+int64(len(content))
	return nil
}

// Get reads a spooled document; false when the bundle carries none.
func (d *DocSpool) Get(hash string) ([]byte, bool, error) {
	p, ok := d.files[hash]
	if !ok {
		return nil, false, nil
	}
	b, err := os.ReadFile(p)
	return b, true, err
}

// Close removes the spool's files.
func (d *DocSpool) Close() {
	if len(d.files) > 0 {
		_ = os.RemoveAll(d.dir)
		d.files = map[string]string{}
	}
}
