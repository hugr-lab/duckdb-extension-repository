package symbols

import (
	"fmt"
	"strings"
)

// Mach-O constants (<mach-o/loader.h>, <mach-o/nlist.h>).
const (
	mhMagic64         = 0xfeedfacf
	fatMagic          = 0xcafebabe
	fatCigam          = 0xbebafeca
	mhDylib           = 6
	mhBundle          = 8
	cpuX8664          = 0x01000007
	cpuARM64          = 0x0100000c
	lcSymtab          = 0x2
	lcDysymtab        = 0xb
	lcDyldInfo        = 0x22
	lcDyldInfoOnly    = 0x80000022
	lcDyldExportsTrie = 0x80000033
	nExt              = 0x01
	nPExt             = 0x10
	nTypeMask         = 0x0e
	nSect             = 0x0e
	nlistSize         = 16
	exportReexport    = 0x08
	maxTrieDepth      = 128
	maxLoadCommands   = 1 << 16
)

// machoScan reads a thin 64-bit Mach-O dylib or bundle: the entries of its export trie, or,
// without one, the external symbols of LC_DYSYMTAB. Names lose the leading "_". A file with two
// sources of exports (both trie commands, or one twice) is refused: the checked one must be the
// one dyld reads.
func machoScan(rd *reader, machine string, v visit) error {
	magic, err := rd.u32(0)
	if err != nil {
		return fmt.Errorf("%w: not a Mach-O file", ErrFormat)
	}
	if magic == fatMagic || magic == fatCigam {
		return fmt.Errorf("%w: a universal Mach-O file; publish one file per platform", ErrFormat)
	}
	if magic != mhMagic64 {
		return fmt.Errorf("%w: not a 64-bit Mach-O file", ErrFormat)
	}
	cpu, _ := rd.u32(4)
	filetype, _ := rd.u32(12)
	ncmds, _ := rd.u32(16)
	sizeofcmds, err := rd.u32(20)
	if err != nil {
		return err
	}
	if want := map[string]uint32{"amd64": cpuX8664, "arm64": cpuARM64}[machine]; cpu != want {
		return fmt.Errorf("%w: the Mach-O CPU type is not the platform's", ErrFormat)
	}
	if filetype != mhDylib && filetype != mhBundle {
		return fmt.Errorf("%w: not a Mach-O dylib or bundle", ErrFormat)
	}
	if ncmds > maxLoadCommands || int64(sizeofcmds) > rd.size-32 {
		return errBounds
	}
	var trieOff, trieSize uint32
	tries, symtabs, dysymtabs := 0, 0, 0
	var symoff, nsyms, stroff, strsize, iext, next uint32
	off := int64(32)
	end := off + int64(sizeofcmds)
	for range ncmds {
		if err := rd.tick(); err != nil {
			return err
		}
		cmd, err := rd.u32(off)
		if err != nil {
			return err
		}
		size, err := rd.u32(off + 4)
		if err != nil {
			return err
		}
		if size < 8 || off+int64(size) > end {
			return fmt.Errorf("%w: a malformed load command", ErrFormat)
		}
		need := map[uint32]uint32{lcDyldExportsTrie: 16, lcDyldInfo: 48, lcDyldInfoOnly: 48, lcSymtab: 24, lcDysymtab: 80}[cmd]
		if size < need {
			return fmt.Errorf("%w: a load command is too short", ErrFormat)
		}
		switch cmd {
		case lcDyldExportsTrie:
			trieOff, _ = rd.u32(off + 8)
			trieSize, _ = rd.u32(off + 12)
			tries++
		case lcDyldInfo, lcDyldInfoOnly:
			if o, _ := rd.u32(off + 40); o != 0 { // dyld info without exports (rebase and bind only)
				trieOff, _ = rd.u32(off + 40)
				trieSize, _ = rd.u32(off + 44)
				tries++
			}
		case lcSymtab:
			symoff, _ = rd.u32(off + 8)
			nsyms, _ = rd.u32(off + 12)
			stroff, _ = rd.u32(off + 16)
			strsize, _ = rd.u32(off + 20)
			symtabs++
		case lcDysymtab:
			iext, _ = rd.u32(off + 16)
			next, _ = rd.u32(off + 20)
			dysymtabs++
		}
		off += int64(size)
	}
	if tries > 1 || symtabs > 1 || dysymtabs > 1 {
		return fmt.Errorf("%w: the Mach-O file has more than one source of exports", ErrFormat)
	}
	unprefix := func(n string, strict bool) error {
		s, ok := strings.CutPrefix(n, "_")
		if !ok {
			return nil // not a C symbol: no name DuckDB looks up
		}
		return v(s, strict)
	}
	switch {
	case tries == 1:
		return walkTrie(rd, int64(trieOff), int64(trieSize), unprefix)
	case symtabs == 1 && dysymtabs == 1:
		if uint64(iext)+uint64(next) > uint64(nsyms) || nsyms > maxSymbols {
			return errBounds
		}
		for i := iext; i < iext+next; i++ {
			if err := rd.tick(); err != nil {
				return err
			}
			e, err := rd.bytes(int64(symoff)+int64(i)*nlistSize, nlistSize)
			if err != nil {
				return err
			}
			strx := uint32(e[0]) | uint32(e[1])<<8 | uint32(e[2])<<16 | uint32(e[3])<<24
			if e[4]&nExt == 0 || e[4]&nPExt != 0 {
				continue
			}
			if strx >= strsize {
				return errBounds
			}
			s, err := rd.cstring(int64(stroff)+int64(strx), int64(stroff)+int64(strsize))
			if err != nil {
				return err
			}
			if err := unprefix(s, e[4]&nTypeMask == nSect); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("%w: the Mach-O file has no export information", ErrFormat)
}

// walkTrie visits the terminal entries of an export trie (dyld's format); a re-export is visited
// as not strict (dyld resolves it, to another library's code). The walk keeps a visited set and a
// depth limit, so a cyclic or deep trie is refused, and charges every prefix it builds to the
// parse's name budget.
func walkTrie(rd *reader, base, size int64, v visit) error {
	if size == 0 {
		return nil
	}
	if base < 0 || size < 0 || base > rd.size-size {
		return errBounds
	}
	trie, err := rd.bytes(base, size)
	if err != nil {
		return err
	}
	uleb := func(p *int) (uint64, error) {
		var x uint64
		for shift := uint(0); ; shift += 7 {
			if *p >= len(trie) || shift > 63 {
				return 0, fmt.Errorf("%w: a malformed export trie", ErrFormat)
			}
			b := trie[*p]
			*p++
			x |= uint64(b&0x7f) << shift
			if b < 0x80 {
				return x, nil
			}
		}
	}
	visited := map[int]bool{}
	type node struct {
		off    int
		prefix string
		depth  int
	}
	stack := []node{{0, "", 0}}
	for len(stack) > 0 {
		if err := rd.tick(); err != nil {
			return err
		}
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[n.off] || n.depth > maxTrieDepth {
			return fmt.Errorf("%w: a cyclic or too deep export trie", ErrFormat)
		}
		visited[n.off] = true
		p := n.off
		term, err := uleb(&p)
		if err != nil {
			return err
		}
		if term > 0 {
			q := p
			flags, err := uleb(&q)
			if err != nil {
				return err
			}
			if err := v(n.prefix, flags&exportReexport == 0); err != nil {
				return err
			}
			if term > uint64(len(trie)) {
				return errBounds
			}
			p += int(term)
		}
		if p >= len(trie) {
			return fmt.Errorf("%w: a malformed export trie", ErrFormat)
		}
		children := int(trie[p])
		p++
		for range children {
			start := p
			for p < len(trie) && trie[p] != 0 {
				p++
			}
			if p >= len(trie) || p-start+len(n.prefix) > maxNameBytes {
				return fmt.Errorf("%w: a malformed export trie", ErrFormat)
			}
			edge := string(trie[start:p])
			p++
			child, err := uleb(&p)
			if err != nil {
				return err
			}
			if child == 0 || child >= uint64(len(trie)) {
				return fmt.Errorf("%w: a malformed export trie", ErrFormat)
			}
			if err := rd.charge(len(n.prefix) + len(edge)); err != nil {
				return err
			}
			stack = append(stack, node{int(child), n.prefix + edge, n.depth + 1})
		}
	}
	return nil
}
