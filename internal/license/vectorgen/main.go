// Command vectorgen writes spec 0012's licence test vectors, and signs licences with the test-only
// key for developers testing the SDK without an issuer:
//
//	go run ./internal/license/vectorgen                         writes internal/license/testdata/vectors.json
//	go run ./internal/license/vectorgen sign -claims <file>     prints a licence signed with the test key
//
// The claims file is the payload as JSON, used byte for byte. No release build trusts the test key.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hugr-lab/duckdb-extension-repository/internal/license/vectors"
)

const testdata = "internal/license/testdata"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "vectorgen:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "sign" {
		fs := flag.NewFlagSet("sign", flag.ContinueOnError)
		claims := fs.String("claims", "", "the payload, a JSON file")
		dir := fs.String("dir", testdata, "the directory with the test keys")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *claims == "" || fs.NArg() != 0 {
			return fmt.Errorf("usage: vectorgen sign -claims <file>")
		}
		payload, err := os.ReadFile(*claims)
		if err != nil {
			return err
		}
		key, err := vectors.LoadKey(*dir, vectors.TrustedKey)
		if err != nil {
			return err
		}
		fmt.Println(vectors.Sign(key, vectors.Header(key), string(payload)))
		return nil
	}
	if len(args) != 0 {
		return fmt.Errorf("usage: vectorgen [sign -claims <file>]")
	}
	b, err := vectors.Generate(testdata)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(testdata, "vectors.json"), b, 0o644)
}
