// Register A-102: a job this node runs for a delegator records the door
// "fleet", so its ledger row names the surface that admitted it instead of
// being one of the door-less cascade rows. A request that already carries a
// door keeps it — the door belongs to the surface that admitted the call, not
// to the hop that executed it.
package fleetnode

import (
	"context"
	"net/http"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The dispatched request reaches the runner stamped "fleet" — checked over the
// real dispatch path (envelope decode → BuildRequest → runner), because the
// point of the field is what the RUNNER sees, not what the guard returns.
func TestDispatchedJobIsRunWithTheFleetDoor(t *testing.T) {
	fr := &fakeRunner{}
	s, _ := newTestServer(t, imageCfg(), fr, nil)
	rec := do(t, s, http.MethodPost, "/fleet/dispatch",
		`{"job_id":"door1","task_type":"image-gen","payload":{"prompt":"hi"}}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	pollJob(t, s, "door1", JobDone)
	reqs := fr.requests()
	if len(reqs) != 1 {
		t.Fatalf("runner ran %d times, want 1", len(reqs))
	}
	if reqs[0].Door != "fleet" {
		t.Fatalf("dispatched request door = %q, want fleet", reqs[0].Door)
	}
}

// The keep-the-origin-door branch: no payload shape puts a door on the wire
// today, so this is the only place that branch is reachable — and it must stay
// reachable, or adding a door to the wire later silently relabels every
// delegated call as fleet-originated.
func TestDispatchDoorKeepsADelegatorsOwnDoor(t *testing.T) {
	if got := dispatchDoor(""); got != "fleet" {
		t.Fatalf("dispatchDoor(\"\") = %q, want fleet", got)
	}
	if got := dispatchDoor("offload_summarize"); got != "offload_summarize" {
		t.Fatalf("dispatchDoor kept %q, want the delegator's own door", got)
	}
	if got := dispatchDoor("cli:summarize"); got != "cli:summarize" {
		t.Fatalf("dispatchDoor kept %q, want the delegator's own door", got)
	}
	// And the runner must receive whatever the guard returned, unchanged.
	fr := &fakeRunner{}
	_ = fr.Run(context.Background(), core.Request{Task: core.TaskSummarize, Door: dispatchDoor("cli:summarize")})
	if reqs := fr.requests(); len(reqs) != 1 || reqs[0].Door != "cli:summarize" {
		t.Fatalf("runner saw %+v, want the preserved door", reqs)
	}
}
