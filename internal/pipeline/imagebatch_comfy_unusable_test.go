package pipeline

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// D-38 (#72), batch lane: a ComfyUI that dies mid-batch stops the batch (render/batch-jobs.mjs runBatchJobs): the job that hit
// it is recorded with the child's "comfy-render exited 3: ..." reason, every later job as "not run: ComfyUI became unusable at
// job i/N (...)", and the parent exits with the child's IGPU_CLASS=comfy_unusable line in its output, so gpugen.Generate returns
// a *RunError of that class for the whole batch. Every index has a row, so batchErrClass never reached the batch error and read
// each row's words, which name no class: the C-83 batch (a poisoned ComfyUI failing jobs 3-6 over 57 minutes) filed every row
// "other", although the single-image lane already filed the same death comfy_unusable.

// typedBatchError is what gpugen.Generate returns for a batch process that ended with the child's class line: a
// *gpugen.RunError (constructible only there) of the given class.
func typedBatchError(t *testing.T, class string) error {
	t.Helper()
	requireNodePipeline(t)
	dir := t.TempDir()
	_, err := gpugen.Generate(context.Background(), gpugen.Spec{
		Exe: "node", Script: classScript(t, dir, class),
		Out: filepath.Join(dir, "never.png"), Timeout: 30 * time.Second, SkipFreeComfy: true,
	})
	var re *gpugen.RunError
	if !errors.As(err, &re) || re.Class != class {
		t.Fatalf("the premise is gone: Generate returned %v, want a RunError of class %s", err, class)
	}
	return err
}

// The rows a batch that died on its third job writes, as runBatchJobs writes them (the end-to-end test below produces them with
// the real runBatchJobs; these are their shapes for the unit test).
const (
	rowOwnFailure   = "comfy-render exited 1: RENDER FAILED: CUDA out of memory"
	rowTriggerFmt   = "comfy-render exited 3: ComfyUI stopped answering mid-render (unreachable 130s, COMFY_DEAD_SEC=120); aborting early to release the GPU slot"
	rowNotRunPrefix = "not run: ComfyUI became unusable at job 3/5 ("
)

// A typed batch error is the cause of the rows the abort wrote (the job it stopped on, the jobs it never started), and of nothing
// else: a job that failed on its own before the abort keeps the class its own words give.
func TestBatchErrClassFilesTheAbortRowsWithTheBatchsTypedClass(t *testing.T) {
	gerr := typedBatchError(t, "comfy_unusable")
	for name, tc := range map[string]struct{ row, want string }{
		"the job the batch stopped on":      {rowTriggerFmt, "comfy_unusable"},
		"a job the batch never started":     {rowNotRunPrefix + rowTriggerFmt + ")", "comfy_unusable"},
		"a job with no row at all":          {"no result recorded (batch aborted?)", "comfy_unusable"},
		"a job that failed before the stop": {rowOwnFailure, "oom"},
		"a job that timed out on its own":   {"comfy-render exited 1: RENDER FAILED: view fetch timeout", "timeout"},
		"a failure of another kind":         {"comfy-render exited 1: RENDER FAILED: node X missing", "other"},
	} {
		if got := batchErrClass(tc.row, gerr); got != tc.want {
			t.Errorf("%s: batchErrClass(%q) = %q, want %q", name, tc.row, got, tc.want)
		}
	}
	// the typed class beats words on the abort rows too: a message that happens to hold "oom" ("zoom") is not an oom
	if got := batchErrClass("not run: ComfyUI became unusable at job 3/5 (comfy-render exited 3: the zoom lens timeout)", gerr); got != "comfy_unusable" {
		t.Errorf("an abort row whose words say oom/timeout = %q, want comfy_unusable", got)
	}
}

// The guards: with no typed batch error the rows keep being read by their words (nothing else moved), and a typed error of
// another class never claims the exit-3 row, which only the ComfyUI class describes.
func TestBatchErrClassKeepsReadingWordsWithoutATypedBatchError(t *testing.T) {
	for name, got := range map[string]string{
		"no batch error":            batchErrClass(rowTriggerFmt, nil),
		"an untyped batch error":    batchErrClass(rowTriggerFmt, errors.New("gpugen: comfy-generate.mjs failed: exit status 1")),
		"a not-run row, no error":   batchErrClass(rowNotRunPrefix+rowTriggerFmt+")", nil),
		"a full-disk not-run row":   batchErrClass("not run: the disk is full at job 2/4, writing renders/b.png (comfy-render exited 1: ENOSPC: no space left on device, write)", errors.New("gpugen: comfy-generate.mjs failed: exit status 1")),
		"a deadline batch, own row": batchErrClass(rowOwnFailure, typedBatchError(t, "timeout")),
	} {
		want := map[string]string{
			"no batch error": "other", "an untyped batch error": "other", "a not-run row, no error": "other",
			"a full-disk not-run row": "disk_full", "a deadline batch, own row": "oom",
		}[name]
		if got != want {
			t.Errorf("%s: err_class = %q, want %q", name, got, want)
		}
	}
	if got := batchErrClass(rowTriggerFmt, typedBatchError(t, "gpu_reset")); got != "other" {
		t.Errorf("an exit-3 row under a gpu_reset batch = %q, want other (only comfy_unusable describes that row)", got)
	}
}

// fileURL is the import specifier of an absolute path, on any OS.
func fileURL(abs string) string {
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// writeBatchStubUnusableComfy writes the parent of a --batch whose third job meets a dead ComfyUI. It runs the REAL runBatchJobs
// of render/batch-jobs.mjs, so the rows in the results file are the ones the real parent writes, and it ends the way
// comfy-generate.mjs does for such a batch: the child's relayed "IGPU_CLASS=comfy_unusable" line on stderr, "IMAGE BATCH FAILED"
// and exit 1. Job 1 renders, job 2 fails on its own (an OOM), job 3 exits 3, jobs 4-5 are never run.
func writeBatchStubUnusableComfy(t *testing.T, dir string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "render", "batch-jobs.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("the runner's batch helpers are not where the test expects them: %v", err)
	}
	script := filepath.Join(dir, "batch-unusable-comfy-stub.mjs")
	content := `import {readFileSync, writeFileSync, appendFileSync} from "node:fs";
import {parseJobs, runBatchJobs, renderExitError} from "` + fileURL(abs) + `";
const args = process.argv.slice(2);
const jobs = parseJobs(readFileSync(args[args.indexOf("--batch") + 1], "utf8"));
const resultsPath = args[args.indexOf("--results") + 1];
writeFileSync(resultsPath, "");
const record = (line) => appendFileSync(resultsPath, line + "\n");
try {
  await runBatchJobs({ jobs, record, runJob: async (job, i) => {
    if (i === 0) { writeFileSync(job.out, "stub-output"); return; }
    if (i === 1) throw renderExitError(1, "RENDER FAILED: CUDA out of memory\n");
    // the child's stderr, relayed verbatim by the parent (relayRenderChild)
    const why = "ComfyUI stopped answering mid-render (unreachable 130s, COMFY_DEAD_SEC=120); aborting early to release the GPU slot";
    console.error("RENDER FAILED: " + why);
    console.error("IGPU_CLASS=comfy_unusable");
    throw renderExitError(3, "RENDER FAILED: " + why + "\n");
  }});
} catch (e) {
  console.error("IMAGE BATCH FAILED:", e.message);
  process.exit(1);
}
`
	if err := os.WriteFile(script, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return script
}

// End to end through RunImageBatch: the stub is the parent of a batch whose ComfyUI dies on its third job, the rows are the real
// runBatchJobs's, and the ledger gets one row per failed job. The job that failed on its own keeps its own class; the job the
// batch stopped on and the two it never ran are filed with the batch's typed class.
func TestImageBatchRecordsAnUnusableComfyWithItsClassOnEveryAbortRow(t *testing.T) {
	requireNodePipeline(t)
	// gpugen frees the ComfyUI it is told about when the run ends: aim that at a local server, never at a ComfyUI that happens to
	// be running on the machine.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	t.Setenv("COMFY_API", srv.URL)

	dir := t.TempDir()
	cfg := config.Default()
	cfg.ImageGenScript = writeBatchStubUnusableComfy(t, dir)
	cfg.MediaDir = dir
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()
	isolateLease(&cfg, dir)
	p := &Pipeline{cfg: cfg, led: led}

	jobs := []ImageBatchJob{{Prompt: "a red bike"}, {Prompt: "a blue car"}, {Prompt: "a green tree"}, {Prompt: "a pink house"}, {Prompt: "a gray sphere"}}
	items, berr := p.RunImageBatch(context.Background(), jobs)
	var re *gpugen.RunError
	if !errors.As(berr, &re) || re.Class != "comfy_unusable" {
		t.Fatalf("the premise is gone: the batch returned %v, want a RunError of class comfy_unusable", berr)
	}
	if len(items) != 5 || !items[0].OK || items[1].OK || items[2].OK || items[3].OK || items[4].OK {
		t.Fatalf("items = %+v, want job 1 ok and jobs 2-5 failed", items)
	}
	for i, prefix := range map[int]string{1: "comfy-render exited 1:", 2: "comfy-render exited 3:", 3: "not run: ComfyUI became unusable at job 3/5", 4: "not run: ComfyUI became unusable at job 3/5"} {
		if !strings.HasPrefix(items[i].Error, prefix) {
			t.Fatalf("job %d error = %q, want the real runBatchJobs row starting %q", i+1, items[i].Error, prefix)
		}
	}

	rows, err := ledger.ReadAll(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var classes []string
	for _, r := range rows {
		if r.Deferred {
			classes = append(classes, r.ErrClass)
			if r.ReasonCode != ledger.ReasonInfrastructure && r.ErrClass == "comfy_unusable" {
				t.Errorf("a comfy_unusable row has reason code %q, want %q", r.ReasonCode, ledger.ReasonInfrastructure)
			}
		}
	}
	want := []string{"oom", "comfy_unusable", "comfy_unusable", "comfy_unusable"}
	if strings.Join(classes, ",") != strings.Join(want, ",") {
		t.Fatalf("err_class of the four failed jobs = %v, want %v (own failure keeps its class; the stop and the two never-run jobs take the batch's)", classes, want)
	}
}
