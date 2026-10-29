package hostneed

import (
	"reflect"
	"strings"
	"testing"
)

// F71: the line `gpu reserve` prints when it declares the media class default names the render helpers a wrapped
// command would have been sized as, and says when the helper runs its own graph. Both come from this package's own
// rules, so the message cannot drift from what is recognised.

// The names are exactly the files ParseRenderCall recognises, sorted, and the ones that are deliberately absent
// (an arbitrary graph, a wrapper) are not among them. The list is a copy: changing it changes nothing here.
func TestRenderHelpersAreExactlyTheFilesParseRenderCallRecognises(t *testing.T) {
	want := []string{"comfy-edit.mjs", "comfy-generate.mjs", "comfy-inpaint.mjs", "comfy-render.mjs", "comfy-run-graph.mjs", "comfy-video.mjs"}
	got := RenderHelpers()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RenderHelpers = %v, want %v", got, want)
	}
	for _, name := range got {
		if _, ok := ParseRenderCall([]string{"node", "render/" + name}); !ok {
			t.Errorf("%s is listed as a render helper but a wrapped command naming it is not recognised", name)
		}
	}
	for _, name := range []string{"comfy-animate.mjs", "driver.py", "render-all.ps1"} {
		if _, ok := ParseRenderCall([]string{"node", "render/" + name}); ok {
			t.Errorf("%s is recognised, so it belongs in the list", name)
		}
	}
	got[0] = "tampered"
	if again := RenderHelpers(); again[0] != "comfy-edit.mjs" {
		t.Errorf("the list handed out is the table's own: a caller changed it to %v", again)
	}
}

// RunsOwnGraph is the one rule for "this call posts the caller's own workflow": Estimate refuses to size such a call
// (the class default stands in), and the hint reads the same answer.
func TestRunsOwnGraphIsTheRuleEstimateRefusesOn(t *testing.T) {
	f := krea2Facts(t, 16)
	for _, c := range []struct {
		cmd   string
		graph bool
	}{
		{"node render/comfy-render.mjs out.png --graph wf.json", true},
		{"node render/comfy-render.mjs out.png --graph=wf.json --api http://127.0.0.1:8190", true},
		{"node render/comfy-video.mjs out.mp4 --graph wf.json", true},
		{"node render/comfy-render.mjs out.png prompt --family sdxl --ckpt RealVisXL.safetensors", false},
		{krea2Cmd, false},
	} {
		call, ok := ParseRenderCall(strings.Fields(c.cmd))
		if !ok {
			t.Fatalf("%q is a render helper call", c.cmd)
		}
		if got := call.RunsOwnGraph(); got != c.graph {
			t.Errorf("RunsOwnGraph(%q) = %v, want %v", c.cmd, got, c.graph)
		}
		if _, estimated := call.Estimate(f); c.graph && estimated {
			t.Errorf("a call that runs its own graph must not be estimated as the helper's default family: %q", c.cmd)
		}
	}
	// a call that is not a graph call IS sized by its own files (the control: the rule is not "never estimate")
	call, _ := ParseRenderCall(strings.Fields(krea2Cmd))
	if n, ok := call.Estimate(f); !ok || n.Source != SourceEstimate {
		t.Errorf("a krea2 call without --graph is sized from its files, got %v %v", n, ok)
	}
}
