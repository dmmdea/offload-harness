package fleetnode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
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
	jobs := NewJobs(time.Hour)
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
		"supported_task_types": ["image-gen", "run-graph"],
		"loadable_model_families": ["sdxl", "comfy-graph"],
		"model_footprints": [{"model_family":"sdxl","quant":"bf16","task_type":"image-gen","vram_peak_gb":9.6}],
		"queue_depth": 0
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

func TestServeTimeoutTable(t *testing.T) {
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	srv := s.httpServer()
	if srv.ReadHeaderTimeout != 5*time.Second || srv.ReadTimeout != 30*time.Second ||
		srv.WriteTimeout != 30*time.Second || srv.IdleTimeout != 120*time.Second {
		t.Fatalf("timeout table wrong: header=%v read=%v write=%v idle=%v",
			srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}
}
