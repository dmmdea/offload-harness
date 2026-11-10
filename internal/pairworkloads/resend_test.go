package pairworkloads

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// 2026-11-09: PAIR's ingress was down while a delegation's subtask was queued and then started on a
// node; both frames were refused and nothing sent them again, so PAIR showed no card for the 13
// minutes the job ran. The producer must deliver the job's latest in-flight frame once PAIR is back.
func TestInFlightFrameRefusedWhilePairIsDownIsResent(t *testing.T) {
	r := newOrphanRig(t)
	var mu sync.Mutex
	refuse := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		no := refuse
		mu.Unlock()
		if no {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		r.pair.handler(w, req)
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL
	p := r.emitter(nil, nil)
	p.retryEvery = 20 * time.Millisecond
	runningJob(p, "agd-hidden")
	if n := r.pair.count(); n != 0 {
		t.Fatalf("PAIR was down, yet it received %d frames", n)
	}

	mu.Lock()
	refuse = false
	mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for r.pair.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if r.pair.count() != 1 {
		t.Fatalf("want exactly the job's latest in-flight frame once PAIR is back, got %d frames", r.pair.count())
	}
	if m, info := r.pair.method(0), r.pair.info(0); m != "workload:started" || info["id"] != "agd-hidden" || info["startedAt"].(float64) != 2000 {
		t.Fatalf("resent frame must be the running frame as it was: %v %v", m, info)
	}

	// Delivered: nothing is kept, so nothing is sent again, and the job closes normally.
	time.Sleep(100 * time.Millisecond)
	if r.pair.count() != 1 {
		t.Fatalf("a delivered frame was sent again: %d frames", r.pair.count())
	}
	p.Emit(Event{JobID: "agd-hidden", Model: "agent-pool", Engine: "vllm", State: "completed", CreatedAt: 1_000, StartedAt: 2_000, CompletedAt: 3_000})
	p.Wait()
	if last := r.pair.count() - 1; r.pair.method(last) != "workload:completed" {
		t.Fatalf("last frame %v, want workload:completed", r.pair.method(last))
	}
}
