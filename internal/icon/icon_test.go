package icon_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/icon"
)

var ctx = context.Background()

type chunk struct {
	typ  string
	data []byte
}

// encode is a w x h PNG as Go's encoder writes it (no optional chunks); paletted adds PLTE (and
// tRNS: one colour is transparent).
func encode(t *testing.T, w, h int, paletted bool) []byte {
	t.Helper()
	var img image.Image
	if paletted {
		p := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.RGBA{0, 0, 0, 0}, color.RGBA{200, 10, 10, 255}})
		for i := range p.Pix {
			p.Pix[i] = uint8(i % 2)
		}
		img = p
	} else {
		r := image.NewRGBA(image.Rect(0, 0, w, h))
		for i := range r.Pix {
			r.Pix[i] = uint8(i * 7)
		}
		img = r
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// noise is a w x w PNG of random pixels: it does not compress.
func noise(t *testing.T, w int) []byte {
	t.Helper()
	r := image.NewRGBA(image.Rect(0, 0, w, w))
	for i := range r.Pix {
		r.Pix[i] = uint8(rand.N(256))
	}
	var b bytes.Buffer
	if err := png.Encode(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func parse(t *testing.T, b []byte) []chunk {
	t.Helper()
	var out []chunk
	for pos := 8; pos < len(b); {
		n := int(binary.BigEndian.Uint32(b[pos:]))
		out = append(out, chunk{string(b[pos+4 : pos+8]), b[pos+8 : pos+8+n]})
		pos += 12 + n
	}
	return out
}

func build(cs []chunk) []byte {
	b := []byte("\x89PNG\r\n\x1a\n")
	for _, c := range cs {
		b = binary.BigEndian.AppendUint32(b, uint32(len(c.data)))
		start := len(b)
		b = append(b, c.typ...)
		b = append(b, c.data...)
		b = binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(b[start:]))
	}
	return b
}

// insert puts c before the first chunk of type before.
func insert(t *testing.T, b []byte, before string, c ...chunk) []byte {
	t.Helper()
	cs := parse(t, b)
	for i, x := range cs {
		if x.typ == before {
			return build(append(append(append([]chunk{}, cs[:i]...), c...), cs[i:]...))
		}
	}
	t.Fatalf("no %s chunk", before)
	return nil
}

func accepted(t *testing.T, name string, b []byte, side int) {
	t.Helper()
	info, err := icon.Check(ctx, b)
	if err != nil || info.Width != side || info.Height != side {
		t.Errorf("%s: %+v %v", name, info, err)
	}
}

func refused(t *testing.T, name string, b []byte, want string) {
	t.Helper()
	_, err := icon.Check(ctx, b)
	if !errors.Is(err, icon.ErrInvalid) || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: %v, want a refusal with %q", name, err, want)
	}
}

func TestAccepted(t *testing.T) {
	for _, side := range []int{64, 256, 512} {
		accepted(t, "rgba", encode(t, side, side, false), side)
	}
	pal := encode(t, 64, 64, true)
	if cs := parse(t, pal); cs[1].typ != "PLTE" || cs[2].typ != "tRNS" {
		t.Fatalf("the paletted fixture: %v", cs[1].typ)
	}
	accepted(t, "paletted with tRNS", pal, 64)
	rgba := encode(t, 64, 64, false)
	optional := []chunk{{"sRGB", []byte{0}}, {"gAMA", []byte{0, 0, 0xb1, 0x8f}}, {"cHRM", make([]byte, 32)}, {"pHYs", make([]byte, 9)},
		{"bKGD", make([]byte, 6)}}
	for _, c := range optional {
		accepted(t, "with "+c.typ, insert(t, rgba, "IDAT", c), 64)
	}
	accepted(t, "with all of them", insert(t, rgba, "IDAT", optional...), 64)
	// on a paletted image: the colour chunks before PLTE, bKGD (one index) after it
	accepted(t, "paletted with every optional chunk", insert(t, insert(t, pal, "PLTE", optional[:4]...), "IDAT", chunk{"bKGD", []byte{1}}), 64)
	// several IDAT chunks, consecutive
	cs := parse(t, rgba)
	var split []chunk
	for _, c := range cs {
		if c.typ == "IDAT" && len(c.data) > 10 {
			split = append(split, chunk{"IDAT", c.data[:10]}, chunk{"IDAT", c.data[10:]})
			continue
		}
		split = append(split, c)
	}
	accepted(t, "two IDAT chunks", build(split), 64)
}

func TestRefused(t *testing.T) {
	rgba := encode(t, 64, 64, false)
	pal := encode(t, 64, 64, true)
	// an optional chunk of the wrong length, out of order, after IDAT
	refused(t, "sRGB of 2 bytes", insert(t, rgba, "IDAT", chunk{"sRGB", []byte{0, 0}}), "wrong length")
	refused(t, "pHYs of 8 bytes", insert(t, rgba, "IDAT", chunk{"pHYs", make([]byte, 8)}), "wrong length")
	refused(t, "bKGD of 1 byte on RGBA", insert(t, rgba, "IDAT", chunk{"bKGD", []byte{1}}), "wrong length")
	refused(t, "gAMA after PLTE", insert(t, pal, "tRNS", chunk{"gAMA", make([]byte, 4)}), "after the palette")
	refused(t, "bKGD before PLTE", insert(t, pal, "PLTE", chunk{"bKGD", []byte{1}}), "before the palette")
	refused(t, "pHYs after IDAT", insert(t, rgba, "IEND", chunk{"pHYs", make([]byte, 9)}), "after the image data")
	// each refused chunk, and an unknown one
	for _, typ := range []string{"iCCP", "tEXt", "zTXt", "iTXt", "eXIf", "acTL", "fcTL", "fdAT", "sPLT", "abCd", "vpAg"} {
		refused(t, typ, insert(t, rgba, "IDAT", chunk{typ, []byte("x")}), "strip it")
	}
	refused(t, "a repeated gAMA", insert(t, rgba, "IDAT", chunk{"gAMA", make([]byte, 4)}, chunk{"gAMA", make([]byte, 4)}), "more than once")
	refused(t, "a second IHDR", insert(t, rgba, "IDAT", parse(t, rgba)[0]), "more than once")
	refused(t, "a missing IHDR", build(parse(t, rgba)[1:]), "does not start with its header")
	// bytes after IEND, an HTML tail among them
	refused(t, "a byte after IEND", append(append([]byte{}, rgba...), 0), "after IEND")
	refused(t, "HTML after IEND", append(append([]byte{}, rgba...), "<html><script>alert(1)</script></html>"...), "after IEND")
	refused(t, "a chunk after IEND", build(append(parse(t, rgba), chunk{"IEND", nil})), "after IEND")
	// other formats
	var j, g bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	_ = jpeg.Encode(&j, img, nil)
	_ = gif.Encode(&g, img, nil)
	refused(t, "JPEG", j.Bytes(), "not a PNG")
	refused(t, "GIF", g.Bytes(), "not a PNG")
	refused(t, "WebP", []byte("RIFF\x24\x00\x00\x00WEBPVP8 \x18\x00\x00\x00"), "not a PNG")
	refused(t, "SVG", []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64"><script>alert(1)</script></svg>`), "not a PNG")
	// the size
	refused(t, "not square", encode(t, 64, 65, false), "square")
	refused(t, "63 pixels", encode(t, 63, 63, false), "square")
	refused(t, "513 pixels", encode(t, 513, 513, false), "square")
	if big := noise(t, 512); len(big) <= icon.MaxBytes {
		t.Fatalf("the noise fixture is %d bytes", len(big))
	} else {
		refused(t, "over 256 KiB", big, "at most 256 KiB")
	}
	// truncated and corrupt
	refused(t, "truncated", rgba[:len(rgba)-5], "truncated")
	refused(t, "cut after a chunk", build(parse(t, rgba)[:2]), "no end")
	corrupt := append([]byte{}, rgba...)
	corrupt[60] ^= 0xff
	refused(t, "a flipped byte", corrupt, "checksum")
	cs := parse(t, rgba)
	for i, c := range cs {
		if c.typ == "IDAT" {
			d := append([]byte{}, c.data...)
			d[len(d)/2] ^= 0x55
			cs[i] = chunk{"IDAT", d}
		}
	}
	refused(t, "corrupt image data", build(cs), "image data")
	// a header claiming 100000x100000: refused before anything is decoded
	cs = parse(t, rgba)
	hdr := append([]byte{}, cs[0].data...)
	binary.BigEndian.PutUint32(hdr[0:], 100000)
	binary.BigEndian.PutUint32(hdr[4:], 100000)
	cs[0] = chunk{"IHDR", hdr}
	refused(t, "100000x100000", build(cs), "square")
	// image data that is not the header's image: a 128x128 image's under a 64x64 header, and the
	// reverse
	small, large := parse(t, encode(t, 64, 64, false)), parse(t, encode(t, 128, 128, false))
	refused(t, "image data too large", build([]chunk{small[0], large[1], small[2]}), "")
	refused(t, "image data too small", build([]chunk{large[0], small[1], large[2]}), "")
	// IDAT chunks split by another chunk
	cs = parse(t, rgba)
	refused(t, "IDAT split", build([]chunk{cs[0], {"IDAT", cs[1].data[:10]}, {"gAMA", make([]byte, 4)}, {"IDAT", cs[1].data[10:]}, cs[2]}), "image data")
	// IDAT(image) IDAT(<html>) IEND: data past the zlib stream's end, which Go's decoder ignores
	tail := build([]chunk{cs[0], cs[1], {"IDAT", []byte("<html><script>alert(1)</script></html>")}, cs[2]})
	if _, err := png.Decode(bytes.NewReader(tail)); err != nil {
		t.Fatalf("the fixture: Go's decoder takes it: %v", err)
	}
	refused(t, "data after the zlib stream", tail, "past its zlib stream")
	refused(t, "an empty IEND with data", build([]chunk{cs[0], cs[1], {"IEND", []byte{0}}}), "not empty")
	refused(t, "no IDAT", build([]chunk{cs[0], cs[2]}), "no image data")
}
