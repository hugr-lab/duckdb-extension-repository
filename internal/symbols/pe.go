package symbols

import "fmt"

// PE constants (the Microsoft PE/COFF specification).
const (
	machAMD64   = 0x8664
	machARM64   = 0xaa64
	fileDLL     = 0x2000
	pe32Plus    = 0x20b
	maxSections = 96
	maxExports  = 1 << 16
)

type peSection struct{ vaddr, size, rawOff uint32 } // size: the raw data the image maps

// peScan reads a PE32+ DLL: the names of its export directory; a forwarder (another DLL's
// function) is visited as not strict. A name is read only within the section that holds it.
func peScan(rd *reader, machine string, v visit) error {
	mz, err := rd.bytes(0, 2)
	if err != nil || string(mz) != "MZ" {
		return fmt.Errorf("%w: not a PE file", ErrFormat)
	}
	lfanew, err := rd.u32(0x3c)
	if err != nil {
		return err
	}
	pe := int64(lfanew)
	sig, err := rd.bytes(pe, 4)
	if err != nil || string(sig) != "PE\x00\x00" {
		return fmt.Errorf("%w: not a PE file", ErrFormat)
	}
	coff := pe + 4
	mach, _ := rd.u16(coff)
	nsec, _ := rd.u16(coff + 2)
	optSize, _ := rd.u16(coff + 16)
	chars, err := rd.u16(coff + 18)
	if err != nil {
		return err
	}
	if want := map[string]uint16{"amd64": machAMD64, "arm64": machARM64}[machine]; mach != want {
		return fmt.Errorf("%w: the PE machine is not the platform's", ErrFormat)
	}
	if chars&fileDLL == 0 {
		return fmt.Errorf("%w: not a PE DLL", ErrFormat)
	}
	if nsec > maxSections {
		return errBounds
	}
	opt := coff + 20
	magic, err := rd.u16(opt)
	if err != nil {
		return err
	}
	if magic != pe32Plus || optSize < 112+8 {
		return fmt.Errorf("%w: not a PE32+ file", ErrFormat)
	}
	nrva, _ := rd.u32(opt + 108)
	if nrva == 0 {
		return fmt.Errorf("%w: the PE file has no export directory", ErrFormat)
	}
	expRVA, _ := rd.u32(opt + 112)
	expSize, err := rd.u32(opt + 116)
	if err != nil {
		return err
	}
	if expRVA == 0 {
		return fmt.Errorf("%w: the PE file has no export directory", ErrFormat)
	}
	secs := make([]peSection, 0, nsec)
	st := opt + int64(optSize)
	for i := range int64(nsec) {
		s := st + i*40
		vsize, _ := rd.u32(s + 8)
		vaddr, _ := rd.u32(s + 12)
		rawSize, _ := rd.u32(s + 16)
		rawOff, err := rd.u32(s + 20)
		if err != nil {
			return err
		}
		size := rawSize
		if vsize != 0 && vsize < size {
			size = vsize // the loader maps no more than the virtual size
		}
		secs = append(secs, peSection{vaddr, size, rawOff})
	}
	// off maps an RVA (and the n bytes after it) to a file offset inside one section's raw data,
	// and returns where that section's mapped data ends
	off := func(rva uint32, n uint32) (int64, int64, error) {
		for _, s := range secs {
			if rva >= s.vaddr && uint64(rva-s.vaddr)+uint64(n) <= uint64(s.size) {
				return int64(s.rawOff) + int64(rva-s.vaddr), int64(s.rawOff) + int64(s.size), nil
			}
		}
		return 0, 0, errBounds
	}
	dir, _, err := off(expRVA, 40)
	if err != nil {
		return err
	}
	nfun, _ := rd.u32(dir + 20)
	nnames, _ := rd.u32(dir + 24)
	funRVA, _ := rd.u32(dir + 28)
	namesRVA, _ := rd.u32(dir + 32)
	ordRVA, err := rd.u32(dir + 36)
	if err != nil {
		return err
	}
	if nnames > maxExports || nfun > maxExports {
		return errBounds
	}
	if nnames == 0 {
		return nil
	}
	funs, _, err := off(funRVA, nfun*4)
	if err != nil {
		return err
	}
	names, _, err := off(namesRVA, nnames*4)
	if err != nil {
		return err
	}
	ords, _, err := off(ordRVA, nnames*2)
	if err != nil {
		return err
	}
	for i := range int64(nnames) {
		if err := rd.tick(); err != nil {
			return err
		}
		nameRVA, err := rd.u32(names + i*4)
		if err != nil {
			return err
		}
		ord, err := rd.u16(ords + i*2)
		if err != nil {
			return err
		}
		if uint32(ord) >= nfun {
			return errBounds
		}
		fn, err := rd.u32(funs + int64(ord)*4)
		if err != nil {
			return err
		}
		forwarder := fn >= expRVA && fn-expRVA < expSize // the function is another DLL's
		no, end, err := off(nameRVA, 1)
		if err != nil {
			return err
		}
		s, err := rd.cstring(no, end)
		if err != nil {
			return err
		}
		if err := v(s, !forwarder); err != nil {
			return err
		}
	}
	return nil
}
