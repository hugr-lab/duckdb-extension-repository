package sinks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
)

// JSONL writes records as JSON lines to stdout or a file (mode 0600), rotated by size: path.1 is
// the newest old file, up to Keep of them (0: 5).
type JSONL struct {
	Path    string // "-": Stdout
	MaxSize int64  // default 100 MiB
	Keep    int
	Stdout  io.Writer // default os.Stdout

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Send writes a batch; a line is never split across files.
func (j *JSONL) Send(_ context.Context, rs []Record) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, r := range rs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Path == "-" {
		w := j.Stdout
		if w == nil {
			w = os.Stdout
		}
		// a line per write: stdout is also the process log's stream
		for line := range bytes.Lines(buf.Bytes()) {
			if _, err := w.Write(line); err != nil {
				return err
			}
		}
		return nil
	}
	limit := j.MaxSize
	if limit == 0 {
		limit = 100 << 20
	}
	if err := j.reopenIfMoved(); err != nil {
		return err
	}
	if err := j.open(); err != nil {
		return err
	}
	if j.size > 0 && j.size+int64(buf.Len()) > limit {
		if err := j.rotate(); err != nil {
			return err
		}
		if err := j.open(); err != nil {
			return err
		}
	}
	if _, err := j.f.Write(buf.Bytes()); err != nil {
		// never half a line: the batch is written again whole
		_ = j.f.Truncate(j.size)
		_ = j.f.Close()
		j.f = nil
		return fmt.Errorf("sinks: writing the events file: %w", err)
	}
	j.size += int64(buf.Len())
	return j.f.Sync()
}

// open opens the file for appending, unless it is open.
func (j *JSONL) open() error {
	if j.f != nil {
		return nil
	}
	// never through a symbolic link; an existing file is made the owner's only
	f, err := os.OpenFile(j.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("sinks: opening the events file: %w", err)
	}
	st, err := f.Stat()
	if err == nil && st.Mode().Perm() != 0o600 {
		err = f.Chmod(0o600)
	}
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("sinks: opening the events file: %w", err)
	}
	j.f, j.size = f, st.Size()
	return nil
}

// reopenIfMoved closes the file if the path no longer names it (another replica holding the
// sink's lease rotated it on a shared volume), and takes its size from the file itself.
func (j *JSONL) reopenIfMoved() error {
	if j.f == nil {
		return nil
	}
	fi, ferr := j.f.Stat()
	pi, perr := os.Lstat(j.Path)
	if ferr == nil && perr == nil && os.SameFile(fi, pi) {
		j.size = fi.Size()
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}

// rotate closes the file and shifts path → path.1 → … → path.Keep (the oldest goes).
func (j *JSONL) rotate() error {
	err := j.f.Close()
	j.f, j.size = nil, 0
	if err != nil {
		return err
	}
	keep := j.Keep
	if keep == 0 {
		keep = 5
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", j.Path, keep))
	for i := keep - 1; i >= 1; i-- {
		if err := os.Rename(fmt.Sprintf("%s.%d", j.Path, i), fmt.Sprintf("%s.%d", j.Path, i+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(j.Path, j.Path+".1")
}

// Close closes the file.
func (j *JSONL) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}
