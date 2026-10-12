package gpugen

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// D-38 (#72): a ComfyUI that stopped answering is a typed class on the Go side. The ComfyUI runners
// (render/comfy-render.mjs, relayed by comfy-generate.mjs) end such a failure with one
// "IGPU_CLASS=comfy_unusable" line and exit RENDER_EXIT_SERVER_UNUSABLE (3). Generate reads the class
// line from the whole output of ANY child, so no new parser is needed: the class only has to be on the
// runner allow-list. Before this, a runner that exited 3 was filed "other".

// comfyUnusableRunner is a stand-in for the ComfyUI runner's failure: its human line carries words a
// looser classifier would claim ("zoom" holds "oom", "timeout"), then the class line, then exit 3.
func comfyUnusableRunner() (exe, script string, args []string) {
	js := `console.error("RENDER FAILED: ComfyUI stopped answering mid-render (unreachable 90s, the zoom lens timeout); aborting early to release the GPU slot");` +
		`console.error("IGPU_CLASS=comfy_unusable");process.exit(3)`
	return "node", "-e", []string{js}
}

// TestGenerateReadsComfyUnusableFromAFakeRunner drives a fake runner through Generate: it prints the class on
// its own line and exits 3. Before the fix runnerClasses dropped the line, so the failure was an untyped
// error that ClassifyErr read from its words ("timeout"), not the RunError this asserts.
func TestGenerateReadsComfyUnusableFromAFakeRunner(t *testing.T) {
	requireNode(t)
	exe, script, args := comfyUnusableRunner()
	_, err := Generate(context.Background(), Spec{
		Exe: exe, Script: script, Args: args,
		Out:           filepath.Join(t.TempDir(), "never.png"),
		Timeout:       30 * time.Second,
		SkipFreeComfy: true,
	})
	if err == nil {
		t.Fatal("a runner that exits 3 must fail")
	}
	var re *RunError
	if !errors.As(err, &re) {
		t.Fatalf("Generate returned %T (%v); want a *RunError carrying the runner's class", err, err)
	}
	if re.Class != "comfy_unusable" {
		t.Fatalf("RunError.Class = %q, want comfy_unusable (err: %v)", re.Class, err)
	}
	if got := ClassifyErr(err); got != "comfy_unusable" {
		t.Fatalf("ClassifyErr = %q, want comfy_unusable", got)
	}
	if !strings.Contains(err.Error(), "RENDER FAILED") {
		t.Errorf("the reason must keep the runner's own failure line, got: %v", err)
	}
}

// TestRunnerClassReadsComfyUnusable is the pure parse: the class line is read, whole-line, at its start.
func TestRunnerClassReadsComfyUnusable(t *testing.T) {
	out := []byte("RENDER FAILED: ComfyUI not reachable on http://127.0.0.1:8188 after ~3min\nIGPU_CLASS=comfy_unusable\n")
	if got := runnerClass(out); got != "comfy_unusable" {
		t.Fatalf("runnerClass = %q, want comfy_unusable", got)
	}
	// a mid-line mention is not a class line
	if got := runnerClass([]byte("note IGPU_CLASS=comfy_unusable inline\n")); got != "" {
		t.Fatalf("runnerClass(inline mention) = %q, want none", got)
	}
}

// TestRunnerClassStillIgnoresAnInventedClass: the allow-list is still closed; engine output that looks like
// the marker cannot invent a class.
func TestRunnerClassStillIgnoresAnInventedClass(t *testing.T) {
	for _, line := range []string{"IGPU_CLASS=comfy_dead", "IGPU_CLASS=comfy_unusable_forever", "IGPU_CLASS=server_unusable"} {
		if got := runnerClass([]byte(line + "\n")); got != "" {
			t.Errorf("runnerClass(%q) = %q, want none", line, got)
		}
	}
	// and the last valid line still wins over an earlier one
	if got := runnerClass([]byte("IGPU_CLASS=timeout\nIGPU_CLASS=gpu_reset\nIGPU_CLASS=comfy_dead\n")); got != "gpu_reset" {
		t.Errorf("runnerClass(valid, valid, invented) = %q, want the last VALID one (gpu_reset)", got)
	}
}
