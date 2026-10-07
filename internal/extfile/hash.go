package extfile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// ChunkSize is the size of the chunks the composite hash is computed over.
const ChunkSize = 1 << 20

// BodyHash is the composite hash of an extension body: the SHA-256 of the concatenated SHA-256 of
// each 1 MiB chunk (the last chunk is shorter and never empty). It is what DuckDB signs
// (src/main/extension/extension_load.cpp, ComputeFinalHash).
type BodyHash [sha256.Size]byte

func (h BodyHash) String() string { return hex.EncodeToString(h[:]) }

// ErrTooLarge is returned when an input exceeds the caller's maximum size.
var ErrTooLarge = errors.New("extfile: input exceeds the maximum size")

// HashBody computes the composite hash of a body read from r, streaming. It reads at most maxSize
// bytes and fails with ErrTooLarge if r holds more. It returns the hash and the body length.
func HashBody(r io.Reader, maxSize int64) (BodyHash, int64, error) {
	var h compositeHasher
	n, err := copyMax(&h, r, maxSize)
	if err != nil {
		return BodyHash{}, 0, err
	}
	return h.Sum(), n, nil
}

// copyMax copies r to w and fails with ErrTooLarge if r holds more than maxSize bytes (checked
// with a one-byte probe, so maxSize may be anything up to math.MaxInt64). A negative maxSize is an
// error.
func copyMax(w io.Writer, r io.Reader, maxSize int64) (int64, error) {
	if maxSize < 0 {
		return 0, fmt.Errorf("extfile: negative maximum size %d", maxSize)
	}
	n, err := io.CopyN(w, r, maxSize)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("extfile: reading: %w", err)
	}
	if n == maxSize {
		var probe [1]byte
		k, err := io.ReadFull(r, probe[:])
		if k > 0 {
			return n, ErrTooLarge
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return n, fmt.Errorf("extfile: reading: %w", err)
		}
	}
	return n, nil
}

// compositeHasher is an io.Writer that computes the composite hash of everything written to it.
// Digests returns the per-chunk SHA-256 values.
type compositeHasher struct {
	chunk   [ChunkSize]byte
	fill    int
	digests []byte
}

func (c *compositeHasher) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		k := copy(c.chunk[c.fill:], p)
		c.fill += k
		p = p[k:]
		if c.fill == ChunkSize {
			c.flush()
		}
	}
	return n, nil
}

func (c *compositeHasher) flush() {
	d := sha256.Sum256(c.chunk[:c.fill])
	c.digests = append(c.digests, d[:]...)
	c.fill = 0
}

// Sum returns the composite hash. A body of zero bytes has no chunks: its hash is SHA-256 of the
// empty string, as in DuckDB.
func (c *compositeHasher) Sum() BodyHash {
	if c.fill > 0 {
		c.flush()
	}
	return sha256.Sum256(c.digests)
}

// Digests returns the per-chunk digests (call after Sum).
func (c *compositeHasher) Digests() [][sha256.Size]byte {
	out := make([][sha256.Size]byte, len(c.digests)/sha256.Size)
	for i := range out {
		copy(out[i][:], c.digests[i*sha256.Size:])
	}
	return out
}
