package symbols

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The fixtures are built here, byte by byte: the smallest shared libraries the readers accept,
// and their malformed variants. Real binaries (DuckDB's, built at the pin) are read by the test at
// the end and by the e2e suite.

type sym struct {
	name    string
	bind    byte // ELF binding
	typ     byte // ELF type
	vis     byte // ELF visibility
	defined bool
}

func fn(name string) sym { return sym{name: name, bind: stbGlobal, typ: sttFunc, defined: true} }

type elfOpts struct {
	machine, etype uint16
	syms           []sym
	moveSection    bool // the SHT_DYNSYM address disagrees with DT_SYMTAB
	compressed     bool
	noDynamic      bool
	shortSection   bool // the SHT_DYNSYM section hides the last symbol the hash table reaches
	gnuHash        bool // DT_GNU_HASH instead of DT_HASH
}

func le(b []byte, off int, v any) {
	switch x := v.(type) {
	case uint16:
		binary.LittleEndian.PutUint16(b[off:], x)
	case uint32:
		binary.LittleEndian.PutUint32(b[off:], x)
	case uint64:
		binary.LittleEndian.PutUint64(b[off:], x)
	}
}

func buildELF(o elfOpts) []byte {
	if o.machine == 0 {
		o.machine = emX8664
	}
	if o.etype == 0 {
		o.etype = etDyn
	}
	// layout: header, 2 program headers, dynstr, dynsym, dynamic, 3 section headers
	strtab := []byte{0}
	syms := make([]byte, symSize) // the null symbol
	for _, s := range o.syms {
		e := make([]byte, symSize)
		le(e, 0, uint32(len(strtab)))
		strtab = append(append(strtab, s.name...), 0)
		e[4] = s.bind<<4 | s.typ
		e[5] = s.vis
		if s.defined {
			le(e, 6, uint16(7))
		}
		syms = append(syms, e...)
	}
	const hdr, ph = 64, 56
	nsyms := len(o.syms) + 1
	strOff := hdr + 2*ph
	symOff := strOff + len(strtab)
	symOff += (8 - symOff%8) % 8
	hashOff := symOff + len(syms)
	var hashTab []byte
	if o.gnuHash {
		// one bucket pointing at symbol 1, a chain over symbols 1..n-1 ending with the low bit set
		hashTab = make([]byte, 16+8+4+4*(nsyms-1))
		le(hashTab, 0, uint32(1))
		le(hashTab, 4, uint32(1))
		le(hashTab, 8, uint32(1))
		le(hashTab, 24, uint32(1))
		for i := 1; i < nsyms; i++ {
			v := uint32(0)
			if i == nsyms-1 {
				v = 1
			}
			le(hashTab, 28+4*(i-1), v)
		}
	} else {
		hashTab = make([]byte, 8+4+4*nsyms)
		le(hashTab, 0, uint32(1))
		le(hashTab, 4, uint32(nsyms))
	}
	dynOff := hashOff + len(hashTab)
	dynOff += (8 - dynOff%8) % 8
	dyn := make([]byte, 96)
	hashTag := uint64(dtHash)
	if o.gnuHash {
		hashTag = dtGNUHash
	}
	for i, kv := range [][2]uint64{{dtSymtab, uint64(symOff)}, {dtStrtab, uint64(strOff)}, {dtStrsz, uint64(len(strtab))},
		{dtSyment, symSize}, {hashTag, uint64(hashOff)}} {
		le(dyn, i*16, kv[0])
		le(dyn, i*16+8, kv[1])
	}
	shOff := dynOff + len(dyn)
	total := shOff + 3*64
	b := make([]byte, total)
	copy(b, "\x7fELF")
	b[4], b[5], b[6] = 2, 1, 1
	le(b, 16, o.etype)
	le(b, 18, o.machine)
	le(b, 32, uint64(hdr))
	le(b, 40, uint64(shOff))
	le(b, 54, uint16(ph))
	le(b, 56, uint16(2))
	le(b, 58, uint16(64))
	le(b, 60, uint16(3))
	// PT_LOAD of the whole file at address = offset; PT_DYNAMIC
	le(b, hdr, uint32(ptLoad))
	le(b, hdr+8, uint64(0))
	le(b, hdr+16, uint64(0))
	le(b, hdr+32, uint64(total))
	le(b, hdr+40, uint64(total))
	dynType := uint32(ptDynamic)
	if o.noDynamic {
		dynType = ptLoad
	}
	le(b, hdr+ph, dynType)
	le(b, hdr+ph+8, uint64(dynOff))
	le(b, hdr+ph+16, uint64(dynOff))
	le(b, hdr+ph+32, uint64(len(dyn)))
	copy(b[strOff:], strtab)
	copy(b[symOff:], syms)
	copy(b[hashOff:], hashTab)
	copy(b[dynOff:], dyn)
	// section 1: .dynsym (link 2), section 2: .dynstr
	s1, s2 := shOff+64, shOff+128
	le(b, s1+4, uint32(shtDynsym))
	addr := uint64(symOff)
	if o.moveSection {
		addr += 8
	}
	if o.compressed {
		le(b, s1+8, uint64(shfCompress))
	}
	le(b, s1+16, addr)
	le(b, s1+24, uint64(symOff))
	secSize := uint64(len(syms))
	if o.shortSection {
		secSize -= symSize
	}
	le(b, s1+32, secSize)
	le(b, s1+40, uint32(2))
	le(b, s2+4, uint32(3))
	le(b, s2+16, uint64(strOff))
	le(b, s2+24, uint64(strOff))
	le(b, s2+32, uint64(len(strtab)))
	return b
}

func uleb(v uint64) []byte {
	var out []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		out = append(out, c)
		if v == 0 {
			return out
		}
	}
}

type machoOpts struct {
	cpu, filetype uint32
	exports       []string // with the leading "_"
	reexport      string
	symtab        bool // LC_SYMTAB + LC_DYSYMTAB instead of the trie
	cyclic        bool
	fat           bool
	twoTries      bool // LC_DYLD_EXPORTS_TRIE twice
}

// trie builds a flat export trie: the root's children are the full names.
func trie(names []string, reexport string, cyclic bool) []byte {
	all := append(append([]string{}, names...), reexport)
	if reexport == "" {
		all = names
	}
	// root: terminal 0, children; then each child node
	var root bytes.Buffer
	root.WriteByte(0)
	root.WriteByte(byte(len(all)))
	// offsets are only known after the root is laid out: two passes with fixed-size ulebs (2 bytes)
	rootLen := 2
	for _, n := range all {
		rootLen += len(n) + 1 + 2
	}
	var kids bytes.Buffer
	for _, n := range all {
		off := rootLen + kids.Len()
		root.WriteString(n)
		root.WriteByte(0)
		root.Write([]byte{byte(off&0x7f) | 0x80, byte(off >> 7)})
		flags := uint64(0)
		if n == reexport {
			flags = exportReexport
		}
		term := append(uleb(flags), uleb(0x1000)...)
		kids.Write(uleb(uint64(len(term))))
		kids.Write(term)
		if cyclic {
			kids.WriteByte(1)
			kids.WriteString("x")
			kids.WriteByte(0)
			kids.Write([]byte{byte(off&0x7f) | 0x80, byte(off >> 7)}) // points to itself
		} else {
			kids.WriteByte(0)
		}
	}
	return append(root.Bytes(), kids.Bytes()...)
}

func buildMachO(o machoOpts) []byte {
	if o.cpu == 0 {
		o.cpu = cpuARM64
	}
	if o.filetype == 0 {
		o.filetype = mhDylib
	}
	var cmds bytes.Buffer
	var data []byte
	const hdr = 32
	cmd := func(words ...uint32) {
		for _, w := range words {
			var x [4]byte
			binary.LittleEndian.PutUint32(x[:], w)
			cmds.Write(x[:])
		}
	}
	ncmds := uint32(0)
	if o.symtab {
		// 2 commands: LC_SYMTAB (24) + LC_DYSYMTAB (80); data after them
		dataOff := uint32(hdr + 24 + 80)
		var strs bytes.Buffer
		strs.WriteByte(0)
		var nl bytes.Buffer
		local := append([]string{"_local_helper"}, o.exports...)
		for i, n := range local {
			e := make([]byte, nlistSize)
			le(e, 0, uint32(strs.Len()))
			strs.WriteString(n)
			strs.WriteByte(0)
			e[4] = nSect
			if i > 0 {
				e[4] |= nExt
			}
			e[5] = 1
			nl.Write(e)
		}
		symoff := dataOff
		stroff := symoff + uint32(nl.Len())
		cmd(lcSymtab, 24, symoff, uint32(len(local)), stroff, uint32(strs.Len()))
		dy := make([]uint32, 20)
		dy[0], dy[1] = lcDysymtab, 80
		dy[2], dy[3] = 0, 1 // locals
		dy[4], dy[5] = 1, uint32(len(o.exports))
		cmd(dy...)
		ncmds = 2
		data = append(nl.Bytes(), strs.Bytes()...)
	} else {
		t := trie(o.exports, o.reexport, o.cyclic)
		at := uint32(hdr + 16)
		if o.twoTries {
			at += 16
		}
		cmd(lcDyldExportsTrie, 16, at, uint32(len(t)))
		ncmds = 1
		if o.twoTries {
			cmd(lcDyldExportsTrie, 16, at, uint32(len(t)))
			ncmds = 2
		}
		data = t
	}
	b := make([]byte, hdr)
	le(b, 0, uint32(mhMagic64))
	if o.fat {
		binary.BigEndian.PutUint32(b, fatMagic)
	}
	le(b, 4, o.cpu)
	le(b, 12, o.filetype)
	le(b, 16, ncmds)
	le(b, 20, uint32(cmds.Len()))
	return append(append(b, cmds.Bytes()...), data...)
}

type peOpts struct {
	machine, chars uint16
	exports        []string
	forwarder      string
	noExports      bool
	shortSection   string // the last name's tail lies past the section's raw size
}

func buildPE(o peOpts) []byte {
	if o.machine == 0 {
		o.machine = machAMD64
	}
	if o.chars == 0 {
		o.chars = fileDLL
	}
	names := append([]string{}, o.exports...)
	if o.forwarder != "" {
		names = append(names, o.forwarder)
	}
	const peOff, optSize, secRaw, secVA = 0x40, 240, 0x200, 0x1000
	// .edata: directory (40), functions, names, ordinals, strings, forwarder string
	n := len(names)
	funcs := 40
	nameArr := funcs + 4*n
	ords := nameArr + 4*n
	strs := ords + 2*n
	var sb bytes.Buffer
	nameRVAs := make([]uint32, n)
	for i, s := range names {
		nameRVAs[i] = uint32(secVA + strs + sb.Len())
		sb.WriteString(s)
		sb.WriteByte(0)
	}
	fwdRVA := uint32(secVA + strs + sb.Len())
	sb.WriteString("OTHER.dll.f")
	sb.WriteByte(0)
	edata := make([]byte, strs+sb.Len())
	copy(edata[strs:], sb.Bytes())
	le(edata, 20, uint32(n))
	le(edata, 24, uint32(n))
	le(edata, 28, uint32(secVA+funcs))
	le(edata, 32, uint32(secVA+nameArr))
	le(edata, 36, uint32(secVA+ords))
	for i := range names {
		f := uint32(0x2000 + i) // code outside .edata
		if names[i] == o.forwarder {
			f = fwdRVA
		}
		le(edata, funcs+4*i, f)
		le(edata, nameArr+4*i, nameRVAs[i])
		le(edata, ords+2*i, uint16(i))
	}
	b := make([]byte, secRaw+len(edata))
	copy(b, "MZ")
	le(b, 0x3c, uint32(peOff))
	copy(b[peOff:], "PE\x00\x00")
	coff := peOff + 4
	le(b, coff, o.machine)
	le(b, coff+2, uint16(1))
	le(b, coff+16, uint16(optSize))
	le(b, coff+18, o.chars)
	opt := coff + 20
	le(b, opt, uint16(pe32Plus))
	le(b, opt+108, uint32(16))
	if !o.noExports {
		le(b, opt+112, uint32(secVA))
		le(b, opt+116, uint32(len(edata)))
	}
	sec := opt + optSize
	copy(b[sec:], ".edata")
	rawSize := len(edata)
	if o.shortSection != "" {
		rawSize -= len(o.shortSection) + 1 + len("OTHER.dll.f") + 1 // the tail and the forwarder string
	}
	le(b, sec+8, uint32(rawSize))
	le(b, sec+12, uint32(secVA))
	le(b, sec+16, uint32(rawSize))
	le(b, sec+20, uint32(secRaw))
	copy(b[secRaw:], edata)
	return b
}

func check(b []byte, platform, entry string) error {
	// DuckDB's footer follows the binary: trailing data must not matter
	b = append(append([]byte{}, b...), make([]byte, 534)...)
	return Check(bytes.NewReader(b), int64(len(b)), platform, entry)
}

func TestEntryPoint(t *testing.T) {
	for _, tc := range []struct{ abi, capi, want string }{
		{"CPP", "", "x_duckdb_cpp_init"},
		{"", "", "x_duckdb_cpp_init"},
		{"C_STRUCT", "v1.2.0", "x_init_c_api"},
		{"C_STRUCT", "v0.0.1", "x_init_c_api"},
		{"C_STRUCT", "v3.0.0", "x_init_c_api"},
		{"C_STRUCT", "v2.0.0", "x_init_c_api_v2"},
		{"C_STRUCT", "2.0.0", "x_init_c_api"}, // DuckDB's ParseSemver needs the v
		{"C_STRUCT_UNSTABLE", "", "x_init_c_api_v2"},
	} {
		if got := EntryPoint("x", tc.abi, tc.capi); got != tc.want {
			t.Errorf("%s %s: %s, want %s", tc.abi, tc.capi, got, tc.want)
		}
	}
}

func TestELF(t *testing.T) {
	const e = "demo_duckdb_cpp_init"
	ok := buildELF(elfOpts{syms: []sym{fn("helper"), fn(e)}})
	if err := check(ok, "linux_amd64", e); err != nil {
		t.Fatal(err)
	}
	if err := check(ok, "linux_amd64_musl", e); err != nil {
		t.Fatal(err)
	}
	weak := sym{name: e, bind: stbWeak, typ: sttFunc, defined: true}
	protected := sym{name: e, bind: stbGlobal, typ: sttFunc, vis: stvProtected, defined: true}
	for name, tc := range map[string]struct {
		b        []byte
		platform string
		want     error
	}{
		"another name's":          {buildELF(elfOpts{syms: []sym{fn("other_duckdb_cpp_init")}}), "linux_amd64", ErrEntryPoint},
		"two extensions":          {buildELF(elfOpts{syms: []sym{fn(e), fn("other_init_c_api")}}), "linux_amd64", ErrEntryPoint},
		"undefined import":        {buildELF(elfOpts{syms: []sym{{name: e, bind: stbGlobal, typ: sttFunc}}}), "linux_amd64", ErrEntryPoint},
		"local":                   {buildELF(elfOpts{syms: []sym{{name: e, typ: sttFunc, defined: true}}}), "linux_amd64", ErrEntryPoint},
		"hidden":                  {buildELF(elfOpts{syms: []sym{{name: e, bind: stbGlobal, typ: sttFunc, vis: 2, defined: true}}}), "linux_amd64", ErrEntryPoint},
		"an object":               {buildELF(elfOpts{syms: []sym{{name: e, bind: stbGlobal, typ: 1, defined: true}}}), "linux_amd64", ErrEntryPoint},
		"wrong machine":           {ok, "linux_arm64", ErrFormat},
		"an executable":           {buildELF(elfOpts{etype: 2, syms: []sym{fn(e)}}), "linux_amd64", ErrFormat},
		"sections disagree":       {buildELF(elfOpts{syms: []sym{fn(e)}, moveSection: true}), "linux_amd64", ErrFormat},
		"compressed":              {buildELF(elfOpts{syms: []sym{fn(e)}, compressed: true}), "linux_amd64", ErrFormat},
		"no dynamic":              {buildELF(elfOpts{syms: []sym{fn(e)}, noDynamic: true}), "linux_amd64", ErrFormat},
		"not ELF":                 {buildPE(peOpts{exports: []string{e}}), "linux_amd64", ErrFormat},
		"truncated":               {ok[:200], "linux_amd64", ErrFormat},
		"another as data":         {buildELF(elfOpts{syms: []sym{fn(e), {name: "x_duckdb_cpp_init", bind: stbGlobal, typ: 1, defined: true}}}), "linux_amd64", ErrEntryPoint},
		"another untyped":         {buildELF(elfOpts{syms: []sym{fn(e), {name: "x_init_c_api", bind: stbGlobal}}}), "linux_amd64", ErrEntryPoint},
		"hidden past the section": {buildELF(elfOpts{syms: []sym{fn(e), fn("x_init_c_api_v2")}, shortSection: true}), "linux_amd64", ErrFormat},
	} {
		if err := Check(bytes.NewReader(tc.b), int64(len(tc.b)), tc.platform, e); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	for name, s := range map[string]sym{"weak": weak, "protected": protected} {
		if err := check(buildELF(elfOpts{syms: []sym{s}}), "linux_amd64", e); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := check(buildELF(elfOpts{machine: emAArch64, syms: []sym{fn(e)}}), "linux_arm64", e); err != nil {
		t.Error(err)
	}
	if err := check(buildELF(elfOpts{syms: []sym{fn("helper"), fn(e)}, gnuHash: true}), "linux_amd64", e); err != nil {
		t.Errorf("DT_GNU_HASH: %v", err)
	}
	if err := check(buildELF(elfOpts{syms: []sym{fn(e), fn("x_duckdb_cpp_init")}, gnuHash: true}), "linux_amd64", e); !errors.Is(err, ErrEntryPoint) {
		t.Errorf("DT_GNU_HASH, two extensions: %v", err)
	}
}

func TestMachO(t *testing.T) {
	const e = "demo_init_c_api"
	for name, b := range map[string][]byte{
		"trie":           buildMachO(machoOpts{exports: []string{"_helper", "_" + e}}),
		"symtab":         buildMachO(machoOpts{exports: []string{"_" + e}, symtab: true}),
		"bundle":         buildMachO(machoOpts{filetype: mhBundle, exports: []string{"_" + e}}),
		"with reexports": buildMachO(machoOpts{exports: []string{"_" + e}, reexport: "_helper"}),
	} {
		if err := check(b, "osx_arm64", e); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := check(buildMachO(machoOpts{cpu: cpuX8664, exports: []string{"_" + e}}), "osx_amd64", e); err != nil {
		t.Error(err)
	}
	for name, tc := range map[string]struct {
		b        []byte
		platform string
		want     error
	}{
		"another name's":   {buildMachO(machoOpts{exports: []string{"_other_init_c_api"}}), "osx_arm64", ErrEntryPoint},
		"two extensions":   {buildMachO(machoOpts{exports: []string{"_" + e, "_x_duckdb_cpp_init"}}), "osx_arm64", ErrEntryPoint},
		"no underscore":    {buildMachO(machoOpts{exports: []string{e}}), "osx_arm64", ErrEntryPoint},
		"wrong cpu":        {buildMachO(machoOpts{exports: []string{"_" + e}}), "osx_amd64", ErrFormat},
		"an executable":    {buildMachO(machoOpts{filetype: 2, exports: []string{"_" + e}}), "osx_arm64", ErrFormat},
		"universal":        {buildMachO(machoOpts{fat: true, exports: []string{"_" + e}}), "osx_arm64", ErrFormat},
		"cyclic trie":      {buildMachO(machoOpts{exports: []string{"_" + e}, cyclic: true}), "osx_arm64", ErrFormat},
		"reexported other": {buildMachO(machoOpts{exports: []string{"_" + e}, reexport: "_other_init_c_api"}), "osx_arm64", ErrEntryPoint},
		"two tries":        {buildMachO(machoOpts{exports: []string{"_" + e}, twoTries: true}), "osx_arm64", ErrFormat},
		"truncated":        {buildMachO(machoOpts{exports: []string{"_" + e}})[:40], "osx_arm64", ErrFormat},
		"symtab truncated": {buildMachO(machoOpts{exports: []string{"_" + e}, symtab: true})[:150], "osx_arm64", ErrFormat},
	} {
		if err := Check(bytes.NewReader(tc.b), int64(len(tc.b)), tc.platform, e); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
}

func TestPE(t *testing.T) {
	const e = "demo_duckdb_cpp_init"
	ok := buildPE(peOpts{exports: []string{"helper", e}})
	for _, p := range []string{"windows_amd64", "windows_amd64_mingw"} {
		if err := check(ok, p, e); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if err := check(buildPE(peOpts{machine: machARM64, exports: []string{e}}), "windows_arm64", e); err != nil {
		t.Error(err)
	}
	for name, tc := range map[string]struct {
		b    []byte
		want error
	}{
		"another name's":        {buildPE(peOpts{exports: []string{"other_duckdb_cpp_init"}}), ErrEntryPoint},
		"two extensions":        {buildPE(peOpts{exports: []string{e, "other_init_c_api_v2"}}), ErrEntryPoint},
		"a forwarder":           {buildPE(peOpts{forwarder: e}), ErrEntryPoint},
		"forwarded other":       {buildPE(peOpts{exports: []string{e}, forwarder: "x_duckdb_cpp_init"}), ErrEntryPoint},
		"name past its section": {buildPE(peOpts{exports: []string{e, "x"}, shortSection: "x"}), ErrFormat},
		"not a DLL":             {buildPE(peOpts{chars: 0x0002, exports: []string{e}}), ErrFormat},
		"no exports":            {buildPE(peOpts{noExports: true}), ErrFormat},
		"wrong machine":         {buildPE(peOpts{machine: machARM64, exports: []string{e}}), ErrFormat},
		"truncated":             {ok[:0x210], ErrFormat},
	} {
		if err := Check(bytes.NewReader(tc.b), int64(len(tc.b)), "windows_amd64", e); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
}

func TestPlatforms(t *testing.T) {
	b := buildELF(elfOpts{syms: []sym{fn("x_duckdb_cpp_init")}})
	for _, p := range []string{"wasm_eh", "wasm_mvp", "freebsd_amd64", "linux_386", "linux"} {
		if err := check(b, p, "x_duckdb_cpp_init"); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// DuckDB's own builds at the pin, when the e2e build is present.
func TestRealBinaries(t *testing.T) {
	dir := filepath.Join("..", "..", "e2e", ".build", "extensions")
	for name, entry := range map[string]string{"demo_capi": "demo_capi_init_c_api",
		"loadable_extension_demo": "loadable_extension_demo_duckdb_cpp_init", "httpfs": "httpfs_duckdb_cpp_init"} {
		f, err := os.Open(filepath.Join(dir, name+".duckdb_extension"))
		if err != nil {
			t.Skip("no e2e build")
		}
		st, _ := f.Stat()
		head := make([]byte, 20)
		_, _ = f.ReadAt(head, 0)
		var platform string
		switch {
		case string(head[:4]) == "\x7fELF" && binary.LittleEndian.Uint16(head[18:]) == emAArch64:
			platform = "linux_arm64"
		case string(head[:4]) == "\x7fELF":
			platform = "linux_amd64"
		case binary.LittleEndian.Uint32(head) == mhMagic64 && binary.LittleEndian.Uint32(head[4:]) == cpuX8664:
			platform = "osx_amd64"
		case binary.LittleEndian.Uint32(head) == mhMagic64:
			platform = "osx_arm64"
		default:
			t.Fatalf("%s: an unknown format", name)
		}
		if err := Check(f, st.Size(), platform, entry); err != nil {
			t.Errorf("%s (%s): %v", name, platform, err)
		}
		f.Close()
	}
}

func FuzzELF(f *testing.F) {
	fuzz(f, buildELF(elfOpts{syms: []sym{fn("a_duckdb_cpp_init")}}), "linux_amd64")
}
func FuzzMachO(f *testing.F) {
	fuzz(f, buildMachO(machoOpts{exports: []string{"_a_init_c_api"}}), "osx_arm64")
}
func FuzzPE(f *testing.F) {
	fuzz(f, buildPE(peOpts{exports: []string{"a_duckdb_cpp_init"}}), "windows_amd64")
}

func fuzz(f *testing.F, seed []byte, platform string) {
	f.Add(seed)
	recoverPanics = false // a reader bug must surface, not be refused
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1<<16 {
			return
		}
		_, _ = Exports(bytes.NewReader(b), int64(len(b)), platform) // must not panic or hang
	})
}
