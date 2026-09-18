// retry_after_unparsable_test.go: review round 1, LOW item 6 — an unparsable
// non-empty Retry-After value is logged once per process rather than
// silently discarded on every 503.

package delegate

import "testing"

func TestParseRetryAfterSecondsIgnoresAnHTTPDateForm(t *testing.T) {
	// RFC 9110's HTTP-date form, not the delta-seconds this fleet's own nodes
	// send — a future proxy could start sending this, and it must be treated
	// as "no hint" (0), never mis-parsed as a huge second count.
	if got := parseRetryAfterSeconds("Wed, 21 Oct 2026 07:28:00 GMT"); got != 0 {
		t.Fatalf("parseRetryAfterSeconds(HTTP-date) = %d, want 0", got)
	}
	// Calling it again must not panic or block on the sync.Once — proves the
	// "log once" bound does not turn into a second failure mode.
	if got := parseRetryAfterSeconds("garbage"); got != 0 {
		t.Fatalf("parseRetryAfterSeconds(garbage) = %d, want 0", got)
	}
	if got := parseRetryAfterSeconds("5"); got != 5 {
		t.Fatalf("parseRetryAfterSeconds(\"5\") = %d, want 5 — the delta-seconds form must still parse", got)
	}
}
