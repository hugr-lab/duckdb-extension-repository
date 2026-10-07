// Command kista is the kista server and its tools.
//
//	kista ext inspect <file> [--key <pub>]...   metadata, body hash, size, signature fingerprint
//	kista ext verify  <file> --key <pub>...     exit 0 if any key verifies the signature
//	kista admin -config <file> <command> ...    the server administrator's CLI (kista admin help)
package main

import (
	"crypto/rsa"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
)

// maxExtensionSize bounds the files the tools read. DuckDB extensions are tens to a few hundred MB.
const maxExtensionSize = 1 << 30

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) >= 1 && args[0] == "admin" {
		return admin(args[1:], os.Environ(), stdout, stderr)
	}
	if len(args) < 2 || args[0] != "ext" {
		fmt.Fprintln(stderr, "usage: kista ext inspect|verify <file> [--key <public key file>]...")
		fmt.Fprintln(stderr, "       kista admin -config <file> <command> ...   (kista admin help)")
		return 2
	}
	switch args[1] {
	case "inspect":
		return inspect(args[2:], stdout, stderr)
	case "verify":
		return verify(args[2:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "kista ext: unknown command %q\n", args[1])
	return 2
}

type keyFlags []string

func (k *keyFlags) String() string     { return fmt.Sprint(*k) }
func (k *keyFlags) Set(v string) error { *k = append(*k, v); return nil }

// parse handles "<file> --key a --key b" in any order.
func parse(name string, args []string, stderr io.Writer) (string, []string, bool) {
	fs := flag.NewFlagSet("kista ext "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var keys keyFlags
	fs.Var(&keys, "key", "public key file (SPKI PEM or base64 DER); repeatable")
	var file string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return "", nil, false
		}
		args = fs.Args()
		if len(args) > 0 {
			if file != "" {
				fmt.Fprintln(stderr, "kista ext: one file at a time")
				return "", nil, false
			}
			file, args = args[0], args[1:]
		}
	}
	if file == "" {
		fmt.Fprintf(stderr, "usage: kista ext %s <file> [--key <public key file>]...\n", name)
		return "", nil, false
	}
	return file, keys, true
}

func loadKeys(paths []string) ([]*extfileKey, error) {
	var out []*extfileKey
	for _, p := range paths {
		data, err := readSmallFile(p)
		if err != nil {
			return nil, err
		}
		pub, err := extfile.ParsePublicKey(string(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, &extfileKey{path: p, fingerprint: extfile.Fingerprint(pub), pub: pub})
	}
	return out, nil
}

// maxKeyFile bounds a public key file: DuckDB itself caps .well-known at 64 KiB.
const maxKeyFile = 64 << 10

// readSmallFile reads a regular file of at most maxKeyFile bytes.
func readSmallFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxKeyFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxKeyFile)
	}
	return data, nil
}

type extfileKey struct {
	path, fingerprint string
	pub               *rsa.PublicKey
}

func openExtension(path string) (*extfile.File, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	ext, err := extfile.Open(f, fi.Size(), maxExtensionSize)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return ext, func() { f.Close() }, nil
}

func inspect(args []string, stdout, stderr io.Writer) int {
	path, keyPaths, ok := parse("inspect", args, stderr)
	if !ok {
		return 2
	}
	keys, err := loadKeys(keyPaths)
	if err != nil {
		fmt.Fprintln(stderr, "kista ext inspect:", err)
		return 1
	}
	ext, done, err := openExtension(path)
	if err != nil {
		fmt.Fprintln(stderr, "kista ext inspect:", err)
		return 1
	}
	defer done()
	m := ext.Metadata
	fmt.Fprintf(stdout, "file:              %s\n", path)
	fmt.Fprintf(stdout, "body size:         %d\n", ext.BodySize)
	fmt.Fprintf(stdout, "body hash:         %s\n", ext.Hash)
	fmt.Fprintf(stdout, "metadata prefix:   %v\n", ext.HasPrefix)
	fmt.Fprintf(stdout, "platform:          %s\n", m.Platform)
	fmt.Fprintf(stdout, "abi:               %s\n", m.ABI)
	if m.ABI == extfile.ABICStruct {
		fmt.Fprintf(stdout, "c api version:     %s\n", m.CAPIVersion)
	} else {
		fmt.Fprintf(stdout, "duckdb version:    %s\n", m.DuckDBVersion)
	}
	fmt.Fprintf(stdout, "extension version: %s\n", m.ExtensionVersion)
	if len(keys) > 0 {
		if fp, ok := verifyWith(ext, keys); ok {
			fmt.Fprintf(stdout, "signature:         %s\n", fp)
		} else {
			fmt.Fprintln(stdout, "signature:         not by any given key")
		}
	}
	return 0
}

func verify(args []string, stdout, stderr io.Writer) int {
	path, keyPaths, ok := parse("verify", args, stderr)
	if !ok {
		return 2
	}
	if len(keyPaths) == 0 {
		fmt.Fprintln(stderr, "kista ext verify: at least one --key is required")
		return 2
	}
	keys, err := loadKeys(keyPaths)
	if err != nil {
		fmt.Fprintln(stderr, "kista ext verify:", err)
		return 1
	}
	ext, done, err := openExtension(path)
	if err != nil {
		fmt.Fprintln(stderr, "kista ext verify:", err)
		return 1
	}
	defer done()
	fp, ok := verifyWith(ext, keys)
	if !ok {
		fmt.Fprintln(stderr, "kista ext verify: the signature does not verify with any given key")
		return 1
	}
	fmt.Fprintln(stdout, fp)
	return 0
}

func verifyWith(ext *extfile.File, keys []*extfileKey) (string, bool) {
	pubs := make([]*rsa.PublicKey, len(keys))
	for i, k := range keys {
		pubs[i] = k.pub
	}
	return extfile.Verify(ext.Hash, ext.Signature, pubs)
}
