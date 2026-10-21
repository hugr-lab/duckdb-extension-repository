//go:build linux

package kubesecret

import (
	"io/fs"
	"syscall"
)

// devOf is the device a file is on, as major:minor in mountinfo's terms (glibc's encoding of
// st_dev, which the kernel's new_encode_dev matches).
func devOf(fi fs.FileInfo) (major, minor uint32, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	d := uint64(st.Dev) //nolint:unconvert // uint64 on every Linux arch
	major = uint32((d>>8)&0xfff | (d>>32)&^0xfff)
	minor = uint32(d&0xff | (d>>12)&^0xff)
	return major, minor, true
}
