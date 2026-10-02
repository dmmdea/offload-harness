package mediaseat

import (
	"strings"
	"testing"
)

// extraVision is a registered extra: a second vision-class model the tier serves by
// alias that is NOT the route's binding (the first vision seat keeps vision_model).
func extraVision() Seat {
	return Seat{Kind: KindVision, Name: "extra-vl", Aliases: []string{"extra-alias"}, Model: "x.gguf",
		MMProj: "xp.gguf", CtxSize: 8192, Residency: Swappable, Extra: true}
}

// TestAnExtraSeatBindsNothing: BindingKey is the one question every consumer (Bindings,
// the one-writer check, the docs table) asks, so an extra answers "" for every kind it
// may carry, and Bindings of a set holding only extras writes nothing at all.
func TestAnExtraSeatBindsNothing(t *testing.T) {
	for _, s := range []Seat{
		extraVision(),
		{Kind: KindOCR, Name: "x-ocr", Model: "m", MMProj: "p", CtxSize: 1, Residency: Swappable, Extra: true},
		{Kind: KindSTT, Name: "x-stt", Model: "m", Bin: "b", Residency: Swappable, Extra: true},
	} {
		if k := s.BindingKey(); k != "" {
			t.Errorf("%s extra seat binds %q, want no key", s.Kind, k)
		}
		if b := Bindings([]Seat{s}); len(b) != 0 {
			t.Errorf("%s extra seat alone writes %v, want an empty fragment", s.Kind, b)
		}
	}
	// And the same seat without the flag still binds (the flag is the only difference).
	s := extraVision()
	s.Extra = false
	if s.BindingKey() != "vision_model" {
		t.Errorf("a non-extra vision seat binds %q, want vision_model", s.BindingKey())
	}
}

// TestTwoVisionSeatsOneExtraPass: the one-writer cap is per bound key, and an extra is
// not a writer. It must hold in both slice orders, because the binding has to come from
// the non-extra seat whichever way the tier lists them.
func TestTwoVisionSeatsOneExtraPass(t *testing.T) {
	primary := good()[0]
	for name, seats := range map[string][]Seat{
		"extra after":  {primary, extraVision()},
		"extra before": {extraVision(), primary},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(seats, "tier"); err != nil {
				t.Fatal(err)
			}
			b := Bindings(seats)
			if b["vision_model"] != "vlm-seat" {
				t.Errorf("vision_model = %v, want vlm-seat (the non-extra seat)", b["vision_model"])
			}
		})
	}
	// Two extras beside one primary: still one writer.
	two := []Seat{primary, extraVision(), extraVision()}
	two[2].Name, two[2].Aliases = "extra-vl-2", []string{"extra-alias-2"}
	if err := Validate(two, "tier"); err != nil {
		t.Fatal(err)
	}
	// The cap still bites for seats that are not extras.
	twoPrimary := []Seat{primary, extraVision()}
	twoPrimary[1].Extra = false
	if err := Validate(twoPrimary, "tier"); err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Errorf("two non-extra vision seats must still be refused, got: %v", err)
	}
}

// TestAnExtraSeatIsStillValidatedLikeAnyOther: the flag removes the binding, nothing else.
// An extra without an mmproj would load as a text model that answers image questions blind.
func TestAnExtraSeatIsStillValidatedLikeAnyOther(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Seat)
		want string
	}{
		{"no mmproj", func(s *Seat) { s.MMProj = "" }, "needs an mmproj"},
		{"no ctx", func(s *Seat) { s.CtxSize = 0 }, "needs its own ctx_size"},
		{"no model", func(s *Seat) { s.Model = "" }, "no model file"},
		{"alias collides with the primary", func(s *Seat) { s.Aliases = []string{"vlm-seat"} }, "collides"},
		{"name collides with the primary", func(s *Seat) { s.Name = "vlm-seat" }, "declared twice"},
		{"bad residency", func(s *Seat) { s.Residency = "heavy" }, "is not swappable or resident"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := extraVision()
			tc.mut(&x)
			err := Validate([]Seat{good()[0], x}, "tier")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want a refusal naming %q, got: %v", tc.want, err)
			}
		})
	}
}

// TestAnExtraSeatMayNotDeclareTasksOrBeRKLLM: tasks are read only through the vision binding
// this seat does not write, and an rkllm seat writes unconstrained_seats / text_tasks that no
// flag here suppresses, so "extra" would be a promise the renderer breaks.
func TestAnExtraSeatMayNotDeclareTasksOrBeRKLLM(t *testing.T) {
	x := extraVision()
	x.Tasks = []string{TaskVQA}
	if err := Validate([]Seat{good()[0], x}, "tier"); err == nil || !strings.Contains(err.Error(), "an extra seat binds no route") {
		t.Errorf("an extra vision seat declaring tasks must be refused as an extra: nothing reads them, got: %v", err)
	}
	r := Seat{Kind: KindRKLLM, Name: "x-rk", Model: "m.rkllm", CtxSize: 4096, Residency: Resident, Extra: true}
	err := Validate([]Seat{r}, "tier")
	if err == nil || !strings.Contains(err.Error(), "extra") {
		t.Errorf("an extra rkllm seat must be refused by name, got: %v", err)
	}
}
