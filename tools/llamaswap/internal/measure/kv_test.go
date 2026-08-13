// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package measure

import (
	"math"
	"strings"
	"testing"

	"llamaswap-pp-cli/internal/gguf"
)

// e4bHeader mirrors the real gemma-4 E4B GGUF header (verified by
// internal/gguf's tests against the file on V:). Kept as a fixture so the KV
// math is testable without the model volume.
func e4bHeader() *gguf.Result {
	pattern := make([]bool, 42)
	for i := range pattern {
		pattern[i] = (i+1)%6 != 0 // 5 sliding-window layers, then 1 full
	}
	return &gguf.Result{
		IsGGUF:               true,
		Architecture:         "gemma4",
		FileSizeBytes:        4215693760,
		BlockCount:           42,
		HeadCount:            8,
		HeadCountKV:          2,
		HeadCountKVSource:    "kv",
		ContextLength:        131072,
		EmbeddingLength:      2560,
		KeyLength:            512,
		ValueLength:          512,
		LengthSource:         "kv:attention.key_length/value_length",
		SlidingWindow:        512,
		SlidingWindowPattern: pattern,
		KeyLengthSWA:         256,
		ValueLengthSWA:       256,
		SharedKVLayers:       18,
	}
}

// The load-bearing test: the estimate for the one seat this box has a
// measured reference for. gemma-4-e4b at 131072 tokens measured 6,150 MiB of
// VRAM (weights + KV + buffers). The interval must contain it.
func TestEstimateKVMatchesMeasuredE4BAt131k(t *testing.T) {
	h := e4bHeader()
	kv, err := EstimateKV(h, 131072, DefaultCacheType, DefaultCacheType)
	if err != nil {
		t.Fatalf("EstimateKV: %v", err)
	}
	if kv.Model != "swa-aware" {
		t.Fatalf("kv model = %q, want swa-aware", kv.Model)
	}
	if kv.LayersAllocating != 24 {
		t.Errorf("layers allocating = %d, want 42-18 = 24", kv.LayersAllocating)
	}
	if kv.LayersFull != 4 || kv.LayersSWA != 20 {
		t.Errorf("full/swa layers = %d/%d, want 4/20", kv.LayersFull, kv.LayersSWA)
	}
	if kv.TotalMiB != 2068 {
		t.Errorf("KV = %d MiB, want 2068 (2048 full + 20 sliding-window)", kv.TotalMiB)
	}

	fit := Fit(h.FileSizeBytes, kv, []GPU{{
		UUID: "GPU-test", Name: "RTX 5070 Ti", Role: "fast-card", UsedMiB: 0, TotalMiB: 16303,
	}}, DefaultMarginMiB, DefaultReserveMiB)

	const measuredMiB = 6150
	if fit.OptimisticMiB > measuredMiB || fit.PessimisticMiB < measuredMiB {
		t.Fatalf("interval [%d, %d] MiB does not contain the measured %d MiB",
			fit.OptimisticMiB, fit.PessimisticMiB, measuredMiB)
	}
	if fit.Verdict != VerdictFits {
		t.Errorf("verdict = %q on a 16 GB card, want fits", fit.Verdict)
	}
	t.Logf("weights=%d MiB kv=%d MiB interval=[%d,%d] MiB measured=%d MiB verdict=%s",
		fit.WeightsMiB, fit.KVMiB, fit.OptimisticMiB, fit.PessimisticMiB, measuredMiB, fit.Verdict)

	// The dense contract formula is kept for auditability and must be shown
	// to be the overestimate it is, not quietly used.
	if kv.ContractDenseMiB <= kv.TotalMiB {
		t.Errorf("contract dense estimate %d MiB should exceed the swa-aware %d MiB", kv.ContractDenseMiB, kv.TotalMiB)
	}
	t.Logf("contract dense formula would say %d MiB of KV (%.1fx the refined number)",
		kv.ContractDenseMiB, float64(kv.ContractDenseMiB)/float64(kv.TotalMiB))
}

// GQA is the difference between a right answer and a 4x-wrong one.
func TestEstimateKVUsesKVHeadsNotHeads(t *testing.T) {
	h := e4bHeader()
	withGQA, err := EstimateKV(h, 8192, DefaultCacheType, DefaultCacheType)
	if err != nil {
		t.Fatal(err)
	}
	h2 := e4bHeader()
	h2.HeadCountKV = h2.HeadCount // pretend no GQA
	withoutGQA, err := EstimateKV(h2, 8192, DefaultCacheType, DefaultCacheType)
	if err != nil {
		t.Fatal(err)
	}
	ratio := float64(withoutGQA.TotalBytes) / float64(withGQA.TotalBytes)
	if math.Abs(ratio-4.0) > 0.001 {
		t.Fatalf("head_count/head_count_kv ratio should scale KV by 4x, got %.3f", ratio)
	}
}

func TestEstimateKVDenseWhenNoSWAMetadata(t *testing.T) {
	h := &gguf.Result{
		IsGGUF: true, Architecture: "bert", BlockCount: 24, HeadCount: 16, HeadCountKV: 16,
		HeadCountKVSource: "fallback:head_count (no attention.head_count_kv key)",
		EmbeddingLength:   1024, ContextLength: 8192, KeyLength: 64, ValueLength: 64,
		LengthSource: "derived:embedding_length/head_count",
	}
	kv, err := EstimateKV(h, 8192, DefaultCacheType, DefaultCacheType)
	if err != nil {
		t.Fatal(err)
	}
	if kv.Model != "dense" {
		t.Errorf("kv model = %q, want dense", kv.Model)
	}
	if kv.LayersFull != 24 || kv.LayersSWA != 0 {
		t.Errorf("layers full/swa = %d/%d, want 24/0", kv.LayersFull, kv.LayersSWA)
	}
	// 24 layers x 16 kv heads x (64+64) x 2 bytes = 98304 B/token
	if kv.BytesPerTokenFull != 98304 {
		t.Errorf("bytes/token = %v, want 98304", kv.BytesPerTokenFull)
	}
	// With key_length derived from embedding/head_count, the refined model
	// and the contract formula must agree exactly.
	if kv.ContractDenseMiB != kv.TotalMiB {
		t.Errorf("dense model %d MiB != contract formula %d MiB", kv.TotalMiB, kv.ContractDenseMiB)
	}
}

func TestEstimateKVRefusesUnknownGeometry(t *testing.T) {
	h := &gguf.Result{IsGGUF: true, Architecture: "mystery"}
	if _, err := EstimateKV(h, 4096, DefaultCacheType, DefaultCacheType); err == nil {
		t.Fatal("expected a refusal when the header declares no geometry")
	}
}

func TestParseCacheTypeBansQ4_0(t *testing.T) {
	ct, err := ParseCacheType("q4_0")
	if err != nil {
		t.Fatal(err)
	}
	if !ct.Banned || ct.BanReason == "" {
		t.Fatal("q4_0 must carry the house ban")
	}
	kv, err := EstimateKV(e4bHeader(), 4096, ct, ct)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range kv.Warnings {
		if strings.Contains(w, "HARD WARN") {
			found = true
		}
	}
	if !found {
		t.Fatal("estimating with q4_0 must emit the hard warning")
	}
	if _, err := ParseCacheType("q3_wat"); err == nil {
		t.Fatal("expected unknown cache type to be rejected")
	}
	if def, err := ParseCacheType(""); err != nil || def.Name != "f16" {
		t.Fatalf("empty cache type should default to f16, got %v %v", def, err)
	}
}

func TestQ8CacheTypeIsBlockAccurate(t *testing.T) {
	ct, err := ParseCacheType("q8_0")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(ct.BytesPerElem-1.0625) > 1e-9 {
		t.Errorf("q8_0 = %v bytes/element, want 34/32 = 1.0625", ct.BytesPerElem)
	}
}

func TestFitStraddleIsUncertainNotAGuess(t *testing.T) {
	kv := KVEstimate{TotalMiB: 4000}
	// capacity = 16303 - 1000 - 1024 = 14279; interval [8000, 8800] fits.
	fits := Fit(4000*MiB, kv, []GPU{{UUID: "a", TotalMiB: 16303, UsedMiB: 1000}}, DefaultMarginMiB, DefaultReserveMiB)
	if fits.Verdict != VerdictFits {
		t.Errorf("verdict = %q, want fits", fits.Verdict)
	}
	// capacity = 9000 - 0 - 1024 = 7976; interval [8000,8800] is entirely above.
	no := Fit(4000*MiB, kv, []GPU{{UUID: "a", TotalMiB: 9000, UsedMiB: 0}}, DefaultMarginMiB, DefaultReserveMiB)
	if no.Verdict != VerdictNoFit {
		t.Errorf("verdict = %q, want does-not-fit", no.Verdict)
	}
	// capacity = 9500 - 0 - 1024 = 8476; inside [8000, 8800] -> refuse.
	unsure := Fit(4000*MiB, kv, []GPU{{UUID: "a", TotalMiB: 9500, UsedMiB: 0}}, DefaultMarginMiB, DefaultReserveMiB)
	if unsure.Verdict != VerdictUncertain {
		t.Fatalf("verdict = %q, want UNCERTAIN", unsure.Verdict)
	}
	if unsure.Settle == "" {
		t.Error("an UNCERTAIN verdict must name the measurement that would settle it")
	}
	if unsure.Cards[0].StraddleBandS == "" {
		t.Error("straddle band must be explained per card")
	}
}

func TestFitCapacityIsNetOfResidentKeepSet(t *testing.T) {
	kv := KVEstimate{TotalMiB: 100}
	cards := []GPU{{UUID: "a", TotalMiB: 16303, UsedMiB: 6000, Role: "utility-card"}}
	got := Fit(1000*MiB, kv, cards, DefaultMarginMiB, DefaultReserveMiB)
	want := 16303 - 6000 - DefaultReserveMiB
	if got.Cards[0].CapacityMiB != want {
		t.Errorf("capacity = %d, want total - resident - reserve = %d", got.Cards[0].CapacityMiB, want)
	}
	if got.Cards[0].Role != "utility-card" {
		t.Errorf("role label lost")
	}
}
