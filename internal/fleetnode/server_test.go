package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestServeTimeoutTable(t *testing.T) {
	s, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	srv := s.httpServer()
	if srv.ReadHeaderTimeout != 5*time.Second || srv.ReadTimeout != 30*time.Second ||
		srv.WriteTimeout != 30*time.Second || srv.IdleTimeout != 120*time.Second {
		t.Fatalf("timeout table wrong: header=%v read=%v write=%v idle=%v",
			srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}
}
