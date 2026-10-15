package compeval

// sent_sha256 proves a no-op pair of runs without the runs sharing a salt.

import "testing"

// The sent_sha256 of a step must not depend on the salt, so a no-op pair of
// runs is provable with the default fresh nonce each run draws.
func TestSentSHA256IsTheSameAcrossNonces(t *testing.T) {
	s, _, _ := rungEntries()
	a, _ := capture(t, []Entry{s}, BenchOpts{Rounds: 1, RunNonce: "ONE", Ladder: incumbent(), Arms: []string{"on"}})
	b, _ := capture(t, []Entry{s}, BenchOpts{Rounds: 1, RunNonce: "TWO", Ladder: incumbent(), Arms: []string{"on"}})
	ka, kb := samplesByStep(a), samplesByStep(b)
	if len(ka) == 0 {
		t.Fatal("no samples")
	}
	for k := range ka {
		if ka[k].SentSHA256 != kb[k].SentSHA256 {
			t.Fatalf("%s: the same transcript under two nonces hashes differently (%s vs %s)", k, ka[k].SentSHA256, kb[k].SentSHA256)
		}
	}
}
