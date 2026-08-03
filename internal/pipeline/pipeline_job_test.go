package pipeline

// Task 6 Step 4: runPipelineJob end to end against the GPU-free
// testdata/fake-scene-swap.mjs stand-in — no real ComfyUI/GPU touched. Locks
// the decisions the brief calls out explicitly: no machine-wide GPU lease
// (only the in-process mediaSlot), the plain (non-deferred) OK:false shape on
// a child render failure with the SCENE-SWAP-FAIL reason surfaced, the
// final_path-first JSON key order, flat media publishing, and the
// Record("", "", taskType, peak) footprint plumbing.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// fakeSceneSwapScript resolves the committed test fixture's absolute path
// (tests run with cwd = this package dir, but Workdir/Script are threaded
// through as absolute paths in production, so tests exercise the same shape).
func fakeSceneSwapScript(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "fake-scene-swap.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(abs); statErr != nil {
		t.Fatalf("fixture missing: %v", statErr)
	}
	return abs
}

// writeTestJobJSON writes a minimal job.json (just an id — the fixture only
// reads that) under dir and returns its path.
func writeTestJobJSON(t *testing.T, dir, id string) string {
	t.Helper()
	jobPath := filepath.Join(dir, "job.json")
	data, err := json.Marshal(map[string]any{
		"id":      id,
		"product": filepath.Join(dir, "product.png"),
		"logo":    filepath.Join(dir, "logo.png"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return jobPath
}

// pipelineJobParams builds the Params a buildPipelineJob call would have
// produced (internal/fleetnode.buildPipelineJob, Task 6 Step 1-3) — the
// runner test constructs these directly since fleetnode's builder is
// unexported and lives in a different package.
func pipelineJobParams(taskType, jobID, jobPath, outRoot, tier string) map[string]any {
	return map[string]any{
		"pipeline_task": taskType,
		"job_id":        jobID,
		"job_path":      jobPath,
		"out_root":      outRoot,
		"tier":          tier,
	}
}

func pipelineTestConfig(t *testing.T, script, workdir string, timeoutSec int, artifacts []string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.MediaDir = t.TempDir()
	cfg.Pipelines = map[string]config.PipelineSpec{
		"scene-swap": {
			Script:     script,
			Workdir:    workdir,
			TimeoutSec: timeoutSec,
			Artifacts:  artifacts,
		},
	}
	return cfg
}

// TestRunPipelineJob_Success: a clean run through the stub CLI publishes both
// artifacts flat under MediaDir as "<id>-<artifact>", and the result JSON's
// FIRST key is final_path (the shared-contracts.md wire invariant: Go struct
// field order IS marshal order).
func TestRunPipelineJob_Success(t *testing.T) {
	requireNodePipeline(t)
	script := fakeSceneSwapScript(t)
	jobDir := t.TempDir()
	cfg := pipelineTestConfig(t, script, filepath.Dir(script), 30, []string{"final.png", "qa-report.json"})

	jobID := "ok-case"
	jobPath := writeTestJobJSON(t, jobDir, jobID)
	outRoot := filepath.Join(jobDir, "out")

	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskPipelineJob,
		Params: pipelineJobParams("scene-swap", jobID, jobPath, outRoot, "16gb"),
	})
	if !res.OK {
		t.Fatalf("expected OK, got defer/failure: %s", res.Reason)
	}
	if res.Deferred {
		t.Error("a successful pipeline job must not be marked Deferred")
	}
	if !bytes.HasPrefix(res.Data, []byte(`{"final_path":`)) {
		t.Fatalf("data does not start with the final_path key (insertion-order invariant): %s", res.Data)
	}

	var out struct {
		FinalPath    string  `json:"final_path"`
		QaReportPath string  `json:"qa_report_path"`
		JobID        string  `json:"job_id"`
		Tier         string  `json:"tier"`
		DurationSec  float64 `json:"duration_sec"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatalf("result not valid JSON: %v (%s)", err, res.Data)
	}
	if out.JobID != jobID {
		t.Errorf("job_id = %q, want %q", out.JobID, jobID)
	}
	if out.Tier != "16gb" {
		t.Errorf("tier = %q, want 16gb", out.Tier)
	}
	if out.DurationSec <= 0 {
		t.Errorf("duration_sec = %v, want > 0", out.DurationSec)
	}

	wantFinal := filepath.Join(cfg.MediaDir, jobID+"-final.png")
	if out.FinalPath != wantFinal {
		t.Errorf("final_path = %q, want %q", out.FinalPath, wantFinal)
	}
	if _, err := os.Stat(out.FinalPath); err != nil {
		t.Errorf("published final.png missing at %q: %v", out.FinalPath, err)
	}
	wantQA := filepath.Join(cfg.MediaDir, jobID+"-qa-report.json")
	if out.QaReportPath != wantQA {
		t.Errorf("qa_report_path = %q, want %q", out.QaReportPath, wantQA)
	}
	if _, err := os.Stat(out.QaReportPath); err != nil {
		t.Errorf("published qa-report.json missing at %q: %v", out.QaReportPath, err)
	}
}

// TestRunPipelineJob_MissingOptionalArtifactOmitted: when a pipeline declares
// an artifact the CLI does not actually produce, the result simply omits it
// (qa_report_path is omitempty) — never an error, since only the PRIMARY
// artifact is required.
func TestRunPipelineJob_MissingOptionalArtifactOmitted(t *testing.T) {
	requireNodePipeline(t)
	script := fakeSceneSwapScript(t)
	jobDir := t.TempDir()
	// Declare a THIRD artifact the fixture never writes.
	cfg := pipelineTestConfig(t, script, filepath.Dir(script), 30, []string{"final.png", "qa-report.json", "extra-report.json"})

	jobID := "ok-case-2"
	jobPath := writeTestJobJSON(t, jobDir, jobID)
	outRoot := filepath.Join(jobDir, "out")

	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskPipelineJob,
		Params: pipelineJobParams("scene-swap", jobID, jobPath, outRoot, "16gb"),
	})
	if !res.OK {
		t.Fatalf("a missing OPTIONAL artifact must not fail the job, got: %s", res.Reason)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	if _, has := out["qa_report_path"]; !has {
		t.Error("qa_report_path (a produced artifact) should be present")
	}
}

// TestRunPipelineJob_ChildFailureSurfacesSceneSwapFailReason: the fixture's
// "fail-case" id exits 1 with a SCENE-SWAP-FAIL stderr line. That line must
// become Result.Reason verbatim-ish (containing the stage), and — per the
// brief's explicit decision — the result is a PLAIN failure (OK:false), not a
// Deferred one: a fleet render failure is not "hand this back to Claude".
func TestRunPipelineJob_ChildFailureSurfacesSceneSwapFailReason(t *testing.T) {
	requireNodePipeline(t)
	script := fakeSceneSwapScript(t)
	jobDir := t.TempDir()
	cfg := pipelineTestConfig(t, script, filepath.Dir(script), 30, []string{"final.png", "qa-report.json"})

	jobID := "fail-case"
	jobPath := writeTestJobJSON(t, jobDir, jobID)
	outRoot := filepath.Join(jobDir, "out")

	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskPipelineJob,
		Params: pipelineJobParams("scene-swap", jobID, jobPath, outRoot, "16gb"),
	})
	if res.OK {
		t.Fatal("expected a failure result for fail-case")
	}
	if res.Deferred {
		t.Error("a child render failure must be a plain OK:false failure, not Deferred (Task 6 brief decision)")
	}
	if !strings.Contains(res.Reason, "SCENE-SWAP-FAIL") || !strings.Contains(res.Reason, "stage=background") {
		t.Errorf("Reason = %q, want it to surface the SCENE-SWAP-FAIL line verbatim", res.Reason)
	}
}

// TestRunPipelineJob_TimeoutErrors: a script that outlives PipelineSpec.
// TimeoutSec is killed promptly and the run fails — no SCENE-SWAP-FAIL line
// was ever printed, so this exercises the "generic exec error" fallback.
func TestRunPipelineJob_TimeoutErrors(t *testing.T) {
	requireNodePipeline(t)
	jobDir := t.TempDir()
	sleepScript := filepath.Join(jobDir, "sleep-stub.mjs")
	if err := os.WriteFile(sleepScript, []byte("setTimeout(() => {}, 30000);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := pipelineTestConfig(t, sleepScript, jobDir, 1, []string{"final.png"})

	jobID := "timeout-case"
	jobPath := writeTestJobJSON(t, jobDir, jobID)
	outRoot := filepath.Join(jobDir, "out")

	p := &Pipeline{cfg: cfg}
	start := time.Now()
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskPipelineJob,
		Params: pipelineJobParams("scene-swap", jobID, jobPath, outRoot, "8gb"),
	})
	elapsed := time.Since(start)
	if res.OK {
		t.Fatal("expected a failure result on timeout")
	}
	if elapsed > 20*time.Second {
		t.Fatalf("timeout did not cut the run short (took %v)", elapsed)
	}
	if res.Reason == "" {
		t.Error("expected a non-empty failure reason for the timeout/exec error")
	}
	if strings.Contains(res.Reason, "SCENE-SWAP-FAIL") {
		t.Errorf("a timeout-killed process should never have printed SCENE-SWAP-FAIL, got %q", res.Reason)
	}
}

// TestRunPipelineJob_NoRouteConfiguredDefers: an unconfigured pipeline_task
// (e.g. a stale/mismatched dispatch) defers cleanly — defer-not-crash, same
// as every other route's "no route configured" guard.
func TestRunPipelineJob_NoRouteConfiguredDefers(t *testing.T) {
	cfg := config.Default() // no Pipelines entries
	p := &Pipeline{cfg: cfg}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskPipelineJob,
		Params: pipelineJobParams("scene-swap", "x", "x", "x", "8gb"),
	})
	if res.OK || !res.Deferred {
		t.Fatalf("expected a clean defer for an unconfigured pipeline task, got OK=%v", res.OK)
	}
}

// TestRunPipelineJob_MediaSlotBusyDefers: with the in-process mediaSlot
// already held, a pipeline job must wait the bounded gpu_wait_ms window and
// then defer as gpu_busy — proving it contends on mediaSlot directly (the
// brief's decision: NO machine-wide GPU lease for pipeline jobs).
func TestRunPipelineJob_MediaSlotBusyDefers(t *testing.T) {
	requireNodePipeline(t)
	script := fakeSceneSwapScript(t)
	cfg := config.Default()
	cfg.GPUWaitMs = 50 // shrink the 90s default for the test
	cfg.Pipelines = map[string]config.PipelineSpec{
		"scene-swap": {Script: script, Workdir: filepath.Dir(script), TimeoutSec: 30, Artifacts: []string{"final.png"}},
	}
	if !takeMediaSlot(0) {
		t.Fatal("test setup: failed to take the media slot")
	}
	defer releaseMediaSlot()

	p := &Pipeline{cfg: cfg}
	start := time.Now()
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskPipelineJob,
		Params: pipelineJobParams("scene-swap", "x", "x", "x", "8gb"),
	})
	if el := time.Since(start); el < 50*time.Millisecond {
		t.Errorf("returned after %v — must wait the bounded window before deferring", el)
	}
	if res.OK || !res.Deferred {
		t.Fatalf("expected a gpu_busy defer, got OK=%v", res.OK)
	}
	if res.Meta.ErrClass != "gpu_busy" {
		t.Errorf("err class = %q, want gpu_busy", res.Meta.ErrClass)
	}
}

// TestRunPipelineJob_RecordsFootprint: a successful run records the observed
// peak into the shared footprint store keyed Record("", "", taskType, peak) —
// no family/quant claim, mirroring image-gen's plumbing per the brief.
func TestRunPipelineJob_RecordsFootprint(t *testing.T) {
	requireNodePipeline(t)
	script := fakeSceneSwapScript(t)
	jobDir := t.TempDir()
	cfg := pipelineTestConfig(t, script, filepath.Dir(script), 30, []string{"final.png", "qa-report.json"})
	p := footprintTestPipeline(t, cfg, 3.5)

	jobID := "footprint-case"
	jobPath := writeTestJobJSON(t, jobDir, jobID)
	outRoot := filepath.Join(jobDir, "out")

	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskPipelineJob,
		Params: pipelineJobParams("scene-swap", jobID, jobPath, outRoot, "16gb"),
	})
	if !res.OK {
		t.Fatalf("expected OK, got defer/failure: %s", res.Reason)
	}
	e := findEntry(t, p.FootprintStore().Entries(), "", "scene-swap")
	if e.VramPeakGiB != 3.5 {
		t.Errorf("vram_peak_gb = %v, want 3.5", e.VramPeakGiB)
	}
	if e.Quant != "" {
		t.Errorf("quant = %q, want \"\" (no family/quant claim)", e.Quant)
	}
}
