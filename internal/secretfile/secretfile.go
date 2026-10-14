// Package secretfile reads the files kista takes secrets, tokens and public material from (spec
// 0013), with one set of rules:
//
//   - A Secret or a Token is opened inside its own directory (os.OpenRoot): a relative link may
//     lead anywhere under that directory, as Kubernetes' atomic-writer layout does (x ->
//     ..data/x, ..data -> ..2026_10_14…), but not out of it, and an absolute link is refused, so
//     a link cannot hand another secret over in its place. The directory must be readable. A
//     Public file's links are followed anywhere: its content is not secret.
//   - A missing file is retried once, for a ..data swap in flight.
//   - It must be a regular file that nobody but its owner may write; a Secret or a Token has one
//     link (a hard link cannot hand another file over either).
//   - Then by class: a Secret is owned by kista's euid with no group or other bits, or owned by
//     root with group kista's egid (not 0) and mode at most 0440 (a Secret volume under fsGroup);
//     a Token (a projected service account token: short-lived, audience-bound) and a Public file
//     are owned by root or by kista's euid, with any read bits.
//
// On systems without Unix ownership only the regular-file, size and directory rules apply.
package secretfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Class is what a file holds, which decides its owner and mode rule.
type Class int

const (
	// Secret: passwords, static keys, private keys, tokens that are secrets.
	Secret Class = iota
	// Token: projected service account tokens.
	Token
	// Public: certificates, CA bundles, credential configurations without secrets.
	Public
)

// Default bounds.
const (
	SmallLimit = 64 << 10 // passwords, tokens, headers
	LargeLimit = 1 << 20  // certificates, CA bundles, TLS keys
)

// ErrRule is a file that breaks its class's rule.
var ErrRule = errors.New("secretfile: the file breaks its rule")

// Read reads a whole file of the class, at most limit bytes.
func Read(path string, class Class, limit int64) ([]byte, error) {
	f, err := open(path, class)
	if errors.Is(err, fs.ErrNotExist) {
		time.Sleep(20 * time.Millisecond) // a ..data swap may have removed the directory just resolved
		f, err = open(path, class)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("secretfile: %s: %w", path, err)
	}
	if err := check(path, fi, class); err != nil {
		return nil, err
	}
	blocking(f) // a regular file: reads need no O_NONBLOCK, and a FUSE server could honour it
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("secretfile: %s: %w", path, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrRule, path, limit)
	}
	return b, nil
}

// open opens the file non-blocking, so a FIFO put in its place cannot hang the open; it is refused
// next.
func open(path string, class Class) (*os.File, error) {
	if class == Public {
		f, err := os.OpenFile(path, os.O_RDONLY|nonblock, 0)
		if err != nil {
			return nil, fmt.Errorf("secretfile: %s: %w", path, unwrapPath(err))
		}
		return f, nil
	}
	dir := filepath.Dir(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("secretfile: %s: %w", path, unwrapPath(err))
		}
		return nil, fmt.Errorf("secretfile: %s: its directory %s cannot be opened (kista must be able to read it): %w",
			path, dir, unwrapPath(err))
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(filepath.Base(path), os.O_RDONLY|nonblock, 0)
	if err != nil {
		return nil, fmt.Errorf("secretfile: %s: %w (a link may not leave the directory, nor be absolute)", path, unwrapPath(err))
	}
	return f, nil
}

// unwrapPath drops os.Root's own path from an error: the caller names the file.
func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

func check(path string, fi fs.FileInfo, class Class) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrRule, path)
	}
	o, ok := owner(fi)
	if !ok {
		return nil
	}
	perm := fi.Mode().Perm()
	if perm&0o022 != 0 {
		return fmt.Errorf("%w: %s has mode %04o; nobody but its owner may write it", ErrRule, path, perm)
	}
	if class != Public && o.nlink != 1 {
		return fmt.Errorf("%w: %s has %d hard links; it must have one", ErrRule, path, o.nlink)
	}
	euid, egid := uint32(os.Geteuid()), uint32(os.Getegid())
	switch class {
	case Secret:
		own := o.uid == euid && perm&0o077 == 0
		mounted := o.uid == 0 && o.gid == egid && egid != 0 && perm&^0o440 == 0
		if !own && !mounted {
			return fmt.Errorf("%w: %s (owner %d, group %d, mode %04o) must be owned by kista (uid %d) with mode 0600 or narrower, "+
				"or owned by root with kista's group (%d, not 0) and mode 0440 or narrower", ErrRule, path, o.uid, o.gid, perm, euid, egid)
		}
	case Token, Public:
		if o.uid != 0 && o.uid != euid {
			return fmt.Errorf("%w: %s must be owned by root or by kista (uid %d), not uid %d", ErrRule, path, euid, o.uid)
		}
	}
	return nil
}

type ids struct {
	uid, gid uint32
	nlink    uint64
}
