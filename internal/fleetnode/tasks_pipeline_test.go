package fleetnode

// Task 6 Step 1: buildPipelineJob's ack-time validation matrix, materialization
// (job.json with injected absolute ref paths), advertisement wiring, and the
// cleanup-scope contract (cleanup ALWAYS removes the whole job dir, not just
// the fetched assets — see buildPipelineJob's doc comment for why).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// pngServer serves minimal valid-magic PNG bytes at /product.png, /logo.png,
// /background.png — enough for ingress.go's sniffImageExt (it checks only the
// 4-byte \x89PNG prefix). httptest binds 127.0.0.1, which passes checkAllowed's
// loopback rule with no extraAllow entry needed.
func pngServer(t *testing.T) *httptest.Server {
	t.Helper()
	body := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testPipelineConfig returns a Config with one valid "scene-swap" pipeline
// entry and Home pinned to an isolated temp dir (so BaseDir()/pipeline-jobs
// never touches the real ~/.local-offload).
func testPipelineConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Home = t.TempDir()
	cfg.Pipelines = map[string]config.PipelineSpec{
		"scene-swap": {
			Script:     "does-not-need-to-exist-for-build.mjs",
			Workdir:    "does-not-need-to-exist-for-build",
			TimeoutSec: 2400,
			Artifacts:  []string{"final.png", "qa-report.json"},
		},
	}
	return cfg
}

func validPipelinePayload(srv *httptest.Server, id string) []byte {
	payload := map[string]any{
		"job_spec": map[string]any{
			"id":         id,
			"canvas":     map[string]any{"w": 1024, "h": 1024},
			"background": map[string]any{"mode": "generate", "prompt": "a clean studio backdrop"},
		},
		"image_refs": map[string]any{
			"product": srv.URL + "/product.png",
			"logo":    srv.URL + "/logo.png",
		},
		"tier": "16gb",
	}
	b, _ := json.Marshal(payload)
	return b
}

func TestBuildPipelineJob_ValidPayload_MaterializesJobJSON(t *testing.T) {
	srv := pngServer(t)
	cfg := testPipelineConfig(t)

	req, cleanup, err := buildPipelineJob(context.Background(), cfg, "scene-swap", validPipelinePayload(srv, "web-1a2b3c4d"))
	if err != nil {
		t.Fatalf("buildPipelineJob: unexpected error: %v", err)
	}
	defer cleanup()

	if req.Task != core.TaskPipelineJob {
		t.Fatalf("Task = %q, want %q", req.Task, core.TaskPipelineJob)
	}
	jobPath, _ := req.Params["job_path"].(string)
	if jobPath == "" {
		t.Fatal("Params[\"job_path\"] missing")
	}
	wantDir := filepath.Join(cfg.BaseDir(), "pipeline-jobs", "web-1a2b3c4d")
	if !strings.HasPrefix(jobPath, wantDir) {
		t.Fatalf("job_path %q not under materialization dir %q", jobPath, wantDir)
	}

	raw, rerr := os.ReadFile(jobPath)
	if rerr != nil {
		t.Fatalf("job.json not written: %v", rerr)
	}
	var jobSpec map[string]json.RawMessage
	if err := json.Unmarshal(raw, &jobSpec); err != nil {
		t.Fatalf("job.json not valid JSON: %v", err)
	}
	var product, logo string
	if err := json.Unmarshal(jobSpec["product"], &product); err != nil || product == "" {
		t.Fatalf("job.json missing injected absolute product path: %v (%s)", err, jobSpec["product"])
	}
	if !filepath.IsAbs(product) {
		t.Fatalf("injected product path %q is not absolute", product)
	}
	if err := json.Unmarshal(jobSpec["logo"], &logo); err != nil || logo == "" {
		t.Fatalf("job.json missing injected absolute logo path: %v", err)
	}
	if _, err := os.Stat(product); err != nil {
		t.Errorf("fetched product asset not on disk: %v", err)
	}

	if req.Params["out_root"].(string) == "" {
		t.Error("Params[\"out_root\"] missing")
	}
	if req.Params["tier"].(string) != "16gb" {
		t.Errorf("Params[\"tier\"] = %v, want 16gb", req.Params["tier"])
	}
	if req.Params["pipeline_task"].(string) != "scene-swap" {
		t.Errorf("Params[\"pipeline_task\"] = %v, want scene-swap", req.Params["pipeline_task"])
	}
}

// TestBuildPipelineJob_StockBackgroundInjectsPath: mode=="stock" requires and
// fetches image_refs.background, injecting job_spec.background.path.
func TestBuildPipelineJob_StockBackgroundInjectsPath(t *testing.T) {
	srv := pngServer(t)
	cfg := testPipelineConfig(t)
	payload := map[string]any{
		"job_spec": map[string]any{
			"id":         "stock-case",
			"background": map[string]any{"mode": "stock"},
		},
		"image_refs": map[string]any{
			"product":    srv.URL + "/product.png",
			"logo":       srv.URL + "/logo.png",
			"background": srv.URL + "/background.png",
		},
		"tier": "8gb",
	}
	b, _ := json.Marshal(payload)
	req, cleanup, err := buildPipelineJob(context.Background(), cfg, "scene-swap", b)
	if err != nil {
		t.Fatalf("buildPipelineJob: unexpected error: %v", err)
	}
	defer cleanup()
	jobPath := req.Params["job_path"].(string)
	raw, _ := os.ReadFile(jobPath)
	var jobSpec map[string]json.RawMessage
	json.Unmarshal(raw, &jobSpec)
	var bg map[string]json.RawMessage
	if err := json.Unmarshal(jobSpec["background"], &bg); err != nil {
		t.Fatalf("background not an object: %v", err)
	}
	var path string
	if err := json.Unmarshal(bg["path"], &path); err != nil || path == "" {
		t.Fatalf("background.path not injected: %v (%s)", err, bg["path"])
	}
	if !filepath.IsAbs(path) {
		t.Errorf("background.path %q not absolute", path)
	}
}

// TestBuildPipelineJob_ValidationMatrix: each rule violation surfaces a 400-
// class error naming the offending field. All validation happens BEFORE any
// network fetch (ack-time, out of buildPipelineJob) — these payloads never
// reach pngServer for the cases that should fail before ref-fetch.
func TestBuildPipelineJob_ValidationMatrix(t *testing.T) {
	srv := pngServer(t)
	cfg := testPipelineConfig(t)

	base := func() map[string]any {
		return map[string]any{
			"job_spec": map[string]any{
				"id":         "valid-id-123",
				"background": map[string]any{"mode": "generate"},
			},
			"image_refs": map[string]any{
				"product": srv.URL + "/product.png",
				"logo":    srv.URL + "/logo.png",
			},
			"tier": "16gb",
		}
	}

	cases := []struct {
		name      string
		mutate    func(p map[string]any)
		wantField string
	}{
		{
			name: "missing job_spec",
			mutate: func(p map[string]any) {
				delete(p, "job_spec")
			},
			wantField: "job_spec",
		},
		{
			name: "missing job_spec.id",
			mutate: func(p map[string]any) {
				js := p["job_spec"].(map[string]any)
				delete(js, "id")
			},
			wantField: "job_spec.id",
		},
		{
			name: "invalid job_spec.id chars",
			mutate: func(p map[string]any) {
				js := p["job_spec"].(map[string]any)
				js["id"] = "not a valid id!!"
			},
			wantField: "job_spec.id",
		},
		{
			name: "missing tier",
			mutate: func(p map[string]any) {
				p["tier"] = ""
			},
			wantField: "tier",
		},
		{
			name: "missing image_refs.product",
			mutate: func(p map[string]any) {
				refs := p["image_refs"].(map[string]any)
				delete(refs, "product")
			},
			wantField: "image_refs.product",
		},
		{
			name: "missing image_refs.logo",
			mutate: func(p map[string]any) {
				refs := p["image_refs"].(map[string]any)
				delete(refs, "logo")
			},
			wantField: "image_refs.logo",
		},
		{
			name: "stock background missing image_refs.background",
			mutate: func(p map[string]any) {
				js := p["job_spec"].(map[string]any)
				js["background"] = map[string]any{"mode": "stock"}
			},
			wantField: "image_refs.background",
		},
		{
			name: "job_spec already contains product",
			mutate: func(p map[string]any) {
				js := p["job_spec"].(map[string]any)
				js["product"] = "/should/not/be/here.png"
			},
			wantField: "product",
		},
		{
			name: "job_spec already contains logo",
			mutate: func(p map[string]any) {
				js := p["job_spec"].(map[string]any)
				js["logo"] = "/should/not/be/here.png"
			},
			wantField: "logo",
		},
		{
			name: "job_spec.background already contains path",
			mutate: func(p map[string]any) {
				js := p["job_spec"].(map[string]any)
				js["background"] = map[string]any{"mode": "generate", "path": "/nope.png"}
			},
			wantField: "path",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base()
			c.mutate(p)
			b, _ := json.Marshal(p)
			_, cleanup, err := buildPipelineJob(context.Background(), cfg, "scene-swap", b)
			if cleanup != nil {
				defer cleanup()
			}
			if err == nil {
				t.Fatalf("expected a validation error, got none")
			}
			if !strings.Contains(err.Error(), c.wantField) {
				t.Errorf("error %q does not name field %q", err.Error(), c.wantField)
			}
		})
	}
}

// TestBuildPipelineJob_CleanupRemovesWholeJobDir: the returned cleanup removes
// the ENTIRE materialization dir (assets/ + job.json + out/), not just the
// fetched-refs subdir FetchRefs itself would remove.
func TestBuildPipelineJob_CleanupRemovesWholeJobDir(t *testing.T) {
	srv := pngServer(t)
	cfg := testPipelineConfig(t)
	req, cleanup, err := buildPipelineJob(context.Background(), cfg, "scene-swap", validPipelinePayload(srv, "cleanup-case"))
	if err != nil {
		t.Fatalf("buildPipelineJob: unexpected error: %v", err)
	}
	jobDir := filepath.Join(cfg.BaseDir(), "pipeline-jobs", "cleanup-case")
	if _, err := os.Stat(jobDir); err != nil {
		t.Fatalf("job dir not materialized: %v", err)
	}
	if _, err := os.Stat(req.Params["job_path"].(string)); err != nil {
		t.Fatalf("job.json not materialized: %v", err)
	}
	cleanup()
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove the whole job dir: stat err = %v", err)
	}
}

// TestBuildPipelineJob_DuplicateJobSpecIDRefusedAtAck: a second dispatch
// carrying the SAME job_spec.id as a still-in-flight (not yet cleaned up)
// first job must be refused at ack — not silently share the job dir, which
// would let the first job's eventual cleanup() rip the second job's live
// assets/job.json out from under it while it's still running.
func TestBuildPipelineJob_DuplicateJobSpecIDRefusedAtAck(t *testing.T) {
	srv := pngServer(t)
	cfg := testPipelineConfig(t)

	req1, cleanup1, err1 := buildPipelineJob(context.Background(), cfg, "scene-swap", validPipelinePayload(srv, "dup-case"))
	if err1 != nil {
		t.Fatalf("first buildPipelineJob: unexpected error: %v", err1)
	}
	defer cleanup1()
	firstJobPath := req1.Params["job_path"].(string)
	firstJobBytes, err := os.ReadFile(firstJobPath)
	if err != nil {
		t.Fatalf("first job's job.json missing: %v", err)
	}

	// Second dispatch, same job_spec.id, while the first is still "in flight"
	// (its cleanup has not run yet).
	_, cleanup2, err2 := buildPipelineJob(context.Background(), cfg, "scene-swap", validPipelinePayload(srv, "dup-case"))
	if cleanup2 != nil {
		cleanup2()
	}
	if err2 == nil {
		t.Fatal("expected the second dispatch with a duplicate job_spec.id to be refused at ack")
	}
	if !strings.Contains(err2.Error(), "dup-case") || !strings.Contains(err2.Error(), "already in flight") {
		t.Errorf("error = %q, want it to name the id and say it is already in flight", err2.Error())
	}

	// The first job's directory/job.json must be completely UNTOUCHED by the
	// refused second attempt.
	gotBytes, err := os.ReadFile(firstJobPath)
	if err != nil {
		t.Fatalf("first job's job.json was disturbed by the refused duplicate: %v", err)
	}
	if string(gotBytes) != string(firstJobBytes) {
		t.Error("first job's job.json content changed after the refused duplicate dispatch")
	}
}

// TestBuildPipelineJob_UnconfiguredPipeline: a task_type with no cfg.Pipelines
// entry is a clean ack-time error (BuildRequest's taskConfigured gate normally
// catches this first; buildPipelineJob defends the same rule directly).
func TestBuildPipelineJob_UnconfiguredPipeline(t *testing.T) {
	cfg := config.Default() // no Pipelines configured
	_, cleanup, err := buildPipelineJob(context.Background(), cfg, "scene-swap", []byte(`{}`))
	if cleanup != nil {
		cleanup()
	}
	if err == nil {
		t.Fatal("expected an error for an unconfigured pipeline")
	}
}

// TestSweepOrphanedPipelineJobs_RemovesEntries: a fake orphan dir (as a crash
// would leave behind — jobs are in-memory, so nothing tracks it after an
// ungraceful stop) is removed by the sweep, and the count reflects it.
func TestSweepOrphanedPipelineJobs_RemovesEntries(t *testing.T) {
	cfg := config.Default()
	cfg.Home = t.TempDir()
	jobsDir := filepath.Join(cfg.BaseDir(), "pipeline-jobs")
	for _, id := range []string{"orphan-1", "orphan-2"} {
		d := filepath.Join(jobsDir, id, "assets")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(jobsDir, id, "job.json"), []byte(`{"id":"`+id+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	swept, err := SweepOrphanedPipelineJobs(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if swept != 2 {
		t.Fatalf("swept = %d, want 2", swept)
	}
	entries, rerr := os.ReadDir(jobsDir)
	if rerr != nil {
		t.Fatalf("pipeline-jobs dir itself should survive (only its CONTENTS are swept): %v", rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("pipeline-jobs dir not empty after sweep: %v", entries)
	}
}

// TestSweepOrphanedPipelineJobs_MissingDirIsNotAnError: a fresh install (or a
// node that has never served a pipeline job) has no pipeline-jobs/ dir at
// all — that is normal, not a sweep failure.
func TestSweepOrphanedPipelineJobs_MissingDirIsNotAnError(t *testing.T) {
	cfg := config.Default()
	cfg.Home = t.TempDir() // pipeline-jobs/ never created under this fresh Home
	swept, err := SweepOrphanedPipelineJobs(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if swept != 0 {
		t.Fatalf("swept = %d, want 0", swept)
	}
}

// TestSweepOrphanedPipelineJobs_UnblocksReusedID: the actual bug scenario —
// after a crash, a fresh dispatch reusing the same job_spec.id must succeed
// (not be falsely refused as "already in flight") once the sweep has run.
func TestSweepOrphanedPipelineJobs_UnblocksReusedID(t *testing.T) {
	srv := pngServer(t)
	cfg := testPipelineConfig(t)

	// Simulate a crash: a job dir left on disk with NO corresponding in-memory
	// Jobs-store entry (there is none in this test — buildPipelineJob is
	// called directly, never through a Jobs store).
	orphanID := "crash-orphan"
	if _, cleanup, err := buildPipelineJob(context.Background(), cfg, "scene-swap", validPipelinePayload(srv, orphanID)); err != nil {
		t.Fatalf("setup: unexpected error: %v", err)
	} else {
		_ = cleanup // deliberately NOT called — this is the orphan
	}

	// Without a sweep, re-dispatching the same job_spec.id is falsely refused.
	if _, cleanup, err := buildPipelineJob(context.Background(), cfg, "scene-swap", validPipelinePayload(srv, orphanID)); err == nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatal("expected the pre-sweep re-dispatch to be refused (the orphan is still on disk)")
	}

	if _, err := SweepOrphanedPipelineJobs(cfg); err != nil {
		t.Fatalf("sweep: unexpected error: %v", err)
	}

	// After the sweep (as fleet-serve runs at startup), the SAME job_spec.id
	// must be dispatchable again.
	req, cleanup, err := buildPipelineJob(context.Background(), cfg, "scene-swap", validPipelinePayload(srv, orphanID))
	if err != nil {
		t.Fatalf("post-sweep re-dispatch still refused: %v", err)
	}
	defer cleanup()
	if req.Params["job_id"].(string) != orphanID {
		t.Errorf("job_id = %v, want %q", req.Params["job_id"], orphanID)
	}
}

// TestSupportedTasks_IncludesPipelineIffConfigured: SupportedTasks advertises
// a pipeline's task_type key exactly when cfg.Pipelines has a valid entry for
// it — mirroring every hardcoded route's taskConfigured gate.
func TestSupportedTasks_IncludesPipelineIffConfigured(t *testing.T) {
	cfg := config.Default()
	if contains(SupportedTasks(cfg), "scene-swap") {
		t.Fatal("scene-swap should not be advertised with no Pipelines entry")
	}
	cfg.Pipelines = map[string]config.PipelineSpec{
		"scene-swap": {
			Script: "s.mjs", Workdir: "wd", TimeoutSec: 10, Artifacts: []string{"final.png"},
		},
	}
	if !contains(SupportedTasks(cfg), "scene-swap") {
		t.Fatal("scene-swap should be advertised once configured")
	}
	if !taskConfigured(cfg, "scene-swap") {
		t.Fatal("taskConfigured should report scene-swap configured")
	}
}

// TestFamilyFor_PipelineTaskClaimsNoFamily: a pipeline task's sizing rides on
// its task-scoped footprint, not an advertised family.
func TestFamilyFor_PipelineTaskClaimsNoFamily(t *testing.T) {
	cfg := config.Default()
	cfg.Pipelines = map[string]config.PipelineSpec{
		"scene-swap": {Script: "s.mjs", Workdir: "wd", TimeoutSec: 10, Artifacts: []string{"final.png"}},
	}
	if f := familyFor(cfg, "scene-swap"); f != "" {
		t.Errorf("familyFor(scene-swap) = %q, want \"\" (no family claim)", f)
	}
}

// TestBuildRequest_RoutesConfiguredPipeline: the dispatch-facing entry point
// (BuildRequest) routes an arbitrary cfg.Pipelines key to buildPipelineJob,
// exactly like image-gen/run-graph route to their own builders.
func TestBuildRequest_RoutesConfiguredPipeline(t *testing.T) {
	srv := pngServer(t)
	cfg := testPipelineConfig(t)
	req, cleanup, err := BuildRequest(context.Background(), cfg, true, "scene-swap", validPipelinePayload(srv, "routed-case"))
	if err != nil {
		t.Fatalf("BuildRequest: unexpected error: %v", err)
	}
	defer cleanup()
	if req.Task != core.TaskPipelineJob {
		t.Fatalf("Task = %q, want %q", req.Task, core.TaskPipelineJob)
	}
}

// TestBuildRequest_UnconfiguredPipelineRejected: an unconfigured task_type
// (not one of the 5 hardcoded families, not a cfg.Pipelines key) is refused
// exactly like today, naming the supported set.
func TestBuildRequest_UnconfiguredPipelineRejected(t *testing.T) {
	cfg := config.Default()
	_, cleanup, err := BuildRequest(context.Background(), cfg, true, "scene-swap", []byte(`{}`))
	cleanup()
	if err == nil {
		t.Fatal("expected an unsupported task_type error")
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
