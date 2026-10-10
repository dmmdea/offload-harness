package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// writeBatchStubJobsFailed writes a node stub for the --batch protocol that ends the way
// render/comfy-generate.mjs does since 0.178.0 when a job failed: job 1 renders, job 2 does not, the
// results file holds a row for each, and the exit code is imagegen.BatchExitJobsFailed.
func writeBatchStubJobsFailed(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "batch-jobs-failed-stub.mjs")
	content := `import {readFileSync, writeFileSync} from "node:fs";
const args = process.argv.slice(2);
const jobsPath = args[args.indexOf("--batch") + 1];
const resultsPath = args[args.indexOf("--results") + 1];
const lines = readFileSync(jobsPath, "utf8").split("\n").filter(l => l.trim());
const results = lines.map((l, i) => {
  const j = JSON.parse(l);
  if (i === 1) return JSON.stringify({i, out: j.out, seed: j.seed, ok: false, ms: 1, error: "comfy-render exited 1: ComfyUI exec error"});
  writeFileSync(j.out, "stub-output");
  return JSON.stringify({i, out: j.out, seed: j.seed, ok: true, ms: 1});
});
writeFileSync(resultsPath, results.join("\n") + "\n");
process.exitCode = 4;
`
	if err := os.WriteFile(script, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return script
}

// A batch that ran every job and had a failed one now ends with its own exit code instead of 0. For a
// Go caller that must change nothing: the per-job status is read from the results file, and the batch
// is not an error ("image batch failed: exit status 4" for 35 good pictures out of 36 would be one).
func TestRunImageBatchReadsPerJobStatusFromAJobsFailedExit(t *testing.T) {
	requireNodePipeline(t)
	// gpugen frees the ComfyUI it is told about when the run ends: aim that at a local server, never at
	// a ComfyUI that happens to be running on the machine.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	t.Setenv("COMFY_API", srv.URL)

	dir := t.TempDir()
	cfg := config.Default()
	cfg.ImageGenScript = writeBatchStubJobsFailed(t, dir)
	cfg.MediaDir = dir
	p := refinerTestPipeline(t, cfg)
	items, err := p.RunImageBatch(context.Background(), []ImageBatchJob{{Prompt: "a red bike"}, {Prompt: "a blue car"}})
	if err != nil {
		t.Fatalf("a batch whose runner exited with the jobs-failed code is a finished batch, not an error: %v", err)
	}
	if len(items) != 2 || !items[0].OK || items[1].OK {
		t.Fatalf("items = %+v, want job 1 ok and job 2 failed", items)
	}
	if items[1].Error != "comfy-render exited 1: ComfyUI exec error" {
		t.Fatalf("the failed job's own error must come through, got %q", items[1].Error)
	}
}
