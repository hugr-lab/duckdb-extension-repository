//go:build unix

package signer

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const unixPerms = true

func ownedByCurrentUser(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// openNoFollow opens path for reading without following a final symlink.
func openNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, fmt.Errorf("%s is a symlink", path)
	}
	return f, err
}
