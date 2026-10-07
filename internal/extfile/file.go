package extfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// File is a parsed extension file.
type File struct {
	r         io.ReaderAt
	BodySize  int64 // everything before the signature
	Metadata  Metadata
	HasPrefix bool // the 19-byte MetadataPrefix precedes the metadata block
	Hash      BodyHash
	Signature []byte // the trailing 256 bytes, as found
}

// Open parses an extension of the given size from r: the metadata, the body hash and the signature.
// It refuses files smaller than FooterSize or larger than maxSize before hashing.
func Open(r io.ReaderAt, size, maxSize int64) (*File, error) {
	if size < FooterSize {
		return nil, fmt.Errorf("%w: %d bytes, at least %d needed", ErrMalformed, size, FooterSize)
	}
	if size > maxSize {
		return nil, ErrTooLarge
	}
	f := &File{r: r, BodySize: size - SignatureSize}

	var block [MetadataSize]byte
	if err := readAt(r, block[:], f.BodySize-MetadataSize); err != nil {
		return nil, fmt.Errorf("extfile: reading metadata: %w", err)
	}
	m, err := ParseMetadata(block)
	if err != nil {
		return nil, err
	}
	f.Metadata = m

	if start := f.BodySize - MetadataSize - int64(len(MetadataPrefix)); start >= 0 {
		prefix := make([]byte, len(MetadataPrefix))
		if err := readAt(r, prefix, start); err == nil {
			f.HasPrefix = bytes.Equal(prefix, MetadataPrefix)
		}
	}

	h, n, err := HashBody(io.NewSectionReader(r, 0, f.BodySize), f.BodySize)
	if err != nil {
		return nil, err
	}
	if n != f.BodySize {
		return nil, fmt.Errorf("extfile: short body read: %d of %d bytes", n, f.BodySize)
	}
	f.Hash = h

	f.Signature = make([]byte, SignatureSize)
	if err := readAt(r, f.Signature, f.BodySize); err != nil {
		return nil, fmt.Errorf("extfile: reading signature: %w", err)
	}
	return f, nil
}

// readAt fills buf from off. io.ReaderAt may return io.EOF with a full read that ends at the end of
// the input: that is success.
func readAt(r io.ReaderAt, buf []byte, off int64) error {
	n, err := r.ReadAt(buf, off)
	if n == len(buf) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Body returns a reader over the body.
func (f *File) Body() *io.SectionReader { return io.NewSectionReader(f.r, 0, f.BodySize) }

// WriteSigned writes the body followed by sig.
func (f *File) WriteSigned(dst io.Writer, sig []byte) error {
	if len(sig) != SignatureSize {
		return fmt.Errorf("extfile: signature is %d bytes, want %d", len(sig), SignatureSize)
	}
	if _, err := io.Copy(dst, f.Body()); err != nil {
		return err
	}
	_, err := dst.Write(sig)
	return err
}
