//go:build unix

package secretfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeInfo struct {
	mode  fs.FileMode
	uid   uint32
	gid   uint32
	links int // 0 means 1
}

func (f fakeInfo) Name() string       { return "x" }
func (f fakeInfo) Size() int64        { return 1 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any {
	st := &syscall.Stat_t{Uid: f.uid, Gid: f.gid, Nlink: 1}
	for i := 1; i < f.links; i++ { // Nlink's type differs by platform
		st.Nlink++
	}
	return st
}

// Every owner and mode shape of every class.
func TestRules(t *testing.T) {
	me, mine := uint32(os.Geteuid()), uint32(os.Getegid())
	if me == 0 || mine == 0 {
		t.Skip("the rules are about a non-root kista")
	}
	other := me + 1
	cases := []struct {
		name  string
		class Class
		fi    fakeInfo
		ok    bool
	}{
		{"secret own 0600", Secret, fakeInfo{0o600, me, mine, 0}, true},
		{"secret own 0400", Secret, fakeInfo{0o400, me, mine, 0}, true},
		{"secret own 0640", Secret, fakeInfo{0o640, me, mine, 0}, false},
		{"secret own 0644", Secret, fakeInfo{0o644, me, mine, 0}, false},
		{"secret mounted 0440", Secret, fakeInfo{0o440, 0, mine, 0}, true},
		{"secret mounted 0400", Secret, fakeInfo{0o400, 0, mine, 0}, true},
		{"secret mounted 0640", Secret, fakeInfo{0o640, 0, mine, 0}, false},
		{"secret mounted 0444", Secret, fakeInfo{0o444, 0, mine, 0}, false},
		{"secret root group 0", Secret, fakeInfo{0o440, 0, 0, 0}, false},
		{"secret root other group", Secret, fakeInfo{0o440, 0, mine + 1, 0}, false},
		{"secret foreign owner", Secret, fakeInfo{0o400, other, mine, 0}, false},
		{"secret group writable", Secret, fakeInfo{0o620, me, mine, 0}, false},
		{"token 0644", Token, fakeInfo{0o644, me, mine, 0}, true},
		{"token projected 0640", Token, fakeInfo{0o640, me, mine, 0}, true},
		{"token root 0644", Token, fakeInfo{0o644, 0, 0, 0}, true},
		{"token foreign owner", Token, fakeInfo{0o644, other, mine, 0}, false},
		{"secret two links", Secret, fakeInfo{0o600, me, mine, 2}, false},
		{"token two links", Token, fakeInfo{0o644, me, mine, 2}, false},
		{"public two links", Public, fakeInfo{0o644, 0, 0, 2}, true},
		{"token other writable", Token, fakeInfo{0o646, me, mine, 0}, false},
		{"public root 0644", Public, fakeInfo{0o644, 0, 0, 0}, true},
		{"public own 0644", Public, fakeInfo{0o644, me, mine, 0}, true},
		{"public foreign", Public, fakeInfo{0o644, other, mine, 0}, false},
		{"public group writable", Public, fakeInfo{0o664, 0, 0, 0}, false},
		{"directory", Token, fakeInfo{fs.ModeDir | 0o700, me, mine, 0}, false},
		{"fifo", Token, fakeInfo{fs.ModeNamedPipe | 0o600, me, mine, 0}, false},
	}
	for _, c := range cases {
		err := check("/x", c.fi, c.class)
		if c.ok != (err == nil) || err != nil && !errors.Is(err, ErrRule) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// Kubernetes' atomic-writer layout is followed; links out of the directory are not.
func TestLinks(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "secrets")
	write(t, filepath.Join(dir, "..2026_10_14", "password"), "s3cret", 0o600)
	for _, l := range [][2]string{{"..2026_10_14", "..data"}, {"..data/password", "password"}} {
		if err := os.Symlink(l[0], filepath.Join(dir, l[1])); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := Read(filepath.Join(dir, "password"), Secret, SmallLimit); err != nil || string(b) != "s3cret" {
		t.Fatalf("the atomic-writer layout: %q %v", b, err)
	}
	// an update: a new timestamped directory, ..data swapped, the old one removed
	write(t, filepath.Join(dir, "..2026_10_15", "password"), "rotated", 0o600)
	if err := os.Symlink("..2026_10_15", filepath.Join(dir, "..data.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "..data.tmp"), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "..2026_10_14")); err != nil {
		t.Fatal(err)
	}
	if b, err := Read(filepath.Join(dir, "password"), Secret, SmallLimit); err != nil || string(b) != "rotated" {
		t.Fatalf("after an update: %q %v", b, err)
	}
	// out of the directory: absolute, upward, and into a sibling whose name has the same prefix
	write(t, filepath.Join(base, "secrets-evil", "x"), "x", 0o600)
	write(t, filepath.Join(base, "outside"), "x", 0o600)
	for name, target := range map[string]string{
		"absolute": filepath.Join(base, "outside"),
		"upward":   "../outside",
		"sibling":  "../secrets-evil/x",
	} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(filepath.Join(dir, name), Secret, SmallLimit); err == nil {
			t.Errorf("%s: a link out of the directory was followed", name)
		}
	}
}

// A public file's links are followed anywhere (certbot's live/ links, system CA bundles); a
// secret's absolute link is refused even inside its directory, and so is a hard link; an
// unreadable directory is named.
func TestLinkRulesByClass(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "archive", "cert.pem"), "cert", 0o644)
	live := filepath.Join(base, "live")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../archive/cert.pem", filepath.Join(live, "cert.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(filepath.Join(live, "cert.pem"), Public, LargeLimit); err != nil {
		t.Fatalf("a public file through a link out of its directory: %v", err)
	}
	if _, err := Read(filepath.Join(live, "cert.pem"), Token, LargeLimit); err == nil {
		t.Fatal("a token through a link out of its directory")
	}
	dir := filepath.Join(base, "secrets")
	write(t, filepath.Join(dir, "password"), "x", 0o600)
	if err := os.Symlink(filepath.Join(dir, "password"), filepath.Join(dir, "absolute")); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(filepath.Join(dir, "absolute"), Secret, SmallLimit); err == nil {
		t.Fatal("an absolute link was followed")
	}
	if err := os.Link(filepath.Join(dir, "password"), filepath.Join(dir, "hard")); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(filepath.Join(dir, "hard"), Secret, SmallLimit); !errors.Is(err, ErrRule) {
		t.Fatalf("a hard link: %v", err)
	}
	locked := filepath.Join(base, "locked")
	write(t, filepath.Join(locked, "password"), "x", 0o600)
	if err := os.Chmod(locked, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if _, err := Read(filepath.Join(locked, "password"), Secret, SmallLimit); err == nil || !strings.Contains(err.Error(), "directory "+locked) {
		t.Fatalf("an unreadable directory: %v", err)
	}
}

// Real files: the mode, the kind and the size.
func TestRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	write(t, p, "eyJ.a.b", 0o644)
	if _, err := Read(p, Token, SmallLimit); err != nil {
		t.Fatalf("a token at 0644: %v", err)
	}
	if _, err := Read(p, Secret, SmallLimit); !errors.Is(err, ErrRule) || !strings.Contains(err.Error(), "0644") {
		t.Fatalf("a secret at 0644: %v", err)
	}
	big := filepath.Join(dir, "big")
	write(t, big, strings.Repeat("x", 100), 0o600)
	if _, err := Read(big, Secret, 99); !errors.Is(err, ErrRule) {
		t.Fatalf("oversized: %v", err)
	}
	if b, err := Read(big, Secret, 100); err != nil || len(b) != 100 {
		t.Fatalf("exactly the limit: %d %v", len(b), err)
	}
	if _, err := Read(dir, Token, SmallLimit); err == nil {
		t.Fatal("a directory was read")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := Read(fifo, Secret, SmallLimit); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO was read")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reading a FIFO hung")
	}
	if _, err := Read(filepath.Join(dir, "missing"), Secret, SmallLimit); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
}
