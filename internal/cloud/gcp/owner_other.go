//go:build !unix

package gcp

import "os"

func ownedByUs(os.FileInfo) bool { return true }
