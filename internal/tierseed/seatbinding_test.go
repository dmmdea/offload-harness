package tierseed

import (
	"github.com/dmmdea/offload-harness/internal/vllmseat"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/mediaseat"
)

func seatProfile() Profile {
	return Profile{
		Backend: "cuda",
		MediaSeats: []mediaseat.Seat{
			{Kind: mediaseat.KindVision, Name: "gemma4-e4b-vision", Model: "m.gguf",
				MMProj: "mmproj.gguf", CtxSize: 8192, Residency: mediaseat.Swappable},
			{Kind: mediaseat.KindSTT, Name: "whisper-stt", Model: "w.bin",
				Bin: "__OFFLOAD_HOME__/bin/whisper-server__EXE__", Residency: mediaseat.Swappable},
		},
	}
}

// TestSeatsProduceTheirOwnBindings: the seat and the config key routing to it come
// from ONE declaration, so the pair cannot drift. Before this, config.Default()
// bound stt_model to an alias no template defined, on every tier.
func TestSeatsProduceTheirOwnBindings(t *testing.T) {
	got, err := Resolve(seatProfile(), "ampere-6", Options{Home: "/srv/offload", GOOS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	if got["vision_model"] != "gemma4-e4b-vision" {
		t.Errorf("vision_model = %v", got["vision_model"])
	}
	if got["stt_model"] != "whisper-stt" {
		t.Errorf("stt_model = %v", got["stt_model"])
	}
}

// TestATierWithNoSeatsBindsNoMediaAliases: the honest default. A route with no
// seat must defer, not name something that was never rendered.
func TestATierWithNoSeatsBindsNoMediaAliases(t *testing.T) {
	got, err := Resolve(Profile{Backend: "cuda", ConfigSeed: map[string]any{"imagegen_steps": 4}}, "t", Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range mediaseat.BoundKeys() {
		if _, ok := got[k]; ok {
			t.Errorf("a seatless tier must not write %q", k)
		}
	}
}

// TestConfigSeedMayNotWriteASeatBinding: two writers is precisely how the binding
// and the seat it names drifted apart, so the seed is refused by name rather than
// silently losing to (or beating) the seat.
func TestConfigSeedMayNotWriteASeatBinding(t *testing.T) {
	p := seatProfile()
	p.ConfigSeed = map[string]any{"stt_model": "some-other-alias"}
	_, err := Resolve(p, "ampere-6", Options{Home: "/srv/offload"})
	if err == nil || !strings.Contains(err.Error(), "written by a media_seat") {
		t.Fatalf("a seed writing a seat binding must be refused, got %v", err)
	}
}

// TestAnInvalidSeatFailsResolution: validation runs where the seed is resolved, so
// an install cannot proceed past a malformed tier row.
func TestAnInvalidSeatFailsResolution(t *testing.T) {
	p := seatProfile()
	p.MediaSeats[0].MMProj = ""
	if _, err := Resolve(p, "ampere-6", Options{Home: "/srv/offload"}); err == nil ||
		!strings.Contains(err.Error(), "mmproj") {
		t.Fatalf("want a refusal naming the missing mmproj, got %v", err)
	}
}

// TestAnInvalidBoundLaneFailsResolution (ADR 0049 Amendment 3, review finding):
// `install seed` — the path that writes a box's config.json — resolves the vLLM
// seat's bindings without going through Artifacts, so the seat's own validation
// must run here too. A bound-lane value the harness config would refuse fails
// the resolution when the seat is ACTIVE, and the fallback path is untouched.
func TestAnInvalidBoundLaneFailsResolution(t *testing.T) {
	p := seatProfile()
	p.VLLMSeat = &vllmseat.Spec{ID: "seat-v", Unit: "seat-v", Port: 18797, Device: "0", ModelRepo: "hub/models--x", MaxModelLen: 4096,
		GPUMemoryUtilization: 0.9, TTLSeconds: 300, Fallback: "seat-cpp", AgentThinking: "sometimes"}
	if _, err := Resolve(p, "ampere-6", Options{Home: "/srv/offload", GOOS: "linux", VLLMSeatActive: true}); err == nil || !strings.Contains(err.Error(), "agent_thinking") {
		t.Fatalf("an active seat with a bad bound-lane value must fail resolution naming the field, got %v", err)
	}
	if _, err := Resolve(p, "ampere-6", Options{Home: "/srv/offload", GOOS: "linux"}); err != nil {
		t.Fatalf("the FALLBACK path binds the llama.cpp seat and must not validate the vLLM lane: %v", err)
	}
}

func rkllmTasksProfile(tasks ...string) Profile {
	return Profile{
		Backend: "rk3588",
		MediaSeats: []mediaseat.Seat{{Kind: mediaseat.KindRKLLM, Name: "npu-vlm", Model: "m.rkllm",
			VisionEncoder: "enc.rknn", CtxSize: 8192, Residency: mediaseat.Swappable, Tasks: tasks}},
	}
}

// TestSeatWritesVisionTasksBesideVisionModel: a seat that declares its vision tasks is the sole
// writer of the node's vision_tasks, like vision_model; a seat that declares none leaves the key
// out, which the node reads as "all three" (today's behaviour).
func TestSeatWritesVisionTasksBesideVisionModel(t *testing.T) {
	got, err := Resolve(rkllmTasksProfile("ocr", "vqa"), "rk", Options{Home: "/srv/offload", GOOS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	if got["vision_model"] != "npu-vlm" {
		t.Errorf("vision_model = %v", got["vision_model"])
	}
	if want := []string{"vqa", "ocr"}; !reflect.DeepEqual(got["vision_tasks"], want) {
		t.Errorf("vision_tasks = %v, want %v", got["vision_tasks"], want)
	}
	got, err = Resolve(rkllmTasksProfile(), "rk", Options{Home: "/srv/offload", GOOS: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	if _, has := got["vision_tasks"]; has {
		t.Errorf("a seat with no tasks must not write vision_tasks, got %v", got["vision_tasks"])
	}
}

// TestConfigSeedMayNotWriteVisionTasks: two writers of the vision binding is how the seat and
// its binding drifted apart, so vision_tasks is refused in a config_seed exactly like vision_model.
func TestConfigSeedMayNotWriteVisionTasks(t *testing.T) {
	p := rkllmTasksProfile("vqa", "ocr")
	p.ConfigSeed = map[string]any{"vision_tasks": []any{"vqa"}}
	_, err := Resolve(p, "rk", Options{Home: "/srv/offload"})
	if err == nil || !strings.Contains(err.Error(), `"vision_tasks" is written by a media_seat`) {
		t.Fatalf("a seed writing vision_tasks must be refused by name, got %v", err)
	}
	p = Profile{Backend: "cuda", ConfigSeed: map[string]any{"vision_tasks": []any{"vqa"}}}
	if _, err := Resolve(p, "t", Options{}); err == nil || !strings.Contains(err.Error(), "written by a media_seat") {
		t.Fatalf("a seatless tier may not seed vision_tasks either, got %v", err)
	}
}
