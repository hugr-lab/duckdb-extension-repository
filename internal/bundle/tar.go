package bundle

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
)

// Entry names (spec 0014, The bundle).
const (
	ManifestEntry = "kista-bundle.json"
	SigEntry      = "kista-bundle.sig"
	PubEntry      = "kista-bundle.pub"

	block = 512
	// maxHead bounds the signature and the public key entries.
	maxHead = 4 << 10
	// MaxSkipped bounds the bytes of entries read through without being used.
	MaxSkipped = 64 << 20
)

// ErrMalformed is a tar the reader refuses.
var ErrMalformed = errors.New("bundle: malformed tar")

// Writer writes a bundle: POSIX ustar headers for regular files, no compression. It hashes every
// byte it writes, end-of-archive included: the bundle's SHA-256, as the import computes it.
type Writer struct {
	w io.Writer
	h hash.Hash
}

// NewWriter writes to w.
func NewWriter(w io.Writer) *Writer {
	h := sha256.New()
	return &Writer{w: io.MultiWriter(w, h), h: h}
}

// Entry writes one regular file of size bytes from r.
func (w *Writer) Entry(name string, size int64, r io.Reader) error {
	if len(name) > 100 || size < 0 || size >= 1<<33 {
		return fmt.Errorf("bundle: entry %q of %d bytes does not fit a ustar header", name, size)
	}
	var hdr [block]byte
	copy(hdr[0:], name)
	copy(hdr[100:], "0000644\x00")
	copy(hdr[108:], "0000000\x00")
	copy(hdr[116:], "0000000\x00")
	copy(hdr[124:], fmt.Sprintf("%011o\x00", size))
	copy(hdr[136:], "00000000000\x00")
	hdr[156] = '0'
	copy(hdr[257:], "ustar\x0000")
	copy(hdr[148:], "        ")
	var sum int64
	for _, b := range hdr {
		sum += int64(b)
	}
	copy(hdr[148:], fmt.Sprintf("%06o\x00 ", sum))
	if _, err := w.w.Write(hdr[:]); err != nil {
		return err
	}
	n, err := io.Copy(w.w, io.LimitReader(r, size))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("bundle: entry %q: %d bytes of %d", name, n, size)
	}
	if pad := (block - size%block) % block; pad > 0 {
		if _, err := w.w.Write(make([]byte, pad)); err != nil {
			return err
		}
	}
	return nil
}

// Close writes the end-of-archive blocks.
func (w *Writer) Close() error {
	_, err := w.w.Write(make([]byte, 2*block))
	return err
}

// SHA256 is the hex SHA-256 of everything written so far.
func (w *Writer) SHA256() string { return hex.EncodeToString(w.h.Sum(nil)) }

// RawEntry is one tar entry as the reader saw it. Only a Regular entry is ever used; the others
// are read through and counted.
type RawEntry struct {
	Name    string
	Size    int64
	Regular bool   // a plain file: typeflag '0', not sparse
	Why     string // why it is not Regular
}

// Reader reads a bundle front to back from its raw headers. It does not use archive/tar, which
// merges PAX and GNU names silently: a PAX global header, GNU long names and links, sparse files,
// links, devices and directories are entries of their own, never Regular. It hashes every byte it
// consumes through the end-of-archive blocks, and reads nothing after them.
type Reader struct {
	r       io.Reader
	h       hash.Hash
	remain  int64 // the current entry's unread bytes
	pad     int64
	ended   bool
	skipped int64
}

// NewReader reads from r.
func NewReader(r io.Reader) *Reader {
	h := sha256.New()
	return &Reader{r: io.TeeReader(r, h), h: h}
}

// SHA256 is the hex SHA-256 of what was read; after the end of archive, the bundle's.
func (t *Reader) SHA256() string { return hex.EncodeToString(t.h.Sum(nil)) }

// Ended reports whether the end-of-archive blocks were read.
func (t *Reader) Ended() bool { return t.ended }

// Skipped is the size of the entries read through so far.
func (t *Reader) Skipped() int64 { return t.skipped }

// Read reads the current entry's data.
func (t *Reader) Read(p []byte) (int, error) {
	if t.remain <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > t.remain {
		p = p[:t.remain]
	}
	n, err := t.r.Read(p)
	t.remain -= int64(n)
	if err == io.EOF && t.remain > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// Skip reads the rest of the current entry through, counting it toward the skipped bytes.
func (t *Reader) Skip() error {
	n, err := io.Copy(io.Discard, t)
	t.skipped += n
	return err
}

// Next finishes the current entry and reads the next header. It returns io.EOF at the end of
// the archive, and io.ErrUnexpectedEOF when the input ends before it.
func (t *Reader) Next() (RawEntry, error) {
	if t.ended {
		return RawEntry{}, io.EOF
	}
	if t.remain > 0 {
		if _, err := io.Copy(io.Discard, t); err != nil {
			return RawEntry{}, err
		}
	}
	if t.pad > 0 {
		if _, err := io.CopyN(io.Discard, t.r, t.pad); err != nil {
			return RawEntry{}, unexpected(err)
		}
		t.pad = 0
	}
	var pax map[string]string
	for {
		var hdr [block]byte
		if _, err := io.ReadFull(t.r, hdr[:]); err != nil {
			return RawEntry{}, unexpected(err)
		}
		if hdr == [block]byte{} {
			if _, err := io.ReadFull(t.r, hdr[:]); err != nil {
				return RawEntry{}, unexpected(err)
			}
			if hdr != [block]byte{} {
				return RawEntry{}, fmt.Errorf("%w: a zero block alone", ErrMalformed)
			}
			t.ended = true
			return RawEntry{}, io.EOF
		}
		if err := checksum(hdr[:]); err != nil {
			return RawEntry{}, err
		}
		if string(hdr[257:265]) != "ustar\x0000" {
			return RawEntry{}, fmt.Errorf("%w: not a POSIX ustar header", ErrMalformed)
		}
		size, err := octal(hdr[124:136])
		if err != nil {
			return RawEntry{}, err
		}
		name := cstr(hdr[0:100])
		if p := cstr(hdr[345:500]); p != "" {
			name = p + "/" + name
		}
		typ := hdr[156]
		if typ == 'x' { // a PAX header for the next entry: its records may rename or resize it
			if size > maxHead*16 {
				return RawEntry{}, fmt.Errorf("%w: a PAX header of %d bytes", ErrMalformed, size)
			}
			data := make([]byte, size)
			if _, err := io.ReadFull(t.r, data); err != nil {
				return RawEntry{}, unexpected(err)
			}
			if _, err := io.CopyN(io.Discard, t.r, (block-size%block)%block); err != nil {
				return RawEntry{}, unexpected(err)
			}
			if pax, err = paxRecords(data); err != nil {
				return RawEntry{}, err
			}
			continue
		}
		e := RawEntry{Name: name, Size: size}
		if v, ok := pax["path"]; ok {
			e.Name = v
		}
		if v, ok := pax["size"]; ok {
			if e.Size, err = strconv.ParseInt(v, 10, 64); err != nil || e.Size < 0 {
				return RawEntry{}, fmt.Errorf("%w: a PAX size %q", ErrMalformed, v)
			}
		}
		switch {
		case typ != '0' && typ != 0:
			e.Why = fmt.Sprintf("entry type %q", typ)
		case pax["sparse"] != "":
			e.Why = "a sparse file"
		default:
			e.Regular = true
		}
		t.remain, t.pad = e.Size, (block-e.Size%block)%block
		return e, nil
	}
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func octal(b []byte) (int64, error) {
	if b[0]&0x80 != 0 {
		return 0, fmt.Errorf("%w: a base-256 size", ErrMalformed)
	}
	s := strings.TrimRight(strings.TrimLeft(cstr(b), " "), " ")
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 8, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: a size %q", ErrMalformed, s)
	}
	return n, nil
}

func checksum(hdr []byte) error {
	want, err := octal(hdr[148:156])
	if err != nil {
		return err
	}
	var sum int64
	for i, b := range hdr {
		if i >= 148 && i < 156 {
			b = ' '
		}
		sum += int64(b)
	}
	if sum != want {
		return fmt.Errorf("%w: a header checksum", ErrMalformed)
	}
	return nil
}

// paxRecords parses "<len> <key>=<value>\n" records. GNU sparse records mark the entry sparse.
func paxRecords(data []byte) (map[string]string, error) {
	out := map[string]string{}
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp <= 0 {
			return nil, fmt.Errorf("%w: a PAX record", ErrMalformed)
		}
		n, err := strconv.Atoi(string(data[:sp]))
		if err != nil || n <= sp+1 || n > len(data) || data[n-1] != '\n' {
			return nil, fmt.Errorf("%w: a PAX record", ErrMalformed)
		}
		kv := string(data[sp+1 : n-1])
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("%w: a PAX record", ErrMalformed)
		}
		if strings.HasPrefix(k, "GNU.sparse.") {
			out["sparse"] = "1"
		} else {
			out[k] = v
		}
		data = data[n:]
	}
	return out, nil
}

// Inflate inflates one gzip member from r into w, at most max bytes, and requires r to end where
// the member ends (nothing after it, no second member). It returns the inflated size and SHA-256.
func Inflate(r io.Reader, max int64, w io.Writer) (int64, string, error) {
	br := bufio.NewReader(r) // a ByteReader: gzip then reads nothing past the member's end
	zr, err := gzip.NewReader(br)
	if err != nil {
		return 0, "", fmt.Errorf("bundle: not a gzip stream: %w", err)
	}
	zr.Multistream(false)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(zr, max+1))
	if err != nil {
		return n, "", fmt.Errorf("bundle: inflating: %w", err)
	}
	if n > max {
		return n, "", fmt.Errorf("bundle: the inflated file is over %d bytes", max)
	}
	if err := zr.Close(); err != nil {
		return n, "", err
	}
	if _, err := br.ReadByte(); err != io.EOF {
		return n, "", errors.New("bundle: data after the gzip member")
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}
