// Command fixture writes an extension file the console's end-to-end test adds (spec 0015): a body,
// DuckDB's metadata footer, an empty signature. With a name, the body is an x86-64 ELF exporting the
// name's entry point (spec 0008's checks pass), and with a public key too, a key block declaring the
// key (spec 0021).
package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/symbols/symtest"
)

func main() {
	if len(os.Args) < 4 || len(os.Args) > 6 {
		fmt.Fprintln(os.Stderr, "usage: fixture <file> <version> <seed byte> [<name> [<licence key PEM>]]")
		os.Exit(2)
	}
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	block, err := extfile.EncodeMetadata(extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: os.Args[2],
		ABI: extfile.ABICPP})
	if err != nil {
		fail(err)
	}
	body := bytes.Repeat([]byte{os.Args[3][0]}, 5000)
	if len(os.Args) == 6 {
		pem, err := os.ReadFile(os.Args[5])
		if err != nil {
			fail(err)
		}
		keys, err := extfile.EncodeLicenseKeys(string(pem))
		if err != nil {
			fail(err)
		}
		body = append(keys, body...)
	}
	if len(os.Args) >= 5 {
		body = append(symtest.ELF(os.Args[4]+"_duckdb_cpp_init"), body...)
	}
	body = append(append(body, extfile.MetadataPrefix...), block[:]...)
	if err := os.WriteFile(os.Args[1], append(body, make([]byte, extfile.SignatureSize)...), 0o600); err != nil {
		fail(err)
	}
}
