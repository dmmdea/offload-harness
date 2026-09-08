package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDualBlackwellHasNoVLLMSeatByMeasurement pins a MEASURED absence. blackwell-3x16
// seeds the 27B vLLM seat on its 5060 Ti pair; blackwell-2x16 looks like the same
// box minus one card, and the obvious "propagation" is to copy that seat across. It
// was deliberately not copied, and on 2026-09-08 the reason was measured rather than
// argued (Benchmarks and Optimizations/2026-09-08-blackwell-2x16-fit/):
//
//   - A TP2 worker of RedHatAI/Qwen3.8-27B-INT4 has a non-KV footprint of 11.22 GiB
//     per card (util 0.85 on the utility pair: "Available KV cache memory: 2.32 GiB",
//     "GPU KV cache size: 141,266 tokens" -> ~60.9k tokens per GiB of pool).
//   - On the 2-card tier one of the two cards IS the display card. At the operating
//     point that keeps the >= 4 GiB desktop floor (util 0.50 = 8.15 GiB per card with
//     3.7-4.1 GB of desktop on the 5070 Ti that day) vLLM profiled
//     "Available KV cache memory: -3.36 GiB" and refused: the weights alone do not fit.
//   - Even a bare desktop (~1 GB DWM) leaves 15.9 - 1 - 4 - 11.22 < 0 GiB for KV under
//     the floor. The only way the seat exists on this tier is by starving the desktop,
//     which is the 2026-09-04 incident (720p-class mode, reboot).
//
// So the tier keeps the llama.cpp 27B agent seat at 131,072 and has NO vllm_seat. A
// future change that adds one must come with a fit measurement that contradicts the
// numbers above; this test makes the copy fail with the reason attached.
func TestDualBlackwellHasNoVLLMSeatByMeasurement(t *testing.T) {
	const tier = "blackwell-2x16"
	raw, err := os.ReadFile(filepath.Join("setup", "templates", "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Profiles map[string]struct {
			VLLMSeat json.RawMessage `json:"vllm_seat"`
			Notes    string          `json:"notes"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("profiles.json is not valid JSON: %v", err)
	}
	p, ok := doc.Profiles[tier]
	if !ok {
		t.Fatalf("tier %q not found — this gate went blind", tier)
	}
	if len(p.VLLMSeat) > 0 && string(p.VLLMSeat) != "null" {
		t.Fatalf("tier %q declares a vllm_seat. Measured 2026-09-08: a TP2 27B worker needs "+
			"11.22 GiB per card before any KV, and the second card of this tier is the display "+
			"card, which keeps a >= 4 GiB desktop floor — util 0.50 profiled -3.36 GiB of KV. "+
			"Re-measure (Benchmarks and Optimizations/2026-09-08-blackwell-2x16-fit/) before seeding one", tier)
	}
	// The notes must carry the measurement, so the next reader of the tier table sees WHY
	// rather than a gap that looks like an omission.
	for _, must := range []string{"2026-09-08", "11.22 GiB", "-3.36 GiB"} {
		if !strings.Contains(p.Notes, must) {
			t.Errorf("tier %q notes do not record the fit measurement (%q missing)", tier, must)
		}
	}
}
