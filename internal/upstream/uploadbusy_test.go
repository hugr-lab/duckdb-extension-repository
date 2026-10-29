package upstream_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"sync"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store/storetest"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream"
)

// gated is a chunk's body that tells when the chunk is being stored (its upload slot and an ingest
// slot taken), then waits for the gate.
type gated struct {
	started chan<- struct{}
	gate    <-chan struct{}
	once    sync.Once
}

func (g *gated) Read([]byte) (int, error) {
	g.once.Do(func() { g.started <- struct{}{} })
	<-g.gate
	return 0, errors.New("gated: released")
}

// Spec 0037: a chunk past its upload's chunks in flight is refused ErrUploadBusy (the API's
// chunks-busy), and one that finds no ingest slot free ErrIngestBusy (ingest-busy), two causes told
// apart.
func TestUploadChunksBusy(t *testing.T) {
	e := storetest.Engines(t)[0]
	g := newGateway(t, e) // its blob service takes 4 ingests at once, as many as an upload's chunks in flight
	g.up.Uploads = upstream.UploadSettings{MinChunkSize: 512, MaxUploads: 2}
	const chunk = 512
	data := make([]byte, 10*chunk)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	create := func() string {
		t.Helper()
		u, err := g.up.CreateUpload(ctx, admin, "site", int64(len(data)), hexOf(data), chunk, upstream.UploadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	first, second := create(), create()
	part := func(n int64) []byte { return data[n*chunk : (n+1)*chunk] }
	started, gate := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	for n := range int64(4) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = g.up.PutUploadChunk(ctx, admin, "site", first, n, &gated{started: started, gate: gate}, chunk, hexOf(part(n)))
		}()
		<-started
	}
	defer func() { close(gate); wg.Wait() }()
	if err := g.up.PutUploadChunk(ctx, admin, "site", first, 4, bytes.NewReader(part(4)), chunk, hexOf(part(4))); !errors.Is(err, upstream.ErrUploadBusy) {
		t.Fatalf("a fifth chunk of the upload: %v", err)
	}
	if err := g.up.PutUploadChunk(ctx, admin, "site", second, 0, bytes.NewReader(part(0)), chunk, hexOf(part(0))); !errors.Is(err, upstream.ErrIngestBusy) {
		t.Fatalf("another upload's chunk without an ingest slot: %v", err)
	}
}
