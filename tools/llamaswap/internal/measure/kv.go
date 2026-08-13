// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package measure

import (
	"fmt"
	"sort"
	"strings"

	"llamaswap-pp-cli/internal/gguf"
)

// MiB is one mebibyte in bytes.
const MiB = 1024 * 1024

// DefaultMarginMiB is the activation + CUDA-context allowance added to the
// pessimistic end of every fit interval. llama.cpp allocates compute buffers
// and the CUDA runtime reserves per-process context memory; neither is in
// the weights and neither is in the KV cache.
const DefaultMarginMiB = 800

// DefaultReserveMiB is held back from every card so a "fits" verdict does
// not mean "fits with zero headroom for the desktop compositor".
const DefaultReserveMiB = 1024

// CacheType is a KV cache element type and its on-disk bytes per element.
type CacheType struct {
	Name         string  `json:"name"`
	BytesPerElem float64 `json:"bytes_per_element"`
	Banned       bool    `json:"banned,omitempty"`
	BanReason    string  `json:"ban_reason,omitempty"`
}

// cacheTypes covers the llama.cpp --cache-type-k/--cache-type-v values.
// Block-quantized entries carry their true per-element cost including the
// block scale (q8_0 stores 32 values in 34 bytes = 1.0625 B/elem), not the
// nominal bit width.
var cacheTypes = map[string]CacheType{
	"f32":  {Name: "f32", BytesPerElem: 4},
	"f16":  {Name: "f16", BytesPerElem: 2},
	"bf16": {Name: "bf16", BytesPerElem: 2},
	"q8_0": {Name: "q8_0", BytesPerElem: 34.0 / 32.0},
	"q5_1": {Name: "q5_1", BytesPerElem: 24.0 / 32.0},
	"q5_0": {Name: "q5_0", BytesPerElem: 22.0 / 32.0},
	"q4_1": {Name: "q4_1", BytesPerElem: 20.0 / 32.0},
	"iq4_nl": {
		Name: "iq4_nl", BytesPerElem: 18.0 / 32.0,
	},
	"q4_0": {
		Name: "q4_0", BytesPerElem: 18.0 / 32.0,
		Banned: true,
		BanReason: "q4_0 KV quantization is banned by house evidence: measured quality loss " +
			"on this box's seats outweighed the VRAM saving. Estimating with it is allowed; " +
			"recommending it is not.",
	},
}

// DefaultCacheType is what llama-server uses when no --cache-type flag is set.
var DefaultCacheType = cacheTypes["f16"]

// ParseCacheType resolves a --cache-type-k/v flag value.
func ParseCacheType(s string) (CacheType, error) {
	key := strings.ToLower(strings.TrimSpace(s))
	if key == "" {
		return DefaultCacheType, nil
	}
	ct, ok := cacheTypes[key]
	if !ok {
		known := make([]string, 0, len(cacheTypes))
		for k := range cacheTypes {
			known = append(known, k)
		}
		sort.Strings(known)
		return CacheType{}, fmt.Errorf("unknown cache type %q (known: %s)", s, strings.Join(known, ", "))
	}
	return ct, nil
}

// KVEstimate is the KV-cache arithmetic for one model at one context length.
// Every input that moved the number is a field so the result can be audited
// without re-running the command.
type KVEstimate struct {
	CtxTokens int `json:"ctx_tokens"`

	HeadCount   int    `json:"head_count"`
	HeadCountKV int    `json:"head_count_kv"`
	KVSource    string `json:"head_count_kv_source"`
	KeyLength   int    `json:"key_length"`
	ValueLength int    `json:"value_length"`
	LenSource   string `json:"key_value_length_source"`

	LayersTotal      int `json:"layers_total"`
	LayersAllocating int `json:"layers_allocating_kv"`
	LayersFull       int `json:"layers_full_attention"`
	LayersSWA        int `json:"layers_sliding_window"`
	SWAWindow        int `json:"swa_window_tokens,omitempty"`
	KeyLengthSWA     int `json:"key_length_swa,omitempty"`
	ValueLengthSWA   int `json:"value_length_swa,omitempty"`

	CacheTypeK CacheType `json:"cache_type_k"`
	CacheTypeV CacheType `json:"cache_type_v"`

	// Model is "swa-aware" when the file declares a sliding-window pattern
	// and/or shared KV layers, "dense" when every layer allocates a full
	// n_ctx cache.
	Model string `json:"kv_model"`

	BytesPerTokenFull float64 `json:"bytes_per_token_full_layers"`
	FullBytes         float64 `json:"full_attention_bytes"`
	SWABytes          float64 `json:"sliding_window_bytes"`
	TotalBytes        float64 `json:"total_bytes"`
	TotalMiB          int     `json:"total_mib"`

	// ContractDenseMiB is the manifest's literal formula
	// (n_layers x n_kv_heads x embedding_length/head_count x 2 x dtype x ctx)
	// kept alongside the refined number so the divergence is visible rather
	// than hidden inside the estimator.
	ContractDenseBytesPerToken float64 `json:"contract_dense_bytes_per_token"`
	ContractDenseMiB           int     `json:"contract_dense_mib"`

	Assumptions []string `json:"assumptions"`
	Warnings    []string `json:"warnings,omitempty"`
}

// EstimateKV computes the KV-cache footprint for a parsed GGUF header at
// ctxTokens.
//
// Three header facts move this number by multiples, and all three are read
// from the file rather than assumed:
//
//   - attention.head_count_kv (GQA). Using head_count instead overestimates
//     by 4x on gemma-4 E4B and 8x on E2B.
//   - attention.key_length/value_length. Gemma's per-head K/V dimension is
//     512, not embedding_length/head_count (=320 on E4B).
//   - the sliding-window pattern and shared_kv_layers. Only full-attention,
//     non-shared layers scale with n_ctx; SWA layers are capped at the
//     window, and trailing shared layers allocate nothing at all.
//
// Validation: E4B (42 blocks, 18 shared, 5:1 SWA pattern, 2 KV heads,
// K/V length 512, f16) at 131072 tokens gives 2,068 MiB of KV, which with
// the 4,020 MiB of weights lands at 6,088 MiB against the 6,150 MiB measured
// on this box at that context (inside the [6,088, 6,888] MiB interval). The
// dense contract formula gives 13,440 MiB of KV for the same seat: a 6.5x
// overestimate that would refuse a fit that demonstrably works.
func EstimateKV(h *gguf.Result, ctxTokens int, ctK, ctV CacheType) (KVEstimate, error) {
	if h == nil || !h.IsGGUF {
		return KVEstimate{}, fmt.Errorf("KV estimate needs a GGUF header")
	}
	if ctxTokens <= 0 {
		return KVEstimate{}, fmt.Errorf("KV estimate needs a positive context length")
	}
	if missing := h.Missing(); len(missing) > 0 {
		// Only the fields the math actually consumes are fatal.
		var fatal []string
		for _, m := range missing {
			switch m {
			case "block_count", "attention.head_count_kv":
				fatal = append(fatal, m)
			}
		}
		if len(fatal) > 0 {
			return KVEstimate{}, fmt.Errorf("GGUF header is missing %s; KV size is unknown, not defaulted", strings.Join(fatal, ", "))
		}
	}
	if h.KeyLength <= 0 || h.ValueLength <= 0 {
		return KVEstimate{}, fmt.Errorf("GGUF header declares neither attention.key_length nor embedding_length/head_count; KV size is unknown")
	}

	e := KVEstimate{
		CtxTokens:      ctxTokens,
		HeadCount:      h.HeadCount,
		HeadCountKV:    h.HeadCountKV,
		KVSource:       h.HeadCountKVSource,
		KeyLength:      h.KeyLength,
		ValueLength:    h.ValueLength,
		LenSource:      h.LengthSource,
		LayersTotal:    h.BlockCount,
		SWAWindow:      h.SlidingWindow,
		KeyLengthSWA:   h.KeyLengthSWA,
		ValueLengthSWA: h.ValueLengthSWA,
		CacheTypeK:     ctK,
		CacheTypeV:     ctV,
		Model:          "dense",
	}

	allocating := h.BlockCount
	if h.SharedKVLayers > 0 && h.SharedKVLayers < h.BlockCount {
		allocating = h.BlockCount - h.SharedKVLayers
		e.Model = "swa-aware"
		e.Assumptions = append(e.Assumptions, fmt.Sprintf(
			"attention.shared_kv_layers=%d: the trailing %d layers reuse an earlier layer's KV cache and allocate none of their own (%d of %d layers allocate)",
			h.SharedKVLayers, h.SharedKVLayers, allocating, h.BlockCount))
	}
	e.LayersAllocating = allocating

	swaKeyLen, swaValLen := h.KeyLengthSWA, h.ValueLengthSWA
	if swaKeyLen <= 0 {
		swaKeyLen = h.KeyLength
	}
	if swaValLen <= 0 {
		swaValLen = h.ValueLength
	}
	e.LayersFull = allocating
	if len(h.SlidingWindowPattern) >= allocating && h.SlidingWindow > 0 {
		full, swa := 0, 0
		for i := 0; i < allocating; i++ {
			if h.SlidingWindowPattern[i] {
				swa++
			} else {
				full++
			}
		}
		e.LayersFull, e.LayersSWA = full, swa
		e.Model = "swa-aware"
		e.Assumptions = append(e.Assumptions, fmt.Sprintf(
			"attention.sliding_window_pattern: %d of the %d allocating layers are sliding-window (cache capped at %d tokens, K/V length %d/%d), %d are full attention (cache scales with n_ctx)",
			swa, allocating, h.SlidingWindow, swaKeyLen, swaValLen, full))
	} else if len(h.SlidingWindowPattern) > 0 && len(h.SlidingWindowPattern) < allocating {
		e.Warnings = append(e.Warnings, fmt.Sprintf(
			"sliding_window_pattern has %d entries for %d allocating layers; treating every layer as full attention (conservative)",
			len(h.SlidingWindowPattern), allocating))
	}

	perTokenPerLayer := float64(h.HeadCountKV) * (float64(h.KeyLength)*ctK.BytesPerElem + float64(h.ValueLength)*ctV.BytesPerElem)
	e.BytesPerTokenFull = perTokenPerLayer * float64(e.LayersFull)
	e.FullBytes = e.BytesPerTokenFull * float64(ctxTokens)

	if e.LayersSWA > 0 {
		swaTokens := h.SlidingWindow
		if swaTokens > ctxTokens {
			swaTokens = ctxTokens
		}
		swaPerTokenPerLayer := float64(h.HeadCountKV) * (float64(swaKeyLen)*ctK.BytesPerElem + float64(swaValLen)*ctV.BytesPerElem)
		e.SWABytes = swaPerTokenPerLayer * float64(e.LayersSWA) * float64(swaTokens)
	}
	e.TotalBytes = e.FullBytes + e.SWABytes
	e.TotalMiB = int(e.TotalBytes / MiB)

	if h.EmbeddingLength > 0 && h.HeadCount > 0 {
		headDim := float64(h.EmbeddingLength) / float64(h.HeadCount)
		e.ContractDenseBytesPerToken = float64(h.BlockCount) * float64(h.HeadCountKV) * headDim *
			(ctK.BytesPerElem + ctV.BytesPerElem)
		e.ContractDenseMiB = int(e.ContractDenseBytesPerToken * float64(ctxTokens) / MiB)
	}

	e.Assumptions = append(e.Assumptions, fmt.Sprintf(
		"n_kv_heads=%d (%s), per-head K/V length %d/%d (%s), cache types k=%s v=%s (%.4g/%.4g bytes per element)",
		h.HeadCountKV, h.HeadCountKVSource, h.KeyLength, h.ValueLength, h.LengthSource,
		ctK.Name, ctV.Name, ctK.BytesPerElem, ctV.BytesPerElem))
	if e.Model == "dense" {
		e.Assumptions = append(e.Assumptions,
			"no sliding-window or shared-KV metadata in this header: every layer is assumed to allocate a full n_ctx cache")
	}
	for _, ct := range []CacheType{ctK, ctV} {
		if ct.Banned {
			e.Warnings = append(e.Warnings, "HARD WARN: "+ct.BanReason)
		}
	}
	return e, nil
}

// FitCard is one card's verdict.
type FitCard struct {
	UUID          string `json:"uuid"`
	Name          string `json:"name"`
	Role          string `json:"role,omitempty"`
	TotalMiB      int    `json:"total_mib"`
	ResidentMiB   int    `json:"resident_now_mib"`
	ReserveMiB    int    `json:"reserve_mib"`
	CapacityMiB   int    `json:"capacity_mib"`
	Verdict       string `json:"verdict"`
	HeadroomMiB   int    `json:"headroom_optimistic_mib"`
	ShortfallMiB  int    `json:"shortfall_pessimistic_mib"`
	StraddleBandS string `json:"straddle,omitempty"`
}

// Fit verdicts.
const (
	VerdictFits      = "fits"
	VerdictNoFit     = "does-not-fit"
	VerdictUncertain = "UNCERTAIN"
)

// FitResult is the interval answer: the estimator never reports a single
// number, because a single number is what invites a confident wrong call.
type FitResult struct {
	WeightsMiB     int `json:"weights_mib"`
	KVMiB          int `json:"kv_mib"`
	MarginMiB      int `json:"activation_cuda_margin_mib"`
	OptimisticMiB  int `json:"optimistic_total_mib"`
	PessimisticMiB int `json:"pessimistic_total_mib"`

	Cards   []FitCard `json:"cards"`
	Verdict string    `json:"verdict"`
	// Settle names the measurement that would collapse an UNCERTAIN verdict
	// to a fact.
	Settle string `json:"what_would_settle_it,omitempty"`
}

// Fit places a weights+KV footprint against measured card capacity.
// Capacity is net of what is resident right now (the keep-set, on this box)
// and of a fixed reserve, so a verdict is never computed against a card's
// nameplate total.
func Fit(weightsBytes int64, kv KVEstimate, cards []GPU, marginMiB, reserveMiB int) FitResult {
	res := FitResult{
		WeightsMiB:     int(weightsBytes / MiB),
		KVMiB:          kv.TotalMiB,
		MarginMiB:      marginMiB,
		OptimisticMiB:  int(weightsBytes/MiB) + kv.TotalMiB,
		PessimisticMiB: int(weightsBytes/MiB) + kv.TotalMiB + marginMiB,
	}
	best := ""
	for _, g := range cards {
		capacity := g.TotalMiB - g.UsedMiB - reserveMiB
		card := FitCard{
			UUID:        g.UUID,
			Name:        g.Name,
			Role:        g.Role,
			TotalMiB:    g.TotalMiB,
			ResidentMiB: g.UsedMiB,
			ReserveMiB:  reserveMiB,
			CapacityMiB: capacity,
			HeadroomMiB: capacity - res.OptimisticMiB,
		}
		switch {
		case res.PessimisticMiB <= capacity:
			card.Verdict = VerdictFits
		case res.OptimisticMiB > capacity:
			card.Verdict = VerdictNoFit
			card.ShortfallMiB = res.OptimisticMiB - capacity
		default:
			card.Verdict = VerdictUncertain
			card.StraddleBandS = fmt.Sprintf("capacity %d MiB sits inside the %d-%d MiB interval", capacity, res.OptimisticMiB, res.PessimisticMiB)
		}
		res.Cards = append(res.Cards, card)
		best = betterVerdict(best, card.Verdict)
	}
	if best == "" {
		best = VerdictUncertain
		res.Settle = "no GPU was measured; run `llamaswap-pp-cli vram` to confirm nvidia-smi is reachable"
	}
	res.Verdict = best
	if best == VerdictUncertain && res.Settle == "" {
		res.Settle = "load the seat once and measure it: `llamaswap-pp-cli bench <model> --runs 1` reports the per-UUID VRAM delta, " +
			"which replaces this interval with a measured number. Until then the honest answer is that the estimate straddles capacity."
	}
	return res
}

func betterVerdict(a, b string) string {
	rank := map[string]int{VerdictNoFit: 0, VerdictUncertain: 1, VerdictFits: 2, "": -1}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
