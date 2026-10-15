package pipeline

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
)

// compressWedgedSeat is the time scale of the wedged-seat fixtures: a floor of
// 200 ms so that a request the seat never answers is seen in a fraction of a
// second, a 700 ms cold-load ceiling for the wait that follows, and the busy
// hold's flat bound.
func compressWedgedSeat(t *testing.T) func() {
	t.Helper()
	restoreLive := compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)
	restoreLoad := compressColdLoad(t, 50*time.Millisecond, 700*time.Millisecond)
	restoreHold := compressBusyHold(t, 300*time.Millisecond, 50*time.Millisecond)
	return func() { restoreHold(); restoreLoad(); restoreLive() }
}

// The loop announces the prefill to the run's observer (on a fleet node, the
// job record) after the monitor has started the prefill's stall clock and before
// it registers the model call. A write that takes longer than the clock plus the
// busy hold's flat bound lets the monitor file a seat-down verdict while no call
// is in flight, which is a verdict that ends the run without the recovery wait:
//
//	seat down: the seat's engine did no work for 0s while this request waited in prefill (allowed 0s; ...)
//
// On a loaded host the write measured 200 to 500 ms against a window of 500 ms
// (200 ms floor plus 300 ms flat bound), and the wedged-seat test failed 5 of 300
// runs that way. At the production floors (60 s and 120 s) the window is minutes
// and a job-record write is nothing; the fixtures' scale has to carry the write.
func TestRunAgentTaskWedgedSeatSurvivesASlowProgressWrite(t *testing.T) {
	defer compressWedgedSeat(t)()
	life := &seatLife{}
	fake := &agentFake{
		rosterIDs:   []string{agentTestSeat},
		running:     life.running,
		seatMetrics: life.metrics,
		loopStream: func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done() // a request that never answers
		},
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	life.base.Store(srv.URL)

	var slow atomic.Bool
	ctx := core.WithProgressReport(context.Background(), func(p core.LiveProgress) {
		if p.Phase == string(agent.PhasePrefill) && slow.CompareAndSwap(false, true) {
			time.Sleep(700 * time.Millisecond) // the first prefill announcement: a slow job-record write
		}
	})
	w := decodeWire(t, seatDownPipeline(t, srv.URL, 0).Run(ctx, agentTestRequest(t, testContract())))

	if !w.Deferred || !strings.HasPrefix(w.Reason, core.SeatDownReason) {
		t.Fatalf("deferred=%v reason=%q, want the %q prefix", w.Deferred, w.Reason, core.SeatDownReason)
	}
	if !strings.Contains(w.Reason, "did not come back") {
		t.Fatalf("reason = %q, want the wait that ran out: a 700 ms progress write at the prefill's announcement let the verdict reach the run before its call was in flight", w.Reason)
	}
}
