package servingtmpl

import (
	"os"
	"path/filepath"
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

// TestSeatsLandInBothTheModelsMapAndTheMatrix is the whole contract. llama-swap
// REJECTS a set naming an unknown var, and a model that reaches the models map but
// no set is one the solver has no valid combination for. A seat that arrives in only
// one of the two places is a broken node either way.
func TestSeatsLandInBothTheModelsMapAndTheMatrix(t *testing.T) {
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
		"    vis: gemma4-e4b-vision", // a matrix var, because a set may name only vars
		"    stt: whisper-stt",
		// Swappable seats are ALTERNATIVES: one big seat on the card at a time.
		`interactive: "+residents & (e4b | e2b | m26 | vis | stt)"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered config missing %q:\n%s", want, got)
		}
	}
	// The resident set must be untouched: it is the memory stack, and a swappable seat
	// joining it would make the stack part of a mutually-exclusive choice.
	if !strings.Contains(got, `residents: "emb & rer"`) {
		t.Errorf("a swappable seat must not join the resident set:\n%s", got)
	}
}

// TestAResidentSeatIsConjoinedNotAlternated: the operator is the whole difference
// between "may be co-resident" and "instead of". A resident seat ANDed into the
// resident set stays loaded; alternated, it would displace the memory stack.
func TestAResidentSeatIsConjoinedNotAlternated(t *testing.T) {
	s := visionSeat()
	s.Residency = mediaseat.Resident
	got, err := Render(linuxCUDA(t), seatParams(s))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `residents: "emb & rer & vis"`) {
		t.Errorf("a resident seat must be conjoined into the resident set:\n%s", got)
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

// TestVisionSeatCarriesItsVramBound: --image-max-tokens is not a quality knob, it is
// what keeps one large screenshot from pushing the seat past an 8 GB card. If it
// silently did not render, the tier would look configured and OOM in use.
func TestVisionSeatCarriesItsVramBound(t *testing.T) {
	s := visionSeat()
	s.ImageMaxTokens = 1024
	s.NoContextShift = true
	got, err := Render(linuxCUDA(t), seatParams(s))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "--image-max-tokens 1024 --no-context-shift") {
		t.Errorf("vision seat lost its VRAM bound:\n%s", got)
	}
	// And a tier that does not ask for one must not get the flag at all.
	plain, err := Render(linuxCUDA(t), seatParams(visionSeat()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "--image-max-tokens") {
		t.Error("an unset image_max_tokens must not render a flag")
	}
}

// TestSttSeatCanTurnFlashAttentionOff: whisper.cpp has flash attention default-ON
// since v1.8.0 and it DEGRADES non-English and noisy audio, so a tier serving Spanish
// asks for -nfa. Rendering it as a no-op would quietly cost accuracy.
func TestSttSeatCanTurnFlashAttentionOff(t *testing.T) {
	s := sttSeat()
	s.NoFlashAttn = true
	got, err := Render(linuxCUDA(t), seatParams(s))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "-nfa") {
		t.Errorf("stt seat did not render -nfa:\n%s", got)
	}
	if strings.Contains(mustRender(t, linuxCUDA(t), seatParams(sttSeat())), "-nfa") {
		t.Error("an stt seat that did not ask for -nfa must not get it")
	}
}

func mustRender(t *testing.T, tmpl string, p Params) string {
	t.Helper()
	got, err := Render(tmpl, p)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestAnAllResidentTemplateRefusesASwappableSeat: those templates exist because the
// tier's premise is that nothing swaps. Quietly reshaping a swappable seat into a
// resident one would give the operator a topology they never asked for on the tier
// where residency is the whole point.
func TestAnAllResidentTemplateRefusesASwappableSeat(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "setup", "templates", "llama-swap.win-cuda-resident.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p := seatParams(visionSeat()) // swappable by default
	p.GOOS = "windows"
	_, err = Render(string(b), p)
	if err == nil || !strings.Contains(err.Error(), "does not place") {
		t.Fatalf("an all-resident template must refuse a swappable seat by name, got %v", err)
	}
	if !strings.Contains(err.Error(), "resident") {
		t.Errorf("the refusal must name the role it DOES place, got: %v", err)
	}
}

// TestEveryTemplateCanPlaceSeats is the first-class invariant made checkable: a tier
// is a HARDWARE class, so a seat it declares must render on every OS that tier can be
// installed on. Until this held, `install.ps1` refused every seat-declaring tier and
// no further tier could declare one.
func TestEveryTemplateCanPlaceSeats(t *testing.T) {
	dir := filepath.Join("..", "..", "setup", "templates")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "llama-swap.") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !SupportsSeats(string(b)) {
			t.Errorf("%s declares no `# offload-seats:` directive, so any tier rendering into it "+
				"loses its media seats by accident of OS", e.Name())
		}
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

// TestACommentAfterTheVarsBlockDoesNotDisplaceTheInsert. The shipped templates carry
// prose comments between the vars and the next key, and one contains a colon
// ("# keep: make it maximally expensive to stop"). Anchoring the insert on "where the
// block ends" put the new var in the MIDDLE of that sentence — observed, not imagined.
// The insert anchors on the last var line instead.
func TestACommentAfterTheVarsBlockDoesNotDisplaceTheInsert(t *testing.T) {
	tmpl := "# offload-seats: swappable\nmodels:\n  offload-e4b:\n    cmd: x\n" +
		"matrix:\n  vars:\n    e4b: offload-e4b\n" +
		"  # note: this comment has a colon\n  # and spans two lines\n" +
		"  sets:\n    interactive: \"(e4b__SEATS_SWAPPABLE__)\"\n"
	got, err := Render(tmpl, seatParams(visionSeat()))
	if err != nil {
		t.Fatalf("a commented vars block must not break placement: %v", err)
	}
	if !strings.Contains(got, "    e4b: offload-e4b\n    vis: gemma4-e4b-vision\n  # note:") {
		t.Errorf("the new var did not land directly after the last existing one:\n%s", got)
	}
	if !strings.Contains(got, `interactive: "(e4b | vis)"`) {
		t.Errorf("seat not joined into the set:\n%s", got)
	}
}

// TestTwoDirectivesAreRefused: first-match-wins would silently pick one answer to
// "where do seats go" while the author believed the other.
func TestTwoDirectivesAreRefused(t *testing.T) {
	tmpl := "# offload-seats: swappable\n# offload-seats: resident\nmodels:\n  a:\n    cmd: x\n" +
		"matrix:\n  vars:\n    a: a\n  sets:\n    s: \"a__SEATS_SWAPPABLE__\"\n"
	if _, err := Render(tmpl, seatParams(visionSeat())); err == nil ||
		!strings.Contains(err.Error(), "directives") {
		t.Fatalf("two directives must be refused, got %v", err)
	}
}

// TestDroppingATrailing26BKeepsTheSectionsBelowIt. The block-extent heuristic ended
// only at a two-space key, so a 26B declared LAST swallowed `matrix:` and the
// `# offload-seats:` directive — promoting a matrix key into the models map.
func TestDroppingATrailing26BKeepsTheSectionsBelowIt(t *testing.T) {
	tmpl := "models:\n  offload-e4b:\n    cmd: x\n  gemma4-26b-a4b:\n    cmd: y\n" +
		"# offload-seats: swappable\nmatrix:\n  vars:\n    e4b: offload-e4b\n    m26: gemma4-26b-a4b\n" +
		"  sets:\n    s: \"e4b__M26_ALT____SEATS_SWAPPABLE__\"\n"
	p := params()
	p.Include26B = false
	p.MoE26B = ""
	got, err := Render(tmpl, p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "matrix:") {
		t.Errorf("dropping a trailing 26B deleted the matrix section:\n%s", got)
	}
	if !strings.Contains(got, "# offload-seats:") {
		t.Errorf("dropping a trailing 26B deleted the seat directive:\n%s", got)
	}
	if strings.Contains(got, "m26") {
		t.Errorf("the 26B's var survived:\n%s", got)
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
	if !strings.Contains(got, `interactive: "+residents & (e4b | e2b | vis | stt)"`) {
		t.Errorf("set expression wrong after dropping the 26B and adding seats:\n%s", got)
	}
}
