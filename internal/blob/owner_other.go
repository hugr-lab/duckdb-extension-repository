//go:build !unix

package blob

import "io/fs"

func ownedByMe(fs.FileInfo) error { return nil }
