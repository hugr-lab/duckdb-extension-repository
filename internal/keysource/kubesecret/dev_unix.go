//go:build unix && !linux

package kubesecret

import (
	"io/fs"
	"syscall"
)

// devOf is the device a file is on, split as BSD and macOS do (major in the top byte). The kind is
// refused here (no mountinfo); this serves the tests, which write their mount table with it.
func devOf(fi fs.FileInfo) (major, minor uint32, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	d := uint64(st.Dev) //nolint:gosec,unconvert // int32 on darwin, uint64 elsewhere
	return uint32(d>>24) & 0xff, uint32(d) & 0xffffff, true
}
