package compeval

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
)

// --- pure metric math --------------------------------------------------------

func TestReuseFracAndRealTokens(t *testing.T) {
	// A full cache hit reports prompt_n=1 (the tail always re-evaluates), so
	// the denominator MUST be cache_n+prompt_n — cache_n/prompt_n would say
	// "1768x reuse".
	hit := ProbeResult{CacheN: 1768, PromptN: 1, Measured: true}
	if f := ReuseFrac(hit); f < 0.999 {
		t.Fatalf("full hit reuse = %v, want ~1", f)
	}
	if got := hit.RealPromptTokens(); got != 1769 {
		t.Fatalf("real tokens = %d, want 1769", got)
	}
	// usage.prompt_tokens wins when present (same number by another route).
	withUsage := ProbeResult{CacheN: 100, PromptN: 20, UsagePromptTokens: 121, Measured: true}
	if got := withUsage.RealPromptTokens(); got != 121 {
		t.Fatalf("usage should win: %d", got)
	}
	if f := ReuseFrac(ProbeResult{}); f != 0 {
		t.Fatalf("empty probe reuse = %v, want 0", f)
	}
}

func TestMedianAndPercentile(t *testing.T) {
	if got := Median([]float64{3, 1, 2}); got != 2 {
		t.Fatalf("median odd = %v", got)
	}
	if got := Median([]float64{4, 1, 2, 3}); got != 2.5 {
		t.Fatalf("median even = %v", got)
	}
	if got := Median(nil); got != 0 {
		t.Fatalf("median empty = %v", got)
	}
	xs := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := Percentile(xs, 0.5); got != 5 {
		t.Fatalf("p50 = %v", got)
	}
	if got := Percentile(xs, 0.95); got != 10 {
		t.Fatalf("p95 = %v", got)
	}
	if got := Percentile(xs, 1.0); got != 10 {
		t.Fatalf("p100 = %v", got)
	}
	// Median must not reorder the caller's slice.
	orig := []float64{3, 1, 2}
	_ = Median(orig)
	if orig[0] != 3 {
		t.Fatal("Median mutated its input")
	}
}

// --- admissibility -----------------------------------------------------------

func ctrls(pos, neg, post float64) []Control {
	mk := func(name string, f float64) Control {
		// cache_n/prompt_n consistent with the requested reuse fraction.
		total := 1000.0
		c := int(f * total)
		return Control{Name: name, Probe: ProbeResult{CacheN: c, PromptN: int(total) - c, Measured: true}, ReuseFrac: f}
	}
	return []Control{mk(CtrlPosPre, pos), mk(CtrlNegPre, neg), mk(CtrlPosPost, post)}
}

func TestAdmissible(t *testing.T) {
	if ok, why := Admissible(ctrls(0.99, 0.001, 0.98)); !ok {
		t.Fatalf("clean controls rejected: %s", why)
	}
	// The exact failure measured live: the model was evicted between two
	// byte-identical requests, so every arm would read as zero reuse.
	ok, why := Admissible(ctrls(0.0, 0.001, 0.98))
	if ok {
		t.Fatal("a failed positive control must be inadmissible")
	}
	if !strings.Contains(why, "evicted") {
		t.Fatalf("reason must name the eviction cause: %q", why)
	}
	if ok, why := Admissible(ctrls(0.99, 0.001, 0.10)); ok || !strings.Contains(why, "DURING") {
		t.Fatalf("residency lost mid-run must be inadmissible and say so: ok=%v %q", ok, why)
	}
	if ok, _ := Admissible(ctrls(0.99, 0.50, 0.98)); ok {
		t.Fatal("a leaking negative control must be inadmissible")
	}
	// Fail CLOSED on missing / unmeasured controls.
	if ok, why := Admissible(nil); ok || !strings.Contains(why, "missing") {
		t.Fatalf("no controls must be inadmissible: ok=%v %q", ok, why)
	}
	unmeasured := ctrls(0.99, 0.0, 0.99)
	unmeasured[0].Probe.Measured = false
	if ok, why := Admissible(unmeasured); ok || !strings.Contains(why, "unmeasured") {
		t.Fatalf("an unmeasured control must be inadmissible: ok=%v %q", ok, why)
	}
	// MULTI-ROUND: a failure in round 1 must NOT be masked by a pass in round
	// 2. Keying controls by name would do exactly that.
	multi := append(ctrls(0.99, 0.001, 0.02), ctrls(0.99, 0.001, 0.99)...)
	if ok, why := Admissible(multi); ok {
		t.Fatalf("a round-1 control failure was masked by round 2: ok=%v %q", ok, why)
	}
	// ...and a clean multi-round run still passes.
	if ok, why := Admissible(append(ctrls(0.99, 0.001, 0.98), ctrls(0.97, 0.002, 0.99)...)); !ok {
		t.Fatalf("clean multi-round run rejected: %s", why)
	}
}

// --- safety margin -----------------------------------------------------------

func TestImpliedMarginAndCoveredRatio(t *testing.T) {
	// The shipped constant: compactionMargin=512 at ctx 8192 / maxOut 1024.
	// Solving S >= (C-M)(1-1/r) backwards, 512 survives only r <= ~1.077.
	if got := CoveredRatio(512, 8192, 1024); got < 1.07 || got > 1.08 {
		t.Fatalf("S=512 covers r=%v, want ~1.077", got)
	}
	// And forwards: a 10% estimator error needs ~651.
	if got := ImpliedMargin(1.1, 8192, 1024); got < 645 || got > 655 {
		t.Fatalf("r=1.1 implies S=%d, want ~651", got)
	}
	if got := ImpliedMargin(2.0, 8192, 1024); got != 3584 {
		t.Fatalf("r=2.0 implies S=%d, want 3584", got)
	}
	// A perfect estimator needs no margin for THIS failure mode.
	if got := ImpliedMargin(1.0, 8192, 1024); got != 0 {
		t.Fatalf("r=1 implies S=%d, want 0", got)
	}
	// Round-trip: the implied margin covers the ratio it was derived from.
	for _, r := range []float64{1.05, 1.3, 1.75} {
		s := ImpliedMargin(r, 8192, 1024)
		if got := CoveredRatio(s, 8192, 1024); got < r-0.01 {
			t.Fatalf("round trip broken: r=%v -> S=%d -> covers %v", r, s, got)
		}
	}
}

func TestMarginTableCostIsHonest(t *testing.T) {
	rows := MarginTable([]float64{1.0, 1.5}, 8192, 1024)
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].ImpliedS != 0 || rows[0].InputBudget != 7168 {
		t.Fatalf("r=1 row wrong: %+v", rows[0])
	}
	// The table must show what safety COSTS: r=1.5 eats a third of the window.
	if rows[1].BudgetCostPct < 30 || rows[1].BudgetCostPct > 35 {
		t.Fatalf("r=1.5 budget cost = %v%%, want ~33%%", rows[1].BudgetCostPct)
	}
}

// --- transcript facts --------------------------------------------------------

func TestDivergenceIndex(t *testing.T) {
	a := []agent.Msg{{Role: "user", Content: "one"}, {Role: "assistant", Content: "two"}}
	appended := append(append([]agent.Msg(nil), a...), agent.Msg{Role: "user", Content: "three"})
	if got := DivergenceIndex(a, appended); got != -1 {
		t.Fatalf("pure append must be -1, got %d", got)
	}
	if got := DivergenceIndex(a, a); got != -1 {
		t.Fatalf("identical must be -1, got %d", got)
	}
	edited := append([]agent.Msg(nil), a...)
	edited[1] = agent.Msg{Role: "assistant", Content: "CHANGED"}
	if got := DivergenceIndex(a, edited); got != 1 {
		t.Fatalf("mid edit must diverge at 1, got %d", got)
	}
	if got := DivergenceIndex(appended, a); got != 2 {
		t.Fatalf("truncation must report the new end (2), got %d", got)
	}
	if !SameTranscript(a, a) || SameTranscript(a, edited) {
		t.Fatal("SameTranscript wrong")
	}
	// Relation is the BYTE-stream view — the one that predicts KV survival.
	// Appending to the LAST message's content must read as an extension (it
	// reuses live); a trailing separator would wrongly call it divergent.
	grown := []agent.Msg{{Role: "user", Content: "one"}, {Role: "assistant", Content: "two and more"}}
	if rel, _, _ := Relation(a, grown); rel != RelExtension {
		t.Fatalf("appending to the last message must be an extension, got %s", rel)
	}
	if rel, _, _ := Relation(a, appended); rel != RelExtension {
		t.Fatalf("appending a new message must be an extension, got %s", rel)
	}
	if rel, _, _ := Relation(appended, a); rel != RelTruncation {
		t.Fatalf("dropping the last message must be a truncation, got %s", rel)
	}
	rel, _, frac := Relation(a, edited)
	if rel != RelDivergent {
		t.Fatalf("an edited older message must be divergent, got %s", rel)
	}
	if frac <= 0 || frac >= 1 {
		t.Fatalf("divergent shared fraction should be strictly inside (0,1), got %v", frac)
	}
	if rel, _, _ := Relation(nil, a); rel != RelNone {
		t.Fatal("no previous request must be RelNone")
	}
	// Tool-call identity participates in equality (an id change breaks the
	// prefix just as a content change does).
	tc1 := []agent.Msg{{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "c1", Name: "f"}}}}
	tc2 := []agent.Msg{{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "c2", Name: "f"}}}}
	if DivergenceIndex(tc1, tc2) != 0 {
		t.Fatal("differing tool-call ids must diverge")
	}
}

func TestWellFormedPrefix(t *testing.T) {
	msgs := []agent.Msg{
		{Role: "user", Content: "objective"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "c1"}}},
		{Role: "tool", ToolCallID: "c1", Content: "result"},
	}
	if !wellFormed(msgs) {
		t.Fatal("complete pair must be well formed")
	}
	if wellFormed(msgs[:2]) {
		t.Fatal("a prefix cut between a tool call and its result must be rejected")
	}
	if !wellFormed(msgs[:1]) {
		t.Fatal("a prefix with no tool calls is well formed")
	}
}

// --- aggregation -------------------------------------------------------------

func TestSummarizeSeparatesRampClasses(t *testing.T) {
	mk := func(first, fired bool, reuse float64, promptN int) Sample {
		total := 1000
		c := int(reuse * float64(total))
		return Sample{
			Arm: "on", FirstStep: first, Fired: fired, ReuseFrac: reuse,
			Probe: ProbeResult{CacheN: c, PromptN: promptN, PromptMS: float64(promptN), UsagePromptTokens: c + promptN, Measured: true},
		}
	}
	st := Summarize("on", []Sample{
		mk(true, false, 0.0, 900),  // conversation start — neither class
		mk(false, false, 0.99, 10), // between fires: append
		mk(false, false, 0.98, 12),
		mk(false, true, 0.01, 800), // the fire: mid-prompt divergence
	})
	if st.FirstSteps != 1 {
		t.Fatalf("first steps = %d, want 1", st.FirstSteps)
	}
	if st.Fires != 1 {
		t.Fatalf("fires = %d, want 1", st.Fires)
	}
	// The ramp must be visible: no-fire steps reuse ~everything, fire steps ~nothing.
	if st.MedianReuseNoFire < 0.9 {
		t.Fatalf("no-fire median reuse = %v, want ~0.985", st.MedianReuseNoFire)
	}
	if st.MedianReuseOnFire > 0.1 {
		t.Fatalf("on-fire median reuse = %v, want ~0.01", st.MedianReuseOnFire)
	}
	// The cold conversation-start sample must NOT drag the no-fire median down.
	if st.MedianReuseNoFire < 0.5 {
		t.Fatal("first step leaked into the no-fire class")
	}
}

// TestSuspectedEvictionSeparatesSchedulerFromLadder: an EXTENSION that did not
// reuse is the scheduler tearing the tier down, not a ladder property. Those
// rows must be flagged, excluded from RATE statistics, and still counted in
// token TOTALS (the server really did prefill everything).
func TestSuspectedEvictionSeparatesSchedulerFromLadder(t *testing.T) {
	ext := func(reuse float64) Sample {
		total := 1000
		c := int(reuse * float64(total))
		return Sample{
			Arm: "on", PrefixRelation: string(RelExtension), ReuseFrac: reuse,
			Probe: ProbeResult{CacheN: c, PromptN: total - c, PromptMS: 100, UsagePromptTokens: total, Measured: true},
		}
	}
	good1, good2, evicted := ext(0.95), ext(0.93), ext(0.0)
	if good1.SuspectedEviction() {
		t.Fatal("a reusing extension must not be flagged")
	}
	if !evicted.SuspectedEviction() {
		t.Fatal("an extension that did not reuse MUST be flagged — that is the eviction signature")
	}
	// A first step is cold by construction, never an eviction.
	first := ext(0.0)
	first.FirstStep = true
	if first.SuspectedEviction() {
		t.Fatal("conversation-start must not be misread as eviction")
	}
	// A DIVERGENT step legitimately reuses nothing — never an eviction.
	div := ext(0.0)
	div.PrefixRelation = string(RelDivergent)
	if div.SuspectedEviction() {
		t.Fatal("a divergent step reusing nothing is the expected ladder cost, not eviction")
	}

	st := Summarize("on", []Sample{good1, good2, evicted})
	if st.Evictions != 1 {
		t.Fatalf("evictions = %d, want 1", st.Evictions)
	}
	// Rates exclude it: the median reuse must not be dragged toward zero.
	if st.MedianReuseFrac < 0.9 {
		t.Fatalf("evicted sample leaked into the rate statistics: median=%v", st.MedianReuseFrac)
	}
	if st.ExtensionReuse < 0.9 {
		t.Fatalf("extension reuse = %v, want ~0.94 over the clean rows", st.ExtensionReuse)
	}
	// Totals keep it: the prefill work really happened.
	if st.TotalRealTokens != 3000 {
		t.Fatalf("totals must include the evicted row: %d", st.TotalRealTokens)
	}
}

func TestFitEstimatorSkipsUnmeasured(t *testing.T) {
	samples := []Sample{
		{EntryKind: "code", EstTokens: 100, EstRatio: 1.4, Probe: ProbeResult{Measured: true}},
		{EntryKind: "code", EstTokens: 100, EstRatio: 1.5, Probe: ProbeResult{Measured: true}},
		{EntryKind: "prose", EstTokens: 100, EstRatio: 0.9, Probe: ProbeResult{Measured: true}},
		{EntryKind: "code", EstTokens: 100, EstRatio: 9.9, Probe: ProbeResult{Measured: false}}, // must be ignored
	}
	fit := FitEstimator(samples)
	if fit.Samples != 3 {
		t.Fatalf("samples = %d, want 3 (unmeasured skipped)", fit.Samples)
	}
	if fit.Max > 1.6 {
		t.Fatalf("unmeasured outlier leaked into the fit: max=%v", fit.Max)
	}
	if _, ok := fit.ByKind["code"]; !ok {
		t.Fatal("per-kind fit missing — content dependence is the whole point")
	}
}

// --- end-to-end sequencing against a serving SIMULATOR ----------------------

// fakeServer reproduces the KV semantics measured on the real stack
// (2026-07-24): an identical or APPENDED prompt reuses the cached prefix
// entirely; a prompt that diverges mid-way collapses to ~nothing (sliding
// window attention cannot truncate mid-sequence); a strict prefix of the
// cached prompt also reuses. evictEvery simulates the scheduler tearing the
// model down between requests.
type fakeServer struct {
	cached     []agent.Msg
	calls      int
	evictEvery int // 1 = evict before every request (the pathological box)
	unmeasured bool
}

func (f *fakeServer) probe(_ context.Context, msgs []agent.Msg) (ProbeResult, error) {
	f.calls++
	if f.unmeasured {
		return ProbeResult{WallMS: 5}, nil // a backend that reports nothing
	}
	total := agent.EstimateTokens(msgs)
	cacheN := 0
	evicted := f.evictEvery > 0 && f.calls%f.evictEvery == 0
	if !evicted {
		// The MEASURED rule (see Relation's doc): reuse is binary. Appends and
		// truncations reuse everything; ANY divergence discards the whole
		// cache no matter how much was shared — a one-line edit at 98% of the
		// prompt cost all 2662 tokens live, exactly like an edit at 4%.
		rel, shared, _ := Relation(f.cached, msgs)
		curLen := len(renderWire(msgs))
		switch rel {
		case RelNone:
			cacheN = 0
		case RelTruncation:
			cacheN = total
		case RelExtension:
			if curLen > 0 {
				cacheN = int(float64(total) * float64(shared) / float64(curLen))
			}
		default:
			cacheN = 1
		}
	}
	if cacheN > total {
		cacheN = total
	}
	promptN := total - cacheN
	if promptN < 1 {
		promptN = 1
	}
	f.cached = append([]agent.Msg(nil), msgs...)
	return ProbeResult{
		CacheN: cacheN, PromptN: promptN,
		PromptMS: float64(promptN) * 0.3, WallMS: float64(promptN)*0.3 + 5,
		UsagePromptTokens: total, UsageCachedTokens: cacheN, Measured: true,
	}, nil
}

func benchCorpus(t *testing.T) []Entry {
	t.Helper()
	body := strings.Repeat("verbose tool output line that the ladder will eventually have to compact\n", 40)
	var turns []Turn
	turns = append(turns,
		Turn{Role: "system", Content: "you are the local agent"},
		Turn{Role: "user", Content: "objective: analyze the repository"},
	)
	for i := 0; i < 8; i++ {
		id := "c" + string(rune('1'+i))
		turns = append(turns,
			Turn{Role: "assistant", ToolCalls: []TurnToolCall{{ID: id, Name: "read_file"}}},
			Turn{Role: "tool", ToolCallID: id, Content: body + "unique tail " + id},
		)
	}
	return []Entry{{ID: "e1", Kind: "tool-text", Turns: turns, KeepRecent: agent.DefaultKeepRecent, ProtectedPrefix: 2}}
}

func TestRunControlsMeasuresBothLadderShapes(t *testing.T) {
	f := &fakeServer{}
	cs, err := RunControls(context.Background(), f.probe, "pre")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Control{}
	for _, c := range cs {
		byName[c.Name] = c
	}
	for _, n := range []string{CtrlPosPre, CtrlNegPre, CtrlAppend, CtrlMidEdit} {
		if _, ok := byName[n]; !ok {
			t.Fatalf("control %s missing", n)
		}
	}
	if byName[CtrlPosPre].ReuseFrac < PosReuseMin {
		t.Fatalf("positive control should reuse: %v", byName[CtrlPosPre].ReuseFrac)
	}
	if byName[CtrlNegPre].ReuseFrac > NegReuseMax {
		t.Fatalf("negative control should not reuse: %v", byName[CtrlNegPre].ReuseFrac)
	}
	// The two shapes the ladder produces, measured as distinct properties:
	// appending reuses, editing mid-prompt does not.
	if byName[CtrlAppend].ReuseFrac < 0.9 {
		t.Fatalf("append control should reuse nearly everything: %v", byName[CtrlAppend].ReuseFrac)
	}
	if byName[CtrlMidEdit].ReuseFrac > 0.1 {
		t.Fatalf("mid-edit control should collapse: %v", byName[CtrlMidEdit].ReuseFrac)
	}
}

func TestRunBenchAdmissibleAndSeparatesEffects(t *testing.T) {
	f := &fakeServer{}
	rep, err := RunBench(context.Background(), f.probe, benchCorpus(t), "hash-x",
		BenchOpts{Ladder: LadderOpts{Skeleton: true}, Rounds: 1, CtxTokens: 8192, MaxOut: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != "ADMISSIBLE" {
		t.Fatalf("verdict = %s (%s)", rep.Verdict, rep.InadmissibleReason)
	}
	if len(rep.Arms) != 2 {
		t.Fatalf("arms = %d", len(rep.Arms))
	}
	var on ArmStats
	for _, a := range rep.Arms {
		if a.Arm == "on" {
			on = a
		}
	}
	if on.Fires == 0 {
		t.Fatal("fixture exerts no compaction pressure — the on arm never fired, so the ramp is untested")
	}
	// The two effects must both be present AND separate.
	if rep.SizeVsCache.OnRealPromptTokens >= rep.SizeVsCache.OffRealPromptTokens {
		t.Fatal("compaction should reduce REAL prompt tokens (the size effect)")
	}
	if rep.SizeVsCache.SizeEffectTokens <= 0 {
		t.Fatalf("size effect = %d", rep.SizeVsCache.SizeEffectTokens)
	}
	if rep.Estimator.Samples == 0 {
		t.Fatal("estimator fit empty — margin tuning has no basis")
	}
	if len(rep.MarginTable) == 0 || len(rep.Limits) == 0 {
		t.Fatal("report must carry the margin table and the honest-limits disclosures")
	}
	// Raw rows are the evidence: every headline must be re-derivable.
	if len(rep.Samples) < 10 {
		t.Fatalf("samples = %d, want the raw per-request rows", len(rep.Samples))
	}
}

func TestRunBenchInconclusiveWhenModelEvicted(t *testing.T) {
	// The exact box condition measured live: something evicts the tier between
	// requests, so a byte-identical resend reuses nothing. The bench must
	// refuse to report, not publish a table of zeros as a compaction finding.
	f := &fakeServer{evictEvery: 1}
	rep, err := RunBench(context.Background(), f.probe, benchCorpus(t), "hash-x",
		BenchOpts{Ladder: LadderOpts{Skeleton: true}, Rounds: 1, CtxTokens: 8192, MaxOut: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != "INCONCLUSIVE" {
		t.Fatalf("verdict = %s, want INCONCLUSIVE", rep.Verdict)
	}
	if !strings.Contains(rep.InadmissibleReason, "evicted") {
		t.Fatalf("reason must name the cause: %q", rep.InadmissibleReason)
	}
	if len(rep.Samples) == 0 {
		t.Fatal("samples must still be emitted as evidence about the environment")
	}
}

func TestRunBenchInconclusiveWhenBackendReportsNothing(t *testing.T) {
	f := &fakeServer{unmeasured: true}
	rep, err := RunBench(context.Background(), f.probe, benchCorpus(t), "hash-x",
		BenchOpts{Rounds: 1, CtxTokens: 8192, MaxOut: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != "INCONCLUSIVE" || !strings.Contains(rep.InadmissibleReason, "unmeasured") {
		t.Fatalf("a backend without timings must be INCONCLUSIVE, got %s (%s)", rep.Verdict, rep.InadmissibleReason)
	}
}
