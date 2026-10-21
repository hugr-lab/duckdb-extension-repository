//go:build !unix

package kubesecret

import "io/fs"

type ids struct {
	uid, gid uint32
	nlink    uint64
}

func owner(fs.FileInfo) (ids, bool) { return ids{}, false }
