package extfile

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
)

func keyPEM(t *testing.T, pub *rsa.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// rewrap re-wraps a PEM's base64 at width columns.
func rewrap(t *testing.T, p string, width int) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(p), "\n")
	b64 := strings.Join(lines[1:len(lines)-1], "")
	out := lines[0] + "\n"
	for len(b64) > width {
		out += b64[:width] + "\n"
		b64 = b64[width:]
	}
	return out + b64 + "\n" + lines[len(lines)-1] + "\n"
}

func encodeKeys(t *testing.T, pems ...string) []byte {
	t.Helper()
	b, err := EncodeLicenseKeys(pems...)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rawBlock builds a block without EncodeLicenseKeys' limits: count and each length as given.
func rawBlock(count byte, keys ...string) []byte {
	b := append([]byte(nil), licenseKeysMagic...)
	b = append(b, count)
	for _, k := range keys {
		b = append(b, byte(len(k)>>8), byte(len(k)))
		b = append(b, k...)
	}
	return append(b, licenseKeysEnd...)
}

// withBlocks returns an extension file whose body holds the blocks at the given offsets of an
// n-byte filler, followed by the metadata.
func withBlocks(t *testing.T, n int, at map[int][]byte) []byte {
	t.Helper()
	b := body(n)
	for off, blk := range at {
		copy(b[off:], blk)
	}
	m, err := EncodeMetadata(Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: "1.0", ABI: ABICPP})
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, MetadataPrefix...)
	b = append(b, m[:]...)
	return append(b, make([]byte, SignatureSize)...)
}

func openKeys(t *testing.T, file []byte) LicenseKeys {
	t.Helper()
	f, err := Open(bytes.NewReader(file), int64(len(file)), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	return f.LicenseKeys
}

func kids(lk LicenseKeys) []string {
	var out []string
	for _, k := range lk.Keys {
		out = append(out, k.Kid)
	}
	return out
}

func TestLicenseKeys(t *testing.T) {
	k1, k2 := newKey(t), newKey(t)
	p1, p2 := keyPEM(t, &k1.PublicKey), keyPEM(t, &k2.PublicKey)
	fp1, fp2 := Fingerprint(&k1.PublicKey), Fingerprint(&k2.PublicKey)

	if lk := openKeys(t, withBlocks(t, 4096, nil)); lk.Err != nil || len(lk.Keys) != 0 {
		t.Errorf("no block: %+v", lk)
	}
	one := encodeKeys(t, p1, p2)
	lk := openKeys(t, withBlocks(t, 4096, map[int][]byte{100: one}))
	if lk.Err != nil || strings.Join(kids(lk), ",") != fp1+","+fp2 || lk.Keys[0].PEM != p1 {
		t.Errorf("one block: %+v", lk)
	}
	if lk := openKeys(t, withBlocks(t, 40000, map[int][]byte{100: one, 20000: one})); lk.Err != nil || len(lk.Keys) != 2 {
		t.Errorf("two identical blocks: %+v", lk)
	}
	// a block across the hashing pass's chunk boundary, and one that ends the body's filler
	split := withBlocks(t, ChunkSize+8192, map[int][]byte{ChunkSize - 7: one})
	if lk := openKeys(t, split); lk.Err != nil || len(lk.Keys) != 2 {
		t.Errorf("split block: %+v", lk)
	}
	if lk := openKeys(t, withBlocks(t, 4096, map[int][]byte{4096 - len(one): one})); lk.Err != nil || len(lk.Keys) != 2 {
		t.Errorf("block at the end: %+v", lk)
	}
	full := []string{p1, p2, keyPEM(t, &newKey(t).PublicKey), keyPEM(t, &newKey(t).PublicKey)}
	if lk := openKeys(t, withBlocks(t, 40000, map[int][]byte{0: encodeKeys(t, full...)})); lk.Err != nil || len(lk.Keys) != 4 {
		t.Errorf("four keys: %+v", lk)
	}

	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, _ := x509.MarshalPKIXPublicKey(&ec.PublicKey)
	ecPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: ecDER}))
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	e3 := *k1
	e3.E = 3
	headers := strings.Replace(p1, "-----\n", "-----\nProc-Type: 4,ENCRYPTED\n\n", 1)
	malformed := map[string][]byte{
		"differing blocks":  append(append([]byte(nil), one...), encodeKeys(t, p1)...),
		"count 0":           rawBlock(0),
		"count 5":           rawBlock(5, append(full, p1)...),
		"empty key":         rawBlock(1, ""),
		"long key":          rawBlock(1, strings.Repeat("a", MaxLicenseKeySize+1)),
		"1024-bit key":      encodeKeys(t, keyPEM(t, &small.PublicKey)),
		"EC key":            encodeKeys(t, ecPEM),
		"e = 3":             encodeKeys(t, keyPEM(t, &e3.PublicKey)),
		"trailing bytes":    encodeKeys(t, p1+"\n"),
		"leading bytes":     encodeKeys(t, "x\n"+p1),
		"leading space":     encodeKeys(t, " "+p1),
		"CRLF":              encodeKeys(t, strings.ReplaceAll(p1, "\n", "\r\n")),
		"76 columns":        encodeKeys(t, rewrap(t, p1, 76)),
		"PEM headers":       encodeKeys(t, headers),
		"private key type":  encodeKeys(t, strings.ReplaceAll(p1, "PUBLIC KEY", "RSA PUBLIC KEY")),
		"duplicate key":     encodeKeys(t, p1, p2, p1),
		"no end marker":     rawBlock(1, p1)[:len(rawBlock(1, p1))-1],
		"count over keys":   rawBlock(2, p1),
		"truncated":         encodeKeys(t, p1)[:len(licenseKeysMagic)+3],
		"two keys, one bad": encodeKeys(t, p1, keyPEM(t, &small.PublicKey)),
	}
	for name, blk := range malformed {
		at := map[int][]byte{100: blk}
		if name == "differing blocks" {
			at = map[int][]byte{100: one, 20000: encodeKeys(t, p1)}
		}
		n := 40000
		if name == "truncated" {
			// the block is cut by the end of the body
			at = map[int][]byte{n - len(blk): blk}
		}
		lk := openKeys(t, withBlocks(t, n, at))
		if !errors.Is(lk.Err, ErrLicenseKeysMalformed) || len(lk.Keys) != 0 {
			t.Errorf("%s: %+v", name, lk)
		}
	}

	many := map[int][]byte{}
	for i := range maxLicenseKeyBlocks + 1 {
		many[i*len(one)] = one
	}
	if lk := openKeys(t, withBlocks(t, 40000, many)); !errors.Is(lk.Err, ErrLicenseKeysMalformed) {
		t.Errorf("too many blocks: %+v", lk)
	}
	many = map[int][]byte{}
	for i := range maxLicenseKeyBlocks {
		many[i*len(one)] = one
	}
	if lk := openKeys(t, withBlocks(t, 40000, many)); lk.Err != nil || len(lk.Keys) != 2 {
		t.Errorf("%d identical blocks: %+v", maxLicenseKeyBlocks, lk)
	}
}

// The scan finds the magic whatever the writes' sizes.
func TestMagicScannerWrites(t *testing.T) {
	data := append(append(body(3000), licenseKeysMagic...), body(50)...)
	data = append(data, licenseKeysMagic[:10]...)
	data = append(data, licenseKeysMagic...)
	want := []int64{3000, int64(3000 + len(licenseKeysMagic) + 50 + 10)}
	for _, size := range []int{1, 2, 7, 20, 21, 22, 100, len(data)} {
		var s magicScanner
		for p := data; len(p) > 0; {
			k := min(size, len(p))
			s.Write(p[:k])
			p = p[k:]
		}
		if len(s.hits) != 2 || s.hits[0] != want[0] || s.hits[1] != want[1] {
			t.Errorf("writes of %d: hits %v, want %v", size, s.hits, want)
		}
	}
}

func TestEncodeLicenseKeysLimits(t *testing.T) {
	if _, err := EncodeLicenseKeys(); err == nil {
		t.Error("no keys")
	}
	if _, err := EncodeLicenseKeys("a", "b", "c", "d", "e"); err == nil {
		t.Error("five keys")
	}
	if _, err := EncodeLicenseKeys(strings.Repeat("a", MaxLicenseKeySize+1)); err == nil {
		t.Error("a long key")
	}
}

func FuzzParseLicenseKeys(f *testing.F) {
	k, err := rsa.GenerateKey(rand.Reader, KeyBits)
	if err != nil {
		f.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	p := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	good, _ := EncodeLicenseKeys(p)
	f.Add(good)
	f.Add(rawBlock(2, p))
	f.Fuzz(func(t *testing.T, b []byte) {
		keys, size, err := parseLicenseKeys(append(append([]byte(nil), licenseKeysMagic...), b...))
		if err != nil {
			return
		}
		if size > maxLicenseBlock || len(keys) < 1 || len(keys) > MaxLicenseKeys {
			t.Fatalf("parsed %d keys, size %d", len(keys), size)
		}
	})
}
