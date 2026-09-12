package visionremote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// fakeNode is a fleet node that advertises the given tasks (and lease), records
// the vision dispatch it receives, and answers the job with a core.Result.
type fakeNode struct {
	node     string
	tasks    []string
	leased   bool
	result   core.Result
	refuse   int // non-zero: answer the dispatch with this status
	mu       sync.Mutex
	payload  map[string]any
	auth     string
	polls    int
	srv      *httptest.Server
	visionOK bool
}

func newFakeNode(t *testing.T, node string, tasks []string, res core.Result) *fakeNode {
	f := &fakeNode{node: node, tasks: tasks, result: res}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		h := map[string]any{"node_id": node, "supported_task_types": tasks, "vision_model": "fake-vlm", "queue_depth": 0}
		if f.leased {
			h["lease"] = map[string]any{"held": true, "class": "text", "busy": true}
		}
		json.NewEncoder(w).Encode(h)
	})
	mux.HandleFunc("POST /fleet/vision", func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		f.payload = p
		f.auth = r.Header.Get("Authorization")
		f.visionOK = true
		f.mu.Unlock()
		if f.refuse != 0 {
			w.WriteHeader(f.refuse)
			json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": "refused for the test"})
			return
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"job_id": p["job_id"], "status": "accepted"})
	})
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.polls++
		f.mu.Unlock()
		data, _ := json.Marshal(f.result)
		json.NewEncoder(w).Encode(map[string]any{"job_id": r.PathValue("id"), "state": "done", "data": json.RawMessage(data)})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNode) dispatched() (map[string]any, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.payload, f.auth
}

// localRunner records whether the in-process path ran.
type localRunner struct {
	mu   sync.Mutex
	runs []core.Request
	res  core.Result
}

func (l *localRunner) Run(ctx context.Context, req core.Request) core.Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs = append(l.runs, req)
	return l.res
}

func (l *localRunner) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.runs)
}

// pngFile writes a real 2x2 PNG (the loader sniffs the bytes) and returns its path.
func pngFile(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "img.png")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func setBusy(t *testing.T, busy bool) {
	t.Helper()
	prev := localBusy
	localBusy = func(config.Config) bool { return busy }
	t.Cleanup(func() { localBusy = prev })
}

func assessReq(img string) core.Request {
	return core.Request{Task: core.TaskAssessImage, Image: img, Params: map[string]any{"brief": "a beach"}}
}

var remoteOK = core.Result{OK: true, Data: json.RawMessage(`{"has_people":false,"has_text":false,"matches_brief":true,"notes":"sand and sea"}`), Meta: core.Meta{Model: "fake-vlm", LatencyMs: 1234}}

// TestDefaultRouteIsLocalAndNeverTouchesTheWire: "" and "local" run in-process
// with no placement stamp — byte-identical to before the route existed. The
// fake node would record any dispatch; it records none.
func TestDefaultRouteIsLocalAndNeverTouchesTheWire(t *testing.T) {
	setBusy(t, true) // even a busy card: local is local
	node := newFakeNode(t, "lenovo", []string{"vision"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.srv.URL}
	local := &localRunner{res: core.Result{OK: true, Data: json.RawMessage(`{"answer":"local"}`)}}
	for _, route := range []string{"", "local", "LOCAL"} {
		res := Run(context.Background(), cfg, local, assessReq(pngFile(t)), route)
		if !res.OK || string(res.Data) != `{"answer":"local"}` || res.Meta.Placement != "" || res.Meta.Node != "" {
			t.Fatalf("route %q: %+v, want the local result unstamped", route, res)
		}
	}
	if p, _ := node.dispatched(); p != nil {
		t.Fatalf("local route dispatched to the node: %v", p)
	}
	if local.count() != 3 {
		t.Fatalf("local ran %d times, want 3", local.count())
	}
}

// TestRemoteShipsTheImageAndReturnsTheNodesResult: route remote reads the
// caller's file, ships it as a data URI with the bearer, and hands back the
// node's core.Result stamped with node + placement — the local runner never runs.
func TestRemoteShipsTheImageAndReturnsTheNodesResult(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "lenovo-ampere16", []string{"vision", "agent"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.srv.URL + "/"}
	cfg.FleetAuthToken = "tok"
	local := &localRunner{}

	res := Run(context.Background(), cfg, local, assessReq(pngFile(t)), "remote")
	if !res.OK || string(res.Data) != string(remoteOK.Data) {
		t.Fatalf("result = %+v, want the node's", res)
	}
	if res.Meta.Node != "lenovo-ampere16" || res.Meta.Placement != "remote: forced" || res.Meta.Model != "fake-vlm" {
		t.Fatalf("meta = %+v, want node + placement stamped over the node's meta", res.Meta)
	}
	if local.count() != 0 {
		t.Fatal("route remote must never run the local seat")
	}
	p, auth := node.dispatched()
	if auth != "Bearer tok" {
		t.Fatalf("auth = %q, want the fleet token", auth)
	}
	if p["task"] != "assess_image" || p["brief"] != "a beach" {
		t.Fatalf("payload = %v, want task + brief", p)
	}
	img, _ := p["image"].(string)
	if !strings.HasPrefix(img, "data:image/png;base64,") {
		t.Fatalf("image = %.40q, want a png data URI", img)
	}
	if id, _ := p["job_id"].(string); !strings.HasPrefix(id, "vision-") {
		t.Fatalf("job_id = %q, want a vision- id", id)
	}
}

// TestRemoteDefersWithAClassWhenNoNodeIsEligible: no lane, a leased card, and
// no remotes at all each defer with the class the route contract promises,
// naming what was probed — and never fall back to local.
func TestRemoteDefersWithAClassWhenNoNodeIsEligible(t *testing.T) {
	setBusy(t, false)
	noLane := newFakeNode(t, "aorus", []string{"agent", "image-gen"}, remoteOK)
	leased := newFakeNode(t, "lenovo", []string{"vision"}, remoteOK)
	leased.leased = true
	local := &localRunner{}
	img := pngFile(t)

	cfg := config.Default()
	cfg.DelegateRemotes = []string{noLane.srv.URL, leased.srv.URL}
	res := Run(context.Background(), cfg, local, assessReq(img), "remote")
	if !res.Deferred || res.DeferClass != core.DeferClassCapacity {
		t.Fatalf("result = %+v, want a capacity defer", res)
	}
	for _, want := range []string{"aorus", "no vision lane", "lenovo", "card leased"} {
		if !strings.Contains(res.Reason, want) {
			t.Fatalf("reason %q does not name %q", res.Reason, want)
		}
	}

	none := config.Default()
	res = Run(context.Background(), none, local, assessReq(img), "remote")
	if !res.Deferred || res.DeferClass != core.DeferClassConfig || !strings.Contains(res.Reason, "delegate_remotes") {
		t.Fatalf("result = %+v, want a config defer naming delegate_remotes", res)
	}
	if local.count() != 0 {
		t.Fatal("route remote must never run the local seat")
	}
}

// TestRemoteRefusalMapsToAClass: a 503 from the node (queue full, leased
// mid-flight, draining) is capacity; a 401 is infrastructure.
func TestRemoteRefusalMapsToAClass(t *testing.T) {
	setBusy(t, false)
	for status, class := range map[int]string{503: core.DeferClassCapacity, 401: core.DeferClassInfrastructure} {
		node := newFakeNode(t, "lenovo", []string{"vision"}, remoteOK)
		node.refuse = status
		cfg := config.Default()
		cfg.DelegateRemotes = []string{node.srv.URL}
		res := Run(context.Background(), cfg, &localRunner{}, assessReq(pngFile(t)), "remote")
		if !res.Deferred || res.DeferClass != class || !strings.Contains(res.Reason, fmt.Sprintf("status %d", status)) {
			t.Fatalf("status %d: result = %+v, want a %s defer", status, res, class)
		}
	}
}

// TestAutoRunsLocalOnAnIdleCard: an idle local card always wins, even with an
// eligible node on the roster.
func TestAutoRunsLocalOnAnIdleCard(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "lenovo", []string{"vision"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.srv.URL}
	local := &localRunner{res: core.Result{OK: true, Data: json.RawMessage(`{"answer":"local"}`)}}
	res := Run(context.Background(), cfg, local, assessReq(pngFile(t)), "auto")
	if string(res.Data) != `{"answer":"local"}` || res.Meta.Placement != "local: gpu idle" {
		t.Fatalf("result = %+v, want the local result stamped idle", res)
	}
	if p, _ := node.dispatched(); p != nil {
		t.Fatal("auto on an idle card must not dispatch")
	}
}

// TestAutoGoesRemoteOnABusyCardAndFallsBackLocal: a held lease sends the work
// to the node; with no eligible node the work still runs local, and the
// placement says why.
func TestAutoGoesRemoteOnABusyCardAndFallsBackLocal(t *testing.T) {
	setBusy(t, true)
	node := newFakeNode(t, "lenovo", []string{"vision"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.srv.URL}
	local := &localRunner{res: core.Result{OK: true, Data: json.RawMessage(`{"answer":"local"}`)}}

	res := Run(context.Background(), cfg, local, assessReq(pngFile(t)), "auto")
	if string(res.Data) != string(remoteOK.Data) || res.Meta.Placement != "remote: local gpu busy" || res.Meta.Node != "lenovo" {
		t.Fatalf("result = %+v, want the node's result stamped busy", res)
	}
	if local.count() != 0 {
		t.Fatal("busy + eligible node must not run local")
	}

	none := config.Default() // no remotes: queued-local beats ineligible-remote
	res = Run(context.Background(), none, local, assessReq(pngFile(t)), "auto")
	if string(res.Data) != `{"answer":"local"}` || !strings.HasPrefix(res.Meta.Placement, "local: gpu busy, ") || !strings.Contains(res.Meta.Placement, "delegate_remotes") {
		t.Fatalf("result = %+v, want the local result with the fallback reason", res)
	}
}

// TestWaitSurvivesATransientPollFailure: the node answers the first two
// polls with a 500 and a dropped body, then the done job — the call must
// return the result, not an infrastructure defer (review finding).
func TestWaitSurvivesATransientPollFailure(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "lenovo", []string{"vision"}, remoteOK)
	var flaky int32
	mux := http.NewServeMux()
	mux.Handle("/", node.srv.Config.Handler)
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&flaky, 1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		node.srv.Config.Handler.ServeHTTP(w, r)
	})
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{front.URL}
	res := Run(context.Background(), cfg, &localRunner{}, assessReq(pngFile(t)), "remote")
	if !res.OK || string(res.Data) != string(remoteOK.Data) {
		t.Fatalf("result = %+v, want the node's result after two failed polls", res)
	}
	if n := atomic.LoadInt32(&flaky); n < 3 {
		t.Fatalf("polls = %d, want the wait to have retried past the two failures", n)
	}
}

// TestWaitGivesUpOnAVanishedJob: a 404 (the node evicted or restarted) ends
// the wait at once with an infrastructure defer naming it.
func TestWaitGivesUpOnAVanishedJob(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "lenovo", []string{"vision"}, remoteOK)
	mux := http.NewServeMux()
	mux.Handle("/", node.srv.Config.Handler)
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": "unknown job"})
	})
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{front.URL}
	res := Run(context.Background(), cfg, &localRunner{}, assessReq(pngFile(t)), "remote")
	if !res.Deferred || res.DeferClass != core.DeferClassInfrastructure || !strings.Contains(res.Reason, "denies holding") {
		t.Fatalf("result = %+v, want an infrastructure defer for the vanished job", res)
	}
}

// TestImageIsCappedOnTheCallerSide: an unreadable or oversize image defers
// exactly as the local loader does, before any node is probed.
func TestImageIsCappedOnTheCallerSide(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "lenovo", []string{"vision"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.srv.URL}
	cfg.VisionMaxImageBytes = 10
	res := Run(context.Background(), cfg, &localRunner{}, assessReq(pngFile(t)), "remote")
	if !res.Deferred || !strings.HasPrefix(res.Reason, "image load: ") {
		t.Fatalf("result = %+v, want an image load defer", res)
	}
	if p, _ := node.dispatched(); p != nil {
		t.Fatal("an oversize image must never be dispatched")
	}
	res = Run(context.Background(), cfg, &localRunner{}, assessReq(filepath.Join(t.TempDir(), "missing.png")), "remote")
	if !res.Deferred || !strings.HasPrefix(res.Reason, "image load: ") {
		t.Fatalf("result = %+v, want an image load defer for a missing file", res)
	}
}

// TestUnrecognizedRouteIsAContractDefer: a typo never silently becomes local.
func TestUnrecognizedRouteIsAContractDefer(t *testing.T) {
	local := &localRunner{}
	res := Run(context.Background(), config.Default(), local, assessReq("x.png"), "cloud")
	if !res.Deferred || res.DeferClass != core.DeferClassContract || local.count() != 0 {
		t.Fatalf("result = %+v (local ran %d), want a contract defer and no run", res, local.count())
	}
}
