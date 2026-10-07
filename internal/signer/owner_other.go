//go:build !unix

package signer

import "os"

// Outside unix the owner and mode checks are skipped (spec 0002: documented).
const unixPerms = false

func ownedByCurrentUser(os.FileInfo) bool { return true }

func openNoFollow(path string) (*os.File, error) { return os.Open(path) }
