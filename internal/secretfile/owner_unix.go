//go:build unix

package secretfile

import (
	"io/fs"
	"os"
	"syscall"
)

const nonblock = syscall.O_NONBLOCK

func owner(fi fs.FileInfo) (ids, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ids{}, false
	}
	return ids{uid: st.Uid, gid: st.Gid, nlink: uint64(st.Nlink)}, true //nolint:unconvert // uint16 on darwin
}

// blocking clears O_NONBLOCK on a regular file's descriptor.
func blocking(f *os.File) {
	if rc, err := f.SyscallConn(); err == nil {
		_ = rc.Control(func(fd uintptr) { _ = syscall.SetNonblock(int(fd), false) })
	}
}
