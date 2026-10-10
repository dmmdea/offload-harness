package imagegen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// A runner script for GenerateBatch: it reads its behaviour from the JSON that stands in for the jobs
// file (--batch <file>), writes the rows it was told to into --results <file>, and exits as told. It is
// what render/comfy-generate.mjs --batch looks like from the Go side, and nothing else.
const batchRunnerJS = `const fs = require('fs');
const a = process.argv.slice(2);
const spec = JSON.parse(fs.readFileSync(a[a.indexOf('--batch') + 1], 'utf8'));
if (spec.rows !== undefined) fs.writeFileSync(a[a.indexOf('--results') + 1], spec.rows);
process.exit(spec.exit);
`

const rowsOneFailed = `{"i":0,"ok":true}` + "\n" + `{"i":1,"ok":false,"error":"comfy-render exited 1"}` + "\n"

// runBatchRunner runs GenerateBatch against the script above: it exits `exit` after writing `rows`
// (nil = writes no results file at all).
func runBatchRunner(t *testing.T, exit int, rows *string) error {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; the batch exit-code test needs the verified toolchain")
	}
	spec := map[string]any{"exit": exit}
	if rows != nil {
		spec["rows"] = *rows
	}
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "runner.js")
	jobs := filepath.Join(dir, "jobs.json")
	results := filepath.Join(dir, "results.jsonl")
	for path, content := range map[string][]byte{script: []byte(batchRunnerJS), jobs: body} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// gpugen frees the ComfyUI it is told about when the run ends; point that at a local server so a
	// test run can never POST /free to a ComfyUI that happens to be running on the machine.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	return GenerateBatch(context.Background(), "node", script, dir, jobs, results, Model{Launch: ComfyLaunch{API: srv.URL}}, 20*time.Second)
}

func str(s string) *string { return &s }

// The runner now exits BatchExitJobsFailed when it ran every job and some failed. For a Go caller that
// is what an exit 0 always was: the results file holds a row per job and the per-job status is read
// from it. Reporting it as a failed batch would turn "35 of 36 pictures done" into an error.
func TestGenerateBatchTreatsTheJobsFailedExitAsAFinishedBatch(t *testing.T) {
	if err := runBatchRunner(t, BatchExitJobsFailed, str(rowsOneFailed)); err != nil {
		t.Fatalf("a batch that ran every job and had a failed one must not be a batch-level error: %v", err)
	}
}

func TestGenerateBatchStillTreatsACleanExitAsFinished(t *testing.T) {
	if err := runBatchRunner(t, 0, str(rowsOneFailed)); err != nil {
		t.Fatalf("exit 0 with a results file is a finished batch: %v", err)
	}
}

// Everything else is still an error the caller must see: a stop (an unusable ComfyUI, a full disk: exit
// 1), the child's server-unusable code, a crash, and a "jobs failed" exit with nothing in the results
// file (the runner died before it could say which).
func TestGenerateBatchStillFailsOnEveryOtherEnding(t *testing.T) {
	cases := []struct {
		name string
		exit int
		rows *string
	}{
		{"stopped (exit 1) with rows", 1, str(rowsOneFailed)},
		{"exit 3 (the child's server-unusable code) with rows", 3, str(rowsOneFailed)},
		{"jobs-failed exit with no results file", BatchExitJobsFailed, nil},
		{"jobs-failed exit with an empty results file", BatchExitJobsFailed, str("")},
		{"clean exit with no results file", 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := runBatchRunner(t, c.exit, c.rows); err == nil {
				t.Fatalf("%s must be an error", c.name)
			}
		})
	}
}

// The exit code is a contract between two languages. Pin the Go copy to the one the runner declares, and
// keep it off the codes that already mean something in the same family (comfy-render.mjs: 1 failure,
// 2 usage, 3 server unusable).
func TestBatchExitJobsFailedMatchesTheRunner(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "render", "batch-jobs.mjs"))
	if err != nil {
		t.Fatalf("read the runner's declaration: %v", err)
	}
	m := regexp.MustCompile(`export const BATCH_EXIT_JOBS_FAILED = (\d+);`).FindSubmatch(src)
	if m == nil {
		t.Fatal("render/batch-jobs.mjs no longer declares BATCH_EXIT_JOBS_FAILED")
	}
	if got := string(m[1]); got != strconv.Itoa(BatchExitJobsFailed) {
		t.Fatalf("render/batch-jobs.mjs declares BATCH_EXIT_JOBS_FAILED = %s, Go has BatchExitJobsFailed = %d; they must be the same number", got, BatchExitJobsFailed)
	}
	for _, taken := range []int{0, 1, 2, 3} {
		if BatchExitJobsFailed == taken {
			t.Fatalf("BatchExitJobsFailed = %d collides with a code the render helpers already use", taken)
		}
	}
}
