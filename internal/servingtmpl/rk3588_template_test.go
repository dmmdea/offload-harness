package servingtmpl

import (
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dmmdea/offload-harness/internal/mediaseat"
)

const rk3588Template = "llama-swap.linux-rk3588.yaml"

// rk3588Params is what the tier renders the template with: its GPU chat window, a home the
// NPU seat's launcher lives under, and no 26B (the template serves none).
func rk3588Params(seats ...mediaseat.Seat) Params {
	return Params{
		LlamaBin: "/opt/offload/build/llama.cpp/build/bin", ModelsDir: "/opt/offload/models",
		Listen: "127.0.0.1:11436", Ctx: 8192, KVType: "f16", FlashAttn: "off", Threads: 4,
		Seats: seats, Home: "/opt/offload", GOOS: "linux", Backend: "rk3588",
	}
}

// TestRK3588TemplateServesOnlyWhatFitsAndRunsItOnTheGPU: the template's own contract, read
// from the raw file. This board has no CPU inference (operator rule), so every llama-server
// entry must offload every layer and pin the Vulkan device, and the model list is the one
// chat entry — the stock vulkan template's offload-e4b does not fit the board's budget.
func TestRK3588TemplateServesOnlyWhatFitsAndRunsItOnTheGPU(t *testing.T) {
	raw := readTmpl(t, rk3588Template)
	var doc struct {
		Macros map[string]string `yaml:"macros"`
		Models map[string]struct {
			Cmd string   `yaml:"cmd"`
			Env []string `yaml:"env"`
		} `yaml:"models"`
	}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for id, m := range doc.Models {
		ids = append(ids, id)
		if !strings.Contains(m.Cmd, "--n-gpu-layers 999") {
			t.Errorf("%s does not offload every layer, which means CPU inference: %q", id, m.Cmd)
		}
		if !hasEnvValue(m.Env, "${vk}") || !hasEnvValue(m.Env, "${ld}") {
			t.Errorf("%s needs the Vulkan device pin and the loader path, env = %v", id, m.Env)
		}
	}
	sort.Strings(ids)
	if got := strings.Join(ids, ","); got != "gemma4-e2b" {
		t.Errorf("models = %s, want only gemma4-e2b — nothing else fits the board's shared-RAM budget", got)
	}
	if doc.Macros["vk"] != "GGML_VK_VISIBLE_DEVICES=0" {
		t.Errorf("vk macro = %q, want the single-device Vulkan pin", doc.Macros["vk"])
	}
	if !strings.Contains(doc.Macros["common"], "--reasoning off") || !strings.Contains(doc.Macros["common"], "--jinja") {
		t.Errorf("the grammar-reliability invariants (--jinja, reasoning off) are missing from ${common}: %q", doc.Macros["common"])
	}
}

// TestRK3588TemplatePlacesSwappableSeatsOnly: the template has no resident set (no embedder,
// no reranker). Declaring `resident` in the seat directive without a marker in some set would
// substitute a resident seat's fragment into nothing — a seat in the models map and in no
// combination. So a resident seat is refused by name instead.
func TestRK3588TemplatePlacesSwappableSeatsOnly(t *testing.T) {
	anchors, err := parseAnchors(readTmpl(t, rk3588Template))
	if err != nil {
		t.Fatal(err)
	}
	if len(anchors.roles) != 1 || !anchors.roles[mediaseat.Swappable] {
		t.Errorf("the template places %v, want exactly swappable", anchors.roles)
	}
	resident := rkllmSeat()
	resident.Residency = mediaseat.Resident
	_, err = Render(readTmpl(t, rk3588Template), rk3588Params(resident))
	if err == nil || !strings.Contains(err.Error(), "does not place") || !strings.Contains(err.Error(), "swappable") {
		t.Fatalf("a resident seat must be refused by name, got %v", err)
	}
}

// TestRK3588TemplateSeatsAreAlternativesToTheGPUEntry: the GPU chat entry and an NPU seat draw
// on the same RAM, so loading one evicts the other; the rendered set says so.
func TestRK3588TemplateSeatsAreAlternativesToTheGPUEntry(t *testing.T) {
	out := mustRender(t, readTmpl(t, rk3588Template), rk3588Params(rkllmSeat()))
	cfg := parseSwapConfig(t, out)
	if got, want := cfg.Matrix.Sets["interactive"], "e2b | rkllm"; got != want {
		t.Errorf("interactive set = %q, want %q", got, want)
	}
	// With no seats declared the set is the chat entry alone, and still names only vars.
	bare := mustRender(t, readTmpl(t, rk3588Template), rk3588Params())
	if got := parseSwapConfig(t, bare).Matrix.Sets["interactive"]; got != "e2b" {
		t.Errorf("a seat-less render's interactive set = %q, want e2b", got)
	}
}

// TestRK3588TemplateRendersLlamaBackedSeatsOnTheGPU: a tier that later adds a vision seat on
// the GPU gets it offloaded like every other GPU seat — the backend renders GPU flags — and
// it sits beside an NPU seat as one more alternative.
func TestRK3588TemplateRendersLlamaBackedSeatsOnTheGPU(t *testing.T) {
	npu := rkllmSeat()
	npu.VisionEncoder = "" // the llama vision seat owns vision_model here
	out := mustRender(t, readTmpl(t, rk3588Template), rk3588Params(visionSeat(), npu))
	blk := ownBlockOf(out, "gemma4-e4b-vision")
	if !strings.Contains(blk, "--n-gpu-layers 99 ") || !strings.Contains(blk, `env: ["${ld}"]`) {
		t.Errorf("a llama-backed seat on the rk3588 backend must offload to the GPU and carry the loader path:\n%s", blk)
	}
	if got := parseSwapConfig(t, out).Matrix.Sets["interactive"]; got != "e2b | vis | rkllm" {
		t.Errorf("interactive set = %q, want e2b | vis | rkllm", got)
	}
}
