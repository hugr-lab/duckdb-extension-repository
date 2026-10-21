//go:build unix

package kubesecret

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/sourcetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
)

var ctx = context.Background()

var (
	key2048, _ = rsa.GenerateKey(rand.Reader, 2048)
	other, _   = rsa.GenerateKey(rand.Reader, 2048)
)

func pemOf(t *testing.T, k *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// volume is a directory built as the kubelet's atomic writer builds a Secret volume:
// <dir>/..<ts>/<key> files, <dir>/..data -> ..<ts>, <dir>/<key> -> ..data/<key>.
type volume struct {
	t    *testing.T
	dir  string
	gen  int
	mode os.FileMode
	gid  uint32 // the group standing in for fsGroup: the files' and the source's
}

// testGroup stands in for fsGroup when the tests run in group 0 (as root): group 0 is refused by
// design, so the files are given this group instead.
const testGroup = 65532

func newVolume(t *testing.T, files map[string][]byte) *volume {
	t.Helper()
	v := &volume{t: t, dir: filepath.Join(t.TempDir(), "prod"), mode: 0o440, gid: uint32(os.Getegid())} //nolint:gosec // a gid fits
	if v.gid == 0 {
		if os.Geteuid() != 0 {
			t.Skip("running in group 0 but not as root: no other group can stand in for fsGroup (group 0 is refused by design)")
		}
		v.gid = testGroup
	}
	if err := os.Mkdir(v.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	v.write(files)
	return v
}

// write makes a new timestamped directory with the files, and swaps ..data to it.
func (v *volume) write(files map[string][]byte) {
	v.t.Helper()
	v.gen++
	ts := fmt.Sprintf("..2026_10_21_10_00_%02d.%09d", v.gen, v.gen)
	if err := os.Mkdir(filepath.Join(v.dir, ts), 0o755); err != nil {
		v.t.Fatal(err)
	}
	for name, b := range files {
		p := filepath.Join(v.dir, ts, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			v.t.Fatal(err)
		}
		if err := os.Chmod(p, v.mode); err != nil {
			v.t.Fatal(err)
		}
		if v.gid != uint32(os.Getegid()) { //nolint:gosec // a gid fits
			if err := os.Chown(p, -1, int(v.gid)); err != nil {
				v.t.Fatal(err)
			}
		}
		link := filepath.Join(v.dir, name)
		if _, err := os.Lstat(link); errors.Is(err, os.ErrNotExist) {
			if err := os.Symlink("..data/"+name, link); err != nil {
				v.t.Fatal(err)
			}
		}
	}
	tmp := filepath.Join(v.dir, "..data_tmp")
	if err := os.Symlink(ts, tmp); err != nil {
		v.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(v.dir, "..data")); err != nil {
		v.t.Fatal(err)
	}
}

// the current timestamped directory
func (v *volume) current() string {
	v.t.Helper()
	ts, err := os.Readlink(filepath.Join(v.dir, "..data"))
	if err != nil {
		v.t.Fatal(err)
	}
	return filepath.Join(v.dir, ts)
}

// mountLine is the volume's line in the shape kind's (containerd) mountinfo shows a Secret volume,
// with the device the directory really is on.
func (v *volume) mountLine(opts, fstype string) string {
	v.t.Helper()
	fi, err := os.Stat(v.dir)
	if err != nil {
		v.t.Fatal(err)
	}
	major, minor, ok := devOf(fi)
	if !ok {
		v.t.Fatal("no device numbers")
	}
	return deviceLine(major, minor, v.dir, opts, fstype)
}

func deviceLine(major, minor uint32, dir, opts, fstype string) string {
	return fmt.Sprintf("1361 1342 %d:%d / %s %s - %s tmpfs rw,size=16271356k,inode64\n", major, minor, dir, opts, fstype)
}

const baseMounts = "1342 1100 0:290 / / rw,relatime master:1 - overlay overlay rw,lowerdir=/x,upperdir=/y,workdir=/z\n" +
	"1343 1342 0:293 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw\n"

func (v *volume) source(t *testing.T, mounts string) *Source {
	t.Helper()
	uid := uint32(os.Geteuid()) //nolint:gosec // a uid fits
	s, err := New(v.dir, Options{MountInfo: func() ([]byte, error) { return []byte(mounts), nil }, Owner: &uid, Group: &v.gid})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (v *volume) secretSource(t *testing.T) *Source {
	return v.source(t, baseMounts+v.mountLine("ro,relatime", "tmpfs"))
}

func registry(t *testing.T, s *Source, opts keysource.Options) *keysource.Registry {
	t.Helper()
	reg := keysource.NewRegistry(signer.Resolver{})
	if opts.Allow == nil {
		opts.Allow = []string{"ext-"}
	}
	if err := reg.Add("k8s", s, opts); err != nil {
		t.Fatal(err)
	}
	return reg
}

// harness is the contract suite's fake: a Secret volume with one key.
type harness struct {
	v *volume
}

func (h *harness) New(t *testing.T, opts keysource.Options) (*keysource.Registry, string) {
	h.v = newVolume(t, map[string][]byte{"ext-prod": pemOf(t, key2048)})
	return registry(t, h.v.secretSource(t), opts), "k8s:ext-prod"
}

func (h *harness) Key() *rsa.PrivateKey { return key2048 }

func (h *harness) Mutate(t *testing.T, m sourcetest.Mutation) bool {
	var k *rsa.PrivateKey
	switch m {
	case sourcetest.Size3072:
		k, _ = rsa.GenerateKey(rand.Reader, 3072)
	case sourcetest.Exponent3:
		k = exponent3(t)
	case sourcetest.PublicKeyChange:
		k = other
	default:
		return false
	}
	h.v.write(map[string][]byte{"ext-prod": pemOf(t, k)})
	return true
}

func exponent3(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	// rsa.GenerateKey always uses 65537: build a 2048-bit key with e = 3
	one, three := big.NewInt(1), big.NewInt(3)
	for range 200 {
		p, err1 := rand.Prime(rand.Reader, 1024)
		q, err2 := rand.Prime(rand.Reader, 1024)
		if err1 != nil || err2 != nil || p.Cmp(q) == 0 {
			continue
		}
		phi := new(big.Int).Mul(new(big.Int).Sub(p, one), new(big.Int).Sub(q, one))
		d := new(big.Int).ModInverse(three, phi)
		if d == nil {
			continue
		}
		k := &rsa.PrivateKey{PublicKey: rsa.PublicKey{N: new(big.Int).Mul(p, q), E: 3}, D: d, Primes: []*big.Int{p, q}}
		k.Precompute()
		if k.Validate() == nil {
			return k
		}
	}
	t.Fatal("no e = 3 key")
	return nil
}

func TestContract(t *testing.T) {
	sourcetest.Run(t, func(*testing.T) sourcetest.Harness { return &harness{} })
}

func TestOpen(t *testing.T) {
	v := newVolume(t, map[string][]byte{"ext-prod": pemOf(t, key2048)})
	reg := registry(t, v.secretSource(t), keysource.Options{})
	sg, err := reg.Open(ctx, "k8s:ext-prod")
	if err != nil {
		t.Fatal(err)
	}
	if !sg.Public().Equal(&key2048.PublicKey) || sg.ID() != "k8s:ext-prod" {
		t.Fatalf("opened %s", sg.ID())
	}
	h := extfile.BodyHash(sha256.Sum256([]byte("body")))
	if _, err := sg.Sign(ctx, h); err != nil {
		t.Fatal(err)
	}
	if got := reg.Protection("k8s:ext-prod"); got != keysource.ProtectionKubeSecret {
		t.Fatalf("protection %q", got)
	}
}

func TestParseKey(t *testing.T) {
	s, _ := New("/etc/kista/signing", Options{})
	for _, ok := range []string{"ext-prod", "ext_2027", strings.Repeat("a", 63)} {
		if _, err := s.ParseKey(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "..data", "..2026_10_21", "ext.pem", "a/b", "../x", strings.Repeat("a", 64), "ext prod"} {
		if _, err := s.ParseKey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, dir := range []string{"", "/", "relative", "/etc/kista/../x", "/etc/kista/"} {
		if _, err := New(dir, Options{}); err == nil {
			t.Errorf("dir %q accepted", dir)
		}
	}
}

func TestParseMountInfo(t *testing.T) {
	// lines in the shapes kind (containerd), EKS (containerd on Amazon Linux) and k3s show for a
	// Secret volume, a ConfigMap volume and an emptyDir with medium Memory
	table := baseMounts +
		"1361 1342 0:310 / /etc/kista/signing/prod ro,relatime - tmpfs tmpfs rw,size=16271356k,inode64\n" +
		"2201 2180 0:58 / /etc/kista/eks ro,relatime - tmpfs tmpfs rw,size=7811012k\n" +
		"3012 2990 0:121 / /etc/kista/k3s ro,relatime shared:5 master:7 - tmpfs tmpfs rw,size=3964124k,mode=755\n" +
		"1362 1342 259:1 /var/lib/kubelet/pods/0b4c/volumes/kubernetes.io~configmap/cfg /etc/kista/cfg ro,relatime - ext4 /dev/nvme0n1p1 rw\n" +
		"1363 1342 0:311 / /etc/kista/mem rw,relatime - tmpfs tmpfs rw,size=65536k\n" +
		"1364 1342 0:312 / /etc/kista/with\\040space ro,relatime - tmpfs tmpfs rw\n"
	ms, err := ParseMountInfo([]byte(table))
	if err != nil || len(ms) != 8 {
		t.Fatalf("%d mounts, %v", len(ms), err)
	}
	if ms[2].Major != 0 || ms[2].Minor != 310 || ms[5].Major != 259 || ms[5].Minor != 1 {
		t.Fatalf("devices: %+v %+v", ms[2], ms[5])
	}
	for dir, want := range map[string]string{
		"/etc/kista/signing/prod": "",
		"/etc/kista/eks":          "",
		"/etc/kista/k3s":          "",
		"/etc/kista/with space":   "",
		"/etc/kista/cfg":          "not tmpfs",
		"/etc/kista/mem":          "read-write",
		"/etc/kista":              "not a mount point",
		"/etc/kista/signing":      "not a mount point",
	} {
		_, err := CheckMount([]byte(table), dir)
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", dir, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: %v, want %q", dir, err, want)
		}
	}
	// the last mount on a point is the one in effect
	stacked := table + "1400 1361 0:400 / /etc/kista/signing/prod rw,relatime - tmpfs tmpfs rw\n"
	if _, err := CheckMount([]byte(stacked), "/etc/kista/signing/prod"); err == nil {
		t.Error("a writable mount over the Secret volume accepted")
	}
	// a read-only tmpfs over a writable one is the mount in effect
	roOverRW := table + "1400 1363 0:401 / /etc/kista/mem ro,relatime - tmpfs tmpfs rw\n"
	if m, err := CheckMount([]byte(roOverRW), "/etc/kista/mem"); err != nil || m.Minor != 401 {
		t.Errorf("ro over rw: %+v %v", m, err)
	}
	for _, bad := range []string{"1 2 3\n", "1 2 0:1 / /x ro - \n", "1 2 0:1 / /x\\04 ro - tmpfs tmpfs rw\n", "1 2 0:1 / /x\\999 ro - tmpfs tmpfs rw\n",
		"1 2 nodev / /x ro - tmpfs tmpfs rw\n", "1 2 0:x / /x ro - tmpfs tmpfs rw\n"} {
		if _, err := ParseMountInfo([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// refused opens a volume after a change, and wants ErrKey with text in the error.
func refused(t *testing.T, s *Source, ref, want string) {
	t.Helper()
	_, err := registry(t, s, keysource.Options{}).Open(ctx, ref)
	if !errors.Is(err, keysource.ErrKey) || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want ErrKey with %q", err, want)
	}
}

func TestMountRefusals(t *testing.T) {
	v := newVolume(t, map[string][]byte{"ext-prod": pemOf(t, key2048)})
	t.Run("not a mount point", func(t *testing.T) {
		refused(t, v.source(t, baseMounts), "k8s:ext-prod", "not a mount point")
	})
	t.Run("a ConfigMap on disk", func(t *testing.T) {
		refused(t, v.source(t, baseMounts+v.mountLine("ro,relatime", "ext4")), "k8s:ext-prod", "not tmpfs")
	})
	t.Run("writable tmpfs", func(t *testing.T) {
		refused(t, v.source(t, baseMounts+v.mountLine("rw,relatime", "tmpfs")), "k8s:ext-prod", "read-write")
	})
	t.Run("a read-only mount over a writable one", func(t *testing.T) {
		s := v.source(t, baseMounts+deviceLine(0, 999, v.dir, "rw,relatime", "tmpfs")+v.mountLine("ro,relatime", "tmpfs"))
		if _, err := registry(t, s, keysource.Options{}).Open(ctx, "k8s:ext-prod"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the directory not on its mount (a later mount over a parent hides it)", func(t *testing.T) {
		fi, _ := os.Stat(v.dir)
		major, minor, _ := devOf(fi)
		refused(t, v.source(t, baseMounts+deviceLine(major, minor+1, v.dir, "ro,relatime", "tmpfs")), "k8s:ext-prod", "is not on the mount")
	})
	t.Run("an unreadable table", func(t *testing.T) {
		refused(t, v.source(t, "garbage\n"), "k8s:ext-prod", "format")
	})
	t.Run("another platform", func(t *testing.T) {
		s, _ := New(v.dir, Options{MountInfo: func() ([]byte, error) { return nil, ErrUnsupported }})
		refused(t, s, "k8s:ext-prod", "needs Linux")
	})
	if runtime.GOOS != "linux" {
		t.Run("the default here", func(t *testing.T) {
			s, _ := New(v.dir, Options{})
			refused(t, s, "k8s:ext-prod", "needs Linux")
		})
	}
}

func TestLayoutRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(t *testing.T, v *volume)
		want   string
	}{
		"no ..data": {func(t *testing.T, v *volume) { must(t, os.Remove(filepath.Join(v.dir, "..data"))) }, "no ..data"},
		"..data a directory": {func(t *testing.T, v *volume) {
			must(t, os.Remove(filepath.Join(v.dir, "..data")))
			must(t, os.Mkdir(filepath.Join(v.dir, "..data"), 0o755))
		}, "not a link"},
		"..data absolute": {func(t *testing.T, v *volume) { relink(t, filepath.Join(v.dir, "..data"), v.current()) }, "..<timestamp>"},
		"..data leaving dir": {func(t *testing.T, v *volume) {
			relink(t, filepath.Join(v.dir, "..data"), "../"+filepath.Base(v.current()))
		}, "..<timestamp>"},
		"..data to a file": {func(t *testing.T, v *volume) {
			must(t, os.WriteFile(filepath.Join(v.dir, "..x"), nil, 0o600))
			relink(t, filepath.Join(v.dir, "..data"), "..x")
		}, "does not lead to a directory"},
		"the key a plain file (a copy)": {func(t *testing.T, v *volume) {
			must(t, os.Remove(filepath.Join(v.dir, "ext-prod")))
			must(t, os.WriteFile(filepath.Join(v.dir, "ext-prod"), pemOf(t, key2048), 0o440))
		}, "not the link"},
		"the key linked absolutely": {func(t *testing.T, v *volume) {
			relink(t, filepath.Join(v.dir, "ext-prod"), filepath.Join(v.current(), "ext-prod"))
		}, "relative link ..data/ext-prod"},
		"the key linked past ..data": {func(t *testing.T, v *volume) {
			relink(t, filepath.Join(v.dir, "ext-prod"), filepath.Base(v.current())+"/ext-prod")
		}, "relative link ..data/ext-prod"},
		"a link leaving dir": {func(t *testing.T, v *volume) {
			outside := filepath.Join(t.TempDir(), "k.pem")
			must(t, os.WriteFile(outside, pemOf(t, key2048), 0o440))
			p := filepath.Join(v.current(), "ext-prod")
			must(t, os.Remove(p))
			must(t, os.Symlink(outside, p))
		}, "may not leave the directory"},
		"a relative link leaving dir": {func(t *testing.T, v *volume) {
			outside := filepath.Join(filepath.Dir(v.dir), "k.pem")
			must(t, os.WriteFile(outside, pemOf(t, key2048), 0o440))
			p := filepath.Join(v.current(), "ext-prod")
			must(t, os.Remove(p))
			must(t, os.Symlink("../../k.pem", p))
		}, "may not leave the directory"},
		"the file removed": {func(t *testing.T, v *volume) { must(t, os.Remove(filepath.Join(v.current(), "ext-prod"))) }, "ext-prod"},
		"not a regular file": {func(t *testing.T, v *volume) {
			p := filepath.Join(v.current(), "ext-prod")
			must(t, os.Remove(p))
			must(t, os.Mkdir(p, 0o755))
		}, "not a regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			v := newVolume(t, map[string][]byte{"ext-prod": pemOf(t, key2048)})
			tc.mutate(t, v)
			refused(t, v.secretSource(t), "k8s:ext-prod", tc.want)
		})
	}
}

func TestFileRefusals(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o640, 0o444, 0o460, 0o540} {
		t.Run(fmt.Sprintf("mode %04o", mode), func(t *testing.T) {
			v := newVolume(t, map[string][]byte{"ext-prod": pemOf(t, key2048)})
			must(t, os.Chmod(filepath.Join(v.current(), "ext-prod"), mode))
			refused(t, v.secretSource(t), "k8s:ext-prod", "mode 0440 or narrower")
		})
	}
	t.Run("mode 0400 is accepted", func(t *testing.T) {
		v := newVolume(t, map[string][]byte{"ext-prod": pemOf(t, key2048)})
		must(t, os.Chmod(filepath.Join(v.current(), "ext-prod"), 0o400))
		if _, err := registry(t, v.secretSource(t), keysource.Options{}).Open(ctx, "k8s:ext-prod"); err != nil {
			t.Fatal(err)
		}
	})
	v := newVolume(t, map[string][]byte{"ext-prod": pemOf(t, key2048)})
	uid, gid := uint32(os.Geteuid()), v.gid //nolint:gosec // a uid fits
	line := v.mountLine("ro", "tmpfs")
	sec := func(owner, group uint32) *Source {
		s, _ := New(v.dir, Options{MountInfo: func() ([]byte, error) { return []byte(line), nil },
			Owner: &owner, Group: &group})
		return s
	}
	t.Run("another owner (shape (a): a file owned by kista)", func(t *testing.T) {
		refused(t, sec(uid+1, gid), "k8s:ext-prod", "must be owned by")
	})
	t.Run("another group", func(t *testing.T) {
		refused(t, sec(uid, gid+1), "k8s:ext-prod", "must be owned by")
	})
	t.Run("kista in group 0", func(t *testing.T) {
		refused(t, sec(uid, 0), "k8s:ext-prod", "group 0")
	})
	t.Run("two hard links", func(t *testing.T) {
		v := newVolume(t, map[string][]byte{"ext-prod": pemOf(t, key2048)})
		must(t, os.Link(filepath.Join(v.current(), "ext-prod"), filepath.Join(v.current(), "copy")))
		refused(t, v.secretSource(t), "k8s:ext-prod", "2 hard links")
	})
	t.Run("too large", func(t *testing.T) {
		v := newVolume(t, map[string][]byte{"ext-prod": make([]byte, signer.MaxKeyFile+1)})
		refused(t, v.secretSource(t), "k8s:ext-prod", "larger than")
	})
	t.Run("not a key, and no content in the error", func(t *testing.T) {
		v := newVolume(t, map[string][]byte{"ext-prod": []byte(sourcetest.Secret)})
		_, err := registry(t, v.secretSource(t), keysource.Options{}).Open(ctx, "k8s:ext-prod")
		if !errors.Is(err, keysource.ErrKey) || strings.Contains(err.Error(), sourcetest.Secret) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("an encrypted key", func(t *testing.T) {
		b := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("x")})
		v := newVolume(t, map[string][]byte{"ext-prod": b})
		refused(t, v.secretSource(t), "k8s:ext-prod", "encrypted")
	})
	t.Run("a key not in the allow list", func(t *testing.T) {
		v := newVolume(t, map[string][]byte{"prod": pemOf(t, key2048)})
		if _, err := registry(t, v.secretSource(t), keysource.Options{}).Open(ctx, "k8s:prod"); !errors.Is(err, keysource.ErrReference) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestAttestationRefused(t *testing.T) {
	v := newVolume(t, map[string][]byte{"ext-prod": pemOf(t, key2048)})
	_, err := registry(t, v.secretSource(t), keysource.Options{}).OpenPurpose(ctx, "k8s:ext-prod", keysource.PurposeAttestation)
	if !errors.Is(err, keysource.ErrKey) || !strings.Contains(err.Error(), "channel keys only") {
		t.Fatalf("got %v", err)
	}
}

// While in use: every re-check reads the file again with every check.
func TestRecheck(t *testing.T) {
	h := extfile.BodyHash(sha256.Sum256([]byte("body")))
	open := func(t *testing.T, mounts *atomic.Value) (*volume, func() error) {
		v := newVolume(t, map[string][]byte{"ext-prod": pemOf(t, key2048)})
		mounts.Store(baseMounts + v.mountLine("ro,relatime", "tmpfs"))
		uid := uint32(os.Geteuid()) //nolint:gosec // a uid fits
		s, _ := New(v.dir, Options{MountInfo: func() ([]byte, error) { return []byte(mounts.Load().(string)), nil }, Owner: &uid, Group: &v.gid})
		sg, err := registry(t, s, keysource.Options{Recheck: time.Nanosecond}).Open(ctx, "k8s:ext-prod")
		if err != nil {
			t.Fatal(err)
		}
		return v, func() error { _, err := sg.Sign(ctx, h); return err }
	}
	t.Run("the same key rewritten in a new ..data keeps signing", func(t *testing.T) {
		var m atomic.Value
		v, sign := open(t, &m)
		v.write(map[string][]byte{"ext-prod": pemOf(t, key2048), "ext-prod-2": pemOf(t, other)})
		if err := sign(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("another key under the name", func(t *testing.T) {
		var m atomic.Value
		v, sign := open(t, &m)
		v.write(map[string][]byte{"ext-prod": pemOf(t, other)})
		if err := sign(); !errors.Is(err, keysource.ErrKey) || !strings.Contains(err.Error(), "another key") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("the data key removed", func(t *testing.T) {
		var m atomic.Value
		v, sign := open(t, &m)
		must(t, os.Remove(filepath.Join(v.dir, "ext-prod")))
		if err := sign(); !errors.Is(err, keysource.ErrKey) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("the mount gone", func(t *testing.T) {
		var m atomic.Value
		_, sign := open(t, &m)
		m.Store(baseMounts)
		if err := sign(); !errors.Is(err, keysource.ErrKey) {
			t.Fatalf("got %v", err)
		}
	})
}

// A file on another device than the mount (a ConfigMap item mounted with subPath over the key) is
// refused; the same file on the mount's device passes.
func TestCheckDevice(t *testing.T) {
	v := newVolume(t, map[string][]byte{"ext-prod": pemOf(t, key2048)})
	fi, err := os.Stat(filepath.Join(v.current(), "ext-prod"))
	if err != nil {
		t.Fatal(err)
	}
	major, minor, ok := devOf(fi)
	if !ok {
		t.Fatal("no device numbers")
	}
	if err := checkDevice(Mount{Major: major, Minor: minor}, fi, "x"); err != nil {
		t.Fatal(err)
	}
	for _, m := range []Mount{{Major: major, Minor: minor + 1}, {Major: major + 1, Minor: minor}} {
		if err := checkDevice(m, fi, "the key is on another filesystem"); err == nil || !strings.Contains(err.Error(), "another filesystem") {
			t.Errorf("%+v: %v", m, err)
		}
	}
}

// Opens honour the caller's context, and only a file found missing (a ..data swap in flight) is
// looked for again: a mode, an owner or a mount that does not fit is final.
func TestLoadRetries(t *testing.T) {
	counting := func(v *volume, line string) (*Source, *atomic.Int64) {
		var n atomic.Int64
		uid := uint32(os.Geteuid()) //nolint:gosec // a uid fits
		s, _ := New(v.dir, Options{MountInfo: func() ([]byte, error) { n.Add(1); return []byte(line), nil }, Owner: &uid, Group: &v.gid})
		return s, &n
	}
	t.Run("a cancelled context", func(t *testing.T) {
		v := newVolume(t, map[string][]byte{"ext-prod": pemOf(t, key2048)})
		s, n := counting(v, v.mountLine("ro", "tmpfs"))
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := registry(t, s, keysource.Options{}).Open(cctx, "k8s:ext-prod"); !errors.Is(err, context.Canceled) || n.Load() != 0 {
			t.Fatalf("got %v after %d reads", err, n.Load())
		}
	})
	t.Run("a wrong mode is final", func(t *testing.T) {
		v := newVolume(t, map[string][]byte{"ext-prod": pemOf(t, key2048)})
		must(t, os.Chmod(filepath.Join(v.current(), "ext-prod"), 0o640))
		s, n := counting(v, v.mountLine("ro", "tmpfs"))
		refused(t, s, "k8s:ext-prod", "mode 0440 or narrower")
		if n.Load() != 1 {
			t.Fatalf("%d attempts", n.Load())
		}
	})
	t.Run("a writable mount is final", func(t *testing.T) {
		v := newVolume(t, map[string][]byte{"ext-prod": pemOf(t, key2048)})
		s, n := counting(v, v.mountLine("rw", "tmpfs"))
		refused(t, s, "k8s:ext-prod", "read-write")
		if n.Load() != 1 {
			t.Fatalf("%d attempts", n.Load())
		}
	})
	t.Run("a missing file is looked for again", func(t *testing.T) {
		v := newVolume(t, map[string][]byte{"ext-prod": pemOf(t, key2048)})
		must(t, os.Remove(filepath.Join(v.current(), "ext-prod")))
		s, n := counting(v, v.mountLine("ro", "tmpfs"))
		refused(t, s, "k8s:ext-prod", "ext-prod")
		if n.Load() != 2 {
			t.Fatalf("%d attempts", n.Load())
		}
	})
}

// TestRealVolume opens a real Secret-like volume with the production checks, no seam: the
// directory KISTA_TEST_KUBESECRET_DIR is a read-only tmpfs mount (Linux) in the kubelet's layout,
// holding ext-test, root-owned with this process's group and mode 0440. A writable or missing mount
// at KISTA_TEST_KUBESECRET_RW_DIR, when set, is refused.
func TestRealVolume(t *testing.T) {
	dir := os.Getenv("KISTA_TEST_KUBESECRET_DIR")
	if dir == "" {
		t.Skip("KISTA_TEST_KUBESECRET_DIR is not set")
	}
	s, err := New(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	sg, err := registry(t, s, keysource.Options{}).Open(ctx, "k8s:ext-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sg.Sign(ctx, extfile.BodyHash(sha256.Sum256([]byte("body")))); err != nil {
		t.Fatal(err)
	}
	// refused look-alikes, each named by a variable: a writable tmpfs; a file bind-mounted over the
	// key (a subPath); a volume hidden by a later mount over its parent, with its path made again
	for env, want := range map[string]string{
		"KISTA_TEST_KUBESECRET_RW_DIR":      "read-write",
		"KISTA_TEST_KUBESECRET_SUBPATH_DIR": "another filesystem",
		"KISTA_TEST_KUBESECRET_HIDDEN_DIR":  "is not on the mount",
	} {
		dir := os.Getenv(env)
		if dir == "" {
			continue
		}
		s, err := New(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := registry(t, s, keysource.Options{}).Open(ctx, "k8s:ext-test"); !errors.Is(err, keysource.ErrKey) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", env, err, want)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// relink replaces a link.
func relink(t *testing.T, link, target string) {
	t.Helper()
	must(t, os.Remove(link))
	must(t, os.Symlink(target, link))
}
