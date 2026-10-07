//go:build unix

package blob

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

func ownedByMe(fi fs.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("blob: cannot read the spool_dir owner")
	}
	if int(st.Uid) != os.Geteuid() {
		return errors.New("blob: spool_dir is owned by another user")
	}
	return nil
}
