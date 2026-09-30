// page_cap_test.go: what counts as "a page failed" for the per-page retry cap (ADR 0063,
// decision 8) - and what never does.
//
// The cap backs a research page off for fifteen minutes after three failed issues, with
// a contract-class defer that says "none of them produced a verified digest". That
// sentence is true only when a seat RAN the page and did not produce one. Anything the
// fleet or the caller did - a full node, a lease, a dead node, a bad token, a cancel, a
// queue deadline - is not the page's fault, and counting it hid an infrastructure signal
// behind a quiet contract-class defer that outlived the lease that caused it.

package delegate

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

func TestPageIssueFailedCountsOnlyWhatASeatRan(t *testing.T) {
	mk := func(mod func(*PlacedResult)) PlacedResult {
		pr := PlacedResult{Node: "n", ranBase: "http://192.0.2.50:1"}
		mod(&pr)
		return pr
	}
	deferred := func(class string) func(*PlacedResult) {
		return func(p *PlacedResult) { p.Result.Deferred, p.Result.DeferClass = true, class }
	}
	errIs := func(msg string) func(*PlacedResult) { return func(p *PlacedResult) { p.Err = msg } }

	notThePagesFault := map[string]PlacedResult{
		"capacity defer":                   mk(deferred(core.DeferClassCapacity)),
		"contract defer":                   mk(deferred(core.DeferClassContract)),
		"infrastructure defer (a lease)":   mk(deferred(core.DeferClassInfrastructure)),
		"config defer":                     mk(deferred(core.DeferClassConfig)),
		"write defer":                      mk(deferred(core.DeferClassWrite)),
		"defer from a node too old to say": mk(deferred("")),
		"placement refused":                mk(errIs(replacementExhaustedPrefix + ": 2 node(s) refused this subtask and none of them ran it")),
		"queue deadline":                   mk(errIs("queue deadline after 5m0s: the node accepted the job but never started it")),
		"caller cancel":                    mk(errIs("canceled: context canceled")),
		"terminal dispatch refusal":        mk(errIs("dispatch http://192.0.2.50:1: status 401: unauthorized")),
		"poll 401":                         mk(errIs("poll: 401 unauthorized (fleet_auth_token mismatch)")),
		"node lost the job":                mk(errIs("node lost job agd-1 3 times (poll 404 after re-dispatch)")),
		"poll deadline, never owned":       mk(errIs("poll deadline after 5m0s: no node answered")),
		"local runner error":               mk(errIs("local run: seat unreachable")),
		// The wait ran out of budget before any node was asked: class budget, but no seat
		// ever saw the page - only Unplaced tells it from a seat that hit its budget.
		"budget defer nobody ran": mk(func(p *PlacedResult) {
			p.Unplaced, p.Result.Deferred, p.Result.DeferClass = true, true, core.DeferClassBudget
		}),
		"shed": mk(func(p *PlacedResult) { p.shed, p.Unplaced, p.Err = true, true, "remote job error: shed" }),
	}
	for name, pr := range notThePagesFault {
		if pageIssueFailed(pr) {
			t.Errorf("%s counted against the page: no seat ran the page, so it is not the page's fault", name)
		}
	}
	theSeatRanIt := map[string]PlacedResult{
		"failed verification": mk(func(p *PlacedResult) { p.AcceptanceFailures = []string{"contains:x"} }),
		"seat abstained":      mk(deferred(core.DeferClassAbstention)),
		"seat hit its budget": mk(deferred(core.DeferClassBudget)),
		"remote job error":    mk(errIs("remote job error: boom")),
	}
	for name, pr := range theSeatRanIt {
		if !pageIssueFailed(pr) {
			t.Errorf("%s did not count against the page: a seat RAN it and it produced no verified digest", name)
		}
	}
	if pageIssueFailed(mk(func(*PlacedResult) {})) {
		t.Error("a clean, verified result counted against the page")
	}
}

// Run level: four issues of one page whose job the node accepts and never starts.
// Each ends as a queue deadline - the fleet's busy day, not the page - so none of
// them may back the page off.
func TestPageCapIgnoresQueueDeadlines(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, 20*time.Millisecond)
	stuck := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-stuck",
		pollState: func(int64) (map[string]any, int) { return map[string]any{"state": "accepted"}, http.StatusOK },
	}
	url := stuck.server().URL
	c := pageContract(uniquePage("the page behind a busy node"))
	c.TimeoutSec = 1
	for issue := 1; issue <= 5; issue++ {
		results, _, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{c}, "remote", []string{url})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(results[0].Err, "queue deadline") {
			t.Fatalf("issue %d: err = %q, want the queue deadline (fixture)", issue, results[0].Err)
		}
	}
	if got := stuck.dispatches.Load(); got != 5 {
		t.Fatalf("the node saw %d dispatches for 5 issues, want 5 - queue deadlines were counted against the page", got)
	}
}

func TestPageCapIgnoresCallerCancels(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	running := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-running",
		pollState: func(int64) (map[string]any, int) { return map[string]any{"state": "running"}, http.StatusOK },
	}
	url := running.server().URL
	page := uniquePage("the page the caller keeps abandoning")
	for issue := 1; issue <= 5; issue++ {
		ctx, cancel := context.WithCancel(t.Context())
		want := int64(issue)
		go func() {
			// cancel only once the node holds the job: a fixed sleep races the dispatch under load
			for running.dispatches.Load() < want {
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
		results, _, err := Run(ctx, testCfg(t), neverLocal(t), []core.AgentContract{pageContract(page)}, "remote", []string{url})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(results[0].Err, "canceled") {
			t.Fatalf("issue %d: err = %q, want the cancel (fixture)", issue, results[0].Err)
		}
	}
	if got := running.dispatches.Load(); got != 5 {
		t.Fatalf("the node saw %d dispatches for 5 issues, want 5 - caller cancels were counted against the page", got)
	}
}

func TestPageCapIgnoresCapacityDefers(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	var localCalls atomic.Int64
	page := uniquePage("the page a busy seat kept deferring")
	for issue := 1; issue <= 5; issue++ {
		results, _, err := Run(t.Context(), testCfg(t), capacityDeferLocal(&localCalls), []core.AgentContract{pageContract(page)}, "local", nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(results[0].Result.Reason, "page retry cap") {
			t.Fatalf("issue %d was backed off (%s) after capacity defers - a busy seat is not the page's failure", issue, results[0].Result.Reason)
		}
	}
	if localCalls.Load() != 5 {
		t.Fatalf("local seat saw %d issues, want 5", localCalls.Load())
	}
}

// TestPageCapIgnoresAnInfrastructureDefer: the local seat is under a text lease, so
// every issue ends as the holder-naming infrastructure defer. Three of them used to
// convert the loud defer into a quiet contract-class one that blamed the page and
// outlived the lease - fifteen minutes of a healthy page refused, with the real cause
// gone from the summary.
func TestPageCapIgnoresAnInfrastructureDefer(t *testing.T) {
	dir, _ := holdLease(t, gpulease.ClassText, "soak")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	page := uniquePage("the page behind a leased seat")
	for issue := 1; issue <= 5; issue++ {
		results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{pageContract(page)}, "auto", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		pr := results[0]
		if pr.Result.DeferClass != core.DeferClassInfrastructure || sum.Infrastructure != 1 || strings.Contains(pr.Result.Reason, "page retry cap") {
			t.Fatalf("issue %d: class = %q infrastructure = %d reason = %q, want the loud holder-naming defer every time - a lease is not the page's failure", issue, pr.Result.DeferClass, sum.Infrastructure, pr.Result.Reason)
		}
	}
}

// TestPageCapIgnoresATerminalDispatchRefusal: a node that answers 401 (a token that is
// wrong for the whole fleet) refuses every issue of every page at dispatch. No seat ran
// anything.
func TestPageCapIgnoresATerminalDispatchRefusal(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node, url := refusingNode(t, "node-auth", http.StatusUnauthorized, nil)
	page := uniquePage("the page behind a bad token")
	for issue := 1; issue <= 5; issue++ {
		results, _, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{pageContract(page)}, "remote", []string{url})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(results[0].Err, "401") {
			t.Fatalf("issue %d: err = %q, want the 401 refusal (fixture)", issue, results[0].Err)
		}
	}
	if got := node.dispatches.Load(); got != 5 {
		t.Fatalf("the node saw %d dispatches for 5 issues, want 5 - a terminal refusal was counted against the page", got)
	}
}
