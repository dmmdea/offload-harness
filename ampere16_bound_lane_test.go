package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// TestAmpere16VLLMSeatDeclaresItsMeasuredBoundLane (ADR 0049 Amendment 3,
// operator decision D5 = A, 2026-09-16; operating point moved by Amendment 4,
// 2026-09-30, by Amendment 5, 2026-10-01, register A-122b, and pinned by
// Amendment 6, 2026-10-02, registers A-119 and A-129). The GSQ seat is the
// tier's bound agent lane at the ONE operating point measured to keep the
// memory stack's embedder AND reranker resident beside it with about 1 GiB of
// the card free: 32,768 with 4 sequences and a 2,048-token batch, started with
// a FIXED 1,524,713,390-byte KV pool (1.42 GiB, 45,472 tokens as vLLM 0.29.0
// sized it warm; card peak 13,951 of 15,356 MiB at vLLM 0.30.0). The pool is
// pinned because a utilization-sized pool is measured during startup profiling,
// device-wide, and a cold compile cache or a memory-stack model loading in that
// window shrank it below one request: three production start failures. vLLM
// ignores util 0.84 while the pool is pinned, so util stays declared as the
// point the workspace was measured at. Amendment 3's util 0.90 with 32
// sequences took the embedder off the card; Amendment 4's util 0.87 with 8
// sequences and a 4,096-token batch left 639 MiB, and the reranker failed to
// start beside the loaded seat 7 times. Its bound-lane settings are the
// configuration the digest-8 gate passed 8/8 at; the tier's config_seed values
// (the fallback seat's 1,024 tokens / 300 s) were measured failing on this seat
// (3/8). A change to any of these is a re-measurement, not an edit — the ADR
// names what has to be re-run.
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
	const adr = "ADR 0049 Amendments 5 and 6: 32,768 with max_num_seqs 4, max_num_batched_tokens 2048 and a pinned 1,524,713,390-byte KV pool is the measured operating point that keeps the embedder and the reranker resident beside the seat with about 1 GiB of the card free (card peak 13,951 of 15,356 MiB at vLLM 0.30.0); re-measure beside both before changing it"
	if s.ID != "qwen38-27b-gsq-vllm" {
		t.Errorf("ampere-16 vllm_seat id = %q, want qwen38-27b-gsq-vllm — %s", s.ID, adr)
	}
	if s.MaxModelLen != 32768 || s.AgentCtxTokens != 32768 {
		t.Errorf("ampere-16 vllm_seat window = %d (agent_ctx_tokens %d), want 32768/32768 — %s", s.MaxModelLen, s.AgentCtxTokens, adr)
	}
	// The pool is the sizing now: vLLM ignores gpu_memory_utilization while kv_cache_memory_bytes is set, and an
	// unpinned seat is sized during startup profiling, which a cold compile cache or a memory-stack model loading in
	// that window shrinks below one request (register A-129: 'To serve at least one request...', three production
	// start failures, reproduced on an empty card with the embedder fired about 50 s in).
	if s.KVCacheMemoryBytes != 1524713390 {
		t.Errorf("ampere-16 vllm_seat kv_cache_memory_bytes = %d, want 1524713390 (the 1.42 GiB pool vLLM 0.29.0 sized on a warm start, 45,472 tokens): without the pin the pool is measured during startup profiling and a cold compile cache or a memory-stack model loading then shrinks it below one 32,768-token request, so the start fails; %s", s.KVCacheMemoryBytes, adr)
	}
	if s.GPUMemoryUtilization < 0.839 || s.GPUMemoryUtilization > 0.841 {
		t.Errorf("ampere-16 vllm_seat gpu_memory_utilization = %v, want 0.84 (vLLM ignores it while the pool is pinned; it stays declared as the point the workspace and the support models were measured at) — at 0.90 the engine ran about 1 GB over its util share and the embedder could not load (every memory write failed for 35 minutes), and at 0.87 (8 sequences, 4,096 batched tokens) the card peaked at 14,717 MiB with 639 MiB free and the reranker failed to start beside the loaded seat 7 times; 0.85 with 4 sequences did not start in the arm that tried it (unexplained, not re-run), and 0.82 was refused in the 2026-09-30 arm before the sequence count came down (KV 0.64 GiB, 1.29 GiB needed for one 32,768-token request); %s", s.GPUMemoryUtilization, adr)
	}
	if s.MaxNumSeqs != 4 {
		t.Errorf("ampere-16 vllm_seat max_num_seqs = %d, want 4 — graph capture and workspaces scale with the sequence count: 32 sequences were the 1 GB over the util share (and util 0.87 with 32 is refused outright, 28 Mamba cache blocks for 32 sequences), and 8 left only 639 MiB beside both support models; %s", s.MaxNumSeqs, adr)
	}
	if s.MaxBatchedTokens != 2048 {
		t.Errorf("ampere-16 vllm_seat max_num_batched_tokens = %d, want 2048 — the engine's activation workspace scales with the batch: at 4,096 (util 0.87, 8 sequences) the card left 639 MiB and the reranker could not start beside the loaded seat, and at 2,048 (util 0.84, 4 sequences) the four ~20k-token requests finished in 544 s where the 4,096 point needed 900+ s; %s", s.MaxBatchedTokens, adr)
	}
	if s.AgentMaxTokens != 4096 || s.AgentThinking != "off" || s.AgentTimeoutSec != 900 || s.AgentSeatTokS != 7.17 {
		t.Errorf("ampere-16 bound-lane settings = max_tokens %d / thinking %q / timeout %d / tok_s %v, want 4096 / off / 900 / 7.17 (the digest-8 8/8 configuration) — %s",
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

// TestAmpere16LaneSeatDeclaresTheEngineItWasMeasuredOn: engine_min_version is a documentary key (it has no Spec
// field, so the typed decode above cannot see it), and it is the one place a reader learns which vLLM the pinned
// pool, the card peak and the digest-8 result were measured on. The reference box moved both seats to 0.30.0 on
// 2026-10-02 (register A-119) after acceptance arms; a seed that still said 0.29.0 would tell a fresh install a
// floor the measured point is not on.
func TestAmpere16LaneSeatDeclaresTheEngineItWasMeasuredOn(t *testing.T) {
	raw, err := os.ReadFile("setup/templates/profiles.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			VLLMSeat struct {
				EngineMinVersion string `json:"engine_min_version"`
			} `json:"vllm_seat"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	if got := doc.Profiles["ampere-16"].VLLMSeat.EngineMinVersion; got != "0.30.0" {
		t.Errorf("ampere-16 vllm_seat engine_min_version = %q, want 0.30.0 (ADR 0049 Amendment 6, register A-119: the pinned pool, the 13,951 MiB card peak and digest-8 8/8 were measured there)", got)
	}
}
