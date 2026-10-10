// Command fixture writes an extension file the console's end-to-end test adds unchecked (spec
// 0015): a body, DuckDB's metadata footer, an empty signature.
package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: fixture <file> <version> <seed byte>")
		os.Exit(2)
	}
	block, err := extfile.EncodeMetadata(extfile.Metadata{Platform: "linux_amd64", DuckDBVersion: "v2.0.0", ExtensionVersion: os.Args[2],
		ABI: extfile.ABICPP})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	body := append(append(bytes.Repeat([]byte{os.Args[3][0]}, 5000), extfile.MetadataPrefix...), block[:]...)
	if err := os.WriteFile(os.Args[1], append(body, make([]byte, extfile.SignatureSize)...), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
