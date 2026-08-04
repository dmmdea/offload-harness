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
	"log"
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

// TestSceneSwapFailReason_UsesLastOccurrence: the doc comment promises "the
// LAST SCENE-SWAP-FAIL line"; with two occurrences in the combined output
// (e.g. an earlier stage's log line happens to echo the marker text), the
// function must return the LAST one — the CLI's own actual terminal failure,
// not an earlier line that merely contains the same substring.
func TestSceneSwapFailReason_UsesLastOccurrence(t *testing.T) {
	msg := `gpugen: fake-scene-swap.mjs failed: exit status 1 (SCENE-SWAP-FAIL stage=product: retrying after transient error
SCENE-SWAP-FAIL stage=background: boom
)`
	got := sceneSwapFailReason(msg)
	want := "SCENE-SWAP-FAIL stage=background: boom"
	if got != want {
		t.Errorf("sceneSwapFailReason = %q, want %q (must pick the LAST occurrence)", got, want)
	}
}

// TestSceneSwapFailReason_AbsentReturnsEmpty: no marker present (e.g. a
// timeout-kill, which prints no such line) returns "" so the caller falls
// back to the generic exec error.
func TestSceneSwapFailReason_AbsentReturnsEmpty(t *testing.T) {
	if got := sceneSwapFailReason("gpugen: x failed: exit status 1 (killed)"); got != "" {
		t.Errorf("sceneSwapFailReason = %q, want \"\" (no SCENE-SWAP-FAIL marker present)", got)
	}
}

// --- Review fix #1: published[] must be index-aligned with spec.Artifacts,
// not compacted — a compacted slice skews FinalPath/QaReportPath onto the
// wrong artifact whenever a MIDDLE artifact is missing. ---

// writeSelectiveArtifactStub writes a node stub that creates ONLY the named
// bare filenames under <out>/<job.id>/ and exits 0, ignoring --tier/--backend
// entirely. Used to test publishPipelineArtifacts'/runPipelineJob's index
// binding independent of the real CLI's final.png+qa-report.json contract
// (fake-scene-swap.mjs always writes both, so it can't produce a
// "middle artifact missing, later one present" shape).
func writeSelectiveArtifactStub(t *testing.T, dir string, files ...string) string {
	t.Helper()
	stub := filepath.Join(dir, "selective-stub.mjs")
	filesJSON, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	src := `import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import path from "node:path";
function argVal(flag) {
  const i = process.argv.indexOf(flag);
  return i >= 0 ? process.argv[i + 1] : undefined;
}
const job = JSON.parse(readFileSync(argVal("--job"), "utf8"));
const outDir = path.join(argVal("--out"), job.id);
mkdirSync(outDir, { recursive: true });
for (const name of ` + string(filesJSON) + `) {
  writeFileSync(path.join(outDir, name), "stub-" + name);
}
process.exit(0);
`
	if err := os.WriteFile(stub, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return stub
}

// TestPublishPipelineArtifacts_IndexBinding: a whitebox proof that a missing
// MIDDLE artifact leaves published[1]=="" rather than sliding artifacts[2]'s
// path into slot 1 — the direct fix for the reviewed positional-skew bug.
func TestPublishPipelineArtifacts_IndexBinding(t *testing.T) {
	dir := t.TempDir()
	outRoot := filepath.Join(dir, "out")
	srcDir := filepath.Join(outRoot, "job1")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "final.png"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	// qa-report.json (artifacts[1]) intentionally NOT written — the missing middle artifact.
	if err := os.WriteFile(filepath.Join(srcDir, "extra.json"), []byte("e"), 0o644); err != nil {
		t.Fatal(err)
	}

	mediaDir := filepath.Join(dir, "media")
	published, err := publishPipelineArtifacts(outRoot, "job1", []string{"final.png", "qa-report.json", "extra.json"}, mediaDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(published) != 3 {
		t.Fatalf("published = %#v, want 3 index-aligned entries (matching len(artifacts))", published)
	}
	if published[0] == "" {
		t.Error("published[0] (final.png, present) must be set")
	}
	if published[1] != "" {
		t.Errorf("published[1] (qa-report.json, MISSING) must be \"\", got %q — this is the positional-skew bug", published[1])
	}
	if published[2] == "" {
		t.Error("published[2] (extra.json, present) must still be published even though [1] is missing")
	}
	if _, statErr := os.Stat(filepath.Join(mediaDir, "job1-extra.json")); statErr != nil {
		t.Errorf("extra.json not actually copied to the media dir: %v", statErr)
	}
}

// TestRunPipelineJob_MiddleArtifactMissingDoesNotSkewQAPath: the E2E proof
// through p.Run — with artifacts ["final.png","qa-report.json","extra.json"]
// and a CLI that produces only final.png + extra.json (skipping the middle
// one), the result must OMIT qa_report_path entirely (never misreport
// extra.json's path as the QA report), while extra.json still lands in
// MediaDir under its own name.
func TestRunPipelineJob_MiddleArtifactMissingDoesNotSkewQAPath(t *testing.T) {
	requireNodePipeline(t)
	jobDir := t.TempDir()
	script := writeSelectiveArtifactStub(t, jobDir, "final.png", "extra.json")
	cfg := pipelineTestConfig(t, script, jobDir, 30, []string{"final.png", "qa-report.json", "extra.json"})

	jobID := "middle-missing-case"
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
	var out map[string]json.RawMessage
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	if _, has := out["qa_report_path"]; has {
		t.Errorf("qa_report_path must be ABSENT when artifacts[1] (qa-report.json) is missing, got %s", out["qa_report_path"])
	}
	wantExtra := filepath.Join(cfg.MediaDir, jobID+"-extra.json")
	if _, statErr := os.Stat(wantExtra); statErr != nil {
		t.Errorf("extra.json (artifacts[2]) should still be published to MediaDir despite artifacts[1] missing: %v", statErr)
	}
}

// --- SP3 polish: empty-artifacts guard hoisted above takeMediaSlot ---

// TestRunPipelineJob_EmptyArtifactsGuardHoistedAboveMediaSlot: a pipeline
// entry with no artifacts configured (only reachable via a Config assembled
// directly, bypassing Load/validatePipelines — see PipelineSpec.valid()'s
// doc comment) can never publish a result, so it must be refused BEFORE
// contending for the mediaSlot — never after burning part of the GPU-wait
// window on a config that could not have run regardless. Proven here by
// holding mediaSlot for the whole test and asserting the defer still returns
// near-instantly instead of waiting out cfg.GPUWaitMs.
func TestRunPipelineJob_EmptyArtifactsGuardHoistedAboveMediaSlot(t *testing.T) {
	// A REAL, resolvable script (not a dummy path) is deliberate: it isolates
	// this test to the artifacts-guard-vs-takeMediaSlot ordering specifically.
	// A dummy/unresolvable script would fail at gpugen.ResolveScript BEFORE
	// takeMediaSlot regardless of where the artifacts guard sits, which would
	// make this test pass for the wrong reason even without the fix.
	script := fakeSceneSwapScript(t)
	cfg := config.Default()
	cfg.GPUWaitMs = 5000 // large enough that "returned fast" is a meaningful signal
	cfg.Pipelines = map[string]config.PipelineSpec{
		"scene-swap": {Script: script, Workdir: filepath.Dir(script), TimeoutSec: 30, Artifacts: nil},
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
	elapsed := time.Since(start)
	if res.OK || !res.Deferred {
		t.Fatalf("expected a clean defer for a no-artifacts pipeline, got OK=%v", res.OK)
	}
	if !strings.Contains(res.Reason, "no artifacts configured") {
		t.Errorf("Reason = %q, want it to name the no-artifacts guard", res.Reason)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("took %v — the empty-artifacts guard must fire BEFORE contending on the (already held) media slot, "+
			"not burn part of the %dms GPU-wait window first", elapsed, cfg.GPUWaitMs)
	}
}

// --- SP3 polish: publishPipelineArtifacts logs WHICH optional-artifact
// outcome happened, instead of silently treating "never produced" and
// "produced but the copy failed" the same way ---

// captureLog redirects the standard logger's output for the duration of the
// test and restores it afterward (log.SetOutput is process-global state).
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return &buf
}

// TestPublishPipelineArtifacts_LogsOptionalArtifactNotProduced: an optional
// artifact the CLI simply never wrote (a legitimate, expected shape — e.g.
// no QA report for this tier) logs a "not produced" line, not a "copy
// failed" one, and still returns success (the existing, unchanged result
// semantics — this only adds a log line).
func TestPublishPipelineArtifacts_LogsOptionalArtifactNotProduced(t *testing.T) {
	dir := t.TempDir()
	outRoot := filepath.Join(dir, "out")
	srcDir := filepath.Join(outRoot, "job1")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "final.png"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	// qa-report.json (the optional artifact) intentionally never written.

	buf := captureLog(t)
	mediaDir := filepath.Join(dir, "media")
	published, err := publishPipelineArtifacts(outRoot, "job1", []string{"final.png", "qa-report.json"}, mediaDir)
	if err != nil {
		t.Fatalf("a missing OPTIONAL artifact must not fail publish: %v", err)
	}
	if published[1] != "" {
		t.Errorf("published[1] must be \"\" for a never-produced optional artifact, got %q", published[1])
	}
	got := buf.String()
	if !strings.Contains(got, `optional artifact "qa-report.json" not produced`) {
		t.Fatalf("log output = %q, want it to say the artifact was not produced", got)
	}
	if strings.Contains(got, "copy") {
		t.Fatalf("log output = %q, must not claim a copy failure for a simply-missing artifact", got)
	}
}

// TestPublishPipelineArtifacts_LogsOptionalArtifactCopyFailed: an optional
// artifact the CLI DID produce, but whose copy into mediaDir fails, must log
// a DIFFERENT line naming the copy failure — today (pre-fix) this silently
// looked identical to "never produced". The copy failure is forced
// portably (no platform-fiddly read-only-dir/ACL trick) by pre-creating a
// DIRECTORY at the exact destination path: os.WriteFile then fails because
// the target is a directory, on both POSIX and Windows.
func TestPublishPipelineArtifacts_LogsOptionalArtifactCopyFailed(t *testing.T) {
	dir := t.TempDir()
	outRoot := filepath.Join(dir, "out")
	srcDir := filepath.Join(outRoot, "job1")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "final.png"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "qa-report.json"), []byte("q"), 0o644); err != nil {
		t.Fatal(err)
	}

	mediaDir := filepath.Join(dir, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	destPath := filepath.Join(mediaDir, "job1-qa-report.json")
	if err := os.MkdirAll(destPath, 0o755); err != nil {
		t.Fatal(err)
	}

	buf := captureLog(t)
	published, err := publishPipelineArtifacts(outRoot, "job1", []string{"final.png", "qa-report.json"}, mediaDir)
	if err != nil {
		t.Fatalf("a failed OPTIONAL artifact copy must not fail the whole publish: %v", err)
	}
	if published[1] != "" {
		t.Errorf("published[1] must be \"\" when the copy failed, got %q", published[1])
	}
	got := buf.String()
	if !strings.Contains(got, `optional artifact "qa-report.json" was produced but copy to media dir failed`) {
		t.Fatalf("log output = %q, want it to distinguish a produced-but-copy-failed optional artifact", got)
	}
}
