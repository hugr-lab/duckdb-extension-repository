package extfile

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"os"
	"path/filepath"
	"testing"
)

func FuzzParseMetadata(f *testing.F) {
	for _, name := range []string{"demo_capi", "httpfs", "loadable_extension_demo"} {
		data, err := os.ReadFile(filepath.Join("testdata", name+".footer"))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data[:MetadataSize])
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var block [MetadataSize]byte
		copy(block[:], data)
		m, err := ParseMetadata(block)
		if err != nil {
			return
		}
		for _, s := range []string{m.Platform, m.DuckDBVersion, m.CAPIVersion, m.ExtensionVersion} {
			for i := 0; i < len(s); i++ {
				if !safeByte(s[i]) {
					t.Fatalf("unsafe byte in %q", s)
				}
			}
			if s == "." || s == ".." {
				t.Fatalf("dot segment %q", s)
			}
		}
	})
}

func FuzzOpen(f *testing.F) {
	for _, name := range []string{"demo_capi", "httpfs"} {
		data, err := os.ReadFile(filepath.Join("testdata", name+".footer"))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(append([]byte("body"), data...))
	}
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		ext, err := Open(bytes.NewReader(data), int64(len(data)), 1<<20)
		if err != nil {
			return
		}
		if ext.BodySize != int64(len(data))-SignatureSize || len(ext.Signature) != SignatureSize {
			t.Fatalf("sizes: %+v", ext)
		}
	})
}

func FuzzParsePublicKey(f *testing.F) {
	key, err := rsa.GenerateKey(rand.Reader, KeyBits)
	if err != nil {
		f.Fatal(err)
	}
	pemKey, _ := MarshalPublicKeyPEM(&key.PublicKey)
	b64, _ := MarshalPublicKeyBase64(&key.PublicKey)
	f.Add(pemKey)
	f.Add(b64)
	f.Add("-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n")
	f.Fuzz(func(t *testing.T, s string) {
		if k, err := ParsePublicKey(s); err == nil {
			if CheckKey(k) != nil {
				t.Fatal("returned a key that fails CheckKey")
			}
		}
	})
}

func FuzzHashBody(f *testing.F) {
	f.Add([]byte("x"), int64(10))
	f.Fuzz(func(t *testing.T, data []byte, max int64) {
		if max < 0 {
			max = -max
		}
		h1, n, err := HashBody(bytes.NewReader(data), max)
		if err != nil {
			if int64(len(data)) <= max {
				t.Fatalf("refused %d bytes with max %d: %v", len(data), max, err)
			}
			return
		}
		var c compositeHasher
		c.Write(data)
		if n != int64(len(data)) || h1 != c.Sum() {
			t.Fatal("streaming and one-shot hashes differ")
		}
	})
}
