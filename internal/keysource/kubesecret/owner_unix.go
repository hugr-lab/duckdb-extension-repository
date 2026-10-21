//go:build unix

package kubesecret

import (
	"io/fs"
	"syscall"
)

type ids struct {
	uid, gid uint32
	nlink    uint64
}

func owner(fi fs.FileInfo) (ids, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ids{}, false
	}
	return ids{uid: st.Uid, gid: st.Gid, nlink: uint64(st.Nlink)}, true //nolint:unconvert // uint16 on darwin
}
