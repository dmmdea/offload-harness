package agent

import (
	"sync"
	"testing"
)

// The distinction the whole instrument turns on: a run where the backend reported
// NOTHING must not be reportable as a measured 0% reuse. This estate has already
// shipped that exact defect once (`duplicate_input_rate` published 0 where nothing
// was measurable, which would have closed a gate that was never measured), so it
// gets a dedicated test rather than an assumption.
func TestNoObservationsReportsInsufficientDataNotZero(t *testing.T) {
	var p PrefillStats
	p.Observe(nil)
	p.Observe(nil)

	r := p.Report()
	if r.Basis != "insufficient_data" {
		t.Fatalf("basis = %q, want insufficient_data", r.Basis)
	}
	if r.ObservedSteps != 0 {
		t.Fatalf("nil Serve was counted as an observation: got %d", r.ObservedSteps)
	}
	if r.KVReusePct != nil {
		t.Fatalf("KVReusePct = %v, want nil — a non-nil zero is indistinguishable from a measured 'nothing was reused'", *r.KVReusePct)
	}
	if r.AvgPrefillTokensPerStep != nil || r.AvgPrefillMSPerStep != nil {
		t.Fatal("per-step averages must be nil when nothing was observed")
	}
}

// A `timings` block of all zeros is a REAL observation (a step that prefilled
// nothing), and must be distinguishable from the absent case above.
func TestZeroedTimingsCountAsAMeasuredObservation(t *testing.T) {
	var p PrefillStats
	p.Observe(&ServeStats{})

	r := p.Report()
	if r.Basis != "measured" {
		t.Fatalf("basis = %q, want measured", r.Basis)
	}
	if r.ObservedSteps != 1 {
		t.Fatalf("observed = %d, want 1", r.ObservedSteps)
	}
	// Reuse is undefined with no prompt tokens at all, so it stays nil while the
	// per-step averages are legitimately 0.
	if r.KVReusePct != nil {
		t.Fatalf("KVReusePct = %v, want nil when there were no prompt tokens", *r.KVReusePct)
	}
	if r.AvgPrefillTokensPerStep == nil || *r.AvgPrefillTokensPerStep != 0 {
		t.Fatal("avg prefill tokens/step should be a measured 0 here")
	}
}

// The denominator is the thing most likely to be got wrong, and getting it wrong
// produces a plausible-looking number above 100%.
func TestKVReuseUsesCachePlusPrefillAsDenominator(t *testing.T) {
	var p PrefillStats
	p.Observe(&ServeStats{CacheN: 900, PromptN: 100, PromptMS: 50})

	r := p.Report()
	if r.KVReusePct == nil {
		t.Fatal("expected a measured reuse pct")
	}
	// 900/(900+100) = 90%. The wrong formula, CacheN/PromptN, gives 900%.
	if got := *r.KVReusePct; got < 89.99 || got > 90.01 {
		t.Fatalf("KVReusePct = %v, want 90 — CacheN/(CacheN+PromptN), never CacheN/PromptN", got)
	}
}

func TestAccumulatesAcrossStepsAndAveragesPerObservedStep(t *testing.T) {
	var p PrefillStats
	p.Observe(&ServeStats{CacheN: 100, PromptN: 300, PromptMS: 30, PredictedN: 7})
	p.Observe(&ServeStats{CacheN: 300, PromptN: 100, PromptMS: 10, PredictedN: 3})
	p.Observe(nil) // must not dilute the averages below

	r := p.Report()
	if r.ObservedSteps != 2 {
		t.Fatalf("observed = %d, want 2 (the nil must not count)", r.ObservedSteps)
	}
	if r.CacheTokens != 400 || r.PrefillTokens != 400 || r.GeneratedToken != 10 {
		t.Fatalf("totals wrong: cache=%d prefill=%d generated=%d", r.CacheTokens, r.PrefillTokens, r.GeneratedToken)
	}
	if got := *r.AvgPrefillTokensPerStep; got != 200 {
		t.Fatalf("avg prefill tokens/step = %v, want 200 (400 over 2 OBSERVED steps, not 3 calls)", got)
	}
	if got := *r.AvgPrefillMSPerStep; got != 20 {
		t.Fatalf("avg prefill ms/step = %v, want 20", got)
	}
}

// Guards the reason this type carries a mutex at all. `--serve` shares ONE *Loop
// across concurrent HTTP handlers, and the token calibrator at the same call site
// was found mutating shared state from several goroutines. This instrument is
// always-on, so it does not get to rely on a flag being off.
// Run with -race for this to mean anything.
func TestObserveIsSafeUnderConcurrency(t *testing.T) {
	var p PrefillStats
	const goroutines, each = 8, 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				p.Observe(&ServeStats{CacheN: 1, PromptN: 1, PromptMS: 1, PredictedN: 1})
			}
		}()
	}
	// Concurrent readers too: Report locks, so a torn read is a failure as much as
	// a lost update.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < each; j++ {
			_ = p.Report()
		}
	}()
	wg.Wait()

	r := p.Report()
	want := goroutines * each
	if r.ObservedSteps != want {
		t.Fatalf("observed = %d, want %d — updates were lost under concurrency", r.ObservedSteps, want)
	}
	if r.CacheTokens != int64(want) || r.PrefillTokens != int64(want) {
		t.Fatalf("totals torn: cache=%d prefill=%d, want %d each", r.CacheTokens, r.PrefillTokens, want)
	}
}

// A nil receiver must be inert rather than panic: the field is embedded by value on
// Loop today, but a future refactor to a pointer must not turn an instrument into a
// crash on the serving path.
func TestNilReceiverIsInert(t *testing.T) {
	var p *PrefillStats
	p.Observe(&ServeStats{CacheN: 5, PromptN: 5})
}
