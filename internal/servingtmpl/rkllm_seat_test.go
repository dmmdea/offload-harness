package servingtmpl

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/mediaseat"
)

// rkllmSeat is the shape the rockchip-rk3588 tier declares: an NPU seat that is a chat
// model AND a VLM, on the A55 cluster.
func rkllmSeat() mediaseat.Seat {
	return mediaseat.Seat{
		Kind: mediaseat.KindRKLLM, Name: "qwen3.5-0.8b-npu", Aliases: []string{"vision", "vlm"},
		Model: "qwen3.5-0.8b_w8a8_rk3588.rkllm", VisionEncoder: "qwen3.5-0.8b_vision_rk3588.rknn",
		CtxSize: 16384, CPUMask: "0x0f", Residency: mediaseat.Swappable, TTL: 300,
	}
}

// ownBlockOf is seatBlockOf for a seat that may be the LAST model in the map: what follows
// such a block can be a column-0 comment rather than another model key, and seatBlockOf
// would carry that comment along.
func ownBlockOf(out, modelID string) string {
	var b strings.Builder
	in := false
	for _, ln := range strings.Split(out, "\n") {
		if in && !strings.HasPrefix(ln, "    ") {
			break
		}
		if ln == "  "+modelID+":" {
			in = true
		}
		if in {
			b.WriteString(ln + "\n")
		}
	}
	return b.String()
}

// TestRKLLMSeatRendersItsLaunchLine pins the block byte for byte: this is the contract
// between the renderer and the launcher (accelerators/rknpu/rkllm-serve.sh), which is
// written and tested elsewhere. A flag renamed on one side and not the other starts a
// seat that dies at argument parsing, and llama-swap reports it as a health-check timeout.
func TestRKLLMSeatRendersItsLaunchLine(t *testing.T) {
	out := mustRender(t, linuxCUDA(t), seatParams(rkllmSeat()))
	want := "  qwen3.5-0.8b-npu:\n" +
		"    aliases: [vision, vlm]\n" +
		"    cmd: >-\n" +
		"      /srv/offload/rknpu/rkllm-serve.sh --model /srv/offload/models/qwen3.5-0.8b_w8a8_rk3588.rkllm --vision-encoder /srv/offload/models/qwen3.5-0.8b_vision_rk3588.rknn\n" +
		"      --ctx-size 16384 --cpu-mask 0x0f --served-name qwen3.5-0.8b-npu --port ${PORT} --host 127.0.0.1\n" +
		"    checkEndpoint: /health\n" +
		"    ttl: 300\n"
	if got := ownBlockOf(out, "qwen3.5-0.8b-npu"); got != want {
		t.Errorf("rkllm seat block:\n%s\nwant:\n%s", got, want)
	}

	cfg := parseSwapConfig(t, out)
	m, ok := cfg.Models["qwen3.5-0.8b-npu"]
	if !ok {
		t.Fatal("the seat is not in the parsed models map")
	}
	// The block is the same document llama-swap parses: the folded cmd is ONE line, and
	// nothing llama.cpp-shaped (loader path, device pin, GPU flags) leaked onto the NPU.
	wantCmd := "/srv/offload/rknpu/rkllm-serve.sh --model /srv/offload/models/qwen3.5-0.8b_w8a8_rk3588.rkllm " +
		"--vision-encoder /srv/offload/models/qwen3.5-0.8b_vision_rk3588.rknn " +
		"--ctx-size 16384 --cpu-mask 0x0f --served-name qwen3.5-0.8b-npu --port ${PORT} --host 127.0.0.1"
	if m.Cmd != wantCmd {
		t.Errorf("cmd = %q, want %q", m.Cmd, wantCmd)
	}
	if len(m.Env) != 0 {
		t.Errorf("an rkllm seat carries no env (the loader path and the device pin are llama.cpp's), got %v", m.Env)
	}
	if strings.Contains(m.Cmd, "--n-gpu-layers") || strings.Contains(m.Cmd, "--flash-attn") {
		t.Errorf("GPU flags are llama-server's and the NPU is not a llama.cpp device: %q", m.Cmd)
	}
	if m.TTL == nil || *m.TTL != 300 {
		t.Errorf("ttl = %v, want 300 (every model unloads after five idle minutes)", m.TTL)
	}
	if vs := Audit(out); len(vs) != 0 {
		t.Errorf("the rendered config breaks the serving-config rules:\n%s", Violations(vs))
	}
}

// TestRKLLMSeatJoinsTheMatrixAsAnAlternative: llama-swap refuses a set naming an unknown
// var, and a model in no set is one the solver has no combination for.
func TestRKLLMSeatJoinsTheMatrixAsAnAlternative(t *testing.T) {
	out := mustRender(t, linuxCUDA(t), seatParams(rkllmSeat()))
	cfg := parseSwapConfig(t, out)
	if got := cfg.Matrix.Vars["rkllm"]; got != "qwen3.5-0.8b-npu" {
		t.Errorf("matrix var rkllm = %q, want the seat name (vars: %v)", got, cfg.Matrix.Vars)
	}
	if got := cfg.Matrix.Sets["interactive"]; !strings.HasSuffix(got, " | rkllm)") {
		t.Errorf("a swappable rkllm seat must be an alternative in the interactive set, got %q", got)
	}
}

// TestRKLLMSeatWithoutAnEncoderIsTextOnly: --vision-encoder is what makes the runtime load
// the vision model, so a seat that names none must not be handed one.
func TestRKLLMSeatWithoutAnEncoderIsTextOnly(t *testing.T) {
	s := rkllmSeat()
	s.VisionEncoder = ""
	out := mustRender(t, linuxCUDA(t), seatParams(s))
	if blk := ownBlockOf(out, s.Name); strings.Contains(blk, "--vision-encoder") {
		t.Errorf("a text-only rkllm seat rendered a vision encoder:\n%s", blk)
	}
}

// TestRKLLMSeatDefaultsToTheShippedLauncherAndTheA55Cluster: a tier that names neither gets
// the launcher the rknpu accelerator ships and the strict CPU reservation.
func TestRKLLMSeatDefaultsToTheShippedLauncherAndTheA55Cluster(t *testing.T) {
	bare := mediaseat.Seat{Kind: mediaseat.KindRKLLM, Name: "npu-chat", Model: "m.rkllm", CtxSize: 4096, Residency: mediaseat.Swappable}
	out := mustRender(t, linuxCUDA(t), seatParams(bare))
	blk := ownBlockOf(out, "npu-chat")
	for _, want := range []string{
		"/srv/offload/rknpu/rkllm-serve.sh --model /srv/offload/models/m.rkllm\n",
		"--ctx-size 4096 --cpu-mask 0x0f --served-name npu-chat",
		"    ttl: 300", // no ttl declared: the operator's five-minute rule, never llama-swap's "forever"
	} {
		if !strings.Contains(blk, want) {
			t.Errorf("bare rkllm seat block missing %q:\n%s", want, blk)
		}
	}
	if strings.Contains(blk, "aliases:") {
		t.Errorf("a seat with no aliases renders no aliases line:\n%s", blk)
	}
}

// TestRKLLMSeatCarriesItsOwnLauncherAndMask: a seat's own values win over the defaults, and
// the launcher may name the install home like every other seat binary.
func TestRKLLMSeatCarriesItsOwnLauncherAndMask(t *testing.T) {
	s := rkllmSeat()
	s.Bin, s.CPUMask = "__OFFLOAD_HOME__/npu/serve.sh", "0xf0"
	out := mustRender(t, linuxCUDA(t), seatParams(s))
	blk := ownBlockOf(out, s.Name)
	if !strings.Contains(blk, "/srv/offload/npu/serve.sh --model") || !strings.Contains(blk, "--cpu-mask 0xf0 ") {
		t.Errorf("the seat's own launcher and mask were not rendered:\n%s", blk)
	}
	if strings.Contains(blk, "rkllm-serve.sh") || strings.Contains(blk, "0x0f") {
		t.Errorf("a default leaked through a seat's own value:\n%s", blk)
	}
}

// TestRKLLMDefaultLauncherNeedsTheInstallHome: the default launcher lives under
// __OFFLOAD_HOME__, so a bare seat rendered with no home must be refused by name — not
// reach the token guard, which reports an unresolved token and no hint which seat asked.
func TestRKLLMDefaultLauncherNeedsTheInstallHome(t *testing.T) {
	bare := mediaseat.Seat{Kind: mediaseat.KindRKLLM, Name: "npu-chat", Model: "m.rkllm", CtxSize: 4096, Residency: mediaseat.Swappable}
	p := seatParams(bare)
	p.Home = ""
	_, err := Render(linuxCUDA(t), p)
	if err == nil || !strings.Contains(err.Error(), "install home") {
		t.Fatalf("a bare rkllm seat with no home must be refused as needing the install home, got %v", err)
	}
	// A seat that names a launcher with no home token needs none.
	p.Seats[0].Bin = "/usr/local/bin/rkllm-serve"
	if _, err := Render(linuxCUDA(t), p); err != nil {
		t.Fatalf("a launcher with no home token renders without a home: %v", err)
	}
}

// TestRKLLMSeatsTakeTheirOwnVarIds: a text-only rkllm seat writes no config key, so a tier
// may declare several, and each needs its own matrix var beside a llama-backed seat's.
func TestRKLLMSeatsTakeTheirOwnVarIds(t *testing.T) {
	first, second := rkllmSeat(), rkllmSeat()
	first.VisionEncoder = "" // the llama vision seat owns vision_model here
	second.Name, second.Aliases, second.VisionEncoder = "npu-chat", nil, ""
	out := mustRender(t, linuxCUDA(t), seatParams(visionSeat(), first, second))
	cfg := parseSwapConfig(t, out)
	if cfg.Matrix.Vars["vis"] != "gemma4-e4b-vision" || cfg.Matrix.Vars["rkllm"] != "qwen3.5-0.8b-npu" || cfg.Matrix.Vars["rkllm2"] != "npu-chat" {
		t.Errorf("each seat needs its own var id, got %v", cfg.Matrix.Vars)
	}
	if got := cfg.Matrix.Sets["interactive"]; !strings.HasSuffix(got, " | vis | rkllm | rkllm2)") {
		t.Errorf("all three seats are alternatives in the interactive set, got %q", got)
	}
}
