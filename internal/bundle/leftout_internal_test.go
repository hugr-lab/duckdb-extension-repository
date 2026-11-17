package bundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// Spec 0046 (review): the entries read through for a left-out source are not remembered, so a
// reader's memory does not grow with entries the manifest does not name.
func TestLeftOutEntriesAreNotRemembered(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	m := Manifest{Format: ManifestFormat, ID: "0192f0c4-1b2a-7c3d-8e4f-0123456789ab", CreatedAt: now,
		Plan:    PlanRef{Tenant: "acme", CreatedAt: now, SHA256: strings.Repeat("b", 64)},
		Sources: []Source{{Kind: "adbc-registry", PackageType: "adbc_driver", Prefix: "https://drivers.example.org/adbc"}}}
	md, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := WriteHead(w, md, nil); err != nil {
		t.Fatal(err)
	}
	const n = 5000
	for i := range n {
		if err := w.Entry(fmt.Sprintf("files/%064x.gz", i), 0, strings.NewReader("")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := Open(bytes.NewReader(buf.Bytes()), now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Next(); err != io.EOF {
		t.Fatalf("reading through: %v", err)
	}
	if len(b.seen) != 0 {
		t.Fatalf("%d names remembered of %d entries the manifest does not name", len(b.seen), n)
	}
	if b.Skipped() < n*512 || b.uncounted != b.Skipped() {
		t.Fatalf("read through: %d bytes, %d uncounted", b.Skipped(), b.uncounted)
	}
}
