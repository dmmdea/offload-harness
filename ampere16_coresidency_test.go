package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

// The ampere-16 card (a 16 GB NVIDIA A2: 15,356 MiB) serves one of its two vLLM seats at a time and keeps the
// memory stack's support models beside whichever is loaded: the embedder that backs the memory authority
// (embeddinggemma 300M Q8, 452-460 MiB) and the reranker (bge-reranker-v2-m3 Q4_K_M, 306-378 MiB depending on its
// batch size). Operator order, 2026-09-30: the memory embedder has absolute priority, so the seats make room for
// it and it is never the one excluded; 2026-10-01 (register A-122b): the reranker stays resident beside the loaded
// seat too, with about 1 GiB of the card free. Two measurements beside both support models:
//
// 2026-09-30:
//
//   - the lane seat declared at util 0.90 with max_num_seqs 32 ran the engine at 14,788 MiB under four
//     concurrent 8k-token requests, about 1 GB above its util share (graph capture and workspaces sized for 32
//     sequences), so the embedder could not load and every memory write failed for 35 minutes;
//   - util 0.82 is refused (KV 0.64 GiB, 1.29 GiB needed for one 32,768-token request) and util 0.87 with 32
//     sequences is refused (28 Mamba cache blocks for 32 sequences): the way down was util 0.87 AND 8 sequences,
//     which peaked at 13,984 MiB and left the card about 520 MiB of headroom with both support models resident;
//   - the 35B seat at util 0.85 peaks at 13,630 MiB (about 900 MiB of headroom) with a 67,025-token KV pool.
//
// 2026-10-01 (A-122b; four concurrent ~20k-token requests, a cold rerank 8 s into the load, rerank + embed every
// 2 s, nvidia-smi peaks):
//
//   - util 0.87 with 8 sequences and 4,096 batched tokens peaked at 14,717 MiB (seat 13,860, embedder 460,
//     reranker 378), 639 MiB of headroom, and the reranker failed to start beside the loaded seat 7 times;
//   - util 0.84 with 4 sequences and 2,048 batched tokens, the reranker at --ctx-size 4096 --batch-size 2048
//     --ubatch-size 2048, peaked at 13,987 MiB (seat 13,204, embedder 458, reranker 306), 1,369 MiB of headroom,
//     with a 45,472-token KV pool (1.39x the window), and the four requests finished in 544 s where the 0.87 point
//     needed 900+ s; util 0.85 with 4 sequences did not start in that arm (unexplained, not re-run);
//   - the 35B seat at util 0.85 with 8 sequences and 4,096 batched tokens leaves 969 MiB beside the smaller reranker.
//
// 2026-10-02 (A-119, A-129; vLLM 0.30.0, the same load beside both support models):
//
//   - the lane seat is started with a FIXED 1,524,713,390-byte KV pool (the 1.42 GiB 0.29.0 sized warm, 45,472
//     tokens) because a utilization-sized pool is measured during startup profiling, and a cold compile cache
//     (profiled activation 0.77 GiB against 0.41 warm) or a memory-stack model loading in that window shrank it below
//     one 32,768-token request: three production start failures. Card peak 13,951 MiB, 1,405 MiB free;
//   - the 35B seat moved to util 0.855 (0.30.0 carries about 0.07 GiB more non-torch memory, and 0.85 lost 3 KV
//     blocks): 96,416 tokens, 1,027-1,037 MiB free under load.
//
// ampere16CoResidency is that measured point, per seat: the HIGHEST gpu_memory_utilization, max_num_seqs and
// max_num_batched_tokens the seat was measured to leave the support models room at (ADR 0049 Amendments 5 and 6), and
// the KV pool it was measured with when it pins one (0 = sized by utilization, as measured).
var ampere16CoResidency = []struct {
	id         string
	maxUtil    float64
	maxSeqs    int
	maxBatched int
	kvPool     int64
}{
	{id: "qwen38-27b-gsq-vllm", maxUtil: 0.84, maxSeqs: 4, maxBatched: 2048, kvPool: 1524713390},
	{id: "qwen36-35b-a3b-gsq-vllm", maxUtil: 0.855, maxSeqs: 8, maxBatched: 4096},
}

// ampere16Seats reads the tier's vLLM seats (the lane seat and the extra seats) straight from the JSON, not
// through the tier parser: a gate that trusts the code it guards goes blind with it.
func ampere16Seats(t *testing.T) map[string]vllmseat.Spec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			VLLMSeat       *vllmseat.Spec  `json:"vllm_seat"`
			ExtraVLLMSeats []vllmseat.Spec `json:"extra_vllm_seats"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	p, ok := doc.Profiles["ampere-16"]
	if !ok {
		t.Fatal("no ampere-16 tier: this gate went blind")
	}
	seats := map[string]vllmseat.Spec{}
	if p.VLLMSeat != nil {
		seats[p.VLLMSeat.ID] = *p.VLLMSeat
	}
	for _, e := range p.ExtraVLLMSeats {
		seats[e.ID] = e
	}
	return seats
}

// TestAmpere16VLLMSeatsStayAtOrUnderTheirMeasuredCoResidencyPoint fails when an ampere-16 vLLM seat's util, its
// sequence count or its batch budget (the last two size the engine's graph capture and workspaces) rises above the
// point it was measured to leave the memory embedder and the reranker room at. It is exact in both directions: a
// seat in the table that this list does not name is a seat nobody measured beside the support models.
func TestAmpere16VLLMSeatsStayAtOrUnderTheirMeasuredCoResidencyPoint(t *testing.T) {
	const why = "the memory embedder (about 460 MiB) and the reranker (306-378 MiB) share this 15,356 MiB card, and above the measured point the engine leaves them no room: the embedder cannot load and every memory write fails while the seat is warm (2026-09-30: util 0.90 with 32 sequences ran the engine at 14,788 MiB and failed every write for 35 minutes), or the reranker cannot start beside the loaded seat (2026-10-01: util 0.87 with 8 sequences and a 4,096 batch left 639 MiB and it failed 7 times). Memory has absolute priority over the seats: re-measure beside both support models before raising it (ADR 0049 Amendment 5)"
	seats := ampere16Seats(t)
	named := map[string]bool{}
	for _, c := range ampere16CoResidency {
		named[c.id] = true
		s, ok := seats[c.id]
		if !ok {
			t.Errorf("ampere-16 no longer declares vLLM seat %s, which this guard names: a seat leaves the table only on an operator's say-so, with its entry removed from ampere16CoResidency in the same change", c.id)
			continue
		}
		if s.GPUMemoryUtilization > c.maxUtil+1e-9 {
			t.Errorf("ampere-16 %s gpu_memory_utilization = %v, above its measured co-residency point %v: %s", c.id, s.GPUMemoryUtilization, c.maxUtil, why)
		}
		if s.MaxNumSeqs > c.maxSeqs {
			t.Errorf("ampere-16 %s max_num_seqs = %d, above its measured co-residency point %d (graph capture and workspaces scale with it): %s", c.id, s.MaxNumSeqs, c.maxSeqs, why)
		}
		if s.MaxBatchedTokens <= 0 {
			t.Errorf("ampere-16 %s declares no max_num_batched_tokens: its measured co-residency point is %d, and an undeclared budget is not a measured one: %s", c.id, c.maxBatched, why)
		}
		if s.MaxBatchedTokens > c.maxBatched {
			t.Errorf("ampere-16 %s max_num_batched_tokens = %d, above its measured co-residency point %d (the engine's activation workspace scales with it): %s", c.id, s.MaxBatchedTokens, c.maxBatched, why)
		}
		// The pool is exact, both ways: a bigger one is a pool nobody measured beside the support models, a smaller
		// or absent one is the util-sized pool whose startup profiling a cold compile cache or a memory-stack model
		// loading can shrink below one request (register A-129), and a pin on a seat the guard measured util-sized is
		// a declaration this guard has never seen.
		if s.KVCacheMemoryBytes != c.kvPool {
			t.Errorf("ampere-16 %s kv_cache_memory_bytes = %d, want its measured pool %d (0 = util-sized, as measured): vLLM ignores the utilization while a pool is pinned, so the pool IS the co-residency point, and an unpinned pool is sized during startup profiling, which a cold compile cache or a memory-stack model loading in that window can shrink below one request (register A-129): %s", c.id, s.KVCacheMemoryBytes, c.kvPool, why)
		}
	}
	for id := range seats {
		if !named[id] {
			t.Errorf("ampere-16 declares vLLM seat %s that ampere16CoResidency does not list: measure it beside the embedder and the reranker and register its point, so the guard protects it", id)
		}
	}
}

// TestAmpere16FastSeatDeclaresItsVLLM030Utilization: the ceiling above lets a seat sit anywhere BELOW its measured
// point, and for the 35B that is the wrong direction to be lenient in. vLLM 0.30.0 carries about 0.07 GiB more
// non-torch memory than 0.29.0, so at the previous util 0.85 the pool lost 3 KV blocks; util 0.855 is the point
// measured on 0.30.0 (96,416 tokens, 1,027-1,037 MiB free under load beside both support models, digest-8 8/8;
// ADR 0049 Amendment 6, register A-119). A seed that stays at 0.85 serves a smaller pool than the one measured.
func TestAmpere16FastSeatDeclaresItsVLLM030Utilization(t *testing.T) {
	s, ok := ampere16Seats(t)["qwen36-35b-a3b-gsq-vllm"]
	if !ok {
		t.Fatal("ampere-16 declares no 35B fast seat: this gate went blind")
	}
	if math.Abs(s.GPUMemoryUtilization-0.855) > 1e-9 {
		t.Errorf("ampere-16 35B gpu_memory_utilization = %v, want 0.855: vLLM 0.30.0 carries about 0.07 GiB more non-torch memory than 0.29.0 and util 0.85 lost 3 KV blocks; 0.855 measured 96,416 tokens with 1,027-1,037 MiB free under load (ADR 0049 Amendment 6, register A-119)", s.GPUMemoryUtilization)
	}
}

// TestAmpere16LaneSeatRendersItsPinnedPoolIntoTheLaunchLine closes the loop the declaration alone leaves open: the
// tier table declares kv_cache_memory_bytes, the validator accepts it, and still the engine would start util-sized
// if the run script the installer renders did not carry it. This renders the COMMITTED ampere-16 lane seat through
// the real linux-systemd template and requires the exact flag on the `exec vllm serve` command (not in the comments
// above it). The 35B is not rendered by the installer (its unit and launch line are the operator's), so its side is
// the declaration: it pins no pool.
func TestAmpere16LaneSeatRendersItsPinnedPoolIntoTheLaunchLine(t *testing.T) {
	seats := ampere16Seats(t)
	lane, ok := seats["qwen38-27b-gsq-vllm"]
	if !ok {
		t.Fatal("ampere-16 declares no lane seat: this gate went blind")
	}
	lane.ModelPath = "/hf/" + lane.ModelRepo + "/snapshots/0123456789abcdef"
	rt := seatRuntime()
	rt.LMCacheOverlay = "" // the lane seat is storeless
	files, err := lane.Artifacts(filepath.Join("setup", "templates", "vllm-seat", "linux-systemd"), rt)
	if err != nil {
		t.Fatalf("the lane seat does not render through the linux-systemd template: %v", err)
	}
	run := files["vllm-seat-run.sh"]
	i := strings.Index(run, "exec vllm serve")
	if i < 0 {
		t.Fatalf("the run script has no `exec vllm serve` line:\n\n%s", run)
	}
	if cmd := run[i:]; !strings.Contains(cmd, " --kv-cache-memory-bytes 1524713390 ") {
		t.Errorf("the rendered launch line does not carry --kv-cache-memory-bytes 1524713390, so the engine would size its KV pool during startup profiling and a cold compile cache or a memory-stack model loading then can fail the start (register A-129):\n\n%s", cmd)
	}
	if fast, ok := seats["qwen36-35b-a3b-gsq-vllm"]; !ok || fast.KVCacheMemoryBytes != 0 {
		t.Errorf("the 35B seat is util-sized as measured (no pool has been measured for it), got declared=%v pool=%d", ok, fast.KVCacheMemoryBytes)
	}
}

// TestAmpere16SeatNotesCarryTheCoResidencyMeasurement keeps the measurement with the declaration, so a generic
// sentence cannot replace it and a later reader sees why util is the number it is (and why the lane seat's
// sequence count is 4 and its batch 2,048, not 32 and 4,096). The 2026-09-30 fragments stay: they are the history
// the 2026-10-01 re-measure refines.
func TestAmpere16SeatNotesCarryTheCoResidencyMeasurement(t *testing.T) {
	need := map[string][]string{
		"qwen38-27b-gsq-vllm": {
			"CO-RESIDENCY RE-MEASURE 2026-09-30", "util 0.87", "max_num_seqs 8", "14,788 MiB", "13,984 MiB", "14,833 MiB",
			"CO-RESIDENCY RE-MEASURE 2026-10-01", "A-122b", "util 0.84", "max_num_seqs 4", "max_num_batched_tokens 2048",
			"45,472 tokens", "13,987 MiB", "1,369 MiB",
		},
		"qwen36-35b-a3b-gsq-vllm": {"CO-RESIDENCY RE-MEASURE 2026-09-30", "util 0.85", "67,025 tokens", "13,630 MiB"},
	}
	// The 2026-10-02 blocks (registers A-119, A-129, A-130) are appended, never substituted: the earlier ones are the
	// history this one refines, and the pin's reason has to stay beside the number it explains.
	need["qwen38-27b-gsq-vllm"] = append(need["qwen38-27b-gsq-vllm"],
		"vLLM 0.30.0 MOVE AND KV POOL PIN 2026-10-02", "A-119", "A-129", "--kv-cache-memory-bytes 1524713390", "1.42 GiB",
		"0.77 GiB against 0.41 warm", "To serve at least one request", "13,951 of 15,356 MiB", "1,405 MiB free", "digest-8 8/8",
		"greedy output identical to 0.29.0 on 20 prompts", "A-130")
	need["qwen36-35b-a3b-gsq-vllm"] = append(need["qwen36-35b-a3b-gsq-vllm"],
		"vLLM 0.30.0 MOVE 2026-10-02", "A-119", "util 0.855", "96,416 tokens", "1,027-1,037 MiB", "3 KV blocks", "digest-8 8/8", "A-130")
	seats := ampere16Seats(t)
	for id, frags := range need {
		s, ok := seats[id]
		if !ok {
			t.Errorf("ampere-16 declares no vLLM seat %s", id)
			continue
		}
		for _, frag := range frags {
			if !strings.Contains(s.Measured, frag) {
				t.Errorf("%s's measured note lost the co-residency measurement: missing %q", id, frag)
			}
		}
	}
}
