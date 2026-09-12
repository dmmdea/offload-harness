// Vision lane tests (0.116.0). Two invariants carry the lane:
//
//  1. Advertisement == admission: "vision" (and vision_model) appear in
//     health exactly when POST /fleet/vision admits — a bound vision_model and
//     the agent lane's reachability rule — and the lane rides the agent
//     lane's bearer gate on dispatch AND on the job poll.
//  2. The caller reads back the FULL core.Result the node's pipeline
//     produced, defers included, so a remote judgment and a local one have
//     the same shape.
package fleetnode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// visionRunner records the request and answers with a fixed core.Result.
type visionRunner struct {
	mu   sync.Mutex
	reqs []core.Request
	res  core.Result
}

func (r *visionRunner) Run(ctx context.Context, req core.Request) core.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	return r.res
}

func (r *visionRunner) last() (core.Request, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		return core.Request{}, false
	}
	return r.reqs[len(r.reqs)-1], true
}

// visionCfg binds a vision model on top of imageCfg.
func visionCfg(token string) config.Config {
	c := imageCfg()
	c.VisionModel = "gemma4-e4b-vision"
	c.FleetAuthToken = token
	return c
}

// tinyPNG is a data URI of a real 1x1 PNG (the pipeline's loader sniffs the
// bytes; the node-side build only checks the prefix and the size).
func tinyPNG() string {
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1F, 0x15, 0xC4, 0x89, 0, 0, 0, 0x0A, 0x49, 0x44, 0x41, 0x54,
		0x78, 0x9C, 0x63, 0, 1, 0, 0, 5, 0, 1, 0x0D, 0x0A, 0x2D, 0xB4, 0, 0, 0, 0, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

func visionBody(jobID, task, image string, extra map[string]string) string {
	m := map[string]any{"job_id": jobID, "task": task, "image": image}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// pollDone polls /fleet/jobs/{id} with the given header until the job is
// terminal, returning the decoded wire.
func pollDone(t *testing.T, s *Server, id string, header map[string]string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := do(t, s, http.MethodGet, "/fleet/jobs/"+id, "", header)
		if rec.Code != http.StatusOK {
			t.Fatalf("poll status = %d (body %s)", rec.Code, rec.Body.String())
		}
		m := decodeMap(t, rec)
		if st, _ := m["state"].(string); st == "done" || st == "error" {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job never reached a terminal state")
	return nil
}

// TestVisionLaneAdvertisementMatchesAdmission drives the (listener, token)
// matrix: whenever health lists "vision" a dispatch is admitted, and whenever
// it does not the dispatch is refused with the agent lane's own verdicts.
func TestVisionLaneAdvertisementMatchesAdmission(t *testing.T) {
	cases := []struct {
		name       string
		token      string
		loopback   bool
		header     string
		advertised bool
		wantStatus int
	}{
		{"loopback, no token", "", true, "", true, http.StatusAccepted},
		{"non-loopback, no token", "", false, "", false, http.StatusForbidden},
		{"non-loopback, token, no header", "s3cret", false, "", true, http.StatusUnauthorized},
		{"non-loopback, token, wrong header", "s3cret", false, "Bearer nope", true, http.StatusUnauthorized},
		{"non-loopback, token, right header", "s3cret", false, "Bearer s3cret", true, http.StatusAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := visionCfg(tc.token)
			r := &visionRunner{res: core.Result{OK: true, Data: json.RawMessage(`{"answer":"a cat"}`)}}
			s, _ := newTestServer(t, cfg, r, authOpts(tc.loopback))

			h := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
			tasks, _ := h["supported_task_types"].([]any)
			listed := false
			for _, x := range tasks {
				if x == VisionTask {
					listed = true
				}
			}
			if listed != tc.advertised {
				t.Fatalf("vision advertised = %v, want %v (tasks %v)", listed, tc.advertised, tasks)
			}
			if _, has := h["vision_model"]; has != tc.advertised {
				t.Fatalf("vision_model present = %v, want %v", has, tc.advertised)
			}
			if tc.advertised && h["vision_model"] != "gemma4-e4b-vision" {
				t.Fatalf("vision_model = %v, want the bound seat", h["vision_model"])
			}

			hdr := map[string]string{}
			if tc.header != "" {
				hdr["Authorization"] = tc.header
			}
			rec := do(t, s, http.MethodPost, "/fleet/vision", visionBody("vj-1", "vqa", tinyPNG(), map[string]string{"question": "what is it?"}), hdr)
			if rec.Code != tc.wantStatus {
				t.Fatalf("dispatch status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestVisionLaneNeedsABoundModel: no vision_model ⇒ not advertised, and a
// dispatch is a plain 400 "unsupported task_type" (the lane is simply not
// there — not a 403, which is reserved for the tokenless-beyond-loopback
// misconfiguration).
func TestVisionLaneNeedsABoundModel(t *testing.T) {
	cfg := imageCfg() // VisionModel empty
	s, _ := newTestServer(t, cfg, &visionRunner{}, authOpts(true))
	h := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
	if _, has := h["vision_model"]; has {
		t.Fatal("vision_model must be absent when no vision model is bound")
	}
	if strings.Contains(string(mustJSON(t, h["supported_task_types"])), `"vision"`) {
		t.Fatalf("vision advertised without a bound model: %v", h["supported_task_types"])
	}
	rec := do(t, s, http.MethodPost, "/fleet/vision", visionBody("vj-2", "ocr", tinyPNG(), nil), nil)
	wantErrorShape(t, rec, http.StatusBadRequest, "unsupported task_type")
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestVisionJobCarriesTheFullResultDefersIncluded: the runner's core.Result
// — here a gpu_busy DEFER — comes back verbatim as the done job's data, and
// the request the runner saw mirrors the MCP handler's param mapping.
func TestVisionJobCarriesTheFullResultDefersIncluded(t *testing.T) {
	cfg := visionCfg("tok")
	deferred := core.Deferf("gpu busy: generation job holds the lock (12s)", "", core.Meta{ErrClass: "gpu_busy", Model: "gemma4-e4b-vision"})
	r := &visionRunner{res: deferred}
	s, _ := newTestServer(t, cfg, r, authOpts(false))
	auth := map[string]string{"Authorization": "Bearer tok"}

	rec := do(t, s, http.MethodPost, "/fleet/vision", visionBody("vj-3", "assess_image", tinyPNG(), map[string]string{"brief": "a beach"}), auth)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch status = %d (body %s)", rec.Code, rec.Body.String())
	}
	m := pollDone(t, s, "vj-3", auth)
	if m["state"] != "done" {
		t.Fatalf("state = %v, want done (a defer is a done job whose data says deferred): %v", m["state"], m)
	}
	var res core.Result
	if err := json.Unmarshal(mustJSON(t, m["data"]), &res); err != nil {
		t.Fatalf("data is not a core.Result: %v", err)
	}
	if !res.Deferred || res.Reason != deferred.Reason || res.Meta.ErrClass != "gpu_busy" || res.Meta.Model != "gemma4-e4b-vision" {
		t.Fatalf("result = %+v, want the runner's defer verbatim", res)
	}
	req, ok := r.last()
	if !ok {
		t.Fatal("runner never ran")
	}
	if req.Task != core.TaskAssessImage || !strings.HasPrefix(req.Image, "data:image/png;base64,") || req.Params["brief"] != "a beach" {
		t.Fatalf("runner saw %+v, want assess_image over the shipped data URI with the brief", req)
	}
}

// TestVisionJobPollIsTokenGated: the job's data is the caller's image judged
// in prose — a poll without the bearer is 401, exactly like an agent job's.
func TestVisionJobPollIsTokenGated(t *testing.T) {
	cfg := visionCfg("tok")
	r := &visionRunner{res: core.Result{OK: true, Data: json.RawMessage(`{"text":"hello"}`)}}
	s, _ := newTestServer(t, cfg, r, authOpts(false))
	auth := map[string]string{"Authorization": "Bearer tok"}
	if rec := do(t, s, http.MethodPost, "/fleet/vision", visionBody("vj-4", "ocr", tinyPNG(), nil), auth); rec.Code != http.StatusAccepted {
		t.Fatalf("dispatch status = %d (body %s)", rec.Code, rec.Body.String())
	}
	rec := do(t, s, http.MethodGet, "/fleet/jobs/vj-4", "", nil)
	wantErrorShape(t, rec, http.StatusUnauthorized, "unauthorized")
	m := pollDone(t, s, "vj-4", auth)
	if m["state"] != "done" {
		t.Fatalf("state = %v, want done", m["state"])
	}
	// The jobs feed keeps the row but never calls it an agent run.
	feed := decodeMap(t, do(t, s, http.MethodGet, "/fleet/jobs", "", nil))
	rows, _ := feed["jobs"].([]any)
	if len(rows) == 0 {
		rows, _ = feed["items"].([]any)
	}
	for _, row := range rows {
		rm, _ := row.(map[string]any)
		if rm["id"] == "vj-4" && rm["agent"] == true {
			t.Fatalf("vision job listed as an agent run: %v", rm)
		}
	}
}

// TestVisionBodyCapFollowsTheImageCap: the route's body limit is
// vision_max_image_bytes inflated by base64 plus slack — a body past it is a
// 400 naming the limit, never a job.
func TestVisionBodyCapFollowsTheImageCap(t *testing.T) {
	cfg := visionCfg("")
	cfg.VisionMaxImageBytes = 1000
	s, _ := newTestServer(t, cfg, &visionRunner{}, authOpts(true))
	if got, want := VisionBodyCap(cfg), int64(1000*4/3+visionBodySlack); got != want {
		t.Fatalf("VisionBodyCap = %d, want %d", got, want)
	}
	big := "data:image/png;base64," + strings.Repeat("A", int(VisionBodyCap(cfg))+1)
	rec := do(t, s, http.MethodPost, "/fleet/vision", visionBody("vj-5", "ocr", big, nil), nil)
	wantErrorShape(t, rec, http.StatusBadRequest, "request body too large")
	// Under the body cap but over the decoded-image cap: the ack-time estimate
	// refuses it with the cap named, before any job exists.
	over := "data:image/png;base64," + strings.Repeat("A", 1400) // ≈1050 decoded bytes > 1000
	rec = do(t, s, http.MethodPost, "/fleet/vision", visionBody("vj-6", "ocr", over, nil), nil)
	wantErrorShape(t, rec, http.StatusBadRequest, "vision_max_image_bytes")
}

// TestVisionPayloadValidation: every caller mistake is a 400 with the field
// named, and the request never reaches the runner.
func TestVisionPayloadValidation(t *testing.T) {
	cfg := visionCfg("")
	r := &visionRunner{}
	s, _ := newTestServer(t, cfg, r, authOpts(true))
	cases := []struct{ name, body, want string }{
		{"unknown task", visionBody("vj-7", "video_describe", tinyPNG(), nil), "not one of vqa, ocr, assess_image"},
		{"path instead of bytes", visionBody("vj-8", "ocr", "C:/on/the/caller/disk.png", nil), "data:image/"},
		{"vqa without question", visionBody("vj-9", "vqa", tinyPNG(), nil), "requires question"},
		{"missing job id", visionBody("", "ocr", tinyPNG(), nil), "job_id required"},
		{"unknown field", `{"job_id":"vj-10","task":"ocr","image":"` + tinyPNG() + `","engine":"npu"}`, "malformed vision body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, s, http.MethodPost, "/fleet/vision", tc.body, nil)
			wantErrorShape(t, rec, http.StatusBadRequest, tc.want)
		})
	}
	if _, ran := r.last(); ran {
		t.Fatal("a refused payload must never reach the runner")
	}
}

// TestVisionIsConcurrencyCapped: the lane runs on the shared llama-swap
// endpoint, so it counts against fleet_max_concurrent_jobs like agent work.
func TestVisionIsConcurrencyCapped(t *testing.T) {
	s, _ := newTestServer(t, visionCfg(""), &visionRunner{}, authOpts(true))
	if !s.concurrencyCapped(VisionTask) {
		t.Fatal("vision must be capped: it contends for the text endpoint")
	}
}
