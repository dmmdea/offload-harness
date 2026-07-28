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

func TestAValidSeatSetPasses(t *testing.T) {
	if err := Validate(good(), "tier"); err != nil {
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
		{"two vision seats", func(s []Seat) []Seat {
			s[1] = Seat{Kind: KindVision, Name: "other", Model: "m", MMProj: "p", CtxSize: 1, Residency: Resident}
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
