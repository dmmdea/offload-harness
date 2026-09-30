package tierdocs

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/mediaseat"
)

// TestMediaSeedKeyCoversMediacapRouteKeys pins mediaSeedKey against the route
// keys internal/mediacap actually derives CONFIGURED / NOT CONFIGURED from
// (mediacap.go: inpaint_script/inpaint_ckpt, gen_edit_script/gen_edit_unet,
// run_graph_script, comfy_dir, edit_python) plus the seed families the docs
// already classified as media. A key on this list drifting to non-media makes
// a tier page assert "ships no media configuration" over a tier that ships
// exactly that — the false-claim wart the partition exists to prevent.
func TestMediaSeedKeyCoversMediacapRouteKeys(t *testing.T) {
	media := []string{
		// mediacap's own route-deciding keys:
		"inpaint_script", "inpaint_ckpt", "inpaint_vae",
		"gen_edit_script", "gen_edit_unet", "gen_edit_preset",
		"run_graph_script", "comfy_dir", "edit_python",
		"upscale_script", "upscale_model",
		"compose_script", "hyperframes_dir", "hyperframes_browser_path", "compose_workers",
		// spawn-per-job seed families:
		"imagegen_family", "imagegen_ckpt", "imagegen_pool_vvram_gb",
		"videogen_family", "videogen_transformer", "videogen_width",
		"musicgen_script", "voicegen_script",
		// sd.cpp engine family:
		"sdcpp_bin", "vae_mode",
	}
	for _, k := range media {
		if !mediaSeedKey(k) {
			t.Errorf("mediaSeedKey(%q) = false — a media route key classified non-media", k)
		}
	}
	nonMedia := []string{
		"agent_model", "agent_timeout_sec", "agent_ctx_tokens",
		"escalation_model", "reasoning_model", "triage_model",
		"fleet_sampler", "nim_model", "nim_endpoint",
	}
	for _, k := range nonMedia {
		if mediaSeedKey(k) {
			t.Errorf("mediaSeedKey(%q) = true — harness config classified as media", k)
		}
	}
}

// TestRKLLMSeatDocumentsWhatItBindsAndHowItRuns: the seat table used to assume every seat
// binds vision_model unless it was stt or ocr, which would document a binding a text-only
// rkllm seat never writes. An rkllm seat also runs outside llama.cpp, so the page says
// which CPUs the runtime may use — the one setting that decides how it shares the board.
func TestRKLLMSeatDocumentsWhatItBindsAndHowItRuns(t *testing.T) {
	vlm := mediaseat.Seat{Kind: mediaseat.KindRKLLM, Name: "npu-vlm", Model: "m.rkllm", VisionEncoder: "enc.rknn",
		CtxSize: 16384, CPUMask: "0xf0", Residency: mediaseat.Swappable}
	chat := mediaseat.Seat{Kind: mediaseat.KindRKLLM, Name: "npu-chat", Model: "c.rkllm", CtxSize: 4096, Residency: mediaseat.Swappable}
	page := renderTier("t", Profile{CtxSize: 8192, MediaSeats: []mediaseat.Seat{vlm, chat}}, nil)
	for _, want := range []string{
		"| `npu-vlm` | rkllm | `vision_model` | `m.rkllm` | swappable |",
		"| `npu-chat` | rkllm | — | `c.rkllm` | swappable |", // a chat seat writes no binding
		"- `npu-vlm`: window 16384, `cpu_mask` `0xf0`, vision encoder `enc.rknn`",
		"- `npu-chat`: window 4096, `cpu_mask` `0x0f`", // the default mask, spelled out
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q:\n%s", want, page)
		}
	}
}

// TestNoRKLLMSeatNoRKLLMProse: the runtime note appears only on a tier that declares one, so
// every other tier's page is exactly what it was.
func TestNoRKLLMSeatNoRKLLMProse(t *testing.T) {
	vision := mediaseat.Seat{Kind: mediaseat.KindVision, Name: "v", Model: "m.gguf", MMProj: "p.gguf", CtxSize: 4096, Residency: mediaseat.Swappable}
	page := renderTier("t", Profile{CtxSize: 8192, MediaSeats: []mediaseat.Seat{vision}}, nil)
	if strings.Contains(page, "rkllm") {
		t.Errorf("a tier without an rkllm seat mentions rkllm:\n%s", page)
	}
	if !strings.Contains(page, "| `v` | vision | `vision_model` | `m.gguf` | swappable |") {
		t.Errorf("a vision seat's row changed:\n%s", page)
	}
}
