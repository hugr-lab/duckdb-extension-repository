//go:build !unix

package secretfile

import (
	"io/fs"
	"os"
)

const nonblock = 0

func owner(fs.FileInfo) (ids, bool) { return ids{}, false }

func blocking(*os.File) {}
