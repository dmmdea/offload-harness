package servingtmpl

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/mediaseat"
)

func visionSeat() mediaseat.Seat {
	return mediaseat.Seat{
		Kind: mediaseat.KindVision, Name: "gemma4-e4b-vision",
		Aliases: []string{"vision", "vlm"},
		Model:   "gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf", MMProj: "mmproj-F16.gguf",
		CtxSize: 8192, Residency: mediaseat.Swappable, TTL: 300,
	}
}

func sttSeat() mediaseat.Seat {
	return mediaseat.Seat{
		Kind: mediaseat.KindSTT, Name: "whisper-stt",
		Aliases: []string{"stt", "whisper"},
		Bin:     "__OFFLOAD_HOME__/build/whisper.cpp/build/bin/whisper-server__EXE__",
		LibDir:  "__OFFLOAD_HOME__/build/whisper.cpp/build/bin",
		Model:   "ggml-large-v3-turbo-q5_0.bin", VADModel: "ggml-silero-v5.1.2.bin",
		Residency: mediaseat.Swappable, TTL: 300,
	}
}

func seatParams(seats ...mediaseat.Seat) Params {
	p := params()
	p.Seats = seats
	p.Home = "/srv/offload"
	p.GOOS = "linux"
	return p
}

// TestSeatsLandInBothTheModelsMapAndTheGroup is the whole contract. llama-swap
// REJECTS a config whose group names a model it cannot find, and a model in no
// group silently joins the implicit default group — which swaps and evicts. So
// a seat that reaches only one of the two places is a broken node either way.
func TestSeatsLandInBothTheModelsMapAndTheGroup(t *testing.T) {
	got, err := Render(linuxCUDA(t), seatParams(visionSeat(), sttSeat()))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  gemma4-e4b-vision:", "  whisper-stt:",
		"aliases: [vision, vlm]", "aliases: [stt, whisper]",
		"--mmproj /srv/offload/models/mmproj-F16.gguf",
		"--ctx-size 8192", // the vision seat's OWN window, not the chat tier's 32768
		"/srv/offload/build/whisper.cpp/build/bin/whisper-server",
		"--vad --vad-model /srv/offload/models/ggml-silero-v5.1.2.bin",
		"members: [offload-e4b, gemma4-e2b, gemma4-26b-a4b, gemma4-e4b-vision, whisper-stt]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered config missing %q:\n%s", want, got)
		}
	}
	// The support tier must be untouched: it is swap:false precisely so the small
	// always-on models survive whatever heavy seat is loaded.
	if !strings.Contains(got, "members: [embeddinggemma, bge-reranker-v2-m3]") {
		t.Error("a swappable seat must not be added to the resident group")
	}
}

// TestWhisperCarriesItsOwnLoaderPath: whisper-server is a SEPARATE self-built
// binary from llama-server, so reusing the template's ${ld} macro would point it
// at the wrong build dir and it would die at exec with a loader error that reads
// nothing like a config problem.
func TestWhisperCarriesItsOwnLoaderPath(t *testing.T) {
	got, err := Render(linuxCUDA(t), seatParams(sttSeat()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `env: ["LD_LIBRARY_PATH=/srv/offload/build/whisper.cpp/build/bin:${LD_LIBRARY_PATH:-}"]`) {
		t.Errorf("the stt seat must carry its OWN LD_LIBRARY_PATH, not the llama macro:\n%s", got)
	}
}

// TestWindowsSeatGetsNoLoaderPathAndAnExeSuffix: the same tier row must render on
// both platforms — that is what __EXE__ is for, and a Windows build is
// self-contained so the POSIX loader path must not appear.
func TestWindowsSeatGetsNoLoaderPathAndAnExeSuffix(t *testing.T) {
	p := seatParams(sttSeat())
	p.GOOS = "windows"
	// Rendered against the Linux template on purpose: this asserts the SEAT's OS
	// handling in isolation, independent of which template carries the directive.
	got, err := Render(linuxCUDA(t), p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "LD_LIBRARY_PATH=/srv/offload/build/whisper.cpp") {
		t.Error("a Windows seat must not carry a POSIX loader path")
	}
	if !strings.Contains(got, "whisper-server.exe") {
		t.Errorf("__EXE__ did not resolve for the target platform:\n%s", got)
	}
}

// TestSeatsAreRefusedByNameWhenTheTemplateCannotPlaceThem. Silently dropping them
// is the exact failure this workstream exists to end: a node that renders clean,
// reports success, and has quietly lost a capability because of its OS.
func TestSeatsAreRefusedByNameWhenTheTemplateCannotPlaceThem(t *testing.T) {
	bare := "models:\n  offload-e4b:\n    cmd: x\ngroups:\n  g:\n    members: [offload-e4b]\n"
	_, err := Render(bare, seatParams(visionSeat()))
	if err == nil {
		t.Fatal("a template with no `# offload-seats:` directive must refuse, not drop the seats")
	}
	for _, want := range []string{"offload-seats", "gemma4-e4b-vision"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q so the gap is actionable, got: %v", want, err)
		}
	}
}

// TestUnmappedResidencyIsRefusedWithTheRolesThatExist: the error has to say what
// the template DOES offer, or the author is left guessing at a vocabulary that is
// per-template by design.
func TestUnmappedResidencyIsRefusedWithTheRolesThatExist(t *testing.T) {
	s := visionSeat()
	s.Residency = "persistent"
	_, err := Render(linuxCUDA(t), seatParams(s))
	if err == nil {
		t.Fatal("an unmapped residency role must be refused")
	}
	for _, want := range []string{"persistent", "resident", "swappable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should name %q, got: %v", want, err)
		}
	}
}

// TestASeatMayNotRedeclareATemplateSeat: the template owns the text tier. A tier
// silently overriding one would produce two blocks with the same key, and YAML
// keeps only one of them — a coin flip over which command actually runs.
func TestASeatMayNotRedeclareATemplateSeat(t *testing.T) {
	s := visionSeat()
	s.Name = "offload-e4b"
	if _, err := Render(linuxCUDA(t), seatParams(s)); err == nil ||
		!strings.Contains(err.Error(), "already defined") {
		t.Fatalf("redeclaring a template-owned seat must be refused, got %v", err)
	}
}

// TestAHomelessSeatPathIsRefusedRatherThanRenderedEmpty: substituting "" for
// __OFFLOAD_HOME__ would emit `cmd: /build/whisper.cpp/...`, an absolute path on
// the wrong root that fails at exec.
func TestAHomelessSeatPathIsRefusedRatherThanRenderedEmpty(t *testing.T) {
	p := seatParams(sttSeat())
	p.Home = ""
	if _, err := Render(linuxCUDA(t), p); err == nil || !strings.Contains(err.Error(), "install home") {
		t.Fatalf("a seat path under __OFFLOAD_HOME__ with no home must be refused, got %v", err)
	}
}

// TestNoSeatsChangesNothing: the overwhelmingly common case. Every tier that
// declares no seats must render exactly what it rendered before seats existed —
// this slice is additive or it is a fleet-wide regression.
func TestNoSeatsChangesNothing(t *testing.T) {
	withNil, err := Render(linuxCUDA(t), params())
	if err != nil {
		t.Fatal(err)
	}
	empty := params()
	empty.Seats = []mediaseat.Seat{}
	withEmpty, err := Render(linuxCUDA(t), empty)
	if err != nil {
		t.Fatal(err)
	}
	if withNil != withEmpty {
		t.Error("nil and empty seat lists must render identically")
	}
	for _, unwanted := range []string{"whisper", "mmproj", "vision"} {
		if strings.Contains(strings.ToLower(withNil), unwanted) {
			t.Errorf("a seatless render leaked %q into the config", unwanted)
		}
	}
}

// TestSeatRenderingSurvivesTheDropped26B: a tier that drops the 26B AND declares
// seats exercises both structural edits in one pass — and ampere-6, the first
// tier to declare seats, is exactly that tier.
func TestSeatRenderingSurvivesTheDropped26B(t *testing.T) {
	p := seatParams(visionSeat(), sttSeat())
	p.Include26B = false
	p.MoE26B = ""
	got, err := Render(linuxCUDA(t), p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "gemma4-26b-a4b") {
		t.Error("the 26B survived alongside the seats")
	}
	if !strings.Contains(got, "members: [offload-e4b, gemma4-e2b, gemma4-e4b-vision, whisper-stt]") {
		t.Errorf("members list wrong after dropping the 26B and adding seats:\n%s", got)
	}
}
