package kubesecret

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Mount is one line of /proc/self/mountinfo, the fields the source reads.
type Mount struct {
	Point   string   // the mount point, unescaped
	Options []string // the per-mount options (ro or rw, and the rest)
	FSType  string
}

// ParseMountInfo parses /proc/self/mountinfo (proc(5)): "id parent major:minor root point options
// [optional fields...] - fstype source super-options". A line it cannot read is an error: the
// table decides whether a key may sign.
func ParseMountInfo(data []byte) ([]Mount, error) {
	var out []Mount
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		sep := slices.Index(f, "-")
		if sep < 6 || sep+1 >= len(f) {
			return nil, fmt.Errorf("kubesecret: mountinfo line %d is not in the kernel's format", n)
		}
		point, err := unescape(f[4])
		if err != nil {
			return nil, fmt.Errorf("kubesecret: mountinfo line %d: %v", n, err)
		}
		out = append(out, Mount{Point: point, Options: strings.Split(f[5], ","), FSType: f[sep+1]})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("kubesecret: reading mountinfo: %v", err)
	}
	return out, nil
}

// unescape undoes the kernel's octal escapes in a path (\040 for a space, \011, \012, \134).
func unescape(s string) (string, error) {
	if !strings.Contains(s, `\`) {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+4 > len(s) {
			return "", errors.New("a truncated escape in a mount point")
		}
		v, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
		if err != nil {
			return "", errors.New("a bad escape in a mount point")
		}
		b.WriteByte(byte(v))
		i += 3
	}
	return b.String(), nil
}

// CheckMount checks that dir is a mount point, tmpfs, mounted read-only, as the kubelet mounts a
// Secret volume (and a projected one). The last line for dir is the mount in effect: a later mount
// on the same point hides the earlier ones.
func CheckMount(mountinfo []byte, dir string) error {
	mounts, err := ParseMountInfo(mountinfo)
	if err != nil {
		return err
	}
	var m *Mount
	for i := range mounts {
		if mounts[i].Point == dir {
			m = &mounts[i]
		}
	}
	switch {
	case m == nil:
		return fmt.Errorf("kubesecret: %s is not a mount point: mount the Secret volume there", dir)
	case m.FSType != "tmpfs":
		return fmt.Errorf("kubesecret: %s is a %s mount, not tmpfs: a Secret volume is tmpfs (a ConfigMap, hostPath or PVC is not)", dir, m.FSType)
	case !slices.Contains(m.Options, "ro"):
		return fmt.Errorf("kubesecret: %s is mounted read-write: a Secret volume is mounted read-only (an emptyDir is not)", dir)
	}
	return nil
}
