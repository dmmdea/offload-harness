// Package health provides offline health monitoring for the local-offload
// harness. It reads the JSONL ledger, groups entries by ModelTier, and
// computes per-tier statistics: EWMA of key signals, P95 latency, drift
// detection via CUSUM (Margin) and Page-Hinkley (TokPerSec), and a
// DEGRADED/OK status flag. It writes a compact JSON summary to outPath.
package health

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"sort"

	"github.com/dmmdea/local-offload/internal/ledger"
)

// TierHealth holds the computed health metrics for one ModelTier.
type TierHealth struct {
	Tier            string  `json:"tier"`
	N               int     `json:"n"`
	EWMAMargin      float64 `json:"ewma_margin"`
	EWMADeferRate   float64 `json:"ewma_defer_rate"`
	EWMATokPerSec   float64 `json:"ewma_tok_per_sec"`
	P95LatencyMs    float64 `json:"p95_latency_ms"`
	DriftMargin     bool    `json:"drift_margin"`
	DriftTokPerSec  bool    `json:"drift_tok_per_sec"`
	Status          string  `json:"status"`     // "OK" | "DEGRADED" — any anomaly (observability)
	RouteSkip       bool    `json:"route_skip"` // true ONLY on a genuine quality collapse — the routing signal
}

// Report is the result returned by Run. It holds per-tier health and any
// advisory notes produced during analysis.
type Report struct {
	Tiers map[string]TierHealth `json:"tiers"`
	Notes []string              `json:"notes,omitempty"`
}

// outFile is the JSON shape written to outPath.
type outFile struct {
	TierTimeoutsMs map[string]int `json:"tier_timeouts_ms"`
	Degraded       []string       `json:"degraded"`
}

// Run reads the ledger at ledgerPath, computes per-tier health statistics,
// writes a compact JSON summary to outPath, and returns the full Report.
// A missing ledger file is treated as zero entries (not an error).
func Run(ledgerPath, outPath string) (Report, error) {
	entries, err := readLedger(ledgerPath)
	if err != nil {
		return Report{}, err
	}

	// Group entries by tier in chronological (TS) order.
	groups := groupByTier(entries)

	report := Report{
		Tiers: make(map[string]TierHealth, len(groups)),
		Notes: nil,
	}

	out := outFile{
		TierTimeoutsMs: make(map[string]int, len(groups)),
		Degraded:       []string{},
	}

	for tier, es := range groups {
		th := analyzeTier(tier, es)
		report.Tiers[tier] = th

		timeout := int(math.Ceil(th.P95LatencyMs * 2))
		out.TierTimeoutsMs[tier] = timeout

		// Observability note for ANY degradation (drift or collapse).
		if th.Status == "DEGRADED" {
			report.Notes = append(report.Notes,
				"tier "+tier+" is DEGRADED (drift or low margin/throughput vs baseline)")
		}
		// Routing skip list: ONLY genuine quality collapses. Drift/throughput
		// degradation is observability-only — it must not route around an
		// otherwise-accurate entry tier (that starved the flywheel).
		if th.RouteSkip {
			out.Degraded = append(out.Degraded, tier)
			report.Notes = append(report.Notes,
				"tier "+tier+" is ROUTE-SKIPPED (quality collapse: margin far below baseline)")
		}
	}

	sort.Strings(out.Degraded)
	sort.Strings(report.Notes)

	if err := writeJSON(outPath, out); err != nil {
		return report, err
	}
	return report, nil
}

// ── ledger reading ────────────────────────────────────────────────────────────

func readLedger(path string) ([]ledger.Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []ledger.Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e ledger.Entry
		if json.Unmarshal(line, &e) != nil {
			continue // skip malformed / partial trailing lines
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// groupByTier partitions entries by ModelTier, preserving insertion order
// (entries are assumed to be appended chronologically by TS).
func groupByTier(entries []ledger.Entry) map[string][]ledger.Entry {
	m := make(map[string][]ledger.Entry)
	for _, e := range entries {
		tier := e.ModelTier
		if tier == "" {
			tier = "unknown"
		}
		m[tier] = append(m[tier], e)
	}
	return m
}

// ── per-tier analysis ─────────────────────────────────────────────────────────

const alpha = 0.25 // EWMA smoothing factor

func analyzeTier(tier string, es []ledger.Entry) TierHealth {
	n := len(es)
	if n == 0 {
		return TierHealth{Tier: tier, N: 0, Status: "OK"}
	}

	// Collect raw series.
	latencies := make([]float64, 0, n)
	margins := make([]float64, 0, n)
	tokPerSecs := make([]float64, 0, n)

	var (
		ewmaMargin    float64
		ewmaDeferRate float64
		ewmaTokPerSec float64
		marginInit    bool
		deferInit     bool
		tokInit       bool
	)

	for _, e := range es {
		latencies = append(latencies, float64(e.LatencyMs))

		// --- EWMA Margin ---
		if !marginInit {
			ewmaMargin = e.Margin
			marginInit = true
		} else {
			ewmaMargin = alpha*e.Margin + (1-alpha)*ewmaMargin
		}
		margins = append(margins, e.Margin)

		// --- EWMA DeferRate (boolean → 0/1) ---
		d := 0.0
		if e.Deferred {
			d = 1.0
		}
		if !deferInit {
			ewmaDeferRate = d
			deferInit = true
		} else {
			ewmaDeferRate = alpha*d + (1-alpha)*ewmaDeferRate
		}

		// --- EWMA TokPerSec ---
		if !tokInit {
			ewmaTokPerSec = e.TokPerSec
			tokInit = true
		} else {
			ewmaTokPerSec = alpha*e.TokPerSec + (1-alpha)*ewmaTokPerSec
		}
		tokPerSecs = append(tokPerSecs, e.TokPerSec)
	}

	p95 := percentile95(latencies)

	// Baseline = mean of first third.
	third := n / 3
	if third < 1 {
		third = 1
	}
	baselineMargin := mean(margins[:third])
	baselineTok := mean(tokPerSecs[:third])

	// Drift detection.
	driftMargin := cusumDetect(margins)
	driftTok := pageHinkley(tokPerSecs)

	const degradeThreshold = 0.5 // EWMA < 50 % of baseline → collapse

	// A genuine QUALITY collapse: the confidence margin (our correctness proxy)
	// has fallen far below this tier's own early-history baseline. This — and only
	// this — justifies routing AROUND the tier to a larger one.
	marginCollapse := baselineMargin > 0 && ewmaMargin < baselineMargin*degradeThreshold

	// Status (observability) flags ANY anomaly: drift (non-stationarity) or a
	// throughput/margin collapse. Drift and throughput are NOT routing signals —
	// a tier can be accurate yet non-stationary or merely slower (the live E2B
	// case: ewma_margin 0.957 but both drift flags tripped). Skipping such a tier
	// to a bigger, slower one is backwards and starved the flywheel. So they
	// surface as DEGRADED for visibility/timeout tuning, but do NOT route-skip.
	tokCollapse := baselineTok > 0 && ewmaTokPerSec < baselineTok*degradeThreshold
	degraded := driftMargin || driftTok || marginCollapse || tokCollapse

	status := "OK"
	if degraded {
		status = "DEGRADED"
	}

	return TierHealth{
		Tier:           tier,
		N:              n,
		EWMAMargin:     ewmaMargin,
		EWMADeferRate:  ewmaDeferRate,
		EWMATokPerSec:  ewmaTokPerSec,
		P95LatencyMs:   p95,
		DriftMargin:    driftMargin,
		DriftTokPerSec: driftTok,
		Status:         status,
		RouteSkip:      marginCollapse, // routing skip = quality collapse only
	}
}

// ── statistical helpers ───────────────────────────────────────────────────────

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// percentile95 returns the 95th-percentile of xs using nearest-rank.
func percentile95(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := make([]float64, len(xs))
	copy(cp, xs)
	sort.Float64s(cp)
	idx := int(math.Ceil(0.95*float64(len(cp)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

// minSD is the minimum meaningful standard deviation. Values at or below this
// threshold are treated as zero-variance (no drift possible from noise alone).
const minSD = 1e-9

// cusumDetect runs a two-sided CUSUM on xs and returns true when cumulative
// deviation exceeds the decision threshold h. Uses a slack k = σ/2.
//
// This is the standard Page (1954) two-sided CUSUM:
//
//	S⁺ₙ = max(0, S⁺ₙ₋₁ + (xₙ − μ) − k)
//	S⁻ₙ = max(0, S⁻ₙ₋₁ − (xₙ − μ) − k)
//	alarm when S⁺ₙ > h or S⁻ₙ > h
func cusumDetect(xs []float64) bool {
	if len(xs) < 4 {
		return false
	}
	mu := mean(xs)
	sd := stddev(xs, mu)
	if sd < minSD {
		// Near-constant series: no meaningful drift can exist.
		return false
	}
	k := sd / 2   // reference (slack)
	h := 4.0 * sd // decision threshold (4σ is a common choice)

	var sPos, sNeg float64
	for _, x := range xs {
		delta := x - mu
		sPos = math.Max(0, sPos+delta-k)
		sNeg = math.Max(0, sNeg-delta-k)
		if sPos > h || sNeg > h {
			return true
		}
	}
	return false
}

// pageHinkley detects a decrease in the mean of xs using the Page-Hinkley test.
// It returns true when the accumulated deviation exceeds the threshold δ.
// We tune λ=1 (minimum detectable shift) and δ=5σ.
func pageHinkley(xs []float64) bool {
	if len(xs) < 4 {
		return false
	}
	mu := mean(xs)
	sd := stddev(xs, mu)
	if sd < minSD {
		// Near-constant series: no meaningful drift can exist.
		return false
	}
	lambda := sd      // minimum detectable shift ≈ 1σ
	delta := 5.0 * sd // alert threshold

	var mT, mMin float64
	for _, x := range xs {
		mT += x - mu + lambda
		if mT < mMin {
			mMin = mT
		}
		if mT-mMin > delta {
			return true
		}
	}
	return false
}

func stddev(xs []float64, mu float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		d := x - mu
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(xs)))
}

// ── output ────────────────────────────────────────────────────────────────────

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write (P4): tmp+rename so the long-running MCP server's reloader only
	// ever reads a complete tier_overrides.json.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
