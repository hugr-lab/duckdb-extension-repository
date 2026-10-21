//go:build !unix

package kubesecret

import "io/fs"

func devOf(fs.FileInfo) (major, minor uint32, ok bool) { return 0, 0, false }
