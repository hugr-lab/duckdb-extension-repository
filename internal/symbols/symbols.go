// Package symbols checks that an extension binary is a shared library of its platform and exports
// the entry point DuckDB looks up for its name, and no other extension's (spec 0008). The readers
// are bounded and read-only: every offset and count is checked against the file, and nothing is
// executed.
package symbols

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Errors. Callers test with errors.Is; messages are for the publisher.
var (
	// ErrFormat is a file that is not a shared library of its platform, or that cannot be read.
	ErrFormat = errors.New("symbols: not a shared library of the platform")
	// ErrEntryPoint is a binary without the name's entry point, or with another extension's.
	ErrEntryPoint = errors.New("symbols: entry point")
)

// Entry-point suffixes DuckDB looks up (src/main/extension/extension_load.cpp:805-860).
const (
	SuffixCPP     = "_duckdb_cpp_init"
	SuffixCAPI    = "_init_c_api"
	SuffixCAPIV2  = "_init_c_api_v2"
	maxNameBytes  = 4096
	maxSymbols    = 1 << 21
	maxAllNames   = 128 << 20 // all names read in one parse
	parseDeadline = 10 * time.Second
)

// EntryPoint is the symbol DuckDB looks up to load name with the footer's ABI and C API version:
// <name>_duckdb_cpp_init for CPP (and an empty ABI), <name>_init_c_api_v2 for C_STRUCT_UNSTABLE and
// for C_STRUCT whose C API version is v2.y.z, <name>_init_c_api for any other C_STRUCT
// (extension_load.cpp:29-37, extension.cpp:141-148).
func EntryPoint(name, abi, capiVersion string) string {
	switch abi {
	case "C_STRUCT_UNSTABLE":
		return name + SuffixCAPIV2
	case "C_STRUCT":
		if capiMajor(capiVersion) == 2 {
			return name + SuffixCAPIV2
		}
		return name + SuffixCAPI
	}
	return name + SuffixCPP
}

// capiMajor parses vX.Y.Z as DuckDB's ParseSemver does; -1 when it does not parse.
func capiMajor(v string) int {
	rest, ok := strings.CutPrefix(v, "v")
	if !ok {
		return -1
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return -1
	}
	n := 0
	for i, p := range parts {
		if p == "" || len(p) > 9 {
			return -1
		}
		v := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return -1
			}
			v = v*10 + int(c-'0')
		}
		if i == 0 {
			n = v
		}
	}
	return n
}

// isEntryPoint reports whether a symbol looks like any extension's entry point.
func isEntryPoint(s string) bool {
	return strings.HasSuffix(s, SuffixCPP) || strings.HasSuffix(s, SuffixCAPI) || strings.HasSuffix(s, SuffixCAPIV2)
}

// visit is called for each name a library resolves for the loader. strict is true when the name
// is one DuckDB's lookup takes as the extension's own entry point (a defined, visible function);
// any resolvable name counts when looking for another extension's entry point.
type visit func(name string, strict bool) error

var errStop = errors.New("symbols: stop")

// Check reads the binary in r (size bytes, the whole file: trailing data such as DuckDB's footer
// is ignored) and checks it against the platform and the entry point it must export: that one as
// a strict export, and no other name with an entry-point suffix in any form the loader resolves.
func Check(r io.ReaderAt, size int64, platform, entryPoint string) error {
	found, other := false, ""
	err := scan(r, size, platform, func(name string, strict bool) error {
		switch {
		case name == entryPoint:
			found = found || strict
		case isEntryPoint(name):
			other = name
			return errStop
		}
		return nil
	})
	switch {
	case other != "":
		return fmt.Errorf("%w: the binary also exports %s, another extension's entry point", ErrEntryPoint, clip(other))
	case err != nil:
		return err
	case !found:
		return fmt.Errorf("%w: the binary does not export %s", ErrEntryPoint, entryPoint)
	}
	return nil
}

// Exports lists the strict exports of a shared library of the platform (for tests and tools).
func Exports(r io.ReaderAt, size int64, platform string) ([]string, error) {
	var out []string
	err := scan(r, size, platform, func(name string, strict bool) error {
		if strict {
			out = append(out, name)
		}
		return nil
	})
	return out, err
}

// recoverPanics is cleared by the fuzz tests, so a reader bug surfaces instead of being refused.
var recoverPanics = true

func scan(r io.ReaderAt, size int64, platform string, v visit) (err error) {
	system, machine, ok := strings.Cut(platform, "_")
	if !ok {
		return fmt.Errorf("%w: unknown platform %q", ErrFormat, platform)
	}
	machine, _, _ = strings.Cut(machine, "_") // linux_amd64_musl, windows_amd64_mingw
	if machine != "amd64" && machine != "arm64" {
		return fmt.Errorf("%w: unknown machine in platform %q", ErrFormat, platform)
	}
	rd := &reader{r: r, size: size, deadline: time.Now().Add(parseDeadline)}
	if recoverPanics {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("%w: the binary could not be read", ErrFormat)
			}
		}()
	}
	switch system {
	case "linux":
		err = elfScan(rd, machine, v)
	case "osx":
		err = machoScan(rd, machine, v)
	case "windows":
		err = peScan(rd, machine, v)
	default:
		return fmt.Errorf("%w: platform %q is not one kista can check", ErrFormat, platform)
	}
	if errors.Is(err, errStop) {
		return nil
	}
	return err
}

func clip(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// reader reads bounded little-endian values from a file.
type reader struct {
	r        io.ReaderAt
	size     int64
	deadline time.Time
	steps    int
	names    int64 // bytes of names read so far
}

var errBounds = fmt.Errorf("%w: an offset or size is outside the file", ErrFormat)

func (rd *reader) bytes(off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || n > rd.size || off > rd.size-n {
		return nil, errBounds
	}
	b := make([]byte, n)
	// a ReaderAt may answer a full read with io.EOF; only a short read is an error
	if got, err := rd.r.ReadAt(b, off); int64(got) < n {
		return nil, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	return b, nil
}

func (rd *reader) u16(off int64) (uint16, error) {
	b, err := rd.bytes(off, 2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

func (rd *reader) u32(off int64) (uint32, error) {
	b, err := rd.bytes(off, 4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (rd *reader) u64(off int64) (uint64, error) {
	b, err := rd.bytes(off, 8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

// cstring reads a NUL-terminated string at off within [off, limit).
func (rd *reader) cstring(off, limit int64) (string, error) {
	if limit > rd.size {
		limit = rd.size
	}
	var sb strings.Builder
	for off < limit {
		n := min(int64(64), limit-off)
		b, err := rd.bytes(off, n)
		if err != nil {
			return "", err
		}
		for _, c := range b {
			if c == 0 {
				return sb.String(), nil
			}
			if sb.Len() >= maxNameBytes {
				return "", fmt.Errorf("%w: a symbol name is too long", ErrFormat)
			}
			sb.WriteByte(c)
		}
		if rd.names += n; rd.names > maxAllNames {
			return "", fmt.Errorf("%w: the binary's names are too large to read", ErrFormat)
		}
		off += n
	}
	return "", fmt.Errorf("%w: an unterminated string", ErrFormat)
}

// charge counts n bytes of names against the parse's budget.
func (rd *reader) charge(n int) error {
	if rd.names += int64(n); rd.names > maxAllNames {
		return fmt.Errorf("%w: the binary's names are too large to read", ErrFormat)
	}
	return nil
}

// tick bounds the work of a parse in time.
func (rd *reader) tick() error {
	rd.steps++
	if rd.steps%4096 == 0 && time.Now().After(rd.deadline) {
		return fmt.Errorf("%w: reading the binary took too long", ErrFormat)
	}
	return nil
}
