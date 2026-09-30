package mediaseat

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// rkllmSeat is the shape the rockchip-rk3588 tier declares: one NPU seat that is a
// chat model AND a VLM (its own vision encoder), on the A55 cluster.
func rkllmSeat() Seat {
	return Seat{Kind: KindRKLLM, Name: "npu-seat", Aliases: []string{"npu-chat", "vision"},
		Model: "m.rkllm", VisionEncoder: "enc.rknn", CtxSize: 16384, CPUMask: "0x0f",
		Residency: Swappable, TTL: 300}
}

// TestRKLLMSeatPassesAndBindsVisionOnlyWithAnEncoder: the seat is a chat model first
// (model/triage_model are the tier's config_seed to name), and it writes vision_model
// only when it can actually read an image. tierseed refuses vision_model in a
// config_seed, so this derivation is the ONLY way the tier can bind it.
func TestRKLLMSeatPassesAndBindsVisionOnlyWithAnEncoder(t *testing.T) {
	s := rkllmSeat()
	if err := Validate([]Seat{s}, "tier"); err != nil {
		t.Fatal(err)
	}
	if got := Bindings([]Seat{s}); len(got) != 1 || got["vision_model"] != "npu-seat" {
		t.Errorf("a VLM rkllm seat binds vision_model = its name, got %v", got)
	}
	textOnly := s
	textOnly.VisionEncoder = ""
	if err := Validate([]Seat{textOnly}, "tier"); err != nil {
		t.Fatal(err)
	}
	if got := Bindings([]Seat{textOnly}); len(got) != 0 {
		t.Errorf("a text-only rkllm seat cannot answer an image question and must bind nothing, got %v", got)
	}
}

// TestRKLLMAndVisionCannotBothWriteVisionModel: the cap is per config KEY. A vision
// seat plus a VLM rkllm seat would leave vision_model decided by map order.
func TestRKLLMAndVisionCannotBothWriteVisionModel(t *testing.T) {
	err := Validate(append(good(), rkllmSeat()), "tier")
	if err == nil || !strings.Contains(err.Error(), "at most one") || !strings.Contains(err.Error(), `"vision_model"`) {
		t.Fatalf("want a refusal naming vision_model, got %v", err)
	}
	// A text-only rkllm seat writes nothing, so it coexists with a vision seat.
	textOnly := rkllmSeat()
	textOnly.VisionEncoder = ""
	if err := Validate(append(good(), textOnly), "tier"); err != nil {
		t.Fatalf("a text-only rkllm seat beside a vision seat is valid: %v", err)
	}
}

func TestRKLLMDefaults(t *testing.T) {
	bare := Seat{Kind: KindRKLLM, Name: "n", Model: "m.rkllm", CtxSize: 4096, Residency: Swappable}
	if got := bare.EffectiveBin(); got != "__RKNPU_HOME__/rkllm-serve.sh" {
		t.Errorf("default bin = %q", got)
	}
	if got := bare.EffectiveCPUMask(); got != "0x0f" {
		t.Errorf("default cpu_mask = %q, want the A55 cluster 0x0f", got)
	}
	own := bare
	own.Bin, own.CPUMask = "/opt/x/serve.sh", "0xf0"
	if own.EffectiveBin() != "/opt/x/serve.sh" || own.EffectiveCPUMask() != "0xf0" {
		t.Errorf("a seat's own bin/cpu_mask must win, got %q / %q", own.EffectiveBin(), own.EffectiveCPUMask())
	}
	// Only rkllm has a default launcher or a mask.
	if v := good()[0]; v.EffectiveBin() != "" || v.EffectiveCPUMask() != "" {
		t.Errorf("a vision seat has no default bin or mask, got %q / %q", v.EffectiveBin(), v.EffectiveCPUMask())
	}
}

// TestRKLLMCPUMaskNeedsThreeCPUs: the runtime REFUSES to start with fewer enabled
// CPUs than the SoC has NPU cores, so a two-bit mask is a seat that never comes up —
// and only says so at load time, on the box.
func TestRKLLMCPUMaskNeedsThreeCPUs(t *testing.T) {
	for _, tc := range []struct {
		mask string
		ok   bool
	}{
		{"", true}, // the default, 0x0f
		{"0x0f", true},
		{"0xf0", true},
		{"0xe0", true}, // A76 x3: exactly the floor
		{"0xff", true},
		{"0X0F", true},
		{"0x7", true},
		{"0x3", false},         // two CPUs
		{"0x80", false},        // one CPU
		{"0x0", false},         // none
		{"0xc0", false},        // A76 x2
		{"0f", false},          // no prefix
		{"0x", false},          // no digits
		{"0xzz", false},        // not hex
		{"15", false},          // decimal
		{"0x0f;id", false},     // the mask is rendered into a command line
		{"0x1ffffffff", false}, // wider than RKLLM's uint32
		{"-0x0f", false},
	} {
		s := rkllmSeat()
		s.CPUMask = tc.mask
		err := Validate([]Seat{s}, "tier")
		if tc.ok && err != nil {
			t.Errorf("cpu_mask %q should pass: %v", tc.mask, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "cpu_mask")) {
			t.Errorf("cpu_mask %q should be refused by name, got %v", tc.mask, err)
		}
	}
}

func TestRKLLMValidateRejects(t *testing.T) {
	zero := 0.0
	for _, tc := range []struct {
		name string
		mut  func(*Seat)
		want string
	}{
		{"no model", func(s *Seat) { s.Model = "" }, "no model file"},
		{"no ctx", func(s *Seat) { s.CtxSize = 0 }, "needs its own ctx_size"},
		{"no residency", func(s *Seat) { s.Residency = "" }, "is not swappable or resident"},
		{"mmproj", func(s *Seat) { s.MMProj = "p.gguf" }, "ignored on an rkllm seat"},
		{"gpu_env", func(s *Seat) { s.GPUEnv = []string{"GGML_VK_VISIBLE_DEVICES=0"} }, "ignored on an rkllm seat"},
		{"temp", func(s *Seat) { s.Temp = &zero }, "ignored on an rkllm seat"},
		{"image_max_tokens", func(s *Seat) { s.ImageMaxTokens = 512 }, "ignored on an rkllm seat"},
		{"lib_dir", func(s *Seat) { s.LibDir = "/x" }, "ignored on an rkllm seat"},
		{"home token in model", func(s *Seat) { s.Model = "__OFFLOAD_HOME__/m.rkllm" }, "may not carry __OFFLOAD_HOME__"},
		{"home token in encoder", func(s *Seat) { s.VisionEncoder = "__OFFLOAD_HOME__/e.rknn" }, "may not carry __OFFLOAD_HOME__"},
		{"rknpu token in model", func(s *Seat) { s.Model = "__RKNPU_HOME__/m.rkllm" }, "may not carry __RKNPU_HOME__"},
		{"literal .exe in the launcher", func(s *Seat) { s.Bin = "/x/serve.exe" }, "__EXE__"},
		{"unsafe name", func(s *Seat) { s.Name = "a b" }, "must match"},
	} {
		s := rkllmSeat()
		tc.mut(&s)
		err := Validate([]Seat{s}, "tier")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want a refusal containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

// TestRKLLMOnlyFieldsAreRefusedOnOtherKinds: a tier author who sets vision_encoder or
// cpu_mask on a llama-server seat believes it applies; it would be silently dropped.
func TestRKLLMOnlyFieldsAreRefusedOnOtherKinds(t *testing.T) {
	for name, mut := range map[string]func([]Seat) []Seat{
		"vision_encoder on a vision seat": func(s []Seat) []Seat { s[0].VisionEncoder = "e.rknn"; return s },
		"cpu_mask on a vision seat":       func(s []Seat) []Seat { s[0].CPUMask = "0x0f"; return s },
		"cpu_mask on an stt seat":         func(s []Seat) []Seat { s[1].CPUMask = "0x0f"; return s },
	} {
		err := Validate(mut(good()), "tier")
		if err == nil || !strings.Contains(err.Error(), "rkllm-only") {
			t.Errorf("%s: want an rkllm-only refusal, got %v", name, err)
		}
	}
}

// The kinds message names the whole closed set, rkllm included.
func TestUnknownKindMessageListsRKLLM(t *testing.T) {
	s := rkllmSeat()
	s.Kind = "npu"
	if err := Validate([]Seat{s}, "tier"); err == nil || !strings.Contains(err.Error(), "rkllm") {
		t.Fatalf("an unknown kind must name the valid set including rkllm, got %v", err)
	}
}

// repeat_penalty is the ONE sampling default an rkllm seat may carry: the runtime has no
// temperature/top_p/top_k seat knobs (those stay refused), but greedy decoding at the
// runtime's 1.0 loops on a small model's image answers, so the seat starts above it.
func TestRKLLMRepeatPenaltyIsAcceptedInRangeAndRefusedOutsideIt(t *testing.T) {
	ptr := func(v float64) *float64 { return &v }
	for _, v := range []float64{0.01, 1.0, 1.1, 10} {
		s := rkllmSeat()
		s.RepeatPenalty = ptr(v)
		if err := Validate([]Seat{s}, "tier"); err != nil {
			t.Errorf("repeat_penalty %v should pass: %v", v, err)
		}
	}
	for _, v := range []float64{0, 0.009, 10.01, -1, math.NaN(), math.Inf(1)} {
		s := rkllmSeat()
		s.RepeatPenalty = ptr(v)
		err := Validate([]Seat{s}, "tier")
		if err == nil || !strings.Contains(err.Error(), "repeat_penalty") {
			t.Errorf("repeat_penalty %v should be refused by name, got %v", v, err)
		}
	}
	s := rkllmSeat() // unset stays valid: no flag renders and the server's 1.0 stands
	if err := Validate([]Seat{s}, "tier"); err != nil {
		t.Fatal(err)
	}
}

// Every other sampling knob stays refused on an rkllm seat: allowing repeat_penalty must not
// reopen them.
func TestRKLLMStillRefusesTheOtherSamplingKnobs(t *testing.T) {
	v, n := 0.5, 20
	for name, mut := range map[string]func(*Seat){
		"temp":  func(s *Seat) { s.Temp = &v },
		"top_p": func(s *Seat) { s.TopP = &v },
		"top_k": func(s *Seat) { s.TopK = &n },
	} {
		s := rkllmSeat()
		mut(&s)
		if err := Validate([]Seat{s}, "tier"); err == nil || !strings.Contains(err.Error(), "ignored on an rkllm seat") {
			t.Errorf("%s on an rkllm seat must stay refused, got %v", name, err)
		}
	}
}

// A repeat_penalty on a llama-server or whisper seat is a knob the renderer would drop.
func TestRepeatPenaltyIsRefusedOnOtherKinds(t *testing.T) {
	v := 1.1
	for i, name := range []string{"vision", "stt"} {
		seats := good()
		seats[i].RepeatPenalty = &v
		err := Validate(seats, "tier")
		if err == nil || !strings.Contains(err.Error(), "rkllm-only") || !strings.Contains(err.Error(), "repeat_penalty") {
			t.Errorf("repeat_penalty on a %s seat: want an rkllm-only refusal, got %v", name, err)
		}
	}
}

// Seat.Tasks is the declared task allowlist. Only the vision subset is bound (vision_tasks),
// in canonical order, and a seat that declares none leaves the key out: today's "all three".
func TestSeatTasksBindTheVisionSubsetInCanonicalOrder(t *testing.T) {
	s := rkllmSeat()
	s.Tasks = []string{"ocr", "vqa"} // declared out of order on purpose
	if err := Validate([]Seat{s}, "tier"); err != nil {
		t.Fatal(err)
	}
	got := Bindings([]Seat{s})
	if want := []string{"vqa", "ocr"}; !reflect.DeepEqual(got["vision_tasks"], want) {
		t.Errorf("vision_tasks = %v, want %v (canonical order vqa, ocr, assess_image)", got["vision_tasks"], want)
	}
	if got["vision_model"] != "npu-seat" {
		t.Errorf("vision_model = %v, want the seat", got["vision_model"])
	}

	s.Tasks = []string{"assess_image", "extract", "vqa", "classify", "ocr"} // all five: text tasks are accepted, not bound
	if err := Validate([]Seat{s}, "tier"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"vqa", "ocr", "assess_image"}; !reflect.DeepEqual(Bindings([]Seat{s})["vision_tasks"], want) {
		t.Errorf("vision_tasks = %v, want the vision subset %v only", Bindings([]Seat{s})["vision_tasks"], want)
	}

	s.Tasks = nil // no declaration: no key, the node serves all three
	if _, has := Bindings([]Seat{s})["vision_tasks"]; has {
		t.Error("a seat that declares no tasks must not write vision_tasks")
	}

	// A vision (llama-server) seat may declare tasks too: the binding follows the seat that binds vision_model.
	v := good()[0]
	v.Tasks = []string{"vqa", "assess_image"}
	if err := Validate([]Seat{v}, "tier"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"vqa", "assess_image"}; !reflect.DeepEqual(Bindings([]Seat{v})["vision_tasks"], want) {
		t.Errorf("a vision seat's vision_tasks = %v, want %v", Bindings([]Seat{v})["vision_tasks"], want)
	}
}

func TestBoundKeysIncludeVisionTasks(t *testing.T) {
	found := false
	for _, k := range BoundKeys() {
		found = found || k == "vision_tasks"
	}
	if !found {
		t.Fatalf("BoundKeys %v must include vision_tasks so a config_seed cannot write it", BoundKeys())
	}
}

func TestSeatTasksValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seat  func() Seat
		tasks []string
		want  string // "" = must pass
	}{
		{"vqa+ocr on a VLM rkllm seat", rkllmSeat, []string{"vqa", "ocr"}, ""},
		{"text tasks beside a vision task", rkllmSeat, []string{"vqa", "classify", "extract"}, ""},
		{"unknown task", rkllmSeat, []string{"vqa", "caption"}, `unknown task "caption"`},
		{"duplicate task", rkllmSeat, []string{"vqa", "vqa"}, `"vqa" is listed twice`},
		{"vision task on a text-only rkllm seat", func() Seat { s := rkllmSeat(); s.VisionEncoder = ""; return s },
			[]string{"vqa"}, "needs a seat that reads images"},
		{"text tasks on a text-only rkllm seat", func() Seat { s := rkllmSeat(); s.VisionEncoder = ""; return s },
			[]string{"classify", "extract"}, ""},
		{"only text tasks on a seat that binds vision_model", rkllmSeat, []string{"classify"}, "name no vision task"},
		{"tasks on an stt seat", func() Seat { return good()[1] }, []string{"vqa"}, "ignored on a stt seat"},
		{"tasks on an ocr seat", ocrSeat, []string{"ocr"}, "ignored on a ocr seat"},
	} {
		s := tc.seat()
		s.Tasks = tc.tasks
		err := Validate([]Seat{s}, "tier")
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: should pass, got %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: want a refusal containing %q, got %v", tc.name, tc.want, err)
		}
	}
}
