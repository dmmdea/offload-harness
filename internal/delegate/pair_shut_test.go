package delegate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
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

// The delegation twin of the 2026-10-09 media-door incident: a door that answers a call-deadline defer for a subtask whose seat ignored
// its context leaves that subtask's card queued in PAIR's Jobs list. The goroutine's own terminal frame
// is dropped once the run is shut, so nothing closed the card for as long as the door's process lived,
// and the orphan sweep then closed it "harness process exited before the job finished". The run closes
// what it opened and never closed, with the deadline's words, before it returns; the finished subtask's
// card is untouched, and the abandoned goroutine's late answer changes nothing.
func TestAnAbandonedSubtasksCardIsClosedWhenTheCallReturns(t *testing.T) {
	pairAppDir(t)
	c := &pairCapture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.PairWorkloadsEnabled = true
	cfg.PairWorkloadsEndpoint = srv.URL
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		// The stuck goroutine records its own rows when it finally returns; let it finish before
		// the temp dir is removed (a Windows handle would block).
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if rows, err := readFinished(cfg.LedgerPath); err == nil && len(rows) >= 3 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
	})
	local := func(ctx context.Context, ac core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		if isSlow(ac) {
			<-release // deaf to ctx
		}
		return localOK(), nil
	}
	results, _, _ := runWithin(t, 6*time.Second, cfg, local,
		[]core.AgentContract{{Goal: "fast one"}, {Goal: "slow one"}}, "local", nil, deadlineIn(time.Second), unblock)
	fast, slow := results[0], results[1]
	if !slow.abandoned {
		t.Fatalf("the slow subtask was not abandoned: %+v", slow)
	}

	// RunWith has returned and delivered its frames (its deferred Wait): read the wire as the door answers.
	terminal := map[string]map[string]any{}
	opened := map[string]bool{}
	for _, f := range c.snapshot() {
		info := pairInfo(f)
		id, _ := info["id"].(string)
		switch f["method"] {
		case "workload:submitted":
			opened[id] = true
		case "workload:completed", "workload:errored":
			if terminal[id] != nil {
				t.Fatalf("card %s closed twice: %v", id, f)
			}
			terminal[id] = info
		}
	}
	if !opened[fast.JobID] || !opened[slow.JobID] || len(opened) != 2 {
		t.Fatalf("want a card for each of the two subtasks, opened %v (fast %s, slow %s)", opened, fast.JobID, slow.JobID)
	}
	if f := terminal[fast.JobID]; f == nil || f["state"] != "completed" || f["error"] != nil {
		t.Fatalf("the finished subtask's card must close completed: %v", f)
	}
	f := terminal[slow.JobID]
	if f == nil {
		t.Fatalf("the abandoned subtask's card was left open when the call returned (closed: %v)", terminal)
	}
	if got, _ := f["error"].(string); f["state"] != "failed" || !strings.HasPrefix(got, "call deadline reached") {
		t.Fatalf("the abandoned subtask's card must close failed with the deadline's words: %v", f)
	}
	if f["model"] == nil || f["engine"] != "llamacpp" || f["createdAt"] == nil || f["completedAt"] == nil {
		t.Fatalf("the close must repeat the identity its in-flight frame named: %v", f)
	}

	// The stuck seat finally answers: its late terminal frame is dropped, the card stays as closed.
	before := len(c.snapshot())
	unblock()
	time.Sleep(300 * time.Millisecond)
	if after := len(c.snapshot()); after != before {
		t.Fatalf("the abandoned goroutine's late answer sent %d more frame(s) to PAIR", after-before)
	}
}

// A card whose terminal frame the run did send is closed for good: a late in-flight frame (a progress
// goroutine's "running") must not reopen it in the run's account, and shutPair closes nothing twice.
func TestShutPairClosesOnlyTheCardsTheRunLeftOpen(t *testing.T) {
	pairAppDir(t)
	c := &pairCapture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.PairWorkloadsEnabled = true
	cfg.PairWorkloadsEndpoint = srv.URL
	r := &runner{cfg: cfg, pair: pairworkloads.New(pairworkloads.FromConfig(cfg))}

	var done, open PlacedResult
	r.pairInflight(&done, "agd-done", "", nil, "seat-m", "queued", true)
	r.pairInflight(&open, "agd-open", "", nil, "seat-m", "queued", true)
	r.pairInflight(&open, "agd-open", "", nil, "seat-m", "running", true)
	r.pairTerminal("agd-done", &done)
	r.pairInflight(&done, "agd-done", "", nil, "seat-m", "running", true) // a late progress goroutine

	r.shutPair()
	r.pair.Wait()
	byID := map[string][]string{}
	for _, f := range c.snapshot() {
		id, _ := pairInfo(f)["id"].(string)
		byID[id] = append(byID[id], f["method"].(string))
	}
	closes := func(id string) int {
		n := 0
		for _, m := range byID[id] {
			if m == "workload:completed" || m == "workload:errored" {
				n++
			}
		}
		return n
	}
	if closes("agd-done") != 1 || closes("agd-open") != 1 {
		t.Fatalf("each card closes exactly once, got %v", byID)
	}
	var last map[string]any
	for _, f := range c.snapshot() {
		if pairInfo(f)["id"] == "agd-open" && f["method"] == "workload:errored" {
			last = pairInfo(f)
		}
	}
	if last == nil || last["startedAt"] == nil {
		t.Fatalf("the open card was running: its close keeps the start its running frame named: %v", last)
	}
}
