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

// TestRK3588TemplateServesNoModelOfItsOwnAndKeepsTheGPUConventions: the template's own
// contract, read from the raw file. llama.cpp on this board's GPU faults on its first compute
// submission (panthor job timeout, vk::DeviceLostError; measured 2026-09-30), so the template
// serves no llama.cpp entry at all and every model is a tier seat. The Vulkan conventions stay
// in its macros for the day a GPU entry measures clean.
func TestRK3588TemplateServesNoModelOfItsOwnAndKeepsTheGPUConventions(t *testing.T) {
	raw := readTmpl(t, rk3588Template)
	var doc struct {
		Macros map[string]string         `yaml:"macros"`
		Models map[string]map[string]any `yaml:"models"`
	}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Models) != 0 {
		ids := make([]string, 0, len(doc.Models))
		for id := range doc.Models {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		t.Errorf("the template serves %v of its own; its GPU entries are blocked until they measure clean on this board", ids)
	}
	if !anchorRe.MatchString(raw) {
		t.Fatal("a template with no model of its own must accept seats, or it renders nothing")
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

// TestRK3588TemplateSeatsAreAlternatives: every seat draws on the same RAM, so the rendered
// set holds them as alternatives, and a set made only of seats does not open on an operator
// (llama-swap rejects that expression).
func TestRK3588TemplateSeatsAreAlternatives(t *testing.T) {
	out := mustRender(t, readTmpl(t, rk3588Template), rk3588Params(rkllmSeat()))
	cfg := parseSwapConfig(t, out)
	if got, want := cfg.Matrix.Sets["interactive"], "rkllm"; got != want {
		t.Errorf("interactive set = %q, want %q", got, want)
	}
	for id, m := range cfg.Models {
		if m.TTL == nil || *m.TTL != 300 {
			t.Errorf("rendered seat %s ttl = %v, want 300", id, m.TTL)
		}
	}
	if len(cfg.Models) != 1 {
		t.Errorf("rendered models = %d, want the one NPU seat", len(cfg.Models))
	}
}

// TestRK3588TemplateRefusesARenderThatServesNothing: with no model of its own, a tier that
// declares no seat would render a config that serves nothing and still starts; the render
// refuses instead of shipping an empty node.
func TestRK3588TemplateRefusesARenderThatServesNothing(t *testing.T) {
	_, err := Render(readTmpl(t, rk3588Template), rk3588Params())
	if err == nil || !strings.Contains(err.Error(), "serves no model") {
		t.Fatalf("a seat-less render must be refused, got %v", err)
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
	if got := parseSwapConfig(t, out).Matrix.Sets["interactive"]; got != "vis | rkllm" {
		t.Errorf("interactive set = %q, want vis | rkllm", got)
	}
}
