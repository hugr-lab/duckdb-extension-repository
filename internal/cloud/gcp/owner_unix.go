//go:build unix

package gcp

import (
	"os"
	"syscall"
)

// ownedByUs reports whether a file is owned by root (a Kubernetes projection) or kista's own user.
func ownedByUs(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && (s.Uid == 0 || int(s.Uid) == os.Getuid())
}
