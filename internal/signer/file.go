package signer

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
)

// maxKeyFile bounds the size of a key file read into memory.
const maxKeyFile = 64 << 10

// File is a Signer backed by an unencrypted PEM private key file. It is for development and tests:
// production configuration must opt in explicitly (spec 0002).
type File struct {
	path string
	key  *rsa.PrivateKey
}

// OpenFile loads a key file. On unix the path is opened without following a final symlink
// (O_NOFOLLOW); on every platform the path must not be a symlink and must be the same file that was
// opened. The opened file must be a regular file and, on unix, owned by the current user with no
// permissions for group or others (other platforms skip the owner and mode checks). The file must
// hold exactly one unencrypted PKCS#8 or PKCS#1 RSA private key, 2048 bits, e = 65537.
func OpenFile(path string) (*File, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, fmt.Errorf("signer: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("signer: %w", err)
	}
	if err := checkKeyFile(path, fi); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil {
		return nil, fmt.Errorf("signer: reading %s: %w", path, err)
	}
	if len(data) > maxKeyFile {
		return nil, fmt.Errorf("signer: %s is larger than %d bytes", path, maxKeyFile)
	}
	key, err := parsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("signer: %s: %w", path, err)
	}
	return &File{path: path, key: key}, nil
}

func checkKeyFile(path string, fi os.FileInfo) error {
	li, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("signer: %w", err)
	}
	if li.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("signer: %s is a symlink", path)
	}
	if !os.SameFile(li, fi) {
		return fmt.Errorf("signer: %s changed while it was opened", path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("signer: %s is not a regular file", path)
	}
	if !unixPerms {
		return nil
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("signer: %s has mode %04o, at most 0600 is allowed", path, perm)
	}
	if !ownedByCurrentUser(fi) {
		return fmt.Errorf("signer: %s is not owned by the current user", path)
	}
	return nil
}

func parsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("data after the PEM block")
	}
	if block.Type == "ENCRYPTED PRIVATE KEY" || block.Headers["Proc-Type"] != "" {
		return nil, errors.New("encrypted keys are not supported")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		var ok bool
		if key, ok = k.(*rsa.PrivateKey); !ok {
			return nil, errors.New("not an RSA key")
		}
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		key = k
	default:
		return nil, fmt.Errorf("PEM type %q is not a private key", block.Type)
	}
	if err := key.Validate(); err != nil {
		return nil, err
	}
	if err := extfile.CheckKey(&key.PublicKey); err != nil {
		return nil, err
	}
	return key, nil
}

// Sign implements Signer.
func (f *File) Sign(_ context.Context, h extfile.BodyHash) ([]byte, error) {
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, h[:])
	if err != nil {
		return nil, fmt.Errorf("signer: %w", err)
	}
	if err := checkSignature(&f.key.PublicKey, h, sig); err != nil {
		return nil, err
	}
	return sig, nil
}

// Public implements Signer.
func (f *File) Public() *rsa.PublicKey { return &f.key.PublicKey }

// ID implements Signer: "file:" and the path.
func (f *File) ID() string { return "file:" + f.path }

// GenerateKeyFile writes a new RSA-2048 private key as PKCS#8 PEM with mode 0600. It refuses to
// overwrite an existing file. It is for development and tests.
func GenerateKeyFile(path string) error {
	key, err := rsa.GenerateKey(rand.Reader, extfile.KeyBits)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("signer: %w", err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
