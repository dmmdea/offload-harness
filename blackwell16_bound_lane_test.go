package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// TestBlackwell16VLLMSeatIsDeclaredFromItsOwnSilicon (ADR 0048 Amendment,
// 2026-09-17). blackwell-16 owed a vLLM seat; it was measured on one RTX
// 5060 Ti rather than copied from ampere-16, and the copy would have been
// wrong: the A2's launch line (fp8_e5m2 KV) produces NaN tokens on this card
// under vLLM 0.29 — every digest-8 contract deferred, twice — while fp8
// (e4m3) on the same FlashInfer backend answers 8/8 and scores level with the
// A2 blind (8.53 vs 8.46). The KV dtype is the load-bearing field of this
// declaration; the rest is the operating point and lane the 8/8 ran at.
func TestBlackwell16VLLMSeatIsDeclaredFromItsOwnSilicon(t *testing.T) {
	raw, err := os.ReadFile("setup/templates/profiles.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			VLLMSeat *vllmseat.Spec `json:"vllm_seat"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	s := doc.Profiles["blackwell-16"].VLLMSeat
	if s == nil {
		t.Fatal("blackwell-16 declares no vllm_seat (ADR 0048: every tier seats a model under vLLM)")
	}
	const why = "ADR 0048 Amendment (2026-09-17): measured on the 5060 Ti, not copied from ampere-16; re-measure before changing"
	if s.ID != "qwen38-27b-gsq-vllm" {
		t.Errorf("blackwell-16 vllm_seat id = %q, want qwen38-27b-gsq-vllm — %s", s.ID, why)
	}
	if s.KVCacheDtype != "fp8" {
		t.Errorf("blackwell-16 kv_cache_dtype = %q, want fp8 (e4m3): fp8_e5m2 through FlashInfer is numerically broken on the RTX 5060 Ti under vLLM 0.29 (raw completions `<tool_call>!!!!…`, 0/8 twice) — %s", s.KVCacheDtype, why)
	}
	if s.MaxModelLen != 49152 || s.AgentCtxTokens != 49152 {
		t.Errorf("blackwell-16 vllm_seat window = %d (agent_ctx_tokens %d), want 49152/49152 (KV 76,314 tokens, 1.55x at util 0.92) — %s", s.MaxModelLen, s.AgentCtxTokens, why)
	}
	if s.GPUMemoryUtilization < 0.919 || s.GPUMemoryUtilization > 0.921 {
		t.Errorf("blackwell-16 vllm_seat gpu_memory_utilization = %v, want 0.92 — %s", s.GPUMemoryUtilization, why)
	}
	if s.AgentMaxTokens != 4096 || s.AgentThinking != "off" || s.AgentTimeoutSec != 900 || s.AgentSeatTokS != 27.75 {
		t.Errorf("blackwell-16 bound-lane settings = max_tokens %d / thinking %q / timeout %d / tok_s %v, want 4096 / off / 900 / 27.75 (the digest-8 8/8 configuration, single-stream rate measured on the coherent arm) — %s",
			s.AgentMaxTokens, s.AgentThinking, s.AgentTimeoutSec, s.AgentSeatTokS, why)
	}
	smp := s.AgentSampling
	if smp == nil || smp.Temperature == nil || *smp.Temperature != 0.7 || smp.TopP == nil || *smp.TopP != 0.8 || smp.TopK == nil || *smp.TopK != 20 || smp.PresencePenalty == nil || *smp.PresencePenalty != 1.5 {
		t.Errorf("blackwell-16 agent_sampling = %v, want the Qwen3.8 vendor values (0.7 / 0.8 / 20 / 1.5) the arm was measured at — %s", smp, why)
	}
	if s.CacheServer != nil {
		t.Errorf("blackwell-16 vllm_seat binds a cache_server; the seat was measured storeless and LMCache 0.5.4 rejects vLLM 0.29's kv_layout (D-117) — %s", why)
	}
	if err := s.Validate("blackwell-16"); err != nil {
		t.Errorf("blackwell-16 vllm_seat must validate as declared: %v", err)
	}
}
