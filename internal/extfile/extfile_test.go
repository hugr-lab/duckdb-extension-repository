package extfile

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// body returns the deterministic test body used for the golden hashes: byte i is (i*31+7) % 251.
func body(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + 7) % 251)
	}
	return b
}

// Golden composite hashes, computed once with duckdb/scripts/compute-extension-hash.sh at the pin.
var golden = map[int]string{
	1:               "b6d58dfa6547c1eb7f0d4ffd3e3bd6452213210ea51baa70b97c31f011187215",
	ChunkSize - 1:   "a7c7938a798409bf2abd3b0ad3d74cce0a3d5a0fb19cbc6f0a20108b4a5fd299",
	ChunkSize:       "7337bd301a08a70edac7eadc94c4d982f550bb0a62ff7df2b150d7e2fa5949a4",
	ChunkSize + 1:   "bdbb3466665ed8cbe8fc26d6cd3ee2d7c88f427bef2982246317b76917649519",
	3*ChunkSize + 7: "60c33ad94c68a37bc117ff8ac45f13291da63403e6a6acd3401333f635250f79",
}

func TestHashBodyGolden(t *testing.T) {
	for n, want := range golden {
		h, size, err := HashBody(bytes.NewReader(body(n)), int64(n))
		if err != nil {
			t.Fatal(err)
		}
		if size != int64(n) || h.String() != want {
			t.Errorf("n=%d: got %s (%d bytes), want %s", n, h, size, want)
		}
	}
	// zero chunks: DuckDB hashes the empty concatenation
	h, _, err := HashBody(bytes.NewReader(nil), 0)
	if err != nil {
		t.Fatal(err)
	}
	if h != BodyHash(sha256.Sum256(nil)) {
		t.Errorf("empty body: got %s", h)
	}
}

func TestHashBodyMaxSize(t *testing.T) {
	if _, _, err := HashBody(bytes.NewReader(body(11)), 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
	if _, _, err := HashBody(bytes.NewReader(body(10)), 10); err != nil {
		t.Fatal(err)
	}
}

func readFooter(t *testing.T, name string) [MetadataSize]byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".footer"))
	if err != nil {
		t.Fatal(err)
	}
	var block [MetadataSize]byte
	copy(block[:], data[:MetadataSize])
	return block
}

func TestParseMetadataRealFooters(t *testing.T) {
	cases := map[string]Metadata{
		"loadable_extension_demo": {Platform: "osx_arm64", DuckDBVersion: "eb0d9df48e", ExtensionVersion: "default-version", ABI: ABICPP},
		"httpfs":                  {Platform: "osx_arm64", DuckDBVersion: "eb0d9df48e", ExtensionVersion: "5e34903685", ABI: ABICPP},
		"demo_capi":               {Platform: "osx_arm64", CAPIVersion: "v1.5.6", ExtensionVersion: "eb0d9df48e", ABI: ABICStruct},
	}
	for name, want := range cases {
		got, err := ParseMetadata(readFooter(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
		}
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	for _, m := range []Metadata{
		{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: "1.2.3", ABI: ABICPP},
		{Platform: "linux_arm64", CAPIVersion: "v1.2.0", ExtensionVersion: "0.1", ABI: ABICStruct},
		{Platform: "osx_arm64", DuckDBVersion: "eb0d9df48e", ExtensionVersion: "", ABI: ABICStructUnstable},
	} {
		block, err := EncodeMetadata(m)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseMetadata(block)
		if err != nil || got != m {
			t.Errorf("round trip of %+v: got %+v, %v", m, got, err)
		}
	}
}

func TestParseMetadataRefuses(t *testing.T) {
	good := Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: "1", ABI: ABICPP}
	enc := func(m Metadata) [MetadataSize]byte {
		b, err := EncodeMetadata(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	mutate := func(f func(*[MetadataSize]byte)) [MetadataSize]byte {
		b := enc(good)
		f(&b)
		return b
	}
	cases := map[string][MetadataSize]byte{
		"zero block":       {},
		"wrong magic":      mutate(func(b *[MetadataSize]byte) { b[7*32] = '5' }),
		"byte after pad":   mutate(func(b *[MetadataSize]byte) { b[6*32+20] = 'x' }), // platform field
		"dot-dot platform": enc(Metadata{Platform: "..", DuckDBVersion: "v2", ABI: ABICPP}),
		"slash in version": enc(Metadata{Platform: "p", DuckDBVersion: "../x", ABI: ABICPP}),
		"empty platform":   enc(Metadata{DuckDBVersion: "v2", ABI: ABICPP}),
		"empty version":    enc(Metadata{Platform: "p", ABI: ABICPP}),
		"unknown abi": mutate(func(b *[MetadataSize]byte) {
			copy(b[3*32:], "RUST\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")
		}),
		"control character": mutate(func(b *[MetadataSize]byte) { b[6*32] = '\n' }),
	}
	for name, block := range cases {
		if _, err := ParseMetadata(block); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: got %v, want ErrMalformed", name, err)
		}
	}
}

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, KeyBits)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// buildExtension returns body ‖ sig for a body of n bytes ending with prefix + metadata.
func buildExtension(t *testing.T, n int, key *rsa.PrivateKey) ([]byte, BodyHash) {
	t.Helper()
	block, err := EncodeMetadata(Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: "1.0", ABI: ABICPP})
	if err != nil {
		t.Fatal(err)
	}
	b := append(body(n), MetadataPrefix...)
	b = append(b, block[:]...)
	h, _, err := HashBody(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, SignatureSize)
	if key != nil {
		sig, err = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
		if err != nil {
			t.Fatal(err)
		}
	}
	return append(b, sig...), h
}

func TestOpenAndVerify(t *testing.T) {
	key, other := newKey(t), newKey(t)
	file, h := buildExtension(t, 3*ChunkSize+5, key)
	f, err := Open(bytes.NewReader(file), int64(len(file)), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if f.Hash != h || !f.HasPrefix || f.BodySize != int64(len(file)-SignatureSize) {
		t.Fatalf("unexpected file %+v", f)
	}
	fp, ok := Verify(f.Hash, f.Signature, []*rsa.PublicKey{&other.PublicKey, &key.PublicKey})
	if !ok || fp != Fingerprint(&key.PublicKey) {
		t.Fatalf("verify: %q %v", fp, ok)
	}
	if _, ok := Verify(f.Hash, f.Signature, []*rsa.PublicKey{&other.PublicKey}); ok {
		t.Fatal("verified with the wrong key")
	}
	// a changed body byte breaks the signature
	file[10] ^= 1
	f2, err := Open(bytes.NewReader(file), int64(len(file)), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Verify(f2.Hash, f2.Signature, []*rsa.PublicKey{&key.PublicKey}); ok {
		t.Fatal("verified a modified body")
	}
	// re-sign: same body, new signature
	sig, _ := rsa.SignPKCS1v15(rand.Reader, other, crypto.SHA256, f2.Hash[:])
	var out bytes.Buffer
	if err := f2.WriteSigned(&out, sig); err != nil {
		t.Fatal(err)
	}
	f3, err := Open(bytes.NewReader(out.Bytes()), int64(out.Len()), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if f3.Hash != f2.Hash {
		t.Fatal("re-signing changed the body")
	}
	if _, ok := Verify(f3.Hash, f3.Signature, []*rsa.PublicKey{&other.PublicKey}); !ok {
		t.Fatal("re-signed file does not verify")
	}
}

func TestOpenRefuses(t *testing.T) {
	if _, err := Open(bytes.NewReader(make([]byte, 511)), 511, 1<<20); !errors.Is(err, ErrMalformed) {
		t.Errorf("short file: %v", err)
	}
	file, _ := buildExtension(t, 100, nil)
	if _, err := Open(bytes.NewReader(file), int64(len(file)), 100); !errors.Is(err, ErrTooLarge) {
		t.Errorf("max size: %v", err)
	}
}

func TestPublicKeyForms(t *testing.T) {
	key := newKey(t)
	pemKey, err := MarshalPublicKeyPEM(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	b64, err := MarshalPublicKeyBase64(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{pemKey, b64, "  " + b64 + "\n"} {
		pub, err := ParsePublicKey(s)
		if err != nil {
			t.Fatal(err)
		}
		if Fingerprint(pub) != Fingerprint(&key.PublicKey) {
			t.Fatal("fingerprint changed in a round trip")
		}
	}
	if got := DedupKeys([]*rsa.PublicKey{&key.PublicKey, mustParse(t, b64), &newKey(t).PublicKey, nil, {}}); len(got) != 2 {
		t.Fatalf("dedup: %d keys", len(got))
	}
	// invalid keys never panic
	if Fingerprint(&rsa.PublicKey{}) != "" || Fingerprint(nil) != "" {
		t.Fatal("fingerprint of an invalid key")
	}
	if _, ok := Verify(BodyHash{}, make([]byte, SignatureSize), []*rsa.PublicKey{nil, {}}); ok {
		t.Fatal("verified with invalid keys")
	}
	odd := key.PublicKey
	odd.E = 3
	if err := CheckKey(&odd); err == nil {
		t.Fatal("e = 3 accepted")
	}
	oddDER, _ := x509.MarshalPKIXPublicKey(&odd)
	oddPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: oddDER}))
	headerPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Headers: map[string]string{"Comment": "hi"}, Bytes: oddDER}))
	wrapped := b64[:40] + "\n" + b64[40:]

	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&key.PublicKey)}))
	big, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		t.Fatal(err)
	}
	bigPEM, _ := MarshalPublicKeyPEM(&big.PublicKey)
	refused := map[string]string{
		"pkcs1 pem":     pkcs1,
		"3072 bits":     bigPEM,
		"trailing data": pemKey + "garbage",
		"two blocks":    pemKey + pemKey,
		"bad base64":    b64[:len(b64)-3] + "!!!",
		"not a key":     "hello",
		"e = 3":         oddPEM,
		"pem headers":   headerPEM,
		"wrapped b64":   wrapped,
		"text before":   "hello\n" + pemKey,
	}
	for name, s := range refused {
		if _, err := ParsePublicKey(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func mustParse(t *testing.T, s string) *rsa.PublicKey {
	t.Helper()
	k, err := ParsePublicKey(s)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestFingerprintFormat(t *testing.T) {
	key := newKey(t)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	sum := sha256.Sum256(der)
	if got, want := Fingerprint(&key.PublicKey), "sha256:"+hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func precompress(t *testing.T, b []byte) (Precompressed, []byte) {
	t.Helper()
	var stream bytes.Buffer
	pre, err := Precompress(bytes.NewReader(b), &stream, int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pre, stream.Bytes()
}

func TestGzipAssembly(t *testing.T) {
	key := newKey(t)
	for _, n := range []int{0, 100, ChunkSize + 3, 3*ChunkSize + 7} {
		file, h := buildExtension(t, n, key)
		bodyBytes, sig := file[:len(file)-SignatureSize], file[len(file)-SignatureSize:]
		pre, stream := precompress(t, bodyBytes)
		if pre.BodyHash != h || pre.BodyLen != int64(len(bodyBytes)) || pre.StreamLen != int64(len(stream)) {
			t.Fatalf("n=%d: record %+v", n, pre)
		}
		if err := VerifyStream(pre, bytes.NewReader(stream)); err != nil {
			t.Fatal(err)
		}
		var gz bytes.Buffer
		if err := WriteGzip(&gz, pre, bytes.NewReader(stream), h, sig); err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		zr.Multistream(false)
		got, err := io.ReadAll(zr) // checks CRC-32 and size at the end
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !bytes.Equal(got, file) {
			t.Fatalf("n=%d: decompressed file differs", n)
		}
		if zr.Name != "" || zr.Comment != "" || len(zr.Extra) != 0 {
			t.Fatal("header fields set")
		}
		if gz.Bytes()[3] != 0 {
			t.Fatal("FLG is not 0")
		}
		gzipT(t, gz.Bytes())
	}
}

// gzipT runs `gzip -t` when available: an independent check of the trailer.
func gzipT(t *testing.T, data []byte) {
	t.Helper()
	path, err := exec.LookPath("gzip")
	if err != nil {
		return
	}
	cmd := exec.Command(path, "-t")
	cmd.Stdin = bytes.NewReader(data)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gzip -t: %v: %s", err, out)
	}
}

func TestStreamChecks(t *testing.T) {
	// a stream with a final block of its own would make DuckDB stop before our signature block
	var final bytes.Buffer
	w, _ := flate.NewWriter(&final, flate.BestSpeed)
	w.Write([]byte("an old body"))
	w.Close()
	if _, _, err := inflateOpen(bytes.NewReader(final.Bytes())); err == nil {
		t.Fatal("a final block was accepted")
	}
	// our own stream is open and inflates to the body
	b := body(70000)
	pre, stream := precompress(t, b)
	h, n, err := inflateOpen(bytes.NewReader(stream))
	if err != nil || h != pre.BodyHash || n != int64(len(b)) {
		t.Fatalf("own stream: %v %d", err, n)
	}
	// a stored stream is trusted only through its record
	if err := VerifyStream(pre, bytes.NewReader(final.Bytes())); !errors.Is(err, ErrStream) {
		t.Fatalf("tampered stream accepted: %v", err)
	}
	tampered := bytes.Clone(stream)
	tampered[len(tampered)/2] ^= 1
	if err := VerifyStream(pre, bytes.NewReader(tampered)); !errors.Is(err, ErrStream) {
		t.Fatalf("flipped byte accepted: %v", err)
	}
	if err := VerifyStream(pre, bytes.NewReader(stream)); err != nil {
		t.Fatal(err)
	}
}

func TestWriteGzipChecks(t *testing.T) {
	b := make([]byte, 3*ChunkSize+100) // random, so the stream spans several chunks
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	pre, stream := precompress(t, b)
	sig := make([]byte, SignatureSize)
	h := pre.BodyHash
	if err := WriteGzip(io.Discard, pre, bytes.NewReader(stream[:len(stream)-1]), h, sig); !errors.Is(err, ErrStream) {
		t.Errorf("short stream: %v", err)
	}
	if err := WriteGzip(io.Discard, pre, bytes.NewReader(append(bytes.Clone(stream), 0)), h, sig); !errors.Is(err, ErrStream) {
		t.Errorf("long stream: %v", err)
	}
	if err := WriteGzip(io.Discard, pre, bytes.NewReader(stream), h, sig[:10]); err == nil {
		t.Error("short signature accepted")
	}
	if err := WriteGzip(io.Discard, pre, bytes.NewReader(stream), BodyHash{1}, sig); !errors.Is(err, ErrStream) {
		t.Errorf("signature over another body: %v", err)
	}
	// A changed chunk is caught before it is sent: the output stops after the good chunks, with no
	// final block, so nothing past the change reaches the client.
	tampered := bytes.Clone(stream)
	tampered[2*ChunkSize+5] ^= 1
	var out bytes.Buffer
	if err := WriteGzip(&out, pre, bytes.NewReader(tampered), h, sig); !errors.Is(err, ErrStream) {
		t.Fatalf("tampered chunk: %v", err)
	}
	if got, want := out.Len(), len(gzipHeader)+2*ChunkSize; got != want {
		t.Fatalf("sent %d bytes before stopping, want %d", got, want)
	}
	if !bytes.Equal(out.Bytes()[len(gzipHeader):], stream[:2*ChunkSize]) {
		t.Fatal("sent bytes are not the verified prefix")
	}
	// a read error after the stream is reported, not ignored
	r := io.MultiReader(bytes.NewReader(stream), errReader{})
	if err := WriteGzip(io.Discard, pre, r, h, sig); err == nil || errors.Is(err, ErrStream) {
		t.Errorf("read error after the stream: %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

// eofReaderAt returns io.EOF together with a full read that ends at the end of the input, which the
// io.ReaderAt contract allows.
type eofReaderAt struct{ b []byte }

func (e eofReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := bytes.NewReader(e.b).ReadAt(p, off)
	if err == nil && off+int64(n) == int64(len(e.b)) {
		err = io.EOF
	}
	return n, err
}

func TestOpenReaderAtEOF(t *testing.T) {
	file, h := buildExtension(t, 10, nil)
	f, err := Open(eofReaderAt{file}, int64(len(file)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if f.Hash != h {
		t.Fatal("hash differs")
	}
}

func TestMaxSizeBounds(t *testing.T) {
	data := body(100)
	h, n, err := HashBody(bytes.NewReader(data), math.MaxInt64)
	if err != nil || n != 100 {
		t.Fatalf("MaxInt64 limit: %d %v", n, err)
	}
	want, _, _ := HashBody(bytes.NewReader(data), 100)
	if h != want {
		t.Fatal("MaxInt64 limit gives another hash")
	}
	if _, _, err := HashBody(bytes.NewReader(data), -1); err == nil {
		t.Fatal("negative limit accepted")
	}
	if _, err := Precompress(bytes.NewReader(data), io.Discard, math.MaxInt64); err != nil {
		t.Fatal(err)
	}
}

func TestPrecompressMaxSize(t *testing.T) {
	if _, err := Precompress(bytes.NewReader(body(11)), io.Discard, 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestABIString(t *testing.T) {
	if !strings.Contains(ABIType(9).String(), "9") || ABICStructUnstable.String() != "C_STRUCT_UNSTABLE" {
		t.Fatal("ABI strings")
	}
}

func TestGzipFrameSizes(t *testing.T) {
	if len(GzipHeader()) != GzipHeaderSize {
		t.Fatal("header size")
	}
	if n := len(GzipTail(Precompressed{}, make([]byte, SignatureSize))); n != GzipTailSize {
		t.Fatalf("tail size %d", n)
	}
	h := GzipHeader()
	h[0] = 0
	if GzipHeader()[0] != 0x1f {
		t.Fatal("GzipHeader returned shared bytes")
	}
}
