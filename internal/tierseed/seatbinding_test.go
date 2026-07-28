package tierseed

import (
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
