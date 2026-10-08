package visionremote

// The vision lane's auto route also reads the vision seat's load (ADR 0078).
//
// ADR 0040 decision 6 sent an auto call to a fleet node only while the machine-wide GPU lease was held. A seat serving
// other requests with no lease anywhere still queued the next image behind them while a node with an idle vision seat
// sat free. The auto route now asks the delegator's own question of the seat THIS call would run on
// (delegate.LocalSeatBusy): another request in flight, a load in progress, or a load that would unload a loaded vLLM
// seat. The lease keeps its word and its placement text.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// stubSeat replaces the seat reading for one test and counts the times it was asked.
func stubSeat(t *testing.T, busy bool, why string) *atomic.Int64 {
	t.Helper()
	var asked atomic.Int64
	prev := seatBusy
	seatBusy = func(_ context.Context, _ config.Config, _ string) (bool, string) {
		asked.Add(1)
		return busy, why
	}
	t.Cleanup(func() { seatBusy = prev })
	return &asked
}

var localAnswer = core.Result{OK: true, Data: json.RawMessage(`{"answer":"local"}`)}

// TestAutoGoesRemoteWhenTheVisionSeatIsBusyWithNoLeaseAnywhere is the defect: no lease, a busy seat, a node with a vision
// seat. The call goes to the node, and the placement says it was the seat.
func TestAutoGoesRemoteWhenTheVisionSeatIsBusyWithNoLeaseAnywhere(t *testing.T) {
	setBusy(t, false)
	asked := stubSeat(t, true, "2 in flight")
	node := newFakeNode(t, "node-c", []string{"vision"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.srv.URL}
	local := &localRunner{res: localAnswer}
	res := Run(context.Background(), cfg, local, assessReq(pngFile(t)), "auto")
	if string(res.Data) != string(remoteOK.Data) || res.Meta.Node != "node-c" || res.Meta.Placement != "remote: local vision seat busy (2 in flight)" {
		t.Fatalf("result = %+v, want the node's result stamped with the seat's load", res)
	}
	if local.count() != 0 || asked.Load() != 1 {
		t.Fatalf("local ran %d times, the seat was read %d times, want 0 and 1", local.count(), asked.Load())
	}
}

// TestAutoFallsBackLocalWhenTheSeatIsBusyAndNoNodeIsEligible: queued-local beats ineligible-remote, as for the lease, and
// the placement carries the seat's reason.
func TestAutoFallsBackLocalWhenTheSeatIsBusyAndNoNodeIsEligible(t *testing.T) {
	setBusy(t, false)
	stubSeat(t, true, "a load or unload is in progress")
	noLane := newFakeNode(t, "node-a", []string{"agent", "image-gen"}, remoteOK) // configured, but it has no vision lane
	cfg := config.Default()
	cfg.DelegateRemotes = []string{noLane.srv.URL}
	local := &localRunner{res: localAnswer}
	res := Run(context.Background(), cfg, local, assessReq(pngFile(t)), "auto")
	if string(res.Data) != `{"answer":"local"}` || local.count() != 1 {
		t.Fatalf("result = %+v, local runs %d, want the local result", res, local.count())
	}
	if !strings.HasPrefix(res.Meta.Placement, "local: vision seat busy (a load or unload is in progress), ") ||
		!strings.Contains(res.Meta.Placement, "node-a") || !strings.Contains(res.Meta.Placement, "no vision lane") {
		t.Fatalf("placement = %q, want the seat's reason and the fallback's, naming the node that could not take it", res.Meta.Placement)
	}
}

// TestAutoNeverReadsTheSeatOfABoxWithNoNodeToSendTheImageTo: the seat is read to choose between this box and a node.
// With no delegate_remotes there is no node to choose, the image runs here whatever the seat does, and the round trip
// to llama-swap is not made for it.
func TestAutoNeverReadsTheSeatOfABoxWithNoNodeToSendTheImageTo(t *testing.T) {
	setBusy(t, false)
	asked := stubSeat(t, true, "9 in flight")
	local := &localRunner{res: localAnswer}
	res := Run(context.Background(), config.Default(), local, assessReq(pngFile(t)), "auto") // no remotes
	if string(res.Data) != `{"answer":"local"}` || res.Meta.Placement != "local: gpu idle" || local.count() != 1 {
		t.Fatalf("result = %+v, local runs %d, want the local result stamped idle: there is nowhere else to send it", res, local.count())
	}
	if asked.Load() != 0 {
		t.Fatalf("the seat was read %d times by a box that has no node to send the image to", asked.Load())
	}
}

// TestAutoRunsLocalWhenTheLeaseAndTheSeatAreBothIdle is the control arm: nothing changed for an idle box.
func TestAutoRunsLocalWhenTheLeaseAndTheSeatAreBothIdle(t *testing.T) {
	setBusy(t, false)
	asked := stubSeat(t, false, "")
	node := newFakeNode(t, "node-c", []string{"vision"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.srv.URL}
	local := &localRunner{res: localAnswer}
	res := Run(context.Background(), cfg, local, assessReq(pngFile(t)), "auto")
	if string(res.Data) != `{"answer":"local"}` || res.Meta.Placement != "local: gpu idle" || asked.Load() != 1 {
		t.Fatalf("result = %+v, seat reads %d, want the local result stamped idle after one read of the seat", res, asked.Load())
	}
	if p, _ := node.dispatched(); p != nil {
		t.Fatal("an idle box dispatched")
	}
}

// TestAHeldLeaseKeepsItsWordsAndSkipsTheSeatRead: when the lease already says busy the seat is not asked (the reading is
// a request to llama-swap), and the placement says what it always said.
func TestAHeldLeaseKeepsItsWordsAndSkipsTheSeatRead(t *testing.T) {
	setBusy(t, true)
	asked := stubSeat(t, true, "5 in flight")
	node := newFakeNode(t, "node-c", []string{"vision"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.srv.URL}
	res := Run(context.Background(), cfg, &localRunner{res: localAnswer}, assessReq(pngFile(t)), "auto")
	if res.Meta.Placement != "remote: local gpu busy" || asked.Load() != 0 {
		t.Fatalf("placement = %q, seat reads %d, want the lease's own words and no read of the seat", res.Meta.Placement, asked.Load())
	}
}

// TestRouteLocalAndRemoteNeverReadTheSeat: the load is the auto route's trigger and nobody else's. Local runs
// in-process whatever the seat does, remote goes to a node whatever the seat does.
func TestRouteLocalAndRemoteNeverReadTheSeat(t *testing.T) {
	setBusy(t, false)
	asked := stubSeat(t, true, "9 in flight")
	node := newFakeNode(t, "node-c", []string{"vision"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.srv.URL}
	local := &localRunner{res: localAnswer}
	if res := Run(context.Background(), cfg, local, assessReq(pngFile(t)), "local"); res.Meta.Placement != "" || local.count() != 1 {
		t.Fatalf("local: %+v, runs %d, want the in-process result unstamped", res, local.count())
	}
	if res := Run(context.Background(), cfg, local, assessReq(pngFile(t)), "remote"); res.Meta.Placement != "remote: forced" {
		t.Fatalf("remote: %+v, want a forced remote placement", res)
	}
	if asked.Load() != 0 {
		t.Fatalf("the seat was read %d times by a route that does not look at it", asked.Load())
	}
}

// TestVisionSeatForFollowsThePipelinesPick: ocr has its own binding when the machine has one; every other vision task
// rides the vision model. The same pick the pipeline makes (visionModelFor), so the seat read is the seat that runs.
func TestVisionSeatForFollowsThePipelinesPick(t *testing.T) {
	both := config.Config{VisionModel: "vlm", OCRModel: "ocr-model"}
	only := config.Config{VisionModel: "vlm"}
	for _, tc := range []struct {
		cfg  config.Config
		task core.TaskType
		want string
	}{
		{both, core.TaskOCR, "ocr-model"},
		{both, core.TaskVQA, "vlm"},
		{both, core.TaskAssessImage, "vlm"},
		{only, core.TaskOCR, "vlm"},
		{only, core.TaskVQA, "vlm"},
		{config.Config{}, core.TaskVQA, ""},
	} {
		if got := visionSeatFor(tc.cfg, string(tc.task)); got != tc.want {
			t.Errorf("visionSeatFor(%+v, %s) = %q, want %q", tc.cfg, tc.task, got, tc.want)
		}
	}
}

// loadedSwap is a llama-swap stand-in serving one loaded seat with `inflight` requests in flight.
func loadedSwap(t *testing.T, id string, inflight int) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + id + `"}]}`))
	})
	mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"running": []map[string]string{{"model": id, "state": "ready", "proxy": "http://" + r.Host + "/direct/" + id}}})
	})
	mux.HandleFunc("/direct/"+id+"/metrics", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "vllm:num_requests_running{engine=\"0\"} %d\nvllm:num_requests_waiting{engine=\"0\"} 0.0\n", inflight)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestTheProductionReadingSendsAnImageOffTheBoxWhileItsSeatServesAnother runs the real seat reading (no stub) against a
// llama-swap whose vision seat holds a request and a box whose lease is free, and shows the seat is the one the task
// names: the same busy vision model sends a vqa call to the node, and a free OCR binding keeps an ocr call local.
func TestTheProductionReadingSendsAnImageOffTheBoxWhileItsSeatServesAnother(t *testing.T) {
	setBusy(t, false)
	node := newFakeNode(t, "node-c", []string{"vision"}, remoteOK)
	cfg := config.Default()
	cfg.DelegateRemotes = []string{node.srv.URL}
	cfg.Endpoint = loadedSwap(t, "vlm", 1)
	cfg.VisionModel = "vlm"
	local := &localRunner{res: localAnswer}

	res := Run(context.Background(), cfg, local, assessReq(pngFile(t)), "auto")
	if string(res.Data) != string(remoteOK.Data) || res.Meta.Placement != "remote: local vision seat busy (1 in flight)" {
		t.Fatalf("vision call: %+v, want the node's result stamped with the seat's one request in flight", res)
	}

	// an ocr call rides its own binding, which this llama-swap does not even list as loaded: nothing to queue behind
	cfg.OCRModel = "ocr-model"
	ocr := core.Request{Task: core.TaskOCR, Image: pngFile(t)}
	res = Run(context.Background(), cfg, local, ocr, "auto")
	if string(res.Data) != `{"answer":"local"}` || res.Meta.Placement != "local: gpu idle" {
		t.Fatalf("ocr call: %+v, want it local: the seat it runs on is idle even though the vision seat is busy", res)
	}
}
