// Package icon checks a catalogue entry's icon (spec 0036): a small square PNG and nothing else.
// The checks refuse rather than rewrite, so the bytes kept are exactly the bytes that passed: a
// strict chunk allow-list, nothing after IEND, the image data one zlib stream ending exactly at the
// last IDAT's last byte, the header's size checked before anything is decoded, and a full decode
// whose size must be the header's.
package icon

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image/png"
	"io"
)

const (
	// MaxBytes is the largest icon: a 256x256 PNG fits.
	MaxBytes = 256 << 10
	// MinSide and MaxSide bound a side, in pixels.
	MinSide, MaxSide = 64, 512
	// ContentType is the one type an icon is declared and served as.
	ContentType = "image/png"
	// maxRaw bounds the image data inflated by the end check: a 16-bit RGBA 512x512 image, its
	// filter bytes and interlacing's included, is well under it.
	maxRaw = 4 << 20
	// decodesAtOnce bounds the full decodes running at once on a replica: 2 MiB each at most.
	decodesAtOnce = 4
)

// Info is what a checked icon is.
type Info struct{ Width, Height int }

// ErrInvalid is an icon refused; its message names the bound.
var ErrInvalid = errors.New("icon: refused")

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

var signature = []byte("\x89PNG\r\n\x1a\n")

// decodes holds a slot per running decode.
var decodes = make(chan struct{}, decodesAtOnce)

// Check checks an icon's bytes, in order: the size, the PNG signature and its chunks, the header's
// dimensions (before anything is decoded, so a decompression bomb allocates nothing), the image
// data's end, and a full decode. A refusal wraps ErrInvalid.
func Check(ctx context.Context, b []byte) (Info, error) {
	if len(b) > MaxBytes {
		return Info{}, refuse("the icon is at most 256 KiB")
	}
	idat, err := chunks(b)
	if err != nil {
		return Info{}, err
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return Info{}, refuse("the PNG header is not valid")
	}
	if cfg.Width != cfg.Height || cfg.Width < MinSide || cfg.Width > MaxSide {
		return Info{}, refuse("the icon is square, %d to %d pixels a side (this one is %dx%d)", MinSide, MaxSide, cfg.Width, cfg.Height)
	}
	if err := zlibEnd(idat); err != nil {
		return Info{}, err
	}
	select {
	case decodes <- struct{}{}:
		defer func() { <-decodes }()
	case <-ctx.Done():
		return Info{}, ctx.Err()
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return Info{}, refuse("the PNG image could not be decoded")
	}
	if bd := img.Bounds(); bd.Min.X != 0 || bd.Min.Y != 0 || bd.Dx() != cfg.Width || bd.Dy() != cfg.Height {
		return Info{}, refuse("the PNG image is not the size its header says")
	}
	return Info{Width: cfg.Width, Height: cfg.Height}, nil
}

// fixed are the small colour and physical chunks allowed, with their exact lengths (bKGD's by the
// colour type, below), and whether each must come before PLTE.
var fixed = map[string]struct {
	length     int
	beforePLTE bool
}{
	"sRGB": {1, true}, "gAMA": {4, true}, "cHRM": {32, true}, "pHYs": {9, false},
}

// refused are chunks named in a refusal: the ones an exporter commonly writes.
var refused = map[string]string{
	"iCCP": "a colour profile (iCCP)", "tEXt": "text (tEXt)", "zTXt": "text (zTXt)", "iTXt": "text (iTXt)", "eXIf": "Exif data (eXIf)",
	"acTL": "animation (acTL)", "fcTL": "animation (fcTL)", "fdAT": "animation (fdAT)", "sPLT": "a suggested palette (sPLT)",
}

// chunks checks the signature and the chunks, and returns the IDAT chunks' data concatenated.
func chunks(b []byte) ([]byte, error) {
	if !bytes.HasPrefix(b, signature) {
		return nil, refuse("the icon is not a PNG file")
	}
	seen := map[string]bool{}
	var colourType byte
	var idat []byte
	inIDAT, afterIDAT, ended := false, false, false
	for pos := len(signature); pos < len(b); {
		if ended {
			return nil, refuse("the PNG has bytes after IEND")
		}
		if len(b)-pos < 12 {
			return nil, refuse("the PNG is truncated")
		}
		n := binary.BigEndian.Uint32(b[pos:])
		if n > uint32(len(b)-pos-12) {
			return nil, refuse("the PNG is truncated")
		}
		typ := string(b[pos+4 : pos+8])
		data := b[pos+8 : pos+8+int(n)]
		if crc32.ChecksumIEEE(b[pos+4:pos+8+int(n)]) != binary.BigEndian.Uint32(b[pos+8+int(n):]) {
			return nil, refuse("a PNG chunk's checksum does not match: the file is corrupt")
		}
		first := pos == len(signature)
		pos += 12 + int(n)
		if first && typ != "IHDR" {
			return nil, refuse("the PNG does not start with its header (IHDR)")
		}
		if typ != "IDAT" && seen[typ] {
			return nil, refuse("the PNG has the chunk %s more than once", typ)
		}
		if typ != "IDAT" && inIDAT {
			inIDAT, afterIDAT = false, true
		}
		before := func(what string) error { // the chunk must come before the image data
			if inIDAT || afterIDAT {
				return refuse("the chunk %s comes after the image data (IDAT); it must come before", what)
			}
			return nil
		}
		switch typ {
		case "IHDR":
			if len(data) != 13 {
				return nil, refuse("the PNG header (IHDR) has the wrong length")
			}
			colourType = data[9]
		case "PLTE":
			if err := before(typ); err != nil {
				return nil, err
			}
			if seen["tRNS"] || seen["bKGD"] {
				return nil, refuse("the palette (PLTE) comes after tRNS or bKGD; it must come before them")
			}
		case "tRNS", "bKGD":
			if err := before(typ); err != nil {
				return nil, err
			}
			if colourType == 3 && !seen["PLTE"] {
				return nil, refuse("the chunk %s comes before the palette (PLTE); it must come after it", typ)
			}
			if typ == "bKGD" {
				want := map[byte]int{0: 2, 4: 2, 2: 6, 6: 6, 3: 1}[colourType]
				if len(data) != want {
					return nil, refuse("the chunk bKGD has the wrong length for the colour type")
				}
			}
		case "sRGB", "gAMA", "cHRM", "pHYs":
			if err := before(typ); err != nil {
				return nil, err
			}
			f := fixed[typ]
			if len(data) != f.length {
				return nil, refuse("the chunk %s has the wrong length (%d bytes, not %d)", typ, len(data), f.length)
			}
			if f.beforePLTE && seen["PLTE"] {
				return nil, refuse("the chunk %s comes after the palette (PLTE); it must come before it", typ)
			}
		case "IDAT":
			if afterIDAT {
				return nil, refuse("the PNG's image data (IDAT) is split by another chunk")
			}
			inIDAT = true
			idat = append(idat, data...)
		case "IEND":
			if len(data) != 0 {
				return nil, refuse("the PNG's end (IEND) is not empty")
			}
			if !seen["IDAT"] {
				return nil, refuse("the PNG has no image data (IDAT)")
			}
			ended = true
		default:
			if what, ok := refused[typ]; ok {
				return nil, refuse("the PNG carries %s, which an icon may not; strip it (oxipng --strip all, pngcrush -rem allb)", what)
			}
			return nil, refuse("the PNG carries a chunk an icon may not; strip it (oxipng --strip all, pngcrush -rem allb)")
		}
		seen[typ] = true
	}
	if !ended {
		return nil, refuse("the PNG has no end (IEND): the file is truncated")
	}
	return idat, nil
}

// zlibEnd checks the image data is one zlib stream that ends exactly at the last IDAT's last byte:
// Go's decoder ignores what follows the stream. The data read is bounded (maxRaw), and the header's
// size was checked before.
func zlibEnd(idat []byte) error {
	br := bytes.NewReader(idat) // an io.ByteReader: zlib reads no byte past its stream
	zr, err := zlib.NewReader(br)
	if err != nil {
		return refuse("the PNG's image data is not a zlib stream")
	}
	n, err := io.Copy(io.Discard, io.LimitReader(zr, maxRaw+1))
	if err != nil {
		return refuse("the PNG's image data is corrupt")
	}
	if n > maxRaw {
		return refuse("the PNG's image data is larger than its size allows")
	}
	if br.Len() != 0 {
		return refuse("the PNG's image data (IDAT) goes on past its zlib stream's end")
	}
	return nil
}
