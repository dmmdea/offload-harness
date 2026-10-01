package tierdocs

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/vllmseat"
)

func laneSpec() *vllmseat.Spec {
	return &vllmseat.Spec{
		ID: "lane-vllm", Unit: "vllm-agent-seat", Port: 18797, Device: "0", MaxModelLen: 32768, GPUMemoryUtilization: 0.9,
		MaxNumSeqs: 32, KVCacheDtype: "fp8_e5m2", ToolCallParser: "qwen3_xml", ReasoningParser: "qwen3",
		Fallback: "lane-fallback", StorelessReason: "lane measured reason\nsecond line",
	}
}

func extraSpec() vllmseat.Spec {
	return vllmseat.Spec{
		ID: "fast-vllm", Aliases: []string{"fast-pool", "fast"}, Unit: "vllm-fast-seat", Port: 18797, Device: "0",
		MaxModelLen: 32768, GPUMemoryUtilization: 0.9, MaxNumSeqs: 8, KVCacheDtype: "fp8_e5m2",
		ToolCallParser: "qwen3_coder", ReasoningParser: "qwen3", TTLSeconds: 300,
		StorelessReason: "fast measured reason", Measured: "fast operating point",
	}
}

func layers() []config.LayerSpec {
	return []config.LayerSpec{
		{Name: "single", Tier: "t", Devices: []string{"0"}, Seats: []config.LayerSeat{{Role: "agent", Model: "lane-vllm", Device: "0", CtxTokens: 32768}}},
		{Name: "fast", Tier: "t", Devices: []string{"0"}, Seats: []config.LayerSeat{{Role: "agent", Model: "fast-vllm", Device: "0", CtxTokens: 32768, MaxInflight: 8}}},
	}
}

// A tier that declares extra vLLM seats and a layers-only composite states both on its
// page: the layer table (with the seat's concurrency), the seat's operating point, and the
// MEASURED reason it has no cache server — the three things the page could not state while
// they lived only in a reference box's hand-edited config.
func TestExtraSeatsLayersAndStorelessReasonsAreRendered(t *testing.T) {
	p := Profile{CtxSize: 8192, VLLMSeat: laneSpec(), ExtraVLLMSeats: []vllmseat.Spec{extraSpec()}, Layers: layers()}
	page := renderTier("t", p, nil)
	for _, want := range []string{
		"\n## Layers\n",
		"| `single` | `t` | `0` | agent → `lane-vllm` (device 0, window 32768) | — | active |",
		"| `fast` | `t` | `0` | agent → `fast-vllm` (device 0, window 32768, 8 in flight) | — | active |",
		"### Extra vLLM seats",
		"#### `fast-vllm`",
		"| aliases | `fast-pool` / `fast` |",
		"| unit | `vllm-fast-seat` |",
		"| max_num_seqs | 8 |",
		// the lane seat's own table states its concurrency as well: max_num_seqs sizes the engine's graph
		// capture and workspaces, so it is a co-residency lever beside gpu_memory_utilization (2026-09-30)
		"| max_num_seqs | 32 |",
		"| tool_call_parser / reasoning_parser | `qwen3_coder` / `qwen3` |",
		"**Cache server:** none.",
		"> fast measured reason",
		"**Measured.**\n\n> fast operating point",
		// the lane seat's own storeless reason, block-quoted line by line
		"### Cache server\n\nThis seat is deliberately **storeless**",
		"> lane measured reason\n> second line",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "## Composes") {
		t.Errorf("a tier that composes nothing must not be headed \"Composes\":\n%s", page)
	}
}

// The composite tier keeps its heading and its list, and a tier with none of this is
// byte-for-byte what it was: nothing here may leak into a page whose tier declares none.
func TestPlainAndCompositeTierPagesAreUnchanged(t *testing.T) {
	composite := renderTier("t3", Profile{CtxSize: 8192, Composes: []string{"a", "b"}, Layers: []config.LayerSpec{
		{Name: "single", Tier: "a", Devices: []string{"0"}, Seats: []config.LayerSeat{{Role: "agent", Model: "m", Device: "0", CtxTokens: 4096}}},
	}}, nil)
	if !strings.Contains(composite, "\n## Composes\n") || strings.Contains(composite, "\n## Layers\n") {
		t.Errorf("a composite tier lost its Composes heading:\n%s", composite)
	}
	if strings.Contains(composite, "in flight") {
		t.Errorf("a seat with no declared concurrency printed one:\n%s", composite)
	}
	plain := renderTier("t0", Profile{CtxSize: 8192, VLLMSeat: &vllmseat.Spec{
		ID: "s", Unit: "u", Port: 1, MaxModelLen: 1, GPUMemoryUtilization: 0.5, MaxNumSeqs: 1, ToolCallParser: "p", ReasoningParser: "r", Fallback: "f",
	}}, nil)
	for _, unwanted := range []string{"Extra vLLM seats", "## Layers", "storeless", "**Cache server:**"} {
		if strings.Contains(plain, unwanted) {
			t.Errorf("a tier with no extra seats and no recorded storeless reason mentions %q:\n%s", unwanted, plain)
		}
	}
}
