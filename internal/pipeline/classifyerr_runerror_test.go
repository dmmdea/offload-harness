package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// D-38 (#72): the lanes that file a gen failure through classifyErr (the ComfyUI image, edit, inpaint, upscale and
// batch lanes) used to read only the WORDS of the error, so a typed gpugen.RunError (the class the runner itself
// reported, or "timeout" for a deadline gpugen observed) was re-guessed from its message. gpugen.ClassifyErr
// already returned the typed class; classifyErr did not.

// classScript is a runner that fails with a typed class: its human line holds words a substring classifier claims
// ("zoom" holds "oom", and there is a "timeout"), then the class line, then exit 3.
func classScript(t *testing.T, dir, class string) string {
	t.Helper()
	p := filepath.Join(dir, "typed-"+class+".mjs")
	src := "console.error('RENDER FAILED: ComfyUI stopped answering mid-render (the zoom lens timeout)');\n" +
		"console.error('IGPU_CLASS=" + class + "');\nprocess.exit(3);\n"
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A typed RunError wins over the words of its message. The error is made by the real gpugen.Generate (a RunError
// is only constructible there) from a fake runner that reports an allow-listed class; the words of its message
// say "oom", the class says otherwise.
func TestClassifyErrHonoursATypedRunErrorClass(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	_, err := gpugen.Generate(context.Background(), gpugen.Spec{
		Exe: "node", Script: classScript(t, dir, "gpu_reset"),
		Out: filepath.Join(dir, "never.png"), Timeout: 30 * time.Second, SkipFreeComfy: true,
	})
	var re *gpugen.RunError
	if !errors.As(err, &re) || re.Class != "gpu_reset" {
		t.Fatalf("the premise is gone: Generate returned %v, want a RunError of class gpu_reset", err)
	}
	if got := classifyErr(err); got != "gpu_reset" {
		t.Fatalf("classifyErr = %q, want gpu_reset (the typed class), not a guess from the words %q", got, err.Error())
	}
	if got, want := classifyErr(err), gpugen.ClassifyErr(err); got != want {
		t.Fatalf("classifyErr = %q but gpugen.ClassifyErr = %q: the two classifiers must agree on a typed error", got, want)
	}
}

// classifyErr keeps reading words for an error with no typed class (the guard: nothing else moved).
func TestClassifyErrStillReadsWordsForAnUntypedError(t *testing.T) {
	for msg, want := range map[string]string{
		"CUDA out of memory":             "oom",
		"context deadline exceeded":      "timeout",
		"dial tcp: connection refused":   "conn_refused",
		"llama-server 503: busy":         "http_5xx",
		"something else entirely":        "other",
		"gpugen: no output at \"x.png\"": "other",
	} {
		if got := classifyErr(errors.New(msg)); got != want {
			t.Errorf("classifyErr(%q) = %q, want %q", msg, got, want)
		}
	}
}

// End to end through a ComfyUI lane: the image lane's stub runner exits 3 with the class line; the deferred result and
// the ledger row both carry err_class comfy_unusable, not "other".
func TestImageLaneRecordsAComfyUnusableFailureWithItsClass(t *testing.T) {
	requireNodePipeline(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.ImageGenScript = classScript(t, dir, "comfy_unusable")
	cfg.MediaDir = dir
	cfg.LedgerPath = filepath.Join(dir, "ledger.jsonl")
	isolateLease(&cfg, dir)
	p := &Pipeline{cfg: cfg}

	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateImage, Input: "a gray sphere"})
	if res.OK || !res.Deferred {
		t.Fatalf("a runner that exits 3 must defer, got ok=%v deferred=%v", res.OK, res.Deferred)
	}
	if res.Meta.ErrClass != "comfy_unusable" {
		t.Fatalf("Meta.ErrClass = %q, want comfy_unusable (reason: %s)", res.Meta.ErrClass, res.Reason)
	}
	rows, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.Task == string(core.TaskGenerateImage) && r.Deferred {
			found = true
			if r.ErrClass != "comfy_unusable" {
				t.Errorf("ledger ErrClass = %q, want comfy_unusable", r.ErrClass)
			}
			// the closed reason-code set still files it as infrastructure through the reason prefix
			if r.ReasonCode != ledger.ReasonInfrastructure {
				t.Errorf("ledger ReasonCode = %q, want %q", r.ReasonCode, ledger.ReasonInfrastructure)
			}
		}
	}
	if !found {
		t.Fatalf("no deferred %s row in the ledger: %+v", core.TaskGenerateImage, rows)
	}
}
