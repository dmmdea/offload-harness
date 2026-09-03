package fleetnode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/hostsample"
)

// fakeRunner injects the test's behavior for Runner.
type fakeRunner struct {
	mu   sync.Mutex
	reqs []core.Request
	fn   func(ctx context.Context, req core.Request) core.Result
}

func (f *fakeRunner) Run(ctx context.Context, req core.Request) core.Result {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(ctx, req)
	}
	return core.Result{OK: true, Data: json.RawMessage(`{"image_path":"x.png"}`)}
}

func (f *fakeRunner) requests() []core.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Request(nil), f.reqs...)
}

// imageCfg is a config advertising image-gen + run-graph (deterministic task set).
func imageCfg() config.Config {
	return config.Config{ImageGenScript: "C:/x/comfy-generate.mjs", RunGraphScript: "C:/x/comfy-run-graph.mjs"}
}

func goodSnapshot() (Snapshot, bool) {
	return Snapshot{TotalGiB: 16, FreeGiB: 12.5, At: time.Now()}, true
}

// newTestServer wires a Server over a fresh Jobs store; the store is drained
// at test end so its goroutines stop.
func newTestServer(t *testing.T, cfg config.Config, r Runner, opts *Options) (*Server, *Jobs) {
	t.Helper()
	jobs := NewJobs(time.Hour, cfg.FleetConcurrencyLimit())
	t.Cleanup(func() { jobs.DrainAndStop(2 * time.Second) })
	o := Options{
		NodeID:     "testnode",
		Snapshot:   goodSnapshot,
		Footprints: func() []FootprintEntry { return nil },
		GpuVendor:  "nvidia",
		GpuArch:    "ampere",
		Cfg:        cfg,
	}
	if opts != nil {
		o = *opts
		o.Cfg = cfg
	}
	return New(r, jobs, o), jobs
}

// do runs one request through the routed handler.
func do(t *testing.T, s *Server, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response not JSON: %v (body %q)", err, rec.Body.String())
	}
	return m
}

func wantErrorShape(t *testing.T, rec *httptest.ResponseRecorder, status int, contains string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["status"] != "error" {
		t.Fatalf("status field = %v, want error", m["status"])
	}
	msg, _ := m["error"].(string)
	if !strings.Contains(msg, contains) {
		t.Fatalf("error = %q, want it to contain %q", msg, contains)
	}
}

// pollJob polls GET /fleet/jobs/{id} until the state matches (or times out).
func pollJob(t *testing.T, s *Server, id string, want JobState) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := do(t, s, http.MethodGet, "/fleet/jobs/"+id, "", nil)
		if rec.Code == http.StatusOK {
			m := decodeMap(t, rec)
			if m["state"] == string(want) {
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never reached state %s", id, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHealthGoldenShape pins the WHOLE payload by exact equality, so any field
// added to health has to be added here deliberately. The four 0.100.0 capacity
// fields (jobs_queued/jobs_running/max_concurrent_jobs/max_queue_depth) are
// always present — 0 is a meaningful value for each, so omitempty would make an
// unlimited limit indistinguishable from a node too old to publish one. The
// values here are the built-in defaults an unconfigured node resolves.
func TestHealthGoldenShape(t *testing.T) {
	opts := &Options{
		NodeID:   "node-a",
		Snapshot: goodSnapshot,
		Footprints: func() []FootprintEntry {
			return []FootprintEntry{{ModelFamily: "sdxl", Quant: "bf16", TaskType: "image-gen", VramPeakGiB: 9.6}}
		},
		GpuVendor: "nvidia",
		GpuArch:   "blackwell",
	}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("health not JSON: %v", err)
	}
	var want map[string]any
	golden := `{
		"node_id": "node-a", "schema_version": 1,
		"gpu_vendor": "nvidia", "gpu_arch": "blackwell",
		"vram_total_gb": 16, "vram_free_gb": 12.5,
		"gpu_util_pct": 0, "gpu_util_known": false,
		"supported_task_types": ["image-gen", "run-graph"],
		"loadable_model_families": ["sdxl", "comfy-graph"],
		"model_footprints": [{"model_family":"sdxl","quant":"bf16","task_type":"image-gen","vram_peak_gb":9.6}],
		"queue_depth": 0,
		"jobs_queued": 0, "jobs_running": 0,
		"max_concurrent_jobs": 4, "max_queue_depth": 32
	}`
	if err := json.Unmarshal([]byte(golden), &want); err != nil {
		t.Fatalf("golden not JSON: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("health shape mismatch:\n got: %s\nwant: %s", rec.Body.String(), golden)
	}
}

func TestHealthEmptyListsAreArraysNotNull(t *testing.T) {
	s, _ := newTestServer(t, config.Config{}, &fakeRunner{}, nil)
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, field := range []string{`"supported_task_types":[]`, `"loadable_model_families":[]`, `"model_footprints":[]`} {
		if !strings.Contains(body, field) {
			t.Fatalf("health body missing %s: %s", field, body)
		}
	}
}

func TestHealth503WhenSnapshotUnavailable(t *testing.T) {
	opts := &Options{
		NodeID:   "n",
		Snapshot: func() (Snapshot, bool) { return Snapshot{}, false },
	}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	wantErrorShape(t, rec, http.StatusServiceUnavailable, "snapshot")
}

// A snapshot older than the staleness bound is as bad as no snapshot: if
// nvidia-smi starts failing (driver reset), the sampler keeps the last good
// snapshot forever — health must stop serving it as live after 30s, or
// hours-stale 200s mislead the dispatcher's routing.
func TestHealth503WhenSnapshotStale(t *testing.T) {
	opts := &Options{
		NodeID: "n",
		Snapshot: func() (Snapshot, bool) {
			return Snapshot{TotalGiB: 16, FreeGiB: 12, At: time.Now().Add(-time.Minute)}, true
		},
	}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	wantErrorShape(t, rec, http.StatusServiceUnavailable, "vram snapshot stale")
}

// A snapshot just inside the bound still serves 200 (the 2s sampler keeps At
// fresh in normal operation).
func TestHealthFreshSnapshotWithinBoundServes200(t *testing.T) {
	opts := &Options{
		NodeID: "n",
		Snapshot: func() (Snapshot, bool) {
			return Snapshot{TotalGiB: 16, FreeGiB: 12, At: time.Now().Add(-5 * time.Second)}, true
		},
	}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestHealthIncludesGpuDevicesWhenPresent locks the wiring from
// Snapshot.Devices to the health JSON's gpu_devices[] — the additive field
// the multi-GPU fix adds. The headline vram_total_gb/vram_free_gb pair (16.3
// / 13.8, matching the LARGER device) must NOT match the smaller card: it
// proves handleHealth passes the snapshot through rather than re-deriving it.
func TestHealthIncludesGpuDevicesWhenPresent(t *testing.T) {
	opts := &Options{
		NodeID: "n",
		Snapshot: func() (Snapshot, bool) {
			return Snapshot{
				TotalGiB: 24.0,
				FreeGiB:  22.0,
				Devices: []GPUDevice{
					{Index: 0, Name: "NVIDIA GeForce RTX 5060 Ti", TotalGiB: 16.0, FreeGiB: 15.0},
					{Index: 1, Name: "NVIDIA GeForce RTX 3090", TotalGiB: 24.0, FreeGiB: 22.0},
				},
				At: time.Now(),
			}, true
		},
	}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	devices, ok := m["gpu_devices"].([]any)
	if !ok || len(devices) != 2 {
		t.Fatalf("gpu_devices = %v, want a 2-entry array", m["gpu_devices"])
	}
	first, _ := devices[0].(map[string]any)
	if first["index"] != float64(0) || first["name"] != "NVIDIA GeForce RTX 5060 Ti" || first["vram_total_gb"] != float64(16) {
		t.Errorf("gpu_devices[0] = %+v", first)
	}
	second, _ := devices[1].(map[string]any)
	if second["index"] != float64(1) || second["name"] != "NVIDIA GeForce RTX 3090" {
		t.Errorf("gpu_devices[1] = %+v", second)
	}
	if m["vram_total_gb"] != float64(24) || m["vram_free_gb"] != float64(22) {
		t.Fatalf("headline vram_total_gb/vram_free_gb = %v/%v, want the LARGER device's numbers (24/22), not the smaller card's", m["vram_total_gb"], m["vram_free_gb"])
	}
}

// TestHealthOmitsGpuDevicesWhenAbsent locks the no-regression contract: a
// single-source snapshot (windows-generic, or any node whose Devices is nil)
// must OMIT the gpu_devices key entirely — not publish an empty array — so a
// consumer can tell "no breakdown available" from "breakdown is empty".
func TestHealthOmitsGpuDevicesWhenAbsent(t *testing.T) {
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil) // newTestServer's default Options uses goodSnapshot, whose Devices is nil
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if _, present := m["gpu_devices"]; present {
		t.Fatalf("gpu_devices present when Snapshot.Devices is nil: %v", m["gpu_devices"])
	}
}

// TestHealthAdvertisesAccelerators: the manifest-declared accelerator list
// (Options.Accelerators — the fleet-serve verb reads it from installed.json's
// `accelerators`, ADR 0024) must reach the health payload so a delegator can
// route NPU-owned work here; a node with none must OMIT the key entirely (the
// pre-accelerator payload stays byte-identical, same rule as gpu_devices).
func TestHealthAdvertisesAccelerators(t *testing.T) {
	opts := &Options{
		NodeID:       "node-acc",
		Snapshot:     goodSnapshot,
		GpuVendor:    "nvidia",
		GpuArch:      "ampere",
		Accelerators: []string{"hailo-8l"},
	}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	accs, ok := m["accelerators"].([]any)
	if !ok || len(accs) != 1 || accs[0] != "hailo-8l" {
		t.Fatalf("accelerators = %v, want [hailo-8l]", m["accelerators"])
	}
	plain, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	mp := decodeMap(t, do(t, plain, http.MethodGet, "/fleet/health", "", nil))
	if _, present := mp["accelerators"]; present {
		t.Fatalf("accelerators key must be absent when the node has none, got %v", mp["accelerators"])
	}
}

func TestDispatchEchoExactness(t *testing.T) {
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	const id = "a3f9-XYZ_0123456789abcdef.fleet~job"
	rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"`+id+`","task_type":"image-gen","payload":{"prompt":"hi"}}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["job_id"] != id {
		t.Fatalf("job_id echo = %v, want exact %q", m["job_id"], id)
	}
	if m["status"] != "accepted" {
		t.Fatalf("status = %v, want accepted", m["status"])
	}
	pollJob(t, s, id, JobDone)
}

func TestDispatchMalformedJSON400(t *testing.T) {
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", `{"job_id": "x", nope}`, nil)
	wantErrorShape(t, rec, http.StatusBadRequest, "malformed")
}

func TestDispatchUnknownEnvelopeField400(t *testing.T) {
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"x","task_type":"image-gen","bogus_field":1,"payload":{"prompt":"hi"}}`, nil)
	wantErrorShape(t, rec, http.StatusBadRequest, "bogus_field")
}

func TestDispatchSizingFieldsAcceptedAndIgnored(t *testing.T) {
	fr := &fakeRunner{}
	s, _ := newTestServer(t, imageCfg(), fr, nil)
	rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"sz1","task_type":"image-gen","model_family":"sdxl","quant":"bf16","priority":3,
		  "width":1024,"height":1024,"num_frames":81,"params_b":3.5,"context_len":4096,
		  "num_layers":32,"hidden_dim":4096,"batch_size":1,"payload":{"prompt":"hi"}}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	pollJob(t, s, "sz1", JobDone)
	reqs := fr.requests()
	if len(reqs) != 1 {
		t.Fatalf("runner ran %d times, want 1", len(reqs))
	}
	// Sizing fields must NOT leak into the translated request params.
	if _, ok := reqs[0].Params["num_frames"]; ok {
		t.Fatal("envelope sizing field leaked into request params")
	}
}

func TestDispatchMissingJobID400(t *testing.T) {
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"task_type":"image-gen","payload":{"prompt":"hi"}}`, nil)
	wantErrorShape(t, rec, http.StatusBadRequest, "job_id required")
}

func TestDispatchUnknownTaskType400ListsSupported(t *testing.T) {
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"x","task_type":"llm","payload":{}}`, nil)
	wantErrorShape(t, rec, http.StatusBadRequest, "image-gen, run-graph")
}

func TestDispatchOversizedBody400(t *testing.T) {
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	big := strings.Repeat("a", maxDispatchBody+1024)
	rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"x","task_type":"image-gen","payload":{"prompt":"`+big+`"}}`, nil)
	wantErrorShape(t, rec, http.StatusBadRequest, "too large")
}

// Wrong Content-Type answers 400, not 415: the dispatch error taxonomy has a
// single caller-mistake class and the dispatcher greps one shape.
func TestDispatchWrongContentType400(t *testing.T) {
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"x","task_type":"image-gen","payload":{"prompt":"hi"}}`,
		map[string]string{"Content-Type": "text/plain"})
	wantErrorShape(t, rec, http.StatusBadRequest, "application/json")
}

func TestDispatchDrain503(t *testing.T) {
	s, jobs := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	jobs.DrainAndStop(10 * time.Millisecond)
	rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"x","task_type":"image-gen","payload":{"prompt":"hi"}}`, nil)
	wantErrorShape(t, rec, http.StatusServiceUnavailable, "node draining")
}

func TestDispatchRunGraphInvalidPayload400AtAck(t *testing.T) {
	fr := &fakeRunner{}
	s, _ := newTestServer(t, imageCfg(), fr, nil)
	rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"rg1","task_type":"run-graph","payload":{"graph":"not-an-object"}}`, nil)
	wantErrorShape(t, rec, http.StatusBadRequest, "graph must be a JSON object")
	if len(fr.requests()) != 0 {
		t.Fatal("invalid run-graph payload must die at the ack, not reach the runner")
	}
	// And the job must not exist — nothing was acked.
	rec = do(t, s, http.MethodGet, "/fleet/jobs/rg1", "", nil)
	wantErrorShape(t, rec, http.StatusNotFound, "unknown job")
}

// Duplicates in accepted/running/done all re-ack 202: the fleet contract
// treats ANY non-202 dispatch response as a refusal the dispatcher may answer
// by re-dispatching the same job_id to ANOTHER node — for a done job (lost
// ack + fast render) that would be a duplicate render fleet-wide. The
// dispatcher's tracker polls /fleet/jobs/{id} after the re-ack and finds the
// terminal state (with data) immediately.
func TestDuplicateDispatchReAcks202ThroughDone(t *testing.T) {
	release := make(chan struct{})
	fr := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		<-release
		return core.Result{OK: true, Data: json.RawMessage(`{"image_path":"out.png"}`)}
	}}
	s, _ := newTestServer(t, imageCfg(), fr, nil)
	body := `{"job_id":"dup1","task_type":"image-gen","payload":{"prompt":"hi"}}`
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want 202", rec.Code)
	}
	pollJob(t, s, "dup1", JobRunning)

	// Duplicate while running: idempotent re-ack, no second render.
	rec = do(t, s, http.MethodPost, "/fleet/dispatch", body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("duplicate-while-running status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["job_id"] != "dup1" || m["status"] != "accepted" {
		t.Fatalf("duplicate re-ack shape wrong: %s", rec.Body.String())
	}

	close(release)
	pollJob(t, s, "dup1", JobDone)

	// Duplicate after done: 202 re-ack (NOT a non-202 refusal — that would
	// invite a duplicate render on another node). The poll finds done + data.
	rec = do(t, s, http.MethodPost, "/fleet/dispatch", body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("duplicate-after-done status = %d, want 202 re-ack (body %s)", rec.Code, rec.Body.String())
	}
	m = decodeMap(t, rec)
	if m["job_id"] != "dup1" || m["status"] != "accepted" {
		t.Fatalf("duplicate-after-done re-ack shape wrong: %s", rec.Body.String())
	}
	pm := pollJob(t, s, "dup1", JobDone)
	data, ok := pm["data"].(map[string]any)
	if !ok || data["image_path"] != "out.png" {
		t.Fatalf("poll after re-ack missing data: %v", pm)
	}
	if len(fr.requests()) != 1 {
		t.Fatalf("runner ran %d times, want exactly 1", len(fr.requests()))
	}
}

// Duplicate after error: 409 — an EXPLICIT refusal (the one duplicate state
// where a non-202 is correct), so the dispatcher may legitimately try the
// failed job on another node. The reason embeds the job's recorded error.
func TestDuplicateDispatchAfterError409(t *testing.T) {
	fr := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		return core.Result{OK: false, Reason: "oom: cudaMalloc failed"}
	}}
	s, _ := newTestServer(t, imageCfg(), fr, nil)
	body := `{"job_id":"dupE","task_type":"image-gen","payload":{"prompt":"hi"}}`
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want 202", rec.Code)
	}
	pollJob(t, s, "dupE", JobError)

	rec = do(t, s, http.MethodPost, "/fleet/dispatch", body, nil)
	wantErrorShape(t, rec, http.StatusConflict, "job previously failed on this node: oom: cudaMalloc failed")
	if len(fr.requests()) != 1 {
		t.Fatalf("runner ran %d times, want exactly 1 (a 409 must never re-run)", len(fr.requests()))
	}
}

// --- Fix 2 (SP3 follow-up review): during drain, a re-dispatch of an
// already-known job_id got a flat 503 rather than a re-ack, because the
// Draining() check ran BEFORE the known-job lookup — so ANY dispatch while
// draining 503'd, even for a job_id this node already owns (running, done,
// or even a job it had already finished). The dispatcher's documented
// contract treats any non-202 as an outright refusal, so that flat 503
// could invite a duplicate render on another node for work already done
// here. The fix moves the known-job lookup BEFORE the Draining() check:
// known+JobError stays 409 (unchanged); known+anything else re-acks 202
// even mid-drain; only a genuinely UNKNOWN job_id is subject to the drain
// refusal (503, unchanged).

// TestDispatchDrainKnownRunningJobReAcks202 is case (a): a re-dispatch of a
// job_id that is RUNNING on this node, issued WHILE a drain is in progress,
// must re-ack 202 — it is not new work, so it must never be refused just
// because the node happens to be draining.
//
// Verified RED against the pre-fix ordering (Draining() checked before the
// known-job lookup): run this test against server.go as it stood before this
// commit and it fails with "status = 503, want 202" — the drain check short-
// circuits before ever reaching the s.jobs.Get(env.JobID) branch. See the
// report for the captured red output.
func TestDispatchDrainKnownRunningJobReAcks202(t *testing.T) {
	release := make(chan struct{})
	fr := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		<-release
		return core.Result{OK: true, Data: json.RawMessage(`{"image_path":"out.png"}`)}
	}}
	s, jobs := newTestServer(t, imageCfg(), fr, nil)
	body := `{"job_id":"drain-known","task_type":"image-gen","payload":{"prompt":"hi"}}`
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want 202", rec.Code)
	}
	pollJob(t, s, "drain-known", JobRunning)

	// Begin a drain with a timeout long enough that the still-blocked job
	// survives as JobRunning for the rest of this test (we release it
	// ourselves once the re-dispatch assertion is done).
	drainDone := make(chan struct{})
	go func() {
		jobs.DrainAndStop(5 * time.Second)
		close(drainDone)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !jobs.Draining() {
		if time.Now().After(deadline) {
			t.Fatal("Draining() never turned true")
		}
		time.Sleep(1 * time.Millisecond)
	}
	// The job must still be running (not yet force-marked interrupted) —
	// confirms we're inside the genuine "draining + known + not-yet-terminal"
	// window the finding describes, not a race that already resolved it.
	if v, ok := jobs.Get("drain-known"); !ok || v.State != JobRunning {
		t.Fatalf("test setup: job state = %+v (ok=%v), want still JobRunning while draining", v, ok)
	}

	rec = do(t, s, http.MethodPost, "/fleet/dispatch", body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("re-dispatch of a known RUNNING job during drain: status = %d, want 202 re-ack (body %s)", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["job_id"] != "drain-known" || m["status"] != "accepted" {
		t.Fatalf("re-ack shape wrong: %s", rec.Body.String())
	}

	close(release)
	<-drainDone
}

// TestDispatchDrainUnknownJobStill503 is case (b): a job_id this node has
// NEVER SEEN, dispatched while draining, still gets the flat 503 refusal —
// the fix only changes the outcome for a job_id the node already knows, so
// genuinely new work during a drain must still be refused (unchanged
// behavior, guarding against the fix accidentally widening the re-ack path).
func TestDispatchDrainUnknownJobStill503(t *testing.T) {
	s, jobs := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	jobs.DrainAndStop(10 * time.Millisecond)
	rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"drain-unknown","task_type":"image-gen","payload":{"prompt":"hi"}}`, nil)
	wantErrorShape(t, rec, http.StatusServiceUnavailable, "node draining")
}

// TestDispatchDrainKnownErroredJobStill409 is case (c): a job_id that
// already FAILED on this node, re-dispatched while draining, still answers
// 409 (unchanged) — a previously-failed job is a legitimate case for the
// dispatcher to retry elsewhere, drain or no drain, so this must NOT become
// a 202 re-ack just because the lookup now runs earlier.
func TestDispatchDrainKnownErroredJobStill409(t *testing.T) {
	fr := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		return core.Result{OK: false, Reason: "oom: cudaMalloc failed"}
	}}
	s, jobs := newTestServer(t, imageCfg(), fr, nil)
	body := `{"job_id":"drain-err","task_type":"image-gen","payload":{"prompt":"hi"}}`
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want 202", rec.Code)
	}
	pollJob(t, s, "drain-err", JobError)

	jobs.DrainAndStop(10 * time.Millisecond) // job is already terminal; nothing left to force-mark

	rec = do(t, s, http.MethodPost, "/fleet/dispatch", body, nil)
	wantErrorShape(t, rec, http.StatusConflict, "job previously failed on this node: oom: cudaMalloc failed")
}

// pipelineDispatchCfg returns a Config with one valid "scene-swap" pipeline
// route, Home pinned to an isolated temp dir (BaseDir()/pipeline-jobs never
// touches the real ~/.local-offload).
func pipelineDispatchCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Home = t.TempDir()
	cfg.Pipelines = map[string]config.PipelineSpec{
		"scene-swap": {
			Script:     "does-not-run.mjs",
			Workdir:    "wd",
			TimeoutSec: 30,
			Artifacts:  []string{"final.png"},
		},
	}
	return cfg
}

func pipelineDispatchBody(jobID, jobSpecID, productURL, logoURL string) string {
	return fmt.Sprintf(
		`{"job_id":%q,"task_type":"scene-swap","payload":{"job_spec":{"id":%q},"image_refs":{"product":%q,"logo":%q},"tier":"16gb"}}`,
		jobID, jobSpecID, productURL, logoURL)
}

// TestDispatchPipelineJobDuplicateReAcksWithoutRebuild is the regression test
// for the review finding: BuildRequest used to run BEFORE the Accept dedupe,
// so a lost-ack re-dispatch of the SAME job_id (still running) reached
// buildPipelineJob a second time and hit its exclusive job-dir Mkdir guard —
// a false "already in flight" 400, which the dispatcher treats as an outright
// REFUSAL (inviting a duplicate render elsewhere). The fix looks up a known
// job_id BEFORE calling BuildRequest at all. Proven here by counting hits on
// a seen-array ref server: a duplicate dispatch must NOT re-fetch either ref
// (the direct proof BuildRequest/buildPipelineJob never ran a second time,
// not just that the runner didn't re-render).
func TestDispatchPipelineJobDuplicateReAcksWithoutRebuild(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	refSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		w.Write([]byte("\x89PNG\r\n\x1a\nfake-image-bytes"))
	}))
	defer refSrv.Close()

	release := make(chan struct{})
	fr := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		<-release
		return core.Result{OK: true, Data: json.RawMessage(`{"final_path":"x.png"}`)}
	}}
	s, _ := newTestServer(t, pipelineDispatchCfg(t), fr, nil)

	body := pipelineDispatchBody("pipe-dup", "pipe-dup-case", refSrv.URL+"/product.png", refSrv.URL+"/logo.png")

	rec := do(t, s, http.MethodPost, "/fleet/dispatch", body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	pollJob(t, s, "pipe-dup", JobRunning)

	mu.Lock()
	firstCount := len(seen)
	mu.Unlock()
	if firstCount != 2 { // product + logo, fetched exactly once
		t.Fatalf("expected 2 ref fetches on the first dispatch, got %d: %v", firstCount, seen)
	}

	// Re-dispatch the SAME envelope job_id while the first job is still running.
	rec = do(t, s, http.MethodPost, "/fleet/dispatch", body, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("duplicate-while-running status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}

	mu.Lock()
	secondCount := len(seen)
	mu.Unlock()
	if secondCount != firstCount {
		t.Errorf("refs were RE-FETCHED on the duplicate dispatch (BuildRequest ran again): %d -> %d hits: %v",
			firstCount, secondCount, seen)
	}

	close(release)
	pollJob(t, s, "pipe-dup", JobDone)
	if len(fr.requests()) != 1 {
		t.Fatalf("runner ran %d times, want exactly 1", len(fr.requests()))
	}
}

// TestDispatchPipelineJobDifferentJobIDSameSpecIDStill400s proves the Mkdir
// collision guard still catches the REAL collision: two DIFFERENT envelope
// job_ids that happen to carry the same job_spec.id (e.g. two independent web
// submissions racing on a duplicate id) must still 400 the second one — only
// a re-dispatch of the identical job_id is fast-pathed to a re-ack.
func TestDispatchPipelineJobDifferentJobIDSameSpecIDStill400s(t *testing.T) {
	refSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("\x89PNG\r\n\x1a\nfake-image-bytes"))
	}))
	defer refSrv.Close()

	release := make(chan struct{})
	fr := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		<-release
		return core.Result{OK: true, Data: json.RawMessage(`{"final_path":"x.png"}`)}
	}}
	s, _ := newTestServer(t, pipelineDispatchCfg(t), fr, nil)

	bodyA := pipelineDispatchBody("job-A", "shared-spec-id", refSrv.URL+"/product.png", refSrv.URL+"/logo.png")
	rec := do(t, s, http.MethodPost, "/fleet/dispatch", bodyA, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first POST status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	pollJob(t, s, "job-A", JobRunning)

	bodyB := pipelineDispatchBody("job-B", "shared-spec-id", refSrv.URL+"/product.png", refSrv.URL+"/logo.png")
	rec = do(t, s, http.MethodPost, "/fleet/dispatch", bodyB, nil)
	wantErrorShape(t, rec, http.StatusBadRequest, "already in flight")

	close(release)
	pollJob(t, s, "job-A", JobDone)
}

func TestJobsUnknown404(t *testing.T) {
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	rec := do(t, s, http.MethodGet, "/fleet/jobs/never-acked", "", nil)
	wantErrorShape(t, rec, http.StatusNotFound, "unknown job")
}

func TestJobsDoneIncludesData(t *testing.T) {
	fr := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		return core.Result{OK: true, Data: json.RawMessage(`{"image_path":"C:/renders/a.png","seed":7}`)}
	}}
	s, _ := newTestServer(t, imageCfg(), fr, nil)
	do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"d1","task_type":"image-gen","payload":{"prompt":"hi"}}`, nil)
	m := pollJob(t, s, "d1", JobDone)
	data, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatalf("done job missing data object: %v", m)
	}
	if data["image_path"] != "C:/renders/a.png" || data["seed"] != float64(7) {
		t.Fatalf("data not the Result payload: %v", data)
	}
	if _, present := m["error"]; present {
		t.Fatalf("done job must not carry error: %v", m)
	}
}

func TestJobsDeferredResultBecomesErrorWithReason(t *testing.T) {
	fr := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		return core.Deferf("gpu_busy: a gen job holds the GPU lock", "", core.Meta{})
	}}
	s, _ := newTestServer(t, imageCfg(), fr, nil)
	do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"e1","task_type":"image-gen","payload":{"prompt":"hi"}}`, nil)
	m := pollJob(t, s, "e1", JobError)
	if m["error"] != "gpu_busy: a gen job holds the GPU lock" {
		t.Fatalf("error = %v, want the defer Reason", m["error"])
	}
	if _, present := m["data"]; present {
		t.Fatalf("error job must not carry data: %v", m)
	}
}

func TestJobsDeferredWithoutReasonStillErrors(t *testing.T) {
	fr := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		return core.Result{OK: false}
	}}
	s, _ := newTestServer(t, imageCfg(), fr, nil)
	do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"e2","task_type":"image-gen","payload":{"prompt":"hi"}}`, nil)
	m := pollJob(t, s, "e2", JobError)
	if m["error"] != "deferred" {
		t.Fatalf("error = %v, want the deferred fallback (an empty error would read as success)", m["error"])
	}
}

// The BuildRequest cleanup (run-graph temp files) must run after the job
// finishes — present during the render, gone once terminal.
func TestRunGraphCleanupRunsAfterJobFinishes(t *testing.T) {
	existedDuringRun := make(chan bool, 1)
	var graphPath string
	var mu sync.Mutex
	fr := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		p, _ := req.Params["graph_path"].(string)
		mu.Lock()
		graphPath = p
		mu.Unlock()
		_, err := os.Stat(p)
		existedDuringRun <- err == nil
		return core.Result{OK: true, Data: json.RawMessage(`{"outputs":{}}`)}
	}}
	s, _ := newTestServer(t, imageCfg(), fr, nil)
	rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"cg1","task_type":"run-graph","payload":{"graph":{"1":{"class_type":"KSampler"}}}}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if !<-existedDuringRun {
		t.Fatal("materialized graph file missing while the job ran")
	}
	pollJob(t, s, "cg1", JobDone)
	mu.Lock()
	p := graphPath
	mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cleanup never removed %s after the job finished", p)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestMediaServesExistingFile confirms the happy path: a file seeded directly
// in the config's MediaDir (the same dir GenerateSdcpp's `out` argument
// targets — see pipeline.go's runImageGen) comes back 200 with its bytes and
// an image/png Content-Type derived from the extension.
func TestMediaServesExistingFile(t *testing.T) {
	dir := t.TempDir()
	want := []byte("\x89PNG\r\n\x1a\nfake-png-bytes")
	if err := os.WriteFile(filepath.Join(dir, "render-abc123.png"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := newTestServer(t, config.Config{MediaDir: dir}, &fakeRunner{}, nil)
	rec := do(t, s, http.MethodGet, "/fleet/media/render-abc123.png", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", ct)
	}
	if rec.Body.String() != string(want) {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
}

// TestMediaMissingFile404 confirms a name that never existed answers the
// file's JSON error shape, not a bare filesystem error.
func TestMediaMissingFile404(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTestServer(t, config.Config{MediaDir: dir}, &fakeRunner{}, nil)
	rec := do(t, s, http.MethodGet, "/fleet/media/never-rendered.png", "", nil)
	wantErrorShape(t, rec, http.StatusNotFound, "not found")
}

// TestMediaRejectsBadFilenames is table-driven over every way a filename can
// stop being a bare name: traversal via a %2F-hidden slash, a literal
// forward slash, a literal backslash (the Windows separator — must be
// rejected even though this route also serves a Linux fleet node), and an
// empty segment. All four must die at validation with the SAME 400 JSON
// shape as every other handler in this file, never reaching the filesystem.
func TestMediaRejectsBadFilenames(t *testing.T) {
	dir := t.TempDir()
	s, _ := newTestServer(t, config.Config{MediaDir: dir}, &fakeRunner{}, nil)
	cases := []struct {
		name string
		path string
	}{
		{"percent-encoded traversal", "/fleet/media/..%2Fescape"},
		{"forward slash", "/fleet/media/a/b"},
		{"backslash", "/fleet/media/a\\b"},
		{"empty", "/fleet/media/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, s, http.MethodGet, tc.path, "", nil)
			wantErrorShape(t, rec, http.StatusBadRequest, "bare name")
		})
	}
}

// TestMediaRejectsSymlinkEscape mirrors internal/agent's established
// symlink-escape regression (TestReadFileRejectsSymlinkEscape): a file inside
// MediaDir that is actually a symlink resolving OUTSIDE MediaDir must not be
// served, even though its bare name passes the traversal-string checks.
// Portable: skips where the OS/user can't create symlinks (matches the
// codebase's existing convention rather than requiring elevation).
func TestMediaRejectsSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "media")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(parent, "secret.png")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "escape.png")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink creation unsupported here: %v", err)
	}
	s, _ := newTestServer(t, config.Config{MediaDir: dir}, &fakeRunner{}, nil)
	rec := do(t, s, http.MethodGet, "/fleet/media/escape.png", "", nil)
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "TOPSECRET") {
		t.Fatalf("SECURITY: symlink escape not rejected, served %q", rec.Body.String())
	}
	wantErrorShape(t, rec, http.StatusNotFound, "not found")
}

// fakeRoster serves GET /v1/models in llama-swap's REAL shape: a canonical id
// the config never mentions, with the harness-bound name published only under
// meta.llamaswap.aliases (the exact deployment shape that broke every id-only
// roster reader — see internal/swapclient's package doc). Every fetch is
// COUNTED into hits: the count is how the residency tests prove single-flight
// and the TTL from the OUTSIDE, without any test running the probe itself.
func fakeRoster(t *testing.T, hits *atomic.Int64, canonical string, aliases ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if hits != nil {
			hits.Add(1)
		}
		quoted := make([]string, len(aliases))
		for i, a := range aliases {
			quoted[i] = fmt.Sprintf("%q", a)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model","meta":{"llamaswap":{"aliases":[%s]}}}]}`,
			canonical, strings.Join(quoted, ","))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// agentHealthCfg opts the node into the agent lane against a fake roster.
func agentHealthCfg(endpoint string) config.Config {
	cfg := imageCfg()
	cfg.FleetAgentEnabled = true
	cfg.AgentModel = "offload-e4b"
	cfg.AgentCtxTokens = 16384
	cfg.Endpoint = endpoint
	return cfg
}

// agentCfg is agentHealthCfg's sibling for tests that drive the server through
// newTestServer's DEFAULT Options (opts=nil, LoopbackListener false): it sets
// FleetAuthToken so AgentLaneSafelyReachable holds without a loopback
// listener, keeping the lane admissible with no *Options override at all.
func agentCfg() config.Config {
	cfg := imageCfg()
	cfg.FleetAgentEnabled = true
	cfg.AgentModel = "offload-e4b"
	cfg.AgentCtxTokens = 8192
	cfg.FleetAuthToken = "test-token"
	return cfg
}

// waitForResidencyProbe blocks until a background refresh has PUBLISHED an
// answer into the residency cache.
//
// Why it exists, and why no residency test may call refreshAgentResidency
// itself: production's ONLY trigger is the `go s.refreshAgentResidency()`
// inside agentResident(), on the health path. Tests that called the refresh
// synchronously never exercised that line — deleting it left the whole suite
// green while, in production, agent_seat_resident would be false forever,
// remoteEligible would never pass, EVERY delegation would silently fall local,
// and the feature would be inert. So the tests drive health GETs only and
// merely OBSERVE the cache's own timestamp here (an in-package read; the
// timestamp is set exclusively by refreshAgentResidency).
func waitForResidencyProbe(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.agentRes.mu.Lock()
		published := !s.agentRes.at.IsZero()
		s.agentRes.mu.Unlock()
		if published {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no residency answer was ever published: the health path never kicked off a background probe — agent_seat_resident would stay false forever and every delegation would silently fall local")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestHealthAgentFieldsPresentWhenEnabled drives the §S3 agent advertisement
// end to end THROUGH THE HEALTH HANDLER, never by calling the refresh: with
// fleet_agent_enabled the payload carries agent_enabled/agent_seat/
// agent_ctx_tokens immediately, while agent_seat_resident starts ABSENT
// (false) — the cache is cold and the handler must NEVER block on a llama-swap
// round-trip (same rule as the reclaim tracker) — and turns true once the
// BACKGROUND probe the first GET kicked off lands. The roster hit count pins
// the other half of the cache contract: one refresh cycle, single-flighted,
// reused by every request inside the TTL — TWO roster GETs per cycle
// (rosterServes for residency, rosterIDs for served_models; see the rosterIDs
// field doc for why they are not one fetch), never more no matter the request
// rate. The fake roster serves the seat ONLY as an alias, pinning
// alias-awareness (the plannerUnserved idiom): an id-only reader would
// advertise a correctly-served seat as non-resident forever.
func TestHealthAgentFieldsPresentWhenEnabled(t *testing.T) {
	var probes atomic.Int64
	roster := fakeRoster(t, &probes, "gemma-4-e4b", "offload-e4b")
	s, _ := newTestServer(t, agentHealthCfg(roster.URL), &fakeRunner{}, authOpts(true))

	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["schema_version"] != float64(1) {
		t.Fatalf("schema_version = %v, want 1 — the agent fields are additive, never a version bump", m["schema_version"])
	}
	if m["agent_enabled"] != true {
		t.Fatalf("agent_enabled = %v, want true", m["agent_enabled"])
	}
	if m["agent_seat"] != "offload-e4b" {
		t.Fatalf("agent_seat = %v, want the resolved planner seat offload-e4b", m["agent_seat"])
	}
	if m["agent_ctx_tokens"] != float64(16384) {
		t.Fatalf("agent_ctx_tokens = %v, want 16384", m["agent_ctx_tokens"])
	}
	if v, present := m["agent_seat_resident"]; present {
		t.Fatalf("agent_seat_resident = %v on the FIRST health (cache cold) — must fail closed until the probe lands, never block the handler", v)
	}
	if n := probes.Load(); n > 2 {
		t.Fatalf("roster probes after ONE health request = %d, want at most 2 (rosterServes + rosterIDs)", n)
	}

	// The background refresh the GET above kicked off is the only thing that
	// can flip the field. Wait for it to publish, then re-read health.
	waitForResidencyProbe(t, s)
	m = decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
	if m["agent_seat_resident"] != true {
		t.Fatalf("agent_seat_resident = %v after a successful ALIAS-served roster probe, want true", m["agent_seat_resident"])
	}
	if n := probes.Load(); n != 2 {
		t.Fatalf("roster probes = %d, want exactly 2 (rosterServes + rosterIDs, single-flighted: concurrent/repeat health requests share one refresh cycle)", n)
	}

	// Inside the TTL every further request is served from the cache — health
	// must not probe per request whatever the request rate.
	for i := 0; i < 20; i++ {
		m = decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
		if m["agent_seat_resident"] != true {
			t.Fatalf("agent_seat_resident = %v on cached health request %d, want true", m["agent_seat_resident"], i)
		}
	}
	if n := probes.Load(); n != 2 {
		t.Fatalf("roster probes after 20 more health requests = %d, want still exactly 2 (the %s TTL)", n, agentResidencyTTL)
	}
}

// TestHealthAgentResidencyFailsClosedWhenRosterUnreachable: llama-swap down
// (or endpoint unset) must read as NOT resident — the delegator then keeps
// work local, the conservative direction — while health itself stays 200 and
// the other agent fields keep advertising (the media lane and the capability
// advertisement do not depend on llama-swap being up). Driven through health
// GETs only, like every residency test (see waitForResidencyProbe).
func TestHealthAgentResidencyFailsClosedWhenRosterUnreachable(t *testing.T) {
	roster := fakeRoster(t, nil, "gemma-4-e4b", "offload-e4b")
	base := roster.URL
	roster.Close() // now refused at connect

	s, _ := newTestServer(t, agentHealthCfg(base), &fakeRunner{}, authOpts(true))
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil) // kicks off the probe
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a dead llama-swap must not 503 health (body %s)", rec.Code, rec.Body.String())
	}
	waitForResidencyProbe(t, s)

	m := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
	if v, present := m["agent_seat_resident"]; present {
		t.Fatalf("agent_seat_resident = %v with the roster unreachable, want absent (fail closed)", v)
	}
	if m["agent_enabled"] != true || m["agent_seat"] != "offload-e4b" {
		t.Fatalf("static agent fields must still advertise: enabled=%v seat=%v", m["agent_enabled"], m["agent_seat"])
	}
}

// TestHealthAgentResidencyProbeFailureIsLogged (M-5): failing closed is right,
// failing closed SILENTLY is not. `agent_seat_resident:false` is the single
// field that stops every remote placement, and with the probe error discarded
// there was nothing anywhere — on the node or on the delegator — saying WHY the
// node advertises itself as unusable. One line per probe (the probe runs at
// most once per TTL window, so this cannot become per-request spam).
func TestHealthAgentResidencyProbeFailureIsLogged(t *testing.T) {
	// The probe logs from a BACKGROUND goroutine while this one reads the
	// buffer, so the sink must be mutex-guarded — a bare bytes.Buffer is a data
	// race the moment the test stops running the probe itself.
	buf := &syncBuffer{}
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })

	roster := fakeRoster(t, nil, "gemma-4-e4b", "offload-e4b")
	base := roster.URL
	roster.Close()

	s, _ := newTestServer(t, agentHealthCfg(base), &fakeRunner{}, authOpts(true))
	do(t, s, http.MethodGet, "/fleet/health", "", nil) // the only trigger there is
	waitForResidencyProbe(t, s)

	out := buf.String()
	if !strings.Contains(out, "residency probe") {
		t.Fatalf("log = %q, want the failed residency probe named", out)
	}
	if !strings.Contains(out, "offload-e4b") {
		t.Fatalf("log = %q, want the seat named — it is what stops every remote placement", out)
	}
}

// TestRefreshAgentResidencyJoinsTheSingleFlight (C-K) is the ONE test in this
// package allowed to call the exported RefreshAgentResidency, and it is about
// the helper's LATCH DISCIPLINE, not about residency itself — every residency
// behavior above is still driven through health GETs only, so the background
// line production depends on stays covered (see waitForResidencyProbe).
//
// The exported helper called the refresher synchronously WITHOUT taking the
// inflight latch, while the refresher clears that latch unconditionally at the
// end. Two consequences, both real: a second concurrent GET against a
// llama-swap that is already being probed, and — because whichever probe
// finishes LAST wins the cache — a staler answer overwriting a fresher one.
func TestRefreshAgentResidencyJoinsTheSingleFlight(t *testing.T) {
	var probes atomic.Int64
	started := make(chan struct{}, 4)
	release := make(chan struct{})

	s, _ := newTestServer(t, agentHealthCfg("http://127.0.0.1:1"), &fakeRunner{}, authOpts(true))
	s.rosterServes = func(ctx context.Context, endpoint, seat string) (bool, error) {
		probes.Add(1)
		started <- struct{}{}
		<-release // hold the probe open so a second one is OBSERVABLE
		return true, nil
	}

	do(t, s, http.MethodGet, "/fleet/health", "", nil) // production's only trigger
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the health path never kicked off the background probe")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RefreshAgentResidency()
	}()

	// Give the exported call every chance to start a duplicate probe.
	select {
	case <-started:
		t.Fatal("RefreshAgentResidency started a SECOND probe while one was already in flight")
	case <-time.After(100 * time.Millisecond):
	}
	if n := probes.Load(); n != 1 {
		t.Fatalf("probes = %d while one was in flight, want 1", n)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RefreshAgentResidency never returned; it must WAIT for the in-flight answer, not hang")
	}
	if n := probes.Load(); n != 1 {
		t.Fatalf("probes = %d, want exactly 1 — the exported helper must join the single-flight, not race it", n)
	}
	// Its contract is "the cache is warm when I return": a cross-package
	// end-to-end caller uses it precisely so the next health GET is
	// deterministic.
	s.agentRes.mu.Lock()
	published, resident := !s.agentRes.at.IsZero(), s.agentRes.resident
	s.agentRes.mu.Unlock()
	if !published || !resident {
		t.Fatalf("cache after RefreshAgentResidency: published=%v resident=%v, want a warm true answer", published, resident)
	}
}

// syncBuffer is a log sink safe to write from a background goroutine and read
// from the test goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestHealthAgentFieldsAbsentWhenDisabled is the additive-off pin, byte level:
// with fleet_agent_enabled false (the default), a config that ALSO carries an
// agent seat + ctx ceiling must produce a health body byte-identical to a
// config with no agent keys at all — the advertisement keys off the operator
// opt-in, never off the mere presence of a seat (every tier binds one).
func TestHealthAgentFieldsAbsentWhenDisabled(t *testing.T) {
	plain, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)

	seeded := imageCfg()
	seeded.AgentModel = "offload-e4b"
	seeded.AgentCtxTokens = 16384
	seeded.Endpoint = "http://127.0.0.1:1" // must never be dialed while disabled
	withSeat, _ := newTestServer(t, seeded, &fakeRunner{}, nil)

	recPlain := do(t, plain, http.MethodGet, "/fleet/health", "", nil)
	recSeeded := do(t, withSeat, http.MethodGet, "/fleet/health", "", nil)
	if recPlain.Code != http.StatusOK || recSeeded.Code != http.StatusOK {
		t.Fatalf("status = %d/%d, want 200/200", recPlain.Code, recSeeded.Code)
	}
	if !bytes.Equal(recPlain.Body.Bytes(), recSeeded.Body.Bytes()) {
		t.Fatalf("disabled-lane health is not byte-identical:\n plain: %s\nseeded: %s",
			recPlain.Body.String(), recSeeded.Body.String())
	}
	for _, key := range []string{"agent_seat", "agent_ctx_tokens", "agent_seat_resident", "agent_enabled"} {
		if strings.Contains(recSeeded.Body.String(), key) {
			t.Fatalf("disabled health leaks %q: %s", key, recSeeded.Body.String())
		}
	}
}

func TestServeTimeoutTable(t *testing.T) {
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	srv := s.httpServer()
	if srv.ReadHeaderTimeout != 5*time.Second || srv.ReadTimeout != 30*time.Second ||
		srv.WriteTimeout != 30*time.Second || srv.IdleTimeout != 120*time.Second {
		t.Fatalf("timeout table wrong: header=%v read=%v write=%v idle=%v",
			srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}
}

// TestRefreshAgentResidencyPublishesEvenOnPanic (R4-9): the roster probe ran
// OUTSIDE any deferred unlock, and `inflight = false` + Broadcast happened only
// on the normal return path. A panic anywhere in that seam therefore froze
// residency for the life of the process — the cached answer could never be
// refreshed again, and every awaitProbe (RefreshAgentResidency, which has no
// timeout of its own) blocked forever waiting for a probe that had already died.
// Latent today because production's only trigger runs on a goroutine and no
// production caller panics; one deferred publish removes the whole class.
func TestRefreshAgentResidencyPublishesEvenOnPanic(t *testing.T) {
	s, _ := newTestServer(t, agentHealthCfg("http://127.0.0.1:1"), &fakeRunner{}, authOpts(true))
	s.rosterServes = func(ctx context.Context, endpoint, seat string) (bool, error) {
		panic("roster probe blew up")
	}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("the panic must still propagate — the fix is the latch, not swallowing failures")
			}
		}()
		s.RefreshAgentResidency()
	}()

	s.agentRes.mu.Lock()
	inflight, published, resident := s.agentRes.inflight, !s.agentRes.at.IsZero(), s.agentRes.resident
	s.agentRes.mu.Unlock()
	if inflight {
		t.Fatal("inflight stayed set after a panicking probe: residency can never refresh again and every waiter blocks forever")
	}
	if !published || resident {
		t.Fatalf("published=%v resident=%v, want a published FAIL-CLOSED answer (an unverified seat reads as not resident)", published, resident)
	}

	// The load-bearing consequence: a synchronous waiter must not hang.
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.agentRes.awaitProbe()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("awaitProbe never returned: the Broadcast that releases waiters was skipped")
	}
}

// --- queue-depth back-pressure (FleetMaxQueueDepth → Config.FleetQueueLimit) ---

// dispatchImage posts one image-gen dispatch with the given job id.
func dispatchImage(t *testing.T, s *Server, id string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"`+id+`","task_type":"image-gen","payload":{"prompt":"hi"}}`, nil)
}

// TestDispatchQueueFull503NewWorkOnly proves all four properties of the cap in
// one lifecycle: (1) a NEW dispatch beyond the limit is refused 503 naming
// "queue full" — the dispatch contract's re-dispatch-elsewhere signal; (2) a
// re-dispatch of a job the node already OWNS is re-acked 202 even while full
// (a lost ack must never become a fleet-wide duplicate because the node was
// busy); (3) the refusal is the GUARD's, not an artifact — the control arm
// below runs the identical sequence with the cap disabled and is admitted; and
// (4) capacity frees when a job finishes.
func TestDispatchQueueFull503NewWorkOnly(t *testing.T) {
	release := make(chan struct{})
	blocked := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return core.Result{OK: true, Data: json.RawMessage(`{"image_path":"x.png"}`)}
	}}
	cfg := imageCfg()
	cfg.FleetMaxQueueDepth = 1
	s, _ := newTestServer(t, cfg, blocked, nil)

	if rec := dispatchImage(t, s, "qf-1"); rec.Code != http.StatusAccepted {
		t.Fatalf("first dispatch = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	// The store is at the limit: new work is refused, owned work is re-acked.
	wantErrorShape(t, dispatchImage(t, s, "qf-2"), http.StatusServiceUnavailable, "queue full")
	if rec := dispatchImage(t, s, "qf-1"); rec.Code != http.StatusAccepted {
		t.Fatalf("re-ack of owned job while full = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}

	close(release)
	pollJob(t, s, "qf-1", JobDone)
	// Terminal jobs are results awaiting pollers, not load: capacity is free.
	if rec := dispatchImage(t, s, "qf-3"); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch after completion = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	pollJob(t, s, "qf-3", JobDone)
}

// TestDispatchQueueUnlimitedControlArm is the control for the test above: the
// identical fill-then-dispatch sequence with the cap disabled (negative =
// unlimited) admits everything — proof the 503 above comes from the guard.
func TestDispatchQueueUnlimitedControlArm(t *testing.T) {
	release := make(chan struct{})
	blocked := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return core.Result{OK: true, Data: json.RawMessage(`{"image_path":"x.png"}`)}
	}}
	cfg := imageCfg()
	cfg.FleetMaxQueueDepth = -1
	s, _ := newTestServer(t, cfg, blocked, nil)

	if rec := dispatchImage(t, s, "qu-1"); rec.Code != http.StatusAccepted {
		t.Fatalf("first dispatch = %d, want 202", rec.Code)
	}
	if rec := dispatchImage(t, s, "qu-2"); rec.Code != http.StatusAccepted {
		t.Fatalf("second dispatch under unlimited = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	close(release)
	pollJob(t, s, "qu-1", JobDone)
	pollJob(t, s, "qu-2", JobDone)
}

func TestHealth_GpuUtilIsBusiestDevice(t *testing.T) {
	opts := &Options{Snapshot: func() (Snapshot, bool) {
		return Snapshot{TotalGiB: 32, FreeGiB: 20, At: time.Now(), Devices: []GPUDevice{
			{Index: 0, UUID: "a", UtilPct: 12, UtilKnown: true},
			{Index: 1, UUID: "b", UtilPct: 37, UtilKnown: true},
		}}, true
	}}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/fleet/health", nil))
	var got struct {
		Util  int  `json:"gpu_util_pct"`
		Known bool `json:"gpu_util_known"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Util != 37 || !got.Known {
		t.Fatalf("got %+v want util=37 known=true", got)
	}
}

func TestHealth_GpuUtilKnownButZero(t *testing.T) {
	// When the busiest device has UtilPct=0 but UtilKnown=true, both fields
	// must be present and accurate. This tests that 0 is not collapsed to absent
	// by omitempty.
	opts := &Options{Snapshot: func() (Snapshot, bool) {
		return Snapshot{TotalGiB: 32, FreeGiB: 20, At: time.Now(), Devices: []GPUDevice{
			{Index: 0, UUID: "a", UtilPct: 0, UtilKnown: true},
		}}, true
	}}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/fleet/health", nil))
	var got struct {
		Util  int  `json:"gpu_util_pct"`
		Known bool `json:"gpu_util_known"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Util != 0 || !got.Known {
		t.Fatalf("got %+v want util=0 known=true", got)
	}
}

func TestHealth_GpuUtilAllUnknown(t *testing.T) {
	// When no device has UtilKnown=true, both fields must still be present:
	// GpuUtilKnown=false and GpuUtilPct=0 (meaningless).
	opts := &Options{Snapshot: func() (Snapshot, bool) {
		return Snapshot{TotalGiB: 32, FreeGiB: 20, At: time.Now(), Devices: []GPUDevice{
			{Index: 0, UUID: "a", UtilPct: 0, UtilKnown: false},
			{Index: 1, UUID: "b", UtilPct: 0, UtilKnown: false},
		}}, true
	}}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/fleet/health", nil))
	var got struct {
		Util  int  `json:"gpu_util_pct"`
		Known bool `json:"gpu_util_known"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Util != 0 || got.Known {
		t.Fatalf("got %+v want util=0 known=false", got)
	}
}

func TestHealth_HostFieldsPresentWhenSet(t *testing.T) {
	opts := &Options{
		Snapshot: goodSnapshot,
		Host: func() (hostsample.Sample, bool) {
			return hostsample.Sample{CPUPct: 42, RAMUsedGiB: 12.5, RAMTotalGiB: 128, Known: true}, true
		},
	}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/fleet/health", nil))
	m := decodeMap(t, rr)
	if got, want := m["host_cpu_pct"], float64(42); got != want {
		t.Fatalf("host_cpu_pct = %v, want %v", got, want)
	}
	if got, want := m["host_ram_used_gb"], 12.5; got != want {
		t.Fatalf("host_ram_used_gb = %v, want %v", got, want)
	}
	if got, want := m["host_ram_total_gb"], float64(128); got != want {
		t.Fatalf("host_ram_total_gb = %v, want %v", got, want)
	}
}

func TestHealth_HostFieldsAbsentWhenNil(t *testing.T) {
	opts := &Options{Snapshot: goodSnapshot}
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, opts)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/fleet/health", nil))
	m := decodeMap(t, rr)
	for _, k := range []string{"host_cpu_pct", "host_ram_used_gb", "host_ram_total_gb"} {
		if _, present := m[k]; present {
			t.Fatalf("%s must be absent when Host is nil, got %v", k, m[k])
		}
	}
}

// TestHealth_ServedModelsRideResidencyRefresh: served_models rides the SAME
// TTL and single-flight latch as agent_seat_resident (agentResident() is the
// one trigger for both). Driven through health GETs only, like every
// residency test — see waitForResidencyProbe's rationale.
func TestHealth_ServedModelsRideResidencyRefresh(t *testing.T) {
	s, _ := newTestServer(t, agentCfg(), &fakeRunner{}, nil)
	s.rosterIDs = func(ctx context.Context, endpoint string) ([]string, error) {
		return []string{"qwen3.5-9b-agent", "gemma-4-e4b"}, nil
	}
	s.rosterServes = func(ctx context.Context, endpoint, seat string) (bool, error) { return true, nil }
	// first call schedules the refresh; poll until published
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/fleet/health", nil))
		var got struct {
			Served []string `json:"served_models"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &got)
		if len(got.Served) == 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("served_models never published")
}

// TestHealth_ServedModelsAbsentWhenIDsFetchFailsButResidentTrue pins the two
// probes' independence: rosterServes succeeding and rosterIDs failing in the
// SAME refresh cycle must publish agent_seat_resident:true (the residency
// probe's own outcome) with served_models absent (fail closed for the ids
// probe alone) — a failed ids fetch must never flip resident, and a
// successful residency probe must never paper over a failed ids fetch with a
// stale or fabricated list.
func TestHealth_ServedModelsAbsentWhenIDsFetchFailsButResidentTrue(t *testing.T) {
	s, _ := newTestServer(t, agentCfg(), &fakeRunner{}, nil)
	s.rosterServes = func(ctx context.Context, endpoint, seat string) (bool, error) { return true, nil }
	s.rosterIDs = func(ctx context.Context, endpoint string) ([]string, error) {
		return nil, errors.New("roster ids fetch failed")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		m := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
		if resident, present := m["agent_seat_resident"]; present && resident == true {
			if v, present := m["served_models"]; present {
				t.Fatalf("served_models = %v, want absent when the ids fetch failed", v)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("agent_seat_resident never turned true")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
