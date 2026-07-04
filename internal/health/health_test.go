package health

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// writeLedger serialises a slice of entries to a temp JSONL file.
func writeLedger(t *testing.T, entries []ledger.Entry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	return p
}

// makeEntry builds a ledger.Entry for a given tier with explicit signals.
func makeEntry(tier string, margin, tokPerSec float64, latencyMs int64, deferred bool) ledger.Entry {
	return ledger.Entry{
		Task:      "triage",
		ModelTier: tier,
		Margin:    margin,
		TokPerSec: tokPerSec,
		LatencyMs: latencyMs,
		Deferred:  deferred,
	}
}

// ── stable tier ──────────────────────────────────────────────────────────────
// 30 entries, all consistent signals — expect OK, no drift.

func stableEntries() []ledger.Entry {
	var es []ledger.Entry
	for i := 0; i < 30; i++ {
		es = append(es, makeEntry("stable-tier", 0.8, 50.0, 200, false))
	}
	return es
}

// ── degrading tier ────────────────────────────────────────────────────────────
// First 10 entries healthy; next 20 entries show a sudden drop in both margin
// and tok/s — well beyond the 4σ CUSUM / PH threshold.

func degradingEntries() []ledger.Entry {
	var es []ledger.Entry
	for i := 0; i < 10; i++ {
		es = append(es, makeEntry("bad-tier", 0.9, 60.0, 300, false))
	}
	for i := 0; i < 20; i++ {
		// margin collapses to 0.05, tok/s collapses to 5
		es = append(es, makeEntry("bad-tier", 0.05, 5.0, 1500, true))
	}
	return es
}

// ── TestStableTierIsOK ────────────────────────────────────────────────────────

func TestStableTierIsOK(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.json")
	p := writeLedger(t, stableEntries())

	rpt, err := Run(p, outPath)
	if err != nil {
		t.Fatal(err)
	}

	th, ok := rpt.Tiers["stable-tier"]
	if !ok {
		t.Fatal("stable-tier missing from report")
	}
	if th.Status != "OK" {
		t.Errorf("stable tier: want Status=OK, got %q (drift_margin=%v drift_tok=%v)",
			th.Status, th.DriftMargin, th.DriftTokPerSec)
	}
	if th.N != 30 {
		t.Errorf("N: want 30, got %d", th.N)
	}

	// EWMA of constant 0.8 should be very close to 0.8.
	if math.Abs(th.EWMAMargin-0.8) > 0.05 {
		t.Errorf("EWMAMargin: want ~0.8, got %f", th.EWMAMargin)
	}
	// P95 of constant 200 ms should be 200.
	if th.P95LatencyMs != 200 {
		t.Errorf("P95LatencyMs: want 200, got %f", th.P95LatencyMs)
	}
}

// ── TestDegradingTierIsDegraded ───────────────────────────────────────────────

func TestDegradingTierIsDegraded(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.json")
	p := writeLedger(t, degradingEntries())

	rpt, err := Run(p, outPath)
	if err != nil {
		t.Fatal(err)
	}

	th, ok := rpt.Tiers["bad-tier"]
	if !ok {
		t.Fatal("bad-tier missing from report")
	}
	if th.Status != "DEGRADED" {
		t.Errorf("degrading tier: want Status=DEGRADED, got %q (drift_margin=%v drift_tok=%v ewma_margin=%f baseline vs ewma_tok=%f)",
			th.Status, th.DriftMargin, th.DriftTokPerSec, th.EWMAMargin, th.EWMATokPerSec)
	}
	if th.N != 30 {
		t.Errorf("N: want 30, got %d", th.N)
	}
}

// ── TestMixedTiers ────────────────────────────────────────────────────────────
// Both tiers in the same ledger; degraded list must contain only bad-tier.

func TestMixedTiers(t *testing.T) {
	var all []ledger.Entry
	all = append(all, stableEntries()...)
	all = append(all, degradingEntries()...)

	outPath := filepath.Join(t.TempDir(), "out.json")
	p := writeLedger(t, all)

	rpt, err := Run(p, outPath)
	if err != nil {
		t.Fatal(err)
	}

	if rpt.Tiers["stable-tier"].Status != "OK" {
		t.Errorf("stable-tier should be OK, got %q", rpt.Tiers["stable-tier"].Status)
	}
	if rpt.Tiers["bad-tier"].Status != "DEGRADED" {
		t.Errorf("bad-tier should be DEGRADED, got %q", rpt.Tiers["bad-tier"].Status)
	}

	// Verify outPath JSON.
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal("outPath not written:", err)
	}
	var of outFile
	if err := json.Unmarshal(b, &of); err != nil {
		t.Fatal("outPath invalid JSON:", err)
	}
	if len(of.Degraded) != 1 || of.Degraded[0] != "bad-tier" {
		t.Errorf("degraded list: want [bad-tier], got %v", of.Degraded)
	}
	// tier_timeouts_ms should be ceil(P95*2) — at least 1 for both tiers.
	if of.TierTimeoutsMs["stable-tier"] <= 0 || of.TierTimeoutsMs["bad-tier"] <= 0 {
		t.Errorf("tier_timeouts_ms missing or zero: %v", of.TierTimeoutsMs)
	}
}

// ── TestMissingLedgerIsEmpty ──────────────────────────────────────────────────

func TestMissingLedgerIsEmpty(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.json")
	rpt, err := Run(filepath.Join(t.TempDir(), "nope.jsonl"), outPath)
	if err != nil {
		t.Fatal("missing ledger should not error:", err)
	}
	if len(rpt.Tiers) != 0 {
		t.Errorf("expected empty tiers, got %v", rpt.Tiers)
	}
}

// ── TestMalformedLinesSkipped ─────────────────────────────────────────────────

func TestMalformedLinesSkipped(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "ledger.jsonl")
	f, _ := os.Create(p)
	// write one valid entry then a partial/truncated line
	enc := json.NewEncoder(f)
	_ = enc.Encode(makeEntry("x-tier", 0.7, 40.0, 100, false))
	_, _ = f.WriteString(`{"model_tier":"x-tier","margin":0.5`) // truncated
	f.Close()

	outPath := filepath.Join(tmp, "out.json")
	rpt, err := Run(p, outPath)
	if err != nil {
		t.Fatal(err)
	}
	th, ok := rpt.Tiers["x-tier"]
	if !ok {
		t.Fatal("x-tier missing")
	}
	if th.N != 1 {
		t.Errorf("malformed line should be skipped: N=%d want 1", th.N)
	}
}

// ── TestP95LatencyTimeout ─────────────────────────────────────────────────────
// ceil(P95 * 2) must equal expected value.

func TestP95LatencyTimeout(t *testing.T) {
	// Build a series where P95 is unambiguously the high-latency value.
	// 10 entries at 100 ms, 10 entries at 500 ms → sorted = [100]*10 + [500]*10
	// n=20, P95 index = ceil(0.95*20)-1 = ceil(19)-1 = 19-1 = 18.
	// sorted[18] = 500 ms → timeout = ceil(500*2) = 1000.
	var es []ledger.Entry
	for i := 0; i < 10; i++ {
		es = append(es, makeEntry("t-tier", 0.8, 50.0, 100, false))
	}
	for i := 0; i < 10; i++ {
		es = append(es, makeEntry("t-tier", 0.8, 50.0, 500, false))
	}

	outPath := filepath.Join(t.TempDir(), "out.json")
	p := writeLedger(t, es)
	_, err := Run(p, outPath)
	if err != nil {
		t.Fatal(err)
	}

	b, _ := os.ReadFile(outPath)
	var of outFile
	_ = json.Unmarshal(b, &of)

	// n=20, P95 nearest-rank index = ceil(0.95*20)-1 = 19-1 = 18.
	// sorted[18] = 500 ms → timeout = ceil(500*2) = 1000.
	got := of.TierTimeoutsMs["t-tier"]
	if got != 1000 {
		t.Errorf("tier_timeouts_ms[t-tier]: want 1000, got %d", got)
	}
}

// ── TestEWMADeferRate ─────────────────────────────────────────────────────────

func TestEWMADeferRate(t *testing.T) {
	// All entries deferred → defer-rate EWMA should converge to 1.
	var es []ledger.Entry
	for i := 0; i < 20; i++ {
		es = append(es, makeEntry("d-tier", 0.5, 30.0, 200, true))
	}
	outPath := filepath.Join(t.TempDir(), "out.json")
	p := writeLedger(t, es)
	rpt, err := Run(p, outPath)
	if err != nil {
		t.Fatal(err)
	}
	th := rpt.Tiers["d-tier"]
	if th.EWMADeferRate < 0.9 {
		t.Errorf("all-deferred tier: EWMADeferRate=%f want ≥0.9", th.EWMADeferRate)
	}
}

// ── drifty-but-healthy tier (the false-degradation regression) ────────────────
// Margin SHIFTS over time (CUSUM fires → Status=DEGRADED for observability) but
// never COLLAPSES below baseline, and throughput is steady. Such a tier must NOT
// land on the routing skip list: skipping an accurate small entry tier to route
// around it to a larger, slower one is backwards, and it starved the flywheel of
// E2B-entry data (live E2B: ewma_margin 0.957, but drift flags tripped → skipped).

func driftyButHealthyEntries() []ledger.Entry {
	var es []ledger.Entry
	for i := 0; i < 10; i++ {
		es = append(es, makeEntry("drifty-tier", 0.5, 50.0, 200, false))
	}
	for i := 0; i < 20; i++ {
		// margin RISES to 0.9 (a sustained shift → drift) but never collapses;
		// throughput steady. Healthy, just non-stationary.
		es = append(es, makeEntry("drifty-tier", 0.9, 50.0, 200, false))
	}
	return es
}

func TestDriftyTierIsNotRouteSkipped(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.json")
	p := writeLedger(t, driftyButHealthyEntries())
	rpt, err := Run(p, outPath)
	if err != nil {
		t.Fatal(err)
	}
	th := rpt.Tiers["drifty-tier"]
	// Observability: a shifting margin trips drift detection.
	if !th.DriftMargin {
		t.Fatalf("expected DriftMargin=true for a shifting-margin tier, got %+v", th)
	}
	// Routing: margin never collapsed, so the tier must NOT be route-skipped.
	if th.RouteSkip {
		t.Errorf("drifty-but-healthy tier must not be route-skipped (ewma_margin=%f)", th.EWMAMargin)
	}
	var of outFile
	b, _ := os.ReadFile(outPath)
	if err := json.Unmarshal(b, &of); err != nil {
		t.Fatal("outPath invalid JSON:", err)
	}
	for _, d := range of.Degraded {
		if d == "drifty-tier" {
			t.Errorf("drifty-but-healthy tier must NOT be in the routing skip list, got %v", of.Degraded)
		}
	}
}

// TestCollapsedTierIsRouteSkipped pins the other side: a genuine margin collapse
// (the correctness proxy falling far below baseline) MUST be route-skipped.
func TestCollapsedTierIsRouteSkipped(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.json")
	p := writeLedger(t, degradingEntries()) // margin 0.9 → 0.05 collapse
	rpt, err := Run(p, outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !rpt.Tiers["bad-tier"].RouteSkip {
		t.Errorf("collapsed tier must be route-skipped, got RouteSkip=false (ewma_margin=%f)",
			rpt.Tiers["bad-tier"].EWMAMargin)
	}
}

// tokCollapseHealthyMarginEntries: throughput collapses (60→5 tok/s) but the
// confidence margin stays high and stable. This isolates the THROUGHPUT axis.
func tokCollapseHealthyMarginEntries() []ledger.Entry {
	var es []ledger.Entry
	for i := 0; i < 10; i++ {
		es = append(es, makeEntry("slow-tier", 0.9, 60.0, 200, false))
	}
	for i := 0; i < 20; i++ {
		es = append(es, makeEntry("slow-tier", 0.9, 5.0, 1500, false)) // slow, but still accurate
	}
	return es
}

// TestTokCollapseIsNotRouteSkipped pins the deliberate behavior change: a tier
// that got SLOW (throughput collapse / drift) but stayed accurate (stable margin)
// is flagged DEGRADED for observability + timeout tuning, but must NOT be
// route-skipped — routing around a slow entry tier to a larger, slower one is
// backwards; throughput is governed by the per-tier timeout, not entry routing.
func TestTokCollapseIsNotRouteSkipped(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.json")
	p := writeLedger(t, tokCollapseHealthyMarginEntries())
	rpt, err := Run(p, outPath)
	if err != nil {
		t.Fatal(err)
	}
	th := rpt.Tiers["slow-tier"]
	if th.Status != "DEGRADED" {
		t.Errorf("throughput-collapsed tier should be DEGRADED for observability, got %q", th.Status)
	}
	if th.RouteSkip {
		t.Errorf("throughput-collapsed-but-accurate tier must NOT be route-skipped (ewma_margin=%f)", th.EWMAMargin)
	}
	var of outFile
	b, _ := os.ReadFile(outPath)
	if err := json.Unmarshal(b, &of); err != nil {
		t.Fatal("outPath invalid JSON:", err)
	}
	for _, d := range of.Degraded {
		if d == "slow-tier" {
			t.Errorf("throughput-collapsed tier must NOT be in the routing skip list, got %v", of.Degraded)
		}
	}
}
