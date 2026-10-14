package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Shutdown waits for background work at most shutdown_timeout (spec 0013), and says so.
func TestWaitBounded(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&out, nil))
	done := make(chan struct{})
	close(done)
	waitBounded(done, time.Now().Add(time.Hour), log, "x")
	if out.Len() != 0 {
		t.Fatalf("logged for work that finished: %s", out.String())
	}
	// one deadline for every wait: the second gives up at once
	start := time.Now()
	deadline := start.Add(50 * time.Millisecond)
	waitBounded(make(chan struct{}), deadline, log, "the background runners")
	waitBounded(make(chan struct{}), deadline, log, "the event writer")
	if time.Since(start) > 500*time.Millisecond || !strings.Contains(out.String(), "the background runners") ||
		!strings.Contains(out.String(), "the event writer") {
		t.Fatalf("after %s: %s", time.Since(start), out.String())
	}
}
