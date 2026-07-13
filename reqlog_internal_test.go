package webhttp

import (
	"strings"
	"testing"
)

// TestFallbackRequestID_uniqueAndValid exercises the crypto/rand-failure
// fallback path directly. All ids in the batch are minted within the same
// wall-clock second, so the timestamp component is identical and any
// uniqueness must come from the process-local counter. It also confirms every
// fallback id stays within the ValidRequestID charset (no dot, no colon).
func TestFallbackRequestID_uniqueAndValid(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
	for range n {
		id := fallbackRequestID()
		if !ValidRequestID(id) {
			t.Fatalf("fallback id %q is not a valid request id", id)
		}
		if strings.ContainsAny(id, ".:") {
			t.Fatalf("fallback id %q contains a charset-invalid byte", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("fallback id %q collided within the same second", id)
		}
		seen[id] = struct{}{}
	}
}
