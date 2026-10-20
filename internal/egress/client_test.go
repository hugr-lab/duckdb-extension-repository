package egress

import "testing"

// A 5xx or 429 is an answer that may pass (spec 0024: a bootstrap tries again); anything else is not.
func TestStatusTemporary(t *testing.T) {
	for code, want := range map[int]bool{500: true, 502: true, 503: true, 429: true, 404: false, 403: false, 400: false, 301: false} {
		if got := (&StatusError{Status: code}).Temporary(); got != want {
			t.Errorf("%d: %v", code, got)
		}
	}
}
