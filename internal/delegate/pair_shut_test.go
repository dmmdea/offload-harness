package delegate

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// TestNoPairFrameIsEmittedOnceTheRunIsShut: a goroutine the call deadline
// abandoned (a seat that ignored its context) can finish after RunWith has begun
// draining the PAIR emitter. Emit adds to a sync.WaitGroup, and an Add racing the
// final Done of a Wait in progress panics the Done — a crash for a frame nobody is
// waiting for. Once the run is shut, a late frame is dropped instead. The control
// shows the same frame IS emitted while the run is open, so the test cannot pass
// by emitting nothing at all.
func TestNoPairFrameIsEmittedOnceTheRunIsShut(t *testing.T) {
	pairAppDir(t)
	c := &pairCapture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.PairWorkloadsEnabled = true
	cfg.PairWorkloadsEndpoint = srv.URL
	r := &runner{cfg: cfg, pair: pairworkloads.New(pairworkloads.FromConfig(cfg))}
	inFlight := func() PlacedResult { return PlacedResult{pairModel: "seat-m", pairEngine: "engine-e", pairCreated: 1} }

	pr := inFlight()
	r.pairTerminal("agd-open", &pr) // the control: an open run emits
	r.pair.Wait()
	if n := len(c.snapshot()); n != 1 {
		t.Fatalf("control: %d frame(s) from an open run, want 1", n)
	}

	r.shutPair()
	pr = inFlight()
	r.pairTerminal("agd-late", &pr) // an abandoned goroutine reporting after the run ended
	r.pair.Wait()
	waitABit()
	if n := len(c.snapshot()); n != 1 {
		t.Fatalf("%d frame(s) after the run was shut, want the control's 1 and nothing later", n)
	}
}
