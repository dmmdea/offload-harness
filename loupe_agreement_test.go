package main

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

func lbl(task, tier string, agreed bool, ts int64) ledger.Entry {
	a := agreed
	return ledger.Entry{Task: task, ModelTier: tier, EscalatedAgreed: &a, TS: ts,
		LabelSource: ledger.LabelSourceLiveEscalation}
}

func shadowLbl(task string, agreed bool, ts int64) ledger.Entry {
	a := agreed
	return ledger.Entry{Task: task, EscalatedAgreed: &a, TS: ts,
		LabelSource: ledger.LabelSourceShadowCounterfactual}
}

// build with no window and no drop counter -- the common case in these tests.
func build(rows []ledger.Entry) AgreementReport { return buildAgreement(rows, 0, 0, nil) }

// The number this view exists to publish is dangerous precisely because it is quotable.
// It must never be emitted as an unconditional flip rate.
func TestUnconditionalRateIsNeverPublished(t *testing.T) {
	r := build([]ledger.Entry{
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
	r := build(nil)
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
	r := build(rows)
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
	// DELIBERATELY OUT OF ORDER. The previous version of this test fed already-sorted
	// stamps, so deleting sortInt64 could not fail it -- it asserted burst counting while
	// silently not exercising the sort the count depends on. A label sidecar is append-only
	// but concurrent writers and clock skew make ordering an assumption, not a guarantee.
	rows := []ledger.Entry{
		lbl("classify", "e2b", true, 72*hour),     // third burst, written first
		lbl("classify", "e2b", true, 60),          // first burst
		lbl("classify", "e2b", true, 24*hour+120), // second burst
		lbl("classify", "e2b", true, 0),           // first burst
		lbl("classify", "e2b", true, 24*hour),     // second burst
	}
	if got := build(rows).Bursts; got != 3 {
		t.Fatalf("Bursts = %d, want 3", got)
	}
	// Control: one tight cluster must report exactly one burst, or the detector is
	// counting rows rather than clusters.
	tight := []ledger.Entry{lbl("classify", "e2b", true, 0), lbl("classify", "e2b", true, 30), lbl("classify", "e2b", true, 60)}
	if got := build(tight).Bursts; got != 1 {
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

// CONFIRMED MAJOR (review): confhead-labels.jsonl has two writers with OPPOSITE populations.
// shadow.LabelQueue writes NON-escalated rows and judges summarize by embedding cosine.
// Pooling them yields a rate that is conditional and unconditional at once.
func TestShadowRowsAreNotPooledIntoTheEscalationRate(t *testing.T) {
	rows := []ledger.Entry{
		lbl("classify", "e2b", true, 1000),
		lbl("classify", "e2b", false, 2000),
		shadowLbl("summarize", false, 3000), // non-escalated, cosine-judged
		shadowLbl("summarize", false, 4000),
	}
	r := build(rows)

	if r.Rows != 2 {
		t.Fatalf("Rows = %d, want 2 — shadow rows leaked into the escalation population", r.Rows)
	}
	if got := *r.DisagreementRate; got < 49.9 || got > 50.1 {
		t.Fatalf("rate = %v, want 50 (1 of 2 live rows); shadow rows would drag it to 75", got)
	}
	if r.ShadowRows != 2 || r.ShadowDisagreed != 2 {
		t.Fatalf("shadow bucket = %d/%d, want 2/2 — they must be reported, not discarded", r.ShadowDisagreed, r.ShadowRows)
	}
	// The task histogram must not carry summarize either: answersAgree never judged it.
	for _, row := range r.ByTask {
		if row.Key == "summarize" {
			t.Fatal("a cosine-judged summarize row reached the answersAgree task histogram")
		}
	}
}

// Rows written before provenance stamping cannot be attributed to either writer. Guessing
// them into one is precisely the error the split exists to prevent.
func TestUnstampedRowsAreExcludedAndSurfaced(t *testing.T) {
	a := false
	rows := []ledger.Entry{
		lbl("classify", "e2b", true, 1000),
		{Task: "classify", EscalatedAgreed: &a, TS: 2000}, // no LabelSource
	}
	r := build(rows)
	if r.Rows != 1 {
		t.Fatalf("Rows = %d, want 1 — an unstamped row was attributed to a writer", r.Rows)
	}
	if r.UnknownSourceRows != 1 {
		t.Fatalf("UnknownSourceRows = %d, want 1 — it must be surfaced, not silently dropped", r.UnknownSourceRows)
	}
}

// CONFIRMED (review): a decided row with ts:0 is counted in Rows but contributes no stamp,
// so a Rows>0 guard did not imply a non-empty slice and stamps[0] panicked — taking down the
// ENTIRE loupe report, not just this block.
func TestDecidedRowWithZeroTimestampDoesNotPanic(t *testing.T) {
	r := build([]ledger.Entry{lbl("classify", "e2b", false, 0)})
	if r.Rows != 1 {
		t.Fatalf("Rows = %d, want 1", r.Rows)
	}
	if r.FirstTS != "" || r.Bursts != 0 {
		t.Fatalf("a row with no usable stamp produced a window/burst claim: first=%q bursts=%d", r.FirstTS, r.Bursts)
	}
}

// CONFIRMED (review): the labels were read unfiltered, so `loupe --since 7` printed a 7-day
// ledger beside an all-time agreement rate under one heading.
func TestSinceWindowFiltersLabels(t *testing.T) {
	const day = int64(86400)
	rows := []ledger.Entry{
		lbl("classify", "e2b", false, 100*day), // recent
		lbl("classify", "e2b", true, 1*day),    // old
		lbl("classify", "e2b", true, 2*day),    // old
	}
	all := build(rows)
	if all.Rows != 3 {
		t.Fatalf("unwindowed Rows = %d, want 3", all.Rows)
	}
	windowed := buildAgreement(rows, 99*day, 7, nil)
	if windowed.Rows != 1 {
		t.Fatalf("windowed Rows = %d, want 1 — the since filter did not reach the labels", windowed.Rows)
	}
	if windowed.WindowDays != 7 {
		t.Fatalf("WindowDays = %d, want 7 — a windowed report must say so", windowed.WindowDays)
	}
}

// A zero-value report must never escape: a consumer keying on "insufficient_data" would
// receive "". This is the invariant TestUnconditionalRateIsNeverPublished protects, which the
// loupe wiring previously violated on the empty-ledger path.
func TestConstructorAlwaysStampsBasis(t *testing.T) {
	r := newAgreementReport()
	if r.Basis == "" || r.UnconditionalBasis == "" {
		t.Fatalf("basis strings empty: basis=%q unconditional=%q", r.Basis, r.UnconditionalBasis)
	}
	if empty := build(nil); empty.Basis == "" || empty.UnconditionalBasis == "" {
		t.Fatalf("empty corpus produced empty basis strings")
	}
}

// nil drops means "coverage unknown"; 0 means "measured, nothing lost". Collapsing them lets
// an unmeasured corpus advertise perfect coverage.
func TestDropsNilIsDistinctFromZero(t *testing.T) {
	if r := build(nil); r.UnparseableDrops != nil {
		t.Fatal("absent drop counter must stay nil, not become 0")
	}
	var zero int64
	if r := buildAgreement(nil, 0, 0, &zero); r.UnparseableDrops == nil || *r.UnparseableDrops != 0 {
		t.Fatal("a measured zero must survive as 0")
	}
}

// Unstamped rows must be RATED, not discarded. Dropping real measurements to achieve a tidy
// taxonomy trades one dishonesty for another: the rows exist and their rate is computable;
// only their provenance is missing.
func TestUnstampedRowsAreRatedNotDiscarded(t *testing.T) {
	a, d := true, false
	rows := []ledger.Entry{
		{Task: "classify", EscalatedAgreed: &a, TS: 1000},
		{Task: "classify", EscalatedAgreed: &d, TS: 2000},
		{Task: "triage", EscalatedAgreed: &d, TS: 3000},
	}
	r := build(rows)
	if r.Rows != 0 {
		t.Fatalf("Rows = %d, want 0 — unstamped rows must not enter the escalation rate", r.Rows)
	}
	if r.UnknownSourceRows != 3 || r.UnknownSourceDisagreed != 2 {
		t.Fatalf("unknown bucket = %d/%d, want 2/3", r.UnknownSourceDisagreed, r.UnknownSourceRows)
	}
	if r.UnknownSourceRate == nil {
		t.Fatal("unknown-provenance rows were counted but not rated — the measurement was thrown away")
	}
	if got := *r.UnknownSourceRate; got < 66.6 || got > 66.7 {
		t.Fatalf("unknown rate = %v, want 66.67", got)
	}
}
