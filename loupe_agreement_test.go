package main

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

func lbl(task, tier string, agreed bool, ts int64) ledger.Entry {
	a := agreed
	return ledger.Entry{Task: task, ModelTier: tier, EscalatedAgreed: &a, TS: ts}
}

// The number this view exists to publish is dangerous precisely because it is quotable.
// It must never be emitted as an unconditional flip rate.
func TestUnconditionalRateIsNeverPublished(t *testing.T) {
	r := buildAgreement([]ledger.Entry{
		lbl("classify", "e2b", true, 1000), lbl("triage", "e2b", false, 2000),
	})
	if r.UnconditionalBasis == "" || r.UnconditionalBasis[:17] != "insufficient_data" {
		t.Fatalf("UnconditionalBasis = %q, want insufficient_data", r.UnconditionalBasis)
	}
	if r.DisagreementRate == nil {
		t.Fatal("the CONDITIONAL rate should be published")
	}
}

// An empty corpus must not read as "nothing ever disagrees" — 0% and "no labels" are
// opposite findings and the pointer is what keeps them apart.
func TestEmptyCorpusReportsNullNotZero(t *testing.T) {
	r := buildAgreement(nil)
	if r.DisagreementRate != nil {
		t.Fatalf("rate = %v, want nil on an empty corpus", *r.DisagreementRate)
	}
	if r.Basis != "insufficient_data" {
		t.Fatalf("Basis = %q", r.Basis)
	}
	if r.Rows != 0 {
		t.Fatalf("Rows = %d", r.Rows)
	}
}

// Rows with no verdict are not part of the population. Counting them would dilute the
// denominator and drag the rate toward agreement.
func TestUndecidedRowsAreNotCounted(t *testing.T) {
	rows := []ledger.Entry{
		lbl("classify", "e2b", false, 1000),
		{Task: "classify", ModelTier: "e2b", TS: 1100}, // no EscalatedAgreed
	}
	r := buildAgreement(rows)
	if r.Rows != 1 {
		t.Fatalf("Rows = %d, want 1 — the undecided row must not count", r.Rows)
	}
	if got := *r.DisagreementRate; got < 99.9 {
		t.Fatalf("rate = %v, want 100 over the one decided row", got)
	}
}

// Burst detection is what lets a reader judge the bench-sweep caveat instead of taking it
// on trust, so it must actually separate clusters rather than always returning 1.
func TestBurstDetectionSeparatesClusters(t *testing.T) {
	const hour = int64(3600)
	rows := []ledger.Entry{
		lbl("classify", "e2b", true, 0),
		lbl("classify", "e2b", true, 60),          // same burst
		lbl("classify", "e2b", true, 24*hour),     // +24h -> new burst
		lbl("classify", "e2b", true, 24*hour+120), // same burst
		lbl("classify", "e2b", true, 72*hour),     // -> third burst
	}
	if got := buildAgreement(rows).Bursts; got != 3 {
		t.Fatalf("Bursts = %d, want 3", got)
	}
	// Control: one tight cluster must report exactly one burst, or the detector is
	// counting rows rather than clusters.
	tight := []ledger.Entry{lbl("classify", "e2b", true, 0), lbl("classify", "e2b", true, 30), lbl("classify", "e2b", true, 60)}
	if got := buildAgreement(tight).Bursts; got != 1 {
		t.Fatalf("Bursts = %d on one cluster, want 1", got)
	}
}

func TestSafeDateToleratesShortValues(t *testing.T) {
	for _, in := range []string{"", "2026", "2026-08-21T10:00:00Z"} {
		_ = safeDate(in) // must not panic on a thin corpus
	}
	if got := safeDate("2026-08-21T10:00:00Z"); got != "2026-08-21" {
		t.Fatalf("safeDate = %q", got)
	}
}
