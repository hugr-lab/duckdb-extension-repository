package extfile

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// Precompressed describes a body stored as a raw deflate stream that ends with a sync flush: it is
// byte-aligned and has no final block, so a final stored block holding a signature can follow it.
//
// The stream is trusted only through StreamChunks: the SHA-256 of each 1 MiB chunk of the stream.
// WriteGzip checks every chunk before sending it, so a stored stream that was changed (for example
// one with a final block of its own, which would make DuckDB stop before our signature block and
// inflate a different body) is never sent past the first changed chunk. StreamHash is the composite
// hash of the stream, for addressing and for scrubbing.
type Precompressed struct {
	BodyHash     BodyHash
	BodyCRC32    uint32
	BodyLen      int64
	StreamLen    int64
	StreamHash   BodyHash
	StreamChunks [][sha256.Size]byte
}

// ErrStream marks a stored deflate stream that does not match its Precompressed record.
var ErrStream = errors.New("extfile: deflate stream does not match its record")

// Precompress compresses a body of at most maxSize bytes into dst as a raw deflate stream ended with
// a sync flush. Before returning, it re-inflates its own output and checks that it decompresses to
// exactly the body (same composite hash and length) and that no block is final. On error, dst holds
// partial output that the caller must discard.
func Precompress(body io.Reader, dst io.Writer, maxSize int64) (Precompressed, error) {
	pr, pw := io.Pipe()
	type check struct {
		hash BodyHash
		n    int64
		err  error
	}
	done := make(chan check, 1)
	go func() {
		h, n, err := inflateOpen(pr)
		_, _ = io.Copy(io.Discard, pr) // let the writer finish whatever happens
		done <- check{h, n, err}
	}()

	var streamHash compositeHasher
	counter := &countWriter{}
	out := io.MultiWriter(dst, pw, &streamHash, counter)

	fw, err := flate.NewWriter(out, flate.BestCompression)
	if err != nil {
		pw.CloseWithError(err)
		<-done
		return Precompressed{}, err
	}
	var bodyHash compositeHasher
	crc := crc32.NewIEEE()
	n, err := copyMax(fw, io.TeeReader(body, io.MultiWriter(&bodyHash, crc)), maxSize)
	if err == nil {
		// Flush ends the stream with a sync flush (an empty stored block, BFINAL=0). Close is never
		// called: it would write a final block.
		err = fw.Flush()
	}
	pw.CloseWithError(err)
	c := <-done
	if err != nil {
		return Precompressed{}, err
	}
	pre := Precompressed{
		BodyHash:   bodyHash.Sum(),
		BodyCRC32:  crc.Sum32(),
		BodyLen:    n,
		StreamLen:  counter.n,
		StreamHash: streamHash.Sum(),
	}
	pre.StreamChunks = streamHash.Digests()
	if c.err != nil {
		return Precompressed{}, fmt.Errorf("%w: %v", ErrStream, c.err)
	}
	if c.hash != pre.BodyHash || c.n != pre.BodyLen {
		return Precompressed{}, fmt.Errorf("%w: re-inflated body differs", ErrStream)
	}
	return pre, nil
}

// inflateOpen inflates a raw deflate stream that must not be final: without a final block the
// inflater runs out of input, so io.ErrUnexpectedEOF is the expected end, and a clean io.EOF means a
// final block was present. It returns the composite hash and length of the inflated data.
func inflateOpen(r io.Reader) (BodyHash, int64, error) {
	var h compositeHasher
	n, err := io.Copy(&h, flate.NewReader(r))
	switch {
	case err == nil:
		return BodyHash{}, n, errors.New("stream has a final block")
	case errors.Is(err, io.ErrUnexpectedEOF):
		return h.Sum(), n, nil
	}
	return BodyHash{}, n, err
}

// gzipHeader: magic, CM=deflate, FLG=0, MTIME=0, XFL=0, OS=255 (unknown). DuckDB refuses the FTEXT,
// FHCRC, FEXTRA and FCOMMENT flags (src/common/gzip_file_system.cpp).
var gzipHeader = []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 255}

// finalStoredBlock is the header of a final stored deflate block of SignatureSize bytes: BFINAL=1,
// BTYPE=00 (the stream before it is byte-aligned), LEN=256 and NLEN=^256, little-endian.
var finalStoredBlock = []byte{0x01, 0x00, 0x01, 0xff, 0xfe}

// WriteGzip writes a single-member gzip of body ‖ sig: the header, the stored stream, a final stored
// block with the signature, and the trailer (CRC-32 and size of the whole uncompressed file).
//
// signed is the body hash the signature was made over; it must equal pre.BodyHash. The stream is
// read in 1 MiB chunks and each chunk is checked against pre.StreamChunks before it is written, so
// nothing that does not match the record is ever sent; the stream must be exactly pre.StreamLen
// bytes. On an error the output is incomplete (no final block), which DuckDB refuses.
func WriteGzip(dst io.Writer, pre Precompressed, stream io.Reader, signed BodyHash, sig []byte) error {
	if len(sig) != SignatureSize {
		return fmt.Errorf("extfile: signature is %d bytes, want %d", len(sig), SignatureSize)
	}
	if signed != pre.BodyHash {
		return fmt.Errorf("%w: signed body hash %s, stream holds %s", ErrStream, signed, pre.BodyHash)
	}
	if want := (pre.StreamLen + ChunkSize - 1) / ChunkSize; int64(len(pre.StreamChunks)) != want {
		return fmt.Errorf("%w: %d chunk digests for %d bytes", ErrStream, len(pre.StreamChunks), pre.StreamLen)
	}
	if _, err := dst.Write(gzipHeader); err != nil {
		return err
	}
	buf := make([]byte, ChunkSize)
	remaining := pre.StreamLen
	for i := 0; remaining > 0; i++ {
		n := int64(ChunkSize)
		if remaining < n {
			n = remaining
		}
		if _, err := io.ReadFull(stream, buf[:n]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return fmt.Errorf("%w: shorter than the recorded %d bytes", ErrStream, pre.StreamLen)
			}
			return err
		}
		if sha256.Sum256(buf[:n]) != pre.StreamChunks[i] {
			return fmt.Errorf("%w: chunk %d differs", ErrStream, i)
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return err
		}
		remaining -= n
	}
	var probe [1]byte
	k, err := io.ReadFull(stream, probe[:])
	if k != 0 {
		return fmt.Errorf("%w: longer than the recorded %d bytes", ErrStream, pre.StreamLen)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	_, err = dst.Write(GzipTail(pre, sig))
	return err
}

// GzipHeaderSize and GzipTailSize frame a stored stream in WriteGzip's output: the file is
// GzipHeaderSize + StreamLen + GzipTailSize bytes.
const (
	GzipHeaderSize = 10
	GzipTailSize   = 5 + SignatureSize + 8
)

// GzipHeader returns the gzip member header WriteGzip writes.
func GzipHeader() []byte { return bytes.Clone(gzipHeader) }

// GzipTail returns what follows the stream in WriteGzip's output: the final stored block with the
// signature, and the trailer. It panics unless sig is SignatureSize bytes (the block header encodes
// that length).
func GzipTail(pre Precompressed, sig []byte) []byte {
	if len(sig) != SignatureSize {
		panic("extfile: GzipTail needs a SignatureSize signature")
	}
	var tail bytes.Buffer
	tail.Write(finalStoredBlock)
	tail.Write(sig)
	var trailer [8]byte
	binary.LittleEndian.PutUint32(trailer[0:], crc32.Update(pre.BodyCRC32, crc32.IEEETable, sig))
	binary.LittleEndian.PutUint32(trailer[4:], uint32(pre.BodyLen+SignatureSize))
	tail.Write(trailer[:])
	return tail.Bytes()
}

// VerifyStream checks a whole stored stream against its record (length, every chunk, the composite
// hash), for scrubbing storage. Serving does not need it: WriteGzip checks as it sends.
func VerifyStream(pre Precompressed, stream io.Reader) error {
	var h compositeHasher
	n, err := copyMax(&h, stream, pre.StreamLen)
	if errors.Is(err, ErrTooLarge) {
		return ErrStream
	}
	if err != nil {
		return err
	}
	if n != pre.StreamLen || h.Sum() != pre.StreamHash {
		return ErrStream
	}
	digests := h.Digests()
	if len(digests) != len(pre.StreamChunks) {
		return ErrStream
	}
	for i := range digests {
		if digests[i] != pre.StreamChunks[i] {
			return ErrStream
		}
	}
	return nil
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
