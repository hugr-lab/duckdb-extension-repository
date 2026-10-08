package symbols

import (
	"fmt"
	"slices"
)

// ELF constants (the System V gABI and the GNU extensions).
const (
	etDyn        = 3
	emX8664      = 62
	emAArch64    = 183
	ptLoad       = 1
	ptDynamic    = 2
	dtNull       = 0
	dtHash       = 4
	dtStrtab     = 5
	dtSymtab     = 6
	dtStrsz      = 10
	dtSyment     = 11
	dtGNUHash    = 0x6ffffef5
	shtDynsym    = 11
	shfCompress  = 0x800
	stbLocal     = 0
	stbGlobal    = 1
	stbWeak      = 2
	sttFunc      = 2
	stvDefault   = 0
	stvProtected = 3
	symSize      = 24
	maxBuckets   = 1 << 24
)

type segment struct{ vaddr, off, filesz, memsz uint64 }

// elfScan reads a 64-bit little-endian ELF shared object as the dynamic loader does: the dynamic
// array at the address PT_DYNAMIC maps, the symbol table DT_SYMTAB points to with as many entries
// as the hash tables reach (the SHT_DYNSYM section must agree and cover them), names bounded by
// DT_STRSZ. Every non-local symbol is visited; a defined global or weak function with default or
// protected visibility is a strict export.
func elfScan(rd *reader, machine string, v visit) error {
	id, err := rd.bytes(0, 64)
	if err != nil || string(id[:4]) != "\x7fELF" {
		return fmt.Errorf("%w: not an ELF file", ErrFormat)
	}
	if id[4] != 2 || id[5] != 1 {
		return fmt.Errorf("%w: not a 64-bit little-endian ELF file", ErrFormat)
	}
	etype, _ := rd.u16(16)
	emach, _ := rd.u16(18)
	if etype != etDyn {
		return fmt.Errorf("%w: not an ELF shared object", ErrFormat)
	}
	if want := map[string]uint16{"amd64": emX8664, "arm64": emAArch64}[machine]; emach != want {
		return fmt.Errorf("%w: the ELF machine is not the platform's", ErrFormat)
	}
	phoff, _ := rd.u64(32)
	shoff, _ := rd.u64(40)
	phentsize, _ := rd.u16(54)
	phnum, _ := rd.u16(56)
	shentsize, _ := rd.u16(58)
	shnum, _ := rd.u16(60)
	if phentsize < 56 || shentsize < 64 {
		return fmt.Errorf("%w: malformed ELF headers", ErrFormat)
	}

	// program headers: the loadable segments (sorted, not overlapping: the loader maps them in
	// order) and the dynamic segment
	var loads []segment
	var dyn *segment
	for i := range uint64(phnum) {
		p := int64(phoff + i*uint64(phentsize))
		typ, err := rd.u32(p)
		if err != nil {
			return err
		}
		off, _ := rd.u64(p + 8)
		vaddr, _ := rd.u64(p + 16)
		filesz, _ := rd.u64(p + 32)
		memsz, err := rd.u64(p + 40)
		if err != nil {
			return err
		}
		switch typ {
		case ptLoad:
			if filesz > memsz || vaddr+memsz < vaddr || off+filesz < off {
				return fmt.Errorf("%w: a malformed loadable segment", ErrFormat)
			}
			loads = append(loads, segment{vaddr, off, filesz, memsz})
		case ptDynamic:
			if dyn != nil {
				return fmt.Errorf("%w: two dynamic segments", ErrFormat)
			}
			dyn = &segment{vaddr, off, filesz, memsz}
		}
	}
	if !slices.IsSortedFunc(loads, func(a, b segment) int { return cmpU64(a.vaddr, b.vaddr) }) {
		return fmt.Errorf("%w: loadable segments out of order", ErrFormat)
	}
	for i := 1; i < len(loads); i++ {
		if loads[i].vaddr < loads[i-1].vaddr+loads[i-1].memsz {
			return fmt.Errorf("%w: overlapping loadable segments", ErrFormat)
		}
	}
	if dyn == nil {
		return fmt.Errorf("%w: the ELF file has no dynamic segment", ErrFormat)
	}
	if _, ok := fileOff(loads, dyn.vaddr, dyn.filesz); !ok {
		return fmt.Errorf("%w: the dynamic segment is not where the loader maps it", ErrFormat)
	}
	// the dynamic array, read where the loader reads it
	dynOff, _ := fileOff(loads, dyn.vaddr, dyn.filesz)
	var symtab, strtab, strsz, syment, hash, gnuHash uint64
	for o := uint64(0); o+16 <= dyn.filesz; o += 16 {
		if err := rd.tick(); err != nil {
			return err
		}
		tag, err := rd.u64(int64(dynOff + o))
		if err != nil {
			return err
		}
		val, _ := rd.u64(int64(dynOff + o + 8))
		if tag == dtNull {
			break
		}
		switch tag {
		case dtSymtab:
			symtab = val
		case dtStrtab:
			strtab = val
		case dtStrsz:
			strsz = val
		case dtSyment:
			syment = val
		case dtHash:
			hash = val
		case dtGNUHash:
			gnuHash = val
		}
	}
	if symtab == 0 || strtab == 0 || strsz == 0 {
		return fmt.Errorf("%w: the ELF file has no dynamic symbol table", ErrFormat)
	}
	if syment != symSize {
		return fmt.Errorf("%w: the dynamic symbol entry size is not 24", ErrFormat)
	}
	// how many symbols the loader can reach through the hash tables
	count := uint64(0)
	if hash != 0 {
		off, ok := fileOff(loads, hash, 8)
		if !ok {
			return errBounds
		}
		nchain, err := rd.u32(int64(off) + 4)
		if err != nil {
			return err
		}
		count = uint64(nchain)
	}
	if gnuHash != 0 {
		n, err := gnuCount(rd, loads, gnuHash)
		if err != nil {
			return err
		}
		count = max(count, n)
	}
	if hash == 0 && gnuHash == 0 {
		return fmt.Errorf("%w: the ELF file has no symbol hash table", ErrFormat)
	}

	// the section header of the dynamic symbol table must agree and cover what the hashes reach
	var secCount uint64
	found := false
	for i := range uint64(shnum) {
		s := int64(shoff + i*uint64(shentsize))
		typ, err := rd.u32(s + 4)
		if err != nil {
			return err
		}
		if typ != shtDynsym {
			continue
		}
		flags, _ := rd.u64(s + 8)
		addr, _ := rd.u64(s + 16)
		off, _ := rd.u64(s + 24)
		size, err := rd.u64(s + 32)
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf("%w: two dynamic symbol tables", ErrFormat)
		}
		if mo, ok := fileOff(loads, symtab, size); addr != symtab || flags&shfCompress != 0 || !ok || mo != off {
			return fmt.Errorf("%w: the dynamic symbol table's section disagrees with the dynamic segment", ErrFormat)
		}
		secCount, found = size/symSize, true
	}
	if !found {
		return fmt.Errorf("%w: the ELF file has no dynamic symbol section", ErrFormat)
	}
	if count > secCount {
		return fmt.Errorf("%w: the hash tables reach past the dynamic symbol section", ErrFormat)
	}
	n := secCount // every entry the section holds, which covers every entry the loader reaches
	if n > maxSymbols {
		return errBounds
	}
	symOff, ok := fileOff(loads, symtab, n*symSize)
	if !ok {
		return fmt.Errorf("%w: the dynamic symbol table is not where the loader maps it", ErrFormat)
	}
	strOff, ok := fileOff(loads, strtab, strsz)
	if !ok {
		return fmt.Errorf("%w: the dynamic string table is not where the loader maps it", ErrFormat)
	}
	for i := uint64(1); i < n; i++ { // entry 0 is the undefined symbol
		if err := rd.tick(); err != nil {
			return err
		}
		e, err := rd.bytes(int64(symOff+i*symSize), symSize)
		if err != nil {
			return err
		}
		name := uint64(e[0]) | uint64(e[1])<<8 | uint64(e[2])<<16 | uint64(e[3])<<24
		info, other := e[4], e[5]
		shndx := uint16(e[6]) | uint16(e[7])<<8
		bind, typ, vis := info>>4, info&0xf, other&3
		if bind == stbLocal {
			continue
		}
		if name >= strsz {
			return errBounds
		}
		s, err := rd.cstring(int64(strOff+name), int64(strOff+strsz))
		if err != nil {
			return err
		}
		strict := shndx != 0 && typ == sttFunc && (bind == stbGlobal || bind == stbWeak) && (vis == stvDefault || vis == stvProtected)
		if err := v(s, strict); err != nil {
			return err
		}
	}
	return nil
}

// gnuCount is the number of symbols a DT_GNU_HASH table reaches: past the highest bucket's chain.
func gnuCount(rd *reader, loads []segment, addr uint64) (uint64, error) {
	off, ok := fileOff(loads, addr, 16)
	if !ok {
		return 0, errBounds
	}
	nbuckets, _ := rd.u32(int64(off))
	symoffset, _ := rd.u32(int64(off) + 4)
	bloomSize, err := rd.u32(int64(off) + 8)
	if err != nil {
		return 0, err
	}
	if nbuckets > maxBuckets || bloomSize > maxBuckets {
		return 0, errBounds
	}
	buckets := off + 16 + uint64(bloomSize)*8
	if _, ok := fileOff(loads, addr+16+uint64(bloomSize)*8, uint64(nbuckets)*4); !ok {
		return 0, errBounds
	}
	top := uint64(0)
	for i := range uint64(nbuckets) {
		if err := rd.tick(); err != nil {
			return 0, err
		}
		b, err := rd.u32(int64(buckets + i*4))
		if err != nil {
			return 0, err
		}
		top = max(top, uint64(b))
	}
	if top < uint64(symoffset) {
		return uint64(symoffset), nil
	}
	chains := buckets + uint64(nbuckets)*4
	for i := top; ; i++ {
		if err := rd.tick(); err != nil {
			return 0, err
		}
		if i-uint64(symoffset) > maxSymbols {
			return 0, errBounds
		}
		c, err := rd.u32(int64(chains + (i-uint64(symoffset))*4))
		if err != nil {
			return 0, err
		}
		if c&1 != 0 {
			return i + 1, nil
		}
	}
}

// fileOff maps [addr, addr+size) to the file through the PT_LOAD segment that holds it.
func fileOff(loads []segment, addr, size uint64) (uint64, bool) {
	for _, l := range loads {
		if addr >= l.vaddr && addr-l.vaddr < l.filesz && size <= l.filesz-(addr-l.vaddr) {
			return l.off + (addr - l.vaddr), true
		}
	}
	return 0, false
}

func cmpU64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
