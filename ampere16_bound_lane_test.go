package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// TestAmpere16VLLMSeatDeclaresItsMeasuredBoundLane (ADR 0049 Amendment 3,
// operator decision D5 = A, 2026-09-16). The GSQ seat is the tier's bound
// agent lane at the ONE operating point measured to share the card with the
// embedder that backs the memory authority: 32,768 @ util 0.90. Its bound-lane
// settings are the configuration the digest-8 gate passed 8/8 at; the tier's
// config_seed values (the fallback seat's 1,024 tokens / 300 s) were measured
// failing on this seat (3/8). A change to any of these is a re-measurement,
// not an edit — the ADR names what has to be re-run.
func TestAmpere16VLLMSeatDeclaresItsMeasuredBoundLane(t *testing.T) {
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
	s := doc.Profiles["ampere-16"].VLLMSeat
	if s == nil {
		t.Fatal("ampere-16 declares no vllm_seat (ADR 0048/0049)")
	}
	const adr = "ADR 0049 Amendment 3 (D5 = A): 32,768 @ 0.90 is the measured co-resident operating point; re-measure before changing it"
	if s.ID != "qwen38-27b-gsq-vllm" {
		t.Errorf("ampere-16 vllm_seat id = %q, want qwen38-27b-gsq-vllm — %s", s.ID, adr)
	}
	if s.MaxModelLen != 32768 || s.AgentCtxTokens != 32768 {
		t.Errorf("ampere-16 vllm_seat window = %d (agent_ctx_tokens %d), want 32768/32768 — %s", s.MaxModelLen, s.AgentCtxTokens, adr)
	}
	if s.GPUMemoryUtilization < 0.899 || s.GPUMemoryUtilization > 0.901 {
		t.Errorf("ampere-16 vllm_seat gpu_memory_utilization = %v, want 0.90 — at 0.92 the seat takes the embedder off the card (measured HTTP 500); %s", s.GPUMemoryUtilization, adr)
	}
	if s.AgentMaxTokens != 4096 || s.AgentThinking != "off" || s.AgentTimeoutSec != 900 || s.AgentSeatTokS != 5.75 {
		t.Errorf("ampere-16 bound-lane settings = max_tokens %d / thinking %q / timeout %d / tok_s %v, want 4096 / off / 900 / 5.75 (the digest-8 8/8 configuration) — %s",
			s.AgentMaxTokens, s.AgentThinking, s.AgentTimeoutSec, s.AgentSeatTokS, adr)
	}
	smp := s.AgentSampling
	if smp == nil || smp.Temperature == nil || *smp.Temperature != 0.7 || smp.TopP == nil || *smp.TopP != 0.8 || smp.TopK == nil || *smp.TopK != 20 || smp.PresencePenalty == nil || *smp.PresencePenalty != 1.5 {
		t.Errorf("ampere-16 agent_sampling = %v, want the Qwen3.8 vendor values (0.7 / 0.8 / 20 / 1.5) both arms were measured at — %s", smp, adr)
	}
	if err := s.Validate("ampere-16"); err != nil {
		t.Errorf("ampere-16 vllm_seat must validate as declared: %v", err)
	}
}
