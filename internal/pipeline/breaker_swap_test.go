package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/breaker"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// TestBreakerFailureGlue (LO-9): only a timeout on a likely-cold-swap call is
// exempt from breaker accounting; every other infra class always counts, and a
// quality defer (empty err class) never counts.
func TestBreakerFailureGlue(t *testing.T) {
	cases := []struct {
		errClass string
		coldSwap bool
		want     bool
	}{
		{"timeout", true, false},      // cold-swap timeout: exempt
		{"timeout", false, true},      // warm timeout: counts
		{"conn_refused", true, true},  // server down is never a swap
		{"http_5xx", true, true},      // load failure 5xx counts
		{"oom", true, true},           // OOM counts
		{"", true, false},             // quality defer never counts
		{"", false, false},            // success never counts
	}
	for _, tc := range cases {
		if got := breakerFailure(tc.errClass, tc.coldSwap); got != tc.want {
			t.Errorf("breakerFailure(%q, cold=%v) = %v, want %v", tc.errClass, tc.coldSwap, got, tc.want)
		}
	}
}

// TestNoteTierCallIdleWindow: first call to a tier is cold; an immediate
// second call is warm; a call after coldSwapIdle of inactivity is cold again.
func TestNoteTierCallIdleWindow(t *testing.T) {
	p := &Pipeline{}
	now := time.Unix(1_000_000, 0)
	p.nowFn = func() time.Time { return now }
	if !p.noteTierCall("tier-x") {
		t.Fatal("first-ever call must be likely-cold")
	}
	now = now.Add(3 * time.Second)
	if p.noteTierCall("tier-x") {
		t.Fatal("an immediate follow-up call must be warm")
	}
	now = now.Add(coldSwapIdle + time.Second)
	if !p.noteTierCall("tier-x") {
		t.Fatal("a call after the idle window must be likely-cold again")
	}
	// Per-tier isolation: a different tier starts cold regardless.
	if !p.noteTierCall("tier-y") {
		t.Fatal("an unseen tier must be likely-cold")
	}
}

// TestColdSwapFirstCallTimeoutDoesNotTripBreaker (LO-9 end-to-end wiring):
// against a hanging endpoint with a 1s request budget and a HAIR-TRIGGER
// breaker (1 failure trips), the FIRST Run's timeout must NOT open the tier's
// breaker (likely cold swap), while the SECOND Run's timeout — now warm —
// must. Before this fix the Jul-1 GPU-contention incident tripped breakers on
// exactly these first-call load timeouts.
func TestColdSwapFirstCallTimeoutDoesNotTripBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second) // longer than the client budget: every call times out
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.RequestTimeoutSec = 1
	cfg.TriageModel = ""     // chain = [Model] only
	cfg.EscalationModel = "" // no escalation tier
	cfg.ReasoningModel = ""  // no terminal reasoning call
	client := llamaclient.New(srv.URL, "", cfg.Model, time.Second)
	p := New(cfg, client, nil, nil)
	p.breakers = breaker.NewGroup(1, 10, time.Minute) // one failure trips

	req := core.Request{Task: core.TaskSummarize, Input: "a sufficiently long input for the offload gate"}

	res := p.Run(context.Background(), req)
	if res.OK || !res.Deferred {
		t.Fatalf("hanging server must defer, got ok=%v", res.OK)
	}
	if res.Meta.ErrClass != "timeout" {
		t.Fatalf("expected err_class timeout, got %q (%s)", res.Meta.ErrClass, res.Reason)
	}
	if st := p.breakers.State(cfg.Model); st != "closed" {
		t.Fatalf("first-call (cold-swap) timeout must NOT trip the breaker; state=%s", st)
	}

	res = p.Run(context.Background(), req)
	if res.OK || !res.Deferred {
		t.Fatalf("second run must defer too, got ok=%v", res.OK)
	}
	if st := p.breakers.State(cfg.Model); st != "open" {
		t.Fatalf("a WARM tier's timeout must still count; state=%s, want open", st)
	}
}
