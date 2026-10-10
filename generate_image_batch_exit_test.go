package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/imagegen"
)

// 2026-10-09: an overnight picture batch lost 21 of 36 pictures and exited 0, so the only way to learn
// it was to grep the log. render/comfy-generate.mjs --batch now ends with imagegen.BatchExitJobsFailed
// (4) when it ran every job and some failed; `local-offload generate-image --batch` is the Go door onto
// the same batch and ends with the same code, while its JSON keeps reporting per-job status. A clean
// batch still exits 0; a batch that stopped (an unusable ComfyUI, a full disk) is still exit 1.

func TestExitStatusOfAVerbsError(t *testing.T) {
	if code, line := exitStatus(errors.New("boom")); code != 1 || line != "error: boom" {
		t.Errorf("a plain error is exit 1 and an 'error:' line, got %d %q", code, line)
	}
	e := &exitCodeError{code: 4, msg: "3 of 36 jobs failed"}
	if code, line := exitStatus(e); code != 4 || line != "3 of 36 jobs failed" {
		t.Errorf("an exitCodeError keeps its own code and line, got %d %q", code, line)
	}
	if code, _ := exitStatus(fmt.Errorf("wrapped: %w", e)); code != 4 {
		t.Errorf("a wrapped exitCodeError keeps its code, got %d", code)
	}
}

// The runner stub reads its behaviour from STUB_MODE: "clean" renders every job; "failed" fails job 2
// and ends with the jobs-failed exit code the real runner uses; "stop" fails job 2 on a full disk, records
// the rest as not run and ends with exit 1 (the runner's stop).
const batchModeStub = `import {readFileSync, writeFileSync} from "node:fs";
const a = process.argv.slice(2);
const jobs = readFileSync(a[a.indexOf("--batch") + 1], "utf8").split("\n").filter(l => l.trim());
const mode = process.env.STUB_MODE || "clean";
const rows = jobs.map((l, i) => {
  const j = JSON.parse(l);
  if (mode === "failed" && i === 1) return JSON.stringify({i, out: j.out, seed: j.seed, ok: false, ms: 1, error: "comfy-render exited 1: ComfyUI exec error"});
  if (mode === "stop" && i === 1) return JSON.stringify({i, out: j.out, seed: j.seed, ok: false, ms: 1, error: "comfy-render exited 1: ENOSPC: no space left on device, write (writing " + j.out + ")"});
  if (mode === "stop" && i > 1) return JSON.stringify({i, out: j.out, seed: j.seed, ok: false, ms: 0, error: "not run: the disk is full at job 2/3, writing " + jobs.map(x => JSON.parse(x).out)[1] + " (comfy-render exited 1: ENOSPC: no space left on device, write)"});
  writeFileSync(j.out, "x");
  return JSON.stringify({i, out: j.out, seed: j.seed, ok: true, ms: 1});
});
writeFileSync(a[a.indexOf("--results") + 1], rows.join("\n") + "\n");
process.exitCode = mode === "failed" ? 4 : mode === "stop" ? 1 : 0;
`

// batchHome writes a hermetic home: a temp state dir (the leaseFixture rule), a config that points the
// image script at the stub and the ledger at a temp file, and a three-job batch. gpugen frees the
// ComfyUI it is told about when a run ends, so COMFY_API is aimed at a closed port: a test run never
// reaches a ComfyUI that happens to be running on the machine.
func batchHome(t *testing.T) (home, cfgPath, jobsPath, ledgerPath string) {
	t.Helper()
	requireNode(t)
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COMFY_API", "http://127.0.0.1:9")
	stub := filepath.Join(home, "batch-mode-stub.mjs")
	if err := os.WriteFile(stub, []byte(batchModeStub), 0o644); err != nil {
		t.Fatal(err)
	}
	ledgerPath = filepath.Join(home, "ledger.jsonl")
	cfgPath = filepath.Join(home, "config.json")
	cfgJSON := `{"endpoint":"http://127.0.0.1:1","state_dir":` + strconvQuote(filepath.ToSlash(home)) + `,"gpu_wait_ms":5000,"media_dir":` + strconvQuote(filepath.ToSlash(filepath.Join(home, "media"))) +
		`,"ledger_path":` + strconvQuote(filepath.ToSlash(ledgerPath)) + `,"imagegen_script":` + strconvQuote(filepath.ToSlash(stub)) + `,"imagegen_family":"krea2"}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	jobsPath = filepath.Join(home, "jobs.jsonl")
	if err := os.WriteFile(jobsPath, []byte(`{"prompt":"a red bike"}`+"\n"+`{"prompt":"a blue car"}`+"\n"+`{"prompt":"a green boat"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, cfgPath, jobsPath, ledgerPath
}

// requireNode skips a test that runs the node runner stub when node is not installed.
func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH")
	}
}

type batchOutput struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
	Result struct {
		Count     int `json:"count"`
		Succeeded int `json:"succeeded"`
		Failed    int `json:"failed"`
		Items     []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"items"`
	} `json:"result"`
}

func parseBatchOutput(t *testing.T, out string) batchOutput {
	t.Helper()
	var b batchOutput
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatalf("the batch JSON: %v\n%s", err, out)
	}
	return b
}

// In-process: the verb returns the error main() maps to the exit status.
func TestGenerateImageBatchWithAFailedJobReturnsTheJobsFailedExit(t *testing.T) {
	_, cfgPath, jobsPath, _ := batchHome(t)
	t.Setenv("STUB_MODE", "failed")
	var err error
	out := captureStdout(t, func() { err = runGenerateImage([]string{"--batch", jobsPath, "--config", cfgPath, "--json"}) })
	code, line := exitStatus(err)
	if err == nil || code != imagegen.BatchExitJobsFailed {
		t.Fatalf("a batch that ran every job and had a failed one must end with exit %d, got err=%v (exit %d)", imagegen.BatchExitJobsFailed, err, code)
	}
	if !strings.Contains(line, "1 of 3 jobs failed") || strings.HasPrefix(line, "error:") {
		t.Errorf("the stderr line should give the counts and is not an 'error:' line: %q", line)
	}
	b := parseBatchOutput(t, out)
	if b.OK || b.Result.Count != 3 || b.Result.Succeeded != 2 || b.Result.Failed != 1 || len(b.Result.Items) != 3 {
		t.Fatalf("the JSON must keep reporting per-job status: %+v", b)
	}
	if got := []bool{b.Result.Items[0].OK, b.Result.Items[1].OK, b.Result.Items[2].OK}; fmt.Sprint(got) != "[true false true]" {
		t.Errorf("per-job ok flags = %v", got)
	}
	if !strings.Contains(b.Result.Items[1].Error, "ComfyUI exec error") {
		t.Errorf("the failed job's own error is lost: %q", b.Result.Items[1].Error)
	}
}

func TestGenerateImageBatchThatCleanlyFinishedReturnsNothing(t *testing.T) {
	_, cfgPath, jobsPath, _ := batchHome(t)
	t.Setenv("STUB_MODE", "clean")
	var err error
	out := captureStdout(t, func() { err = runGenerateImage([]string{"--batch", jobsPath, "--config", cfgPath, "--json"}) })
	if err != nil {
		t.Fatalf("a clean batch ends with exit 0, got %v", err)
	}
	if b := parseBatchOutput(t, out); !b.OK || b.Result.Succeeded != 3 || b.Result.Failed != 0 {
		t.Errorf("clean batch JSON: %+v", b)
	}
}

// A stop is still exit 1 with an 'error:' line, not the jobs-failed code, and the ledger now records the
// jobs the full disk took as disk_full (they were "other").
func TestGenerateImageBatchThatStoppedOnAFullDiskIsStillExit1AndLedgeredAsDiskFull(t *testing.T) {
	_, cfgPath, jobsPath, ledgerPath := batchHome(t)
	t.Setenv("STUB_MODE", "stop")
	var err error
	out := captureStdout(t, func() { err = runGenerateImage([]string{"--batch", jobsPath, "--config", cfgPath, "--json"}) })
	code, line := exitStatus(err)
	if err == nil || code != 1 || !strings.HasPrefix(line, "error: ") {
		t.Fatalf("a stopped batch is exit 1 with an 'error:' line, got err=%v (exit %d, %q)", err, code, line)
	}
	b := parseBatchOutput(t, out)
	if b.OK || b.Result.Count != 3 || b.Result.Failed != 2 || b.Result.Succeeded != 1 || !strings.Contains(b.Result.Items[2].Error, "not run: the disk is full") {
		t.Errorf("the stop's per-job rows must still be reported: %+v", b)
	}
	raw, rerr := os.ReadFile(ledgerPath)
	if rerr != nil {
		t.Fatalf("the ledger: %v", rerr)
	}
	classes := map[string]int{}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row struct {
			Task     string `json:"task"`
			ErrClass string `json:"err_class"`
		}
		if json.Unmarshal([]byte(l), &row) == nil && row.ErrClass != "" {
			classes[row.ErrClass]++
		}
	}
	if classes["disk_full"] != 2 || classes["other"] != 0 {
		t.Errorf("the two jobs the full disk took must be ledgered as disk_full, got classes %v\n%s", classes, raw)
	}
}

// Through the built binary: what a shell caller sees. Building takes a while, so -short skips it.
func TestGenerateImageBatchExitCodesThroughTheBuiltBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the product binary")
	}
	requireNode(t)
	// The binary is built BEFORE batchHome moves HOME to a temp directory: HOME is the home of the
	// binary's runs, and the compiler keeps the caller's own HOME, module cache and build cache. On Linux
	// GOPATH, and with it GOMODCACHE and GOCACHE, follow HOME, so a build under the temp one downloaded the
	// whole module tree into the test's temp directory (45 s per run on CI) and left it behind: Go's
	// module cache is read-only, and the directory could not be removed ("TempDir RemoveAll cleanup:
	// unlinkat .../go/pkg/mod/...: permission denied", 2026-10-10). A Windows host pins GOMODCACHE in its
	// own go env, which is why this only ever failed on Linux.
	exe := filepath.Join(t.TempDir(), "local-offload")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	_, cfgPath, jobsPath, _ := batchHome(t)
	for _, c := range []struct {
		mode   string
		code   int
		stderr string
	}{
		{"clean", 0, ""},
		{"failed", 4, "generate-image --batch: 1 of 3 jobs failed (exit 4)"},
		{"stop", 1, "error: image batch failed"},
	} {
		cmd := exec.Command(exe, "generate-image", "--batch", jobsPath, "--config", cfgPath, "--json")
		cmd.Env = append(os.Environ(), "STUB_MODE="+c.mode)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		got := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			got = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("%s: %v", c.mode, err)
		}
		if got != c.code {
			t.Errorf("%s batch: exit %d, want %d\nstderr: %s", c.mode, got, c.code, stderr.String())
		}
		if c.stderr != "" && !strings.Contains(stderr.String(), c.stderr) {
			t.Errorf("%s batch: stderr %q lacks %q", c.mode, stderr.String(), c.stderr)
		}
		if b := parseBatchOutput(t, stdout.String()); b.Result.Count != 3 {
			t.Errorf("%s batch: the JSON must carry the per-job items: %s", c.mode, stdout.String())
		}
	}
}
