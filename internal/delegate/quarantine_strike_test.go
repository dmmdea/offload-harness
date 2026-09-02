package delegate

import (
	"testing"
	"time"
)

// Only DOCUMENT-FINGERPRINT failures strike a node: a user's over-strict
// contains: or a thin page's min_items is the contract's fault and must never
// quarantine the seat that answered honestly.
func TestStrikeOnFingerprintKeysOnlyOnTheDocanchorTag(t *testing.T) {
	q := NewQuarantine(10 * time.Minute)
	r := &runner{quarantine: q}
	base := "http://lenovo:18811"

	r.strikeOnFingerprint(base, []string{"contains:setpts: not found in output", "min_items:items:3: got 1"})
	r.strikeOnFingerprint(base, []string{"contains:setpts: not found in output"})
	if q.Blocked(base) || r.quarantined.Load() != 0 {
		t.Fatal("contract-caused failures must not quarantine")
	}

	fp := "regex:(?i)(?P<docanchor>setpts|atempo|timestamps): no match in output"
	r.strikeOnFingerprint(base, []string{"contains:x: not found", fp})
	if q.Blocked(base) {
		t.Fatal("one fingerprint failure is a strike, not a block")
	}
	r.strikeOnFingerprint(base, []string{fp, fp}) // two tagged failures in ONE result = one strike
	if !q.Blocked(base) || r.quarantined.Load() != 1 {
		t.Fatalf("second fingerprint failure must flip the node: blocked=%v flips=%d", q.Blocked(base), r.quarantined.Load())
	}
	r.strikeOnFingerprint(base, []string{fp})
	if r.quarantined.Load() != 1 {
		t.Fatal("a strike while blocked must not count a second flip")
	}
	var none *runner = &runner{}
	none.strikeOnFingerprint(base, []string{fp}) // nil quarantine is inert
}

// A quarantined node is not a placement candidate: fetchViews skips it and
// names the reason among the probe errors.
func TestFetchViewsSkipsQuarantinedNodes(t *testing.T) {
	q := NewQuarantine(10 * time.Minute)
	base := "http://127.0.0.1:1" // nothing listens; a non-quarantined probe would fail differently
	q.Strike(base)
	q.Strike(base)
	r := &runner{quarantine: q, remotes: []string{base}}
	views, bases, errs := r.fetchViews(t.Context())
	if len(views) != 0 || len(bases) != 0 {
		t.Fatalf("a quarantined node must not be a candidate: %v", bases)
	}
	if len(errs) != 1 || !containsStr(errs[0], "quarantined until") {
		t.Fatalf("the skip must be named among the probe errors: %v", errs)
	}
}

func containsStr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
