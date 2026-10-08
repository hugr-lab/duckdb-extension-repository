// Package symtest builds the smallest x86-64 ELF shared object that exports given functions, for
// tests of code that runs spec 0008's entry-point check (the readers' own tests build every format
// and their malformed variants).
package symtest

import "encoding/binary"

// ELF returns a linux_amd64 shared object exporting the named functions.
func ELF(exports ...string) []byte {
	le16 := binary.LittleEndian.PutUint16
	le32 := binary.LittleEndian.PutUint32
	le64 := binary.LittleEndian.PutUint64
	strtab := []byte{0}
	syms := make([]byte, 24)
	for _, n := range exports {
		e := make([]byte, 24)
		le32(e, uint32(len(strtab)))
		strtab = append(append(strtab, n...), 0)
		e[4] = 1<<4 | 2 // global function
		le16(e[6:], 7)  // defined
		syms = append(syms, e...)
	}
	const hdr, ph = 64, 56
	nsyms := len(exports) + 1
	strOff := hdr + 2*ph
	symOff := strOff + len(strtab)
	symOff += (8 - symOff%8) % 8
	hashOff := symOff + len(syms) // DT_HASH: one bucket, nchain = the symbols
	dynOff := hashOff + 8 + 4 + 4*nsyms
	dynOff += (8 - dynOff%8) % 8
	shOff := dynOff + 96
	total := shOff + 3*64
	b := make([]byte, total)
	copy(b, "\x7fELF")
	b[4], b[5], b[6] = 2, 1, 1
	le16(b[16:], 3)  // ET_DYN
	le16(b[18:], 62) // x86-64
	le64(b[32:], hdr)
	le64(b[40:], uint64(shOff))
	le16(b[54:], ph)
	le16(b[56:], 2)
	le16(b[58:], 64)
	le16(b[60:], 3)
	le32(b[hdr:], 1) // PT_LOAD: the whole file at address = offset
	le64(b[hdr+32:], uint64(total))
	le64(b[hdr+40:], uint64(total))
	le32(b[hdr+ph:], 2) // PT_DYNAMIC
	le64(b[hdr+ph+8:], uint64(dynOff))
	le64(b[hdr+ph+16:], uint64(dynOff))
	le64(b[hdr+ph+32:], 96)
	copy(b[strOff:], strtab)
	copy(b[symOff:], syms)
	le32(b[hashOff:], 1)
	le32(b[hashOff+4:], uint32(nsyms))
	for i, kv := range [][2]uint64{{6, uint64(symOff)}, {5, uint64(strOff)}, {10, uint64(len(strtab))}, {11, 24}, {4, uint64(hashOff)}} {
		le64(b[dynOff+16*i:], kv[0]) // DT_SYMTAB, DT_STRTAB, DT_STRSZ, DT_SYMENT, DT_HASH
		le64(b[dynOff+16*i+8:], kv[1])
	}
	s1, s2 := shOff+64, shOff+128
	le32(b[s1+4:], 11) // SHT_DYNSYM
	le64(b[s1+16:], uint64(symOff))
	le64(b[s1+24:], uint64(symOff))
	le64(b[s1+32:], uint64(len(syms)))
	le32(b[s1+40:], 2)
	le32(b[s2+4:], 3) // SHT_STRTAB
	le64(b[s2+16:], uint64(strOff))
	le64(b[s2+24:], uint64(strOff))
	le64(b[s2+32:], uint64(len(strtab)))
	return b
}
