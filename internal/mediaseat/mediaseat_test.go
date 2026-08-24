package mediaseat

import (
	"strings"
	"testing"
)

func good() []Seat {
	return []Seat{
		{Kind: KindVision, Name: "vlm-seat", Model: "m.gguf", MMProj: "mmproj.gguf", CtxSize: 8192, Residency: Swappable},
		{Kind: KindSTT, Name: "whisper-stt", Model: "w.bin", Bin: "__OFFLOAD_HOME__/bin/whisper-server__EXE__", Residency: Swappable},
	}
}

// ocrSeat is the PaddleOCR-VL shape: llama-server VLM seat + shipped chat
// template + the vendor-required temp 0.
func ocrSeat() Seat {
	zero := 0.0
	return Seat{Kind: KindOCR, Name: "ocr-seat", Model: "p.gguf", MMProj: "pm.gguf",
		ChatTemplate: "p-template.jinja", Temp: &zero, CtxSize: 4096, Residency: Swappable}
}

// TestOCRSeatPassesAndBinds: the ocr kind is a first-class seat — valid alongside
// vision+stt, and it binds ocr_model without touching the other keys.
func TestOCRSeatPassesAndBinds(t *testing.T) {
	s := append(good(), ocrSeat())
	if err := Validate(s, "tier"); err != nil {
		t.Fatal(err)
	}
	b := Bindings(s)
	if b["ocr_model"] != "ocr-seat" || b["vision_model"] != "vlm-seat" || b["stt_model"] != "whisper-stt" {
		t.Errorf("bindings = %v", b)
	}
}

func TestAValidSeatSetPasses(t *testing.T) {
	if err := Validate(good(), "tier"); err != nil {
		t.Fatal(err)
	}
}

// TestPerSeatKnobsAreAccepted: the device pin and the Vulkan mmproj mitigation are
// valid on the kinds they apply to. gpu_env is any kind (a device pin); no_mmproj_offload
// is vision-only (already refused on stt in TestValidateRejects).
func TestPerSeatKnobsAreAccepted(t *testing.T) {
	s := good()
	s[0].NoMmprojOffload = true // vision: keep CLIP on CPU
	s[0].GPUEnv = []string{"GGML_VK_VISIBLE_DEVICES=0"}
	s[1].GPUEnv = []string{"CUDA_VISIBLE_DEVICES=1"} // stt: pin a card
	if err := Validate(s, "tier"); err != nil {
		t.Fatal(err)
	}
}

// TestBindingsAreDerivedFromSeats: the point of the package. A tier that serves a
// capability gets the binding; a tier that does not gets nothing, and the route
// honestly defers instead of naming a seat that was never rendered.
func TestBindingsAreDerivedFromSeats(t *testing.T) {
	b := Bindings(good())
	if b["vision_model"] != "vlm-seat" || b["stt_model"] != "whisper-stt" {
		t.Errorf("bindings = %v", b)
	}
	if len(Bindings(nil)) != 0 {
		t.Error("a tier with no seats must bind nothing — an unset route is honest, a phantom one is not")
	}
}

func TestValidateRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func([]Seat) []Seat
		want string
	}{
		{"unknown kind", func(s []Seat) []Seat { s[0].Kind = "music"; return s }, "unknown kind"},
		{"no name", func(s []Seat) []Seat { s[0].Name = ""; return s }, "no name"},
		{"no model", func(s []Seat) []Seat { s[0].Model = ""; return s }, "no model file"},
		{"bad residency", func(s []Seat) []Seat { s[0].Residency = "heavy"; return s }, "is not swappable or resident"},
		{"vision without mmproj", func(s []Seat) []Seat { s[0].MMProj = ""; return s }, "needs an mmproj"},
		{"vision without ctx", func(s []Seat) []Seat { s[0].CtxSize = 0; return s }, "needs its own ctx_size"},
		{"stt without bin", func(s []Seat) []Seat { s[1].Bin = ""; return s }, "needs a bin"},
		{"literal .exe", func(s []Seat) []Seat { s[1].Bin = "/bin/whisper-server.exe"; return s }, "__EXE__"},
		{"duplicate name", func(s []Seat) []Seat { s[1].Name = s[0].Name; return s }, "declared twice"},
		{"alias collides", func(s []Seat) []Seat { s[1].Aliases = []string{"vlm-seat"}; return s }, "collides"},
		// A name becomes a YAML key AND an inline flow-sequence member, so these are
		// not cosmetic: "a,b" splits into a member naming no model (llama-swap refuses
		// the config at startup) and "a:b" makes the document malformed.
		{"comma in name", func(s []Seat) []Seat { s[0].Name = "a,b"; return s }, "must match"},
		{"colon in name", func(s []Seat) []Seat { s[0].Name = "a:b"; return s }, "must match"},
		{"bracket in name", func(s []Seat) []Seat { s[0].Name = "a]b"; return s }, "must match"},
		{"bad alias", func(s []Seat) []Seat { s[0].Aliases = []string{"a,b"}; return s }, "must match"},
		{"home token in model", func(s []Seat) []Seat { s[0].Model = "__OFFLOAD_HOME__/m.gguf"; return s },
			"may not carry __OFFLOAD_HOME__"},
		{"vision with bin", func(s []Seat) []Seat { s[0].Bin = "/x/llama-server"; return s }, "always"},
		{"stt with mmproj", func(s []Seat) []Seat { s[1].MMProj = "p.gguf"; return s }, "vision/ocr-only"},
		// Kind-mismatched knobs are refused rather than silently ignored: a tier author
		// who sets one believes it applies.
		{"stt with image_max_tokens", func(s []Seat) []Seat { s[1].ImageMaxTokens = 512; return s }, "vision/ocr-only"},
		{"stt with no_context_shift", func(s []Seat) []Seat { s[1].NoContextShift = true; return s }, "vision/ocr-only"},
		{"stt with no_mmproj_offload", func(s []Seat) []Seat { s[1].NoMmprojOffload = true; return s }, "vision/ocr-only"},
		{"vision with no_flash_attn", func(s []Seat) []Seat { s[0].NoFlashAttn = true; return s }, "whisper.cpp flag"},
		{"two vision seats", func(s []Seat) []Seat {
			s[1] = Seat{Kind: KindVision, Name: "other", Model: "m", MMProj: "p", CtxSize: 1, Residency: Resident}
			return s
		}, "at most one"},
		// OCR-kind refusals mirror vision's (same llama-server seat class) plus its own knobs.
		{"ocr without mmproj", func(s []Seat) []Seat { s[0] = ocrSeat(); s[0].MMProj = ""; return s }, "needs an mmproj"},
		{"ocr without ctx", func(s []Seat) []Seat { s[0] = ocrSeat(); s[0].CtxSize = 0; return s }, "needs its own ctx_size"},
		{"ocr with bin", func(s []Seat) []Seat { s[0] = ocrSeat(); s[0].Bin = "/x/llama-server"; return s }, "always"},
		{"stt with chat_template", func(s []Seat) []Seat { s[1].ChatTemplate = "t.jinja"; return s }, "vision/ocr only"},
		{"stt with temp", func(s []Seat) []Seat { z := 0.0; s[1].Temp = &z; return s }, "vision/ocr only"},
		{"home token in chat_template", func(s []Seat) []Seat {
			s[0] = ocrSeat()
			s[0].ChatTemplate = "__OFFLOAD_HOME__/t.jinja"
			return s
		}, "may not carry __OFFLOAD_HOME__"},
		{"temp out of range", func(s []Seat) []Seat { s[0] = ocrSeat(); v := 3.0; s[0].Temp = &v; return s }, "outside [0, 2]"},
		{"two ocr seats", func(s []Seat) []Seat {
			s[0] = ocrSeat()
			s[1] = Seat{Kind: KindOCR, Name: "other-ocr", Model: "m", MMProj: "p", CtxSize: 1, Residency: Swappable}
			return s
		}, "at most one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.mut(good()), "tier")
			if err == nil {
				t.Fatalf("want a refusal naming %q, got none", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal should say %q, got: %v", tc.want, err)
			}
		})
	}
}
