// Package kubesecret is the key source for RSA keys held in a Kubernetes Secret and mounted into
// kista's pods (spec 0026). Its root of trust is the cluster's (etcd encryption at rest, RBAC on
// Secrets), weaker than every other source by design; the configuration acknowledges that, and kista
// says it at every start, in the audit trail and in the admin output.
//
// At every open and re-check the source accepts exactly a Secret volume, and refuses a look-alike
// made by mistake:
//
//  1. the mount: the source's directory is a mount point, tmpfs, mounted read-only (Linux only:
//     /proc/self/mountinfo; other platforms refuse the kind);
//  2. the layout: the kubelet's atomic writer, <dir>/..data a relative link to a ..<timestamp>
//     directory, <dir>/<name> the relative link ..data/<name>, the file opened inside dir
//     (os.OpenRoot), so no link may leave it;
//  3. the file: regular, one hard link, owned by root with kista's group (not 0), mode 0440 or
//     narrower (spec 0013's shape (b); a file owned by kista is not a Secret volume's);
//  4. the key: one unencrypted PKCS#8 or PKCS#1 RSA key, 2048 bits, e = 65537, at most 64 KiB, as
//     the file signer parses it.
//
// The mount and layout checks catch a misconfiguration; they are no defence against whoever controls
// the pod spec.
package kubesecret

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

// Kind is the source kind.
const Kind = "kubesecret"

// ErrUnsupported is the kind on a platform without /proc/self/mountinfo.
var ErrUnsupported = errors.New("kubesecret: this kind needs Linux: it checks /proc/self/mountinfo that the directory is a Secret volume")

// Options are the source's test seams; production leaves them zero.
type Options struct {
	// MountInfo returns the mount table in /proc/self/mountinfo's format; nil reads it (Linux), or
	// returns ErrUnsupported elsewhere.
	MountInfo func() ([]byte, error)
	// Owner and Group are the owner and group a key file must have; nil: root, and kista's egid.
	Owner, Group *uint32
}

// Source is one Secret volume.
type Source struct {
	dir       string
	mountInfo func() ([]byte, error)
	owner     uint32
	group     uint32
}

// New returns a source over a Secret volume's mount path. It touches nothing: the directory is
// checked at every open.
func New(dir string, opts Options) (*Source, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || dir == "/" {
		return nil, fmt.Errorf("kubesecret: the directory must be an absolute, clean path below /")
	}
	s := &Source{dir: dir, mountInfo: opts.MountInfo, group: uint32(os.Getegid())} //nolint:gosec // a gid fits
	if s.mountInfo == nil {
		s.mountInfo = readMountInfo
	}
	if opts.Owner != nil {
		s.owner = *opts.Owner
	}
	if opts.Group != nil {
		s.group = *opts.Group
	}
	return s, nil
}

// Kind implements keysource.Backend.
func (s *Source) Kind() string { return Kind }

// keyName is a Secret data key without dots: never ..data, a timestamped directory, or a path.
var keyName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)

// ParseKey parses "<name>": the name is the version (a key is never replaced under its name).
func (s *Source) ParseKey(ref string) (keysource.Key, error) {
	if !keyName.MatchString(ref) {
		return keysource.Key{}, fmt.Errorf("kubesecret key %q is not a Secret data key [A-Za-z0-9_-]{1,63}", ref)
	}
	return keysource.Key{Name: ref}, nil
}

// handle is an opened key.
type handle struct {
	s    *Source
	name string
	key  *rsa.PrivateKey
}

// Open implements keysource.Backend: every check, then the key. A kubesecret source holds channel
// keys only (spec 0026, open question 4).
func (s *Source) Open(_ context.Context, k keysource.Key) (keysource.Handle, error) {
	if k.Purpose != "" && k.Purpose != keysource.PurposeChannel {
		return nil, fmt.Errorf("%w: a kubesecret source holds channel keys only, not %s keys", keysource.ErrKey, k.Purpose)
	}
	key, err := s.load(k.Name)
	if err != nil {
		return nil, err
	}
	return &handle{s: s, name: k.Name, key: key}, nil
}

// load runs every check and parses the key. A failure is retried once after a moment: the
// kubelet's ..data swap may be in flight.
func (s *Source) load(name string) (*rsa.PrivateKey, error) {
	key, err := s.loadOnce(name)
	if err != nil && !errors.Is(err, ErrUnsupported) {
		time.Sleep(20 * time.Millisecond)
		key, err = s.loadOnce(name)
	}
	return key, err
}

func (s *Source) loadOnce(name string) (*rsa.PrivateKey, error) {
	if !keyName.MatchString(name) {
		return nil, fmt.Errorf("%w: %q is not a Secret data key", keysource.ErrKey, name)
	}
	mi, err := s.mountInfo()
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			return nil, fmt.Errorf("%w: %w", keysource.ErrKey, err)
		}
		return nil, fmt.Errorf("%w: kubesecret: reading the mount table: %v", keysource.ErrKey, err)
	}
	if err := CheckMount(mi, s.dir); err != nil {
		return nil, fmt.Errorf("%w: %w", keysource.ErrKey, err)
	}
	if err := s.checkLayout(name); err != nil {
		return nil, fmt.Errorf("%w: %w", keysource.ErrKey, err)
	}
	data, err := s.read(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", keysource.ErrKey, err)
	}
	key, err := signer.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("%w: kubesecret: %s/%s: %v", keysource.ErrKey, s.dir, name, err)
	}
	return key, nil
}

// checkLayout checks the atomic writer's links: ..data a relative link to a ..<timestamp> directory
// inside dir, and <name> the relative link ..data/<name>.
func (s *Source) checkLayout(name string) error {
	data := filepath.Join(s.dir, "..data")
	li, err := os.Lstat(data)
	if err != nil {
		return fmt.Errorf("kubesecret: %s has no ..data link: not a Secret volume (%v)", s.dir, unwrapPath(err))
	}
	if li.Mode()&fs.ModeSymlink == 0 {
		return fmt.Errorf("kubesecret: %s/..data is not a link: not a Secret volume", s.dir)
	}
	target, err := os.Readlink(data)
	if err != nil {
		return fmt.Errorf("kubesecret: %s/..data: %v", s.dir, unwrapPath(err))
	}
	if !strings.HasPrefix(target, "..") || target == ".." || target == "..data" || strings.ContainsAny(target, `/\`) {
		return fmt.Errorf("kubesecret: %s/..data must link to a ..<timestamp> directory beside it", s.dir)
	}
	ti, err := os.Lstat(filepath.Join(s.dir, target))
	if err != nil || !ti.IsDir() {
		return fmt.Errorf("kubesecret: %s/..data does not lead to a directory beside it", s.dir)
	}
	p := filepath.Join(s.dir, name)
	ni, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("kubesecret: %s: %v", p, unwrapPath(err))
	}
	if ni.Mode()&fs.ModeSymlink == 0 {
		return fmt.Errorf("kubesecret: %s is not the link ..data/%s a Secret volume has", p, name)
	}
	if l, err := os.Readlink(p); err != nil || l != "..data/"+name {
		return fmt.Errorf("kubesecret: %s must be the relative link ..data/%s", p, name)
	}
	return nil
}

// read opens the key inside dir (no link may leave it) and checks the file: regular, one link,
// root-owned with kista's group (not 0), mode 0440 or narrower; at most 64 KiB.
func (s *Source) read(name string) ([]byte, error) {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, fmt.Errorf("kubesecret: %s cannot be opened (kista must be able to read it): %v", s.dir, unwrapPath(err))
	}
	defer func() { _ = root.Close() }()
	p := filepath.Join(s.dir, name)
	// the file behind the links, before it is opened: a FIFO or a device is refused without an open
	before, err := root.Stat(name)
	if err != nil {
		return nil, fmt.Errorf("kubesecret: %s: %v (a link may not leave the directory, nor be absolute)", p, unwrapPath(err))
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("kubesecret: %s is not a regular file", p)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("kubesecret: %s: %v", p, unwrapPath(err))
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("kubesecret: %s: %v", p, unwrapPath(err))
	}
	if !fi.Mode().IsRegular() || !os.SameFile(before, fi) {
		return nil, fmt.Errorf("kubesecret: %s changed while it was opened", p)
	}
	if err := s.checkFile(p, fi); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, signer.MaxKeyFile+1))
	if err != nil {
		return nil, fmt.Errorf("kubesecret: reading %s: %v", p, unwrapPath(err))
	}
	if len(b) > signer.MaxKeyFile {
		return nil, fmt.Errorf("kubesecret: %s is larger than %d bytes", p, signer.MaxKeyFile)
	}
	return b, nil
}

// checkFile is spec 0013's shape (b), alone: shape (a), a file owned by kista, is not a Secret
// volume's.
func (s *Source) checkFile(p string, fi fs.FileInfo) error {
	o, ok := owner(fi)
	if !ok {
		return fmt.Errorf("kubesecret: %s: this platform reports no Unix owner", p)
	}
	perm := fi.Mode().Perm()
	switch {
	case o.nlink != 1:
		return fmt.Errorf("kubesecret: %s has %d hard links; it must have one", p, o.nlink)
	case s.group == 0:
		return fmt.Errorf("kubesecret: kista runs with group 0; a Secret volume's key must be readable through kista's own group (fsGroup)")
	case o.uid != s.owner || o.gid != s.group || perm&^0o440 != 0:
		return fmt.Errorf("kubesecret: %s (owner %d, group %d, mode %04o) must be owned by %d with kista's group %d and mode 0440 or narrower, as a Secret volume's file under fsGroup",
			p, o.uid, o.gid, perm, s.owner, s.group)
	}
	return nil
}

// unwrapPath drops the path an fs error carries: the caller names the file.
func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}

func (h *handle) Public() *rsa.PublicKey { return &h.key.PublicKey }

// Identity is the key's name: the name is the version.
func (h *handle) Identity() string { return h.name }

// Check implements keysource.Handle: the file is read again with every check, and must hold the
// same key. A key changed under its name, or a file gone, fails closed.
func (h *handle) Check(context.Context) error {
	key, err := h.s.load(h.name)
	if err != nil {
		return err
	}
	if !key.PublicKey.Equal(&h.key.PublicKey) {
		return fmt.Errorf("%w: kubesecret: %s/%s holds another key now; a key is never replaced under its name", keysource.ErrKey, h.s.dir, h.name)
	}
	return nil
}

// Sign implements keysource.Handle: PKCS#1 v1.5 over the SHA-256 digest, in memory.
func (h *handle) Sign(_ context.Context, digest []byte) ([]byte, string, error) {
	if len(digest) != 32 {
		return nil, "", errors.New("kubesecret: the digest must be 32 bytes")
	}
	sig, err := rsa.SignPKCS1v15(rand.Reader, h.key, crypto.SHA256, digest)
	if err != nil {
		return nil, "", errors.New("kubesecret: signing failed")
	}
	return sig, h.name, nil
}
