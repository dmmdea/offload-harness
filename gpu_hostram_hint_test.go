package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/hostneed"
)

// F71. A `gpu reserve` whose wrapped command is not a render helper the estimator knows (a driver script that
// runs several of them, a Python wrapper) declares the MEDIA CLASS DEFAULT: the largest ComfyUI family the box
// binds, sized as if it streamed from RAM. The declaring line said "class default" inside its parenthesis and
// nothing else, so a caller whose weights sit on the cards declared 46.8 GiB, waited a queue cycle on a figure
// nobody had stated, and could not tell from the line that the fix was --ram. The declaring line is unchanged;
// a SECOND line says why the figure is the class default and that --ram states the real one.

// classDefaultBox is a box that binds one ComfyUI family (Krea 2) and whose model tree holds none of its files,
// so every machine sizes it by the documented family sizes (24.5 + 8.3 = 32.8 GiB): a media lease with nothing
// it can size declares that figure, the class default.
func classDefaultBox(t *testing.T) config.Config {
	t.Helper()
	return config.Config{ComfyDir: t.TempDir(), ImageGenScript: "render/comfy-generate.mjs", ImageGenFamily: "krea2", ImageGenCkpt: "krea2_turbo_bf16.safetensors"}
}

// reserveSays resolves the need the way the verb does on a box that has not enabled card-scoped leases (no
// card table is read, so the card is unknown) and returns what it printed, line by line, with the need it
// returned. No output is nil.
func reserveSays(t *testing.T, ramGiven bool, ramGiB float64, class gpulease.Class, args []string, cfg config.Config) ([]string, hostneed.Need) {
	t.Helper()
	var out bytes.Buffer
	need := resolveReserveHostRAM(ramGiven, ramGiB, class, args, nil, cfg, false, &out)
	if out.Len() == 0 {
		return nil, need
	}
	text := out.String()
	if !strings.HasSuffix(text, "\n") {
		t.Fatalf("every line the verb prints ends with a newline, got %q", text)
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n"), need
}

// wantDeclaringFirst pins the declaring line as it always was: first, the prefix a script greps, the figure's own
// sentence, and the only line that says "declaring".
func wantDeclaringFirst(t *testing.T, lines []string, need hostneed.Need) {
	t.Helper()
	if len(lines) == 0 || lines[0] != "gpu reserve: declaring "+need.String() {
		t.Fatalf("the declaring line comes first and is unchanged (%q), got %q", "gpu reserve: declaring "+need.String(), lines)
	}
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "declaring") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("a script that greps `declaring` must still see exactly one line, saw %d in %q", n, lines)
	}
}

// The reason phrases, one per way the class default is reached. Each hint carries its own and none of the others.
const (
	whyUnrecognised = "not a recognised render helper"
	whyGraph        = "runs its own --graph"
	whyUnsized      = "could not be sized"
	whyNoCommand    = "no command is wrapped"
)

var allWhys = []string{whyUnrecognised, whyGraph, whyUnsized, whyNoCommand}

// wantHint checks the second line: it says the figure is the class default, gives ITS reason and none of the
// other three, and tells the caller that --ram states the real figure (0 when the weights sit on the cards).
func wantHint(t *testing.T, name string, lines []string, why string, extra ...string) {
	t.Helper()
	if len(lines) != 2 {
		t.Fatalf("%s: want the declaring line and exactly one line of why, got %d line(s): %q", name, len(lines), lines)
	}
	hint := lines[1]
	if !strings.HasPrefix(hint, "gpu reserve: ") {
		t.Errorf("%s: the hint carries the verb's prefix, got %q", name, hint)
	}
	for _, want := range append([]string{why, "class default", "--ram <GiB>", "--ram 0"}, extra...) {
		if !strings.Contains(hint, want) {
			t.Errorf("%s: the hint must say %q, got %q", name, want, hint)
		}
	}
	for _, other := range allWhys {
		if other != why && strings.Contains(hint, other) {
			t.Errorf("%s: the hint gives the wrong reason (%q), got %q", name, other, hint)
		}
	}
}

// THE DRIVER-SCRIPT CASE. A media lease around a script of its own: the line after the declaring one names the
// helpers that WOULD have been sized, and says --ram states the figure.
func TestReserveNamesTheUnrecognisedWrapperWhenItDeclaresTheClassDefault(t *testing.T) {
	cfg := classDefaultBox(t)
	for name, args := range map[string][]string{
		"a python driver":                         {"python", "driver.py"},
		"a driver script path":                    {"pwsh", "-File", "scripts/render-all.ps1", "--clip", "7"},
		"a helper that sizes nothing (run-graph)": strings.Fields("node render/comfy-run-graph.mjs wf.json"),
	} {
		lines, need := reserveSays(t, false, 0, gpulease.ClassMedia, args, cfg)
		if need.Source != hostneed.SourceClassDefault || need.GiB < 32 {
			t.Fatalf("%s: the fixture must reach the class default (the bound Krea 2 family, 32.8 GiB), got %v", name, need)
		}
		wantDeclaringFirst(t, lines, need)
		wantHint(t, name, lines, whyUnrecognised, "comfy-generate", "comfy-render", "comfy-edit", "comfy-inpaint", "comfy-video")
	}
}

// A recognised helper that runs the caller's own graph loads what the graph says, which the lease cannot read:
// it takes the class default too, and the line says that is why, naming the helper.
func TestReserveNamesTheGraphWhenARecognisedHelperRunsOne(t *testing.T) {
	cfg := classDefaultBox(t)
	for _, c := range []struct {
		name   string
		class  gpulease.Class
		args   []string
		script string
	}{
		{"krea2 words plus --graph", gpulease.ClassMedia, append(krea2Words(), "--graph", "wf.json"), "comfy-generate.mjs"},
		{"comfy-video --graph=", gpulease.ClassMedia, strings.Fields("node render/comfy-video.mjs out.mp4 --graph=wf.json"), "comfy-video.mjs"},
		{"comfy-render --graph on a text lease", gpulease.ClassText, strings.Fields("node render/comfy-render.mjs out.png --graph wf.json"), "comfy-render.mjs"},
	} {
		lines, need := reserveSays(t, false, 0, c.class, c.args, cfg)
		if need.Source != hostneed.SourceClassDefault {
			t.Fatalf("%s: a graph call takes the class default, got %v", c.name, need)
		}
		wantDeclaringFirst(t, lines, need)
		wantHint(t, c.name, lines, whyGraph, c.script)
	}
}

// A recognised helper whose family has no documented size and whose files are not in the model tree cannot be
// sized either. The reason is its own: the helper is known, so the line must not call it an unrecognised wrapper.
func TestReserveNamesTheUnsizedHelperWhenItsFilesCannotBeSized(t *testing.T) {
	cfg := classDefaultBox(t)
	args := strings.Fields("node render/comfy-video.mjs o.mp4 s.png prompt --model hunyuan")
	lines, need := reserveSays(t, false, 0, gpulease.ClassMedia, args, cfg)
	if need.Source != hostneed.SourceClassDefault {
		t.Fatalf("an unsizable hunyuan call takes the class default, got %v", need)
	}
	wantDeclaringFirst(t, lines, need)
	wantHint(t, "an unsized comfy-video call", lines, whyUnsized, "comfy-video.mjs")
}

// A detached hold wraps no command, so there is nothing to size: the class default is the figure, and the line
// says so instead of calling a missing command an unrecognised one.
func TestReserveNamesTheEmptyCommandOfAHoldThatDeclaresTheClassDefault(t *testing.T) {
	cfg := classDefaultBox(t)
	lines, need := reserveSays(t, false, 0, gpulease.ClassMedia, nil, cfg)
	if need.Source != hostneed.SourceClassDefault {
		t.Fatalf("a media hold with no command takes the class default, got %v", need)
	}
	wantDeclaringFirst(t, lines, need)
	wantHint(t, "a hold", lines, whyNoCommand)
}

// THE HINT CHANGES NO FIGURE. What the verb returns (and so what the lease declares, `gpu status` shows and the
// grant admits) is what hostneed.Resolve says, hint or no hint.
func TestTheHintChangesNoFigure(t *testing.T) {
	cfg := classDefaultBox(t)
	for _, c := range []struct {
		name  string
		class gpulease.Class
		args  []string
	}{
		{"an unrecognised wrapper", gpulease.ClassMedia, []string{"python", "driver.py"}},
		{"a graph call", gpulease.ClassMedia, strings.Fields("node render/comfy-render.mjs out.png --graph wf.json")},
		{"an unsized helper", gpulease.ClassMedia, strings.Fields("node render/comfy-video.mjs o.mp4 s.png prompt --model hunyuan")},
		{"a hold", gpulease.ClassMedia, nil},
		{"a recognised helper with an estimate", gpulease.ClassMedia, krea2Words()},
		{"a text bench", gpulease.ClassText, []string{"python", "bench.py"}},
	} {
		want := hostneed.Resolve(hostneed.Request{Class: c.class, Args: c.args}, hostneed.Facts{Cfg: cfg, Env: os.Getenv})
		_, got := reserveSays(t, false, 0, c.class, c.args, cfg)
		if got != want {
			t.Errorf("%s: the verb returned %+v, hostneed.Resolve says %+v: the hint must change no figure", c.name, got, want)
		}
	}
}

// The line is for the figure the CALLER did not choose. Everything else prints what it printed before: the
// explicit figure (0 is a value), a figure sized from the helper's own files, nothing for a text bench, and
// nothing on a box whose class default is 0.
func TestReserveSaysNothingMoreWhenTheFigureIsNotTheClassDefault(t *testing.T) {
	cfg := classDefaultBox(t)
	for _, ram := range []float64{0, 12} {
		lines, need := reserveSays(t, true, ram, gpulease.ClassMedia, []string{"python", "driver.py"}, cfg)
		if need.Source != hostneed.SourceExplicit || need.GiB != ram {
			t.Fatalf("--ram %g is the explicit figure, got %v", ram, need)
		}
		if len(lines) != 1 {
			t.Errorf("--ram %g prints exactly the declaring line and no hint, got %q", ram, lines)
		}
		wantDeclaringFirst(t, lines, need)
	}
	// a recognised helper with an estimate (the documented Krea 2 sizes: the box has no model tree)
	lines, need := reserveSays(t, false, 0, gpulease.ClassMedia, krea2Words(), cfg)
	if need.Source == hostneed.SourceClassDefault || need.GiB < 32 {
		t.Fatalf("a krea2 call is sized from its own family, got %v", need)
	}
	if len(lines) != 1 {
		t.Errorf("a figure estimated from the helper's own files needs no hint, got %q", lines)
	}
	wantDeclaringFirst(t, lines, need)
	// a text lease declares nothing and says nothing
	if lines, _ := reserveSays(t, false, 0, gpulease.ClassText, []string{"python", "bench.py"}, cfg); lines != nil {
		t.Errorf("a text bench says nothing, got %q", lines)
	}
	// a media lease on a box that binds no ComfyUI family: the class default is 0, so there is no line at all
	for name, empty := range map[string]config.Config{"no ComfyUI install": {}, "an install that binds no family": {ComfyDir: t.TempDir()}} {
		lines, need := reserveSays(t, false, 0, gpulease.ClassMedia, []string{"python", "driver.py"}, empty)
		if need.GiB != 0 || lines != nil {
			t.Errorf("%s: a media lease whose class default is 0 declares nothing and says nothing, got %v %q", name, need, lines)
		}
	}
}

// THE WIRING. The line reaches the operator: `gpu reserve --class media --devices 2 -- <not a helper>` prints the
// declaring line and the hint straight after it on stderr, and the same command with --ram 0 prints the explicit
// line only.
func TestReserveVerbPrintsTheHintRightAfterTheDeclaringLine(t *testing.T) {
	cfg, _ := scopedLeaseFixture(t)
	emptyComfyDir(t, cfg)
	setConfigKey(t, cfg, "imagegen_script", "render/comfy-generate.mjs")
	setConfigKey(t, cfg, "imagegen_family", "krea2")
	setConfigKey(t, cfg, "imagegen_ckpt", "krea2_turbo_bf16.safetensors")
	useCardTable(t, "")
	t.Setenv("LO_HELPER_SLEEP_MS", "0")

	// run reserves with the flags and returns the declaring line and the line printed right after it ("" when it was the last).
	run := func(extraFlags ...string) []string {
		t.Helper()
		var err error
		flags := append([]string{"--class", "media", "--devices", "2", "--wait", "0", "--reason", "driver"}, extraFlags...)
		stderr := captureStderr(t, func() { err = runGPUReserve(reserveWithCmd(cfg, flags, "driver.py")) })
		if err != nil {
			t.Fatalf("reserve %v: %v\nstderr:\n%s", extraFlags, err, stderr)
		}
		lines := strings.Split(strings.TrimRight(stderr, "\r\n"), "\n")
		for i, l := range lines {
			if strings.HasPrefix(l, "gpu reserve: declaring ") {
				next := ""
				if i+1 < len(lines) {
					next = lines[i+1]
				}
				return []string{l, next}
			}
		}
		t.Fatalf("reserve %v printed no declaring line:\n%s", extraFlags, stderr)
		return nil
	}

	got := run()
	if !strings.Contains(got[0], "class default") {
		t.Fatalf("a media lease around a driver script declares the class default, got %q", got)
	}
	wantHint(t, "the verb", got, whyUnrecognised, "comfy-generate", "comfy-video")

	explicit := run("--ram", "0")
	if !strings.Contains(explicit[0], "0.0 GiB host RAM (explicit") || strings.Contains(explicit[1], "--ram <GiB>") || strings.Contains(explicit[1], "class default") {
		t.Fatalf("--ram 0 prints the explicit line and no hint after it, got %q", explicit)
	}
}
