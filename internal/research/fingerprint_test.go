package research

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// A realistic page: prose about ffmpeg retiming WITH the navigation chrome a
// fetched docs page carries (the words a boilerplate-heavy fingerprint would
// have matched against a phantom answer).
const retimePage = `Home Documentation Download Download Releases License Support GitHub Search Navigation Contact Privacy
FFmpeg retiming guide. Changing playback speed with ffmpeg means rewriting presentation timestamps: the setpts filter
rescales every video frame's timestamp (setpts=0.5*PTS doubles the speed), and the atempo filter stretches or
compresses the audio to keep it in sync with the retimed video. For factors outside atempo's 0.5–2.0 range, chain
several atempo filters. When the framerate must stay constant after retiming, add an fps filter after setpts so
duplicated or dropped frames land on a stable timeline. Slow motion works the same way with setpts=2.0*PTS and
atempo=0.5. Timestamps that drift after concatenation are fixed by resetting PTS with setpts=PTS-STARTPTS before
the retime step. The retime is lossless for timestamps but re-encodes audio, so keep the sample rate explicit.
Latest version 7.1 released; download the stable installation package from the website. Version history and
release notes are on the documentation page. Documentation | Download | Version | Release | License | Support`

func TestDocFingerprintRejectsPhantomAndAcceptsAbstractiveSummary(t *testing.T) {
	acc := DocFingerprint(retimePage)
	if len(acc) != 2 {
		t.Fatalf("want two fingerprint checks, got %v", acc)
	}
	for _, a := range acc {
		if !strings.HasPrefix(a, "regex:(?i)(?P<docanchor>") {
			t.Fatalf("fingerprint must be a tagged regex check: %q", a)
		}
		for _, boiler := range []string{"version", "download", "release", "documentation", "license", "support"} {
			if strings.Contains(a, boiler) {
				t.Fatalf("boilerplate %q leaked into the fingerprint: %q", boiler, a)
			}
		}
	}
	contract := core.AgentContract{Goal: "summarise", Acceptance: acc}
	phantom := core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion,
		Output: "The latest stable Go version is 1.26; download the release from go.dev and follow the installation documentation."}
	if fails := delegate.EvalAcceptance(contract, phantom); len(fails) == 0 {
		t.Fatalf("a digest of a different document must fail the fingerprint; acceptance=%v", acc)
	}
	honest := core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion,
		Output: "To change playback speed, rescale the video timestamps with setpts and stretch the audio with atempo so both stay in sync; chain atempo for large factors."}
	if fails := delegate.EvalAcceptance(contract, honest); len(fails) != 0 {
		t.Fatalf("an abstractive summary of the page must pass: %v (acceptance=%v)", fails, acc)
	}
}

func TestDocFingerprintSkipsThinPages(t *testing.T) {
	if got := DocFingerprint("Hello world. Short page."); got != nil {
		t.Fatalf("a page with fewer than 6 distinctive tokens has nothing to fingerprint, got %v", got)
	}
}

func TestBuildAppendsTheFingerprintToEveryUsablePage(t *testing.T) {
	specs, _ := Build(Request{Goal: "summarise the retime page"}, []Fetched{{URL: "https://x/retime", Text: retimePage}})
	if len(specs) != 1 {
		t.Fatalf("want 1 spec, got %d", len(specs))
	}
	tagged := 0
	for _, a := range specs[0].AgentContract.Acceptance {
		if strings.Contains(a, "(?P<docanchor>") {
			tagged++
		}
	}
	if tagged != 2 {
		t.Fatalf("every research contract carries both fingerprint halves, got %d in %v", tagged, specs[0].AgentContract.Acceptance)
	}
}
