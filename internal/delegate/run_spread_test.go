package delegate

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// eligibleNode is a fake fleet node that passes the hard gate and answers every
// job with the given output (so acceptance "contains:qube" passes or fails by
// what the caller puts in output).
func eligibleNode(t *testing.T, id, output string) (*fakeNode, string) {
	t.Helper()
	f := &fakeNode{t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: id}
	f.pollByJob = func(jobID string, n int64) (map[string]any, int) {
		w := remoteWire(output, `{"answer":"`+output+`"}`)
		w.NodeID = id
		return doneWire(t, w), 200
	}
	return f, f.server().URL
}

func passingLocal(calls *atomic.Int64) LocalRunner {
	return func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		calls.Add(1)
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: "local-seat",
			Output: "qube answered locally", Structured: json.RawMessage(`{"answer":"qube"}`), StopReason: "done"}, nil
	}
}

func failingLocal(calls *atomic.Int64) LocalRunner {
	return func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		calls.Add(1)
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: "local-seat",
			Output: "no idea", Structured: json.RawMessage(`{"answer":"no idea"}`), StopReason: "done"}, nil
	}
}

// abstainingLocal is a local seat that honestly abstains (the retry-eligible
// defer class) — used where a test needs the retry to run and ALSO fail.
func abstainingLocal() LocalRunner {
	return func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: "local-seat",
			Deferred: true, DeferClass: core.DeferClassAbstention, Reason: "output failed schema: missing required field answer"}, nil
	}
}

func contracts(n int) []core.AgentContract {
	out := make([]core.AgentContract, 0, n)
	for i := 0; i < n; i++ {
		c := remoteContract()
		c.Goal = "answer question " + string(rune('A'+i))
		out = append(out, c)
	}
	return out
}

// TestRunSpreadDealsAcrossLocalAndEveryEligibleRemote: route=spread deals four
// subtasks local, A, B, local — measured before it existed, auto put all four on
// one box and remote put all four on the other.
func TestRunSpreadDealsAcrossLocalAndEveryEligibleRemote(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "qube from A")
	nodeB, urlB := eligibleNode(t, "node-b", "qube from B")
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), testCfg(t), passingLocal(&localCalls), contracts(4), "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 4 || sum.Retried != 0 {
		t.Fatalf("summary = %+v, want 4 succeeded, 0 retried", sum)
	}
	wantNodes := []string{"", "node-a", "node-b", ""} // "" = local (the test cfg has no FleetNodeID → hostname)
	for i, pr := range results {
		switch {
		case wantNodes[i] == "" && !pr.ranLocal:
			t.Errorf("subtask %d: ran on %s, want local", i, pr.Node)
		case wantNodes[i] != "" && pr.Node != wantNodes[i]:
			t.Errorf("subtask %d: ran on %s, want %s", i, pr.Node, wantNodes[i])
		}
		if !strings.HasPrefix(pr.PlacementReason, "route=spread → ") {
			t.Errorf("subtask %d: placement reason %q", i, pr.PlacementReason)
		}
	}
	if localCalls.Load() != 2 || nodeA.dispatches.Load() != 1 || nodeB.dispatches.Load() != 1 {
		t.Fatalf("distribution local=%d A=%d B=%d, want 2/1/1", localCalls.Load(), nodeA.dispatches.Load(), nodeB.dispatches.Load())
	}
}

// TestRunSpreadWithNoEligibleRemoteRunsLocalAndSaysWhy: a remote that fails the
// gate (agent lane off) leaves every subtask local with the reason naming it.
func TestRunSpreadWithNoEligibleRemoteRunsLocalAndSaysWhy(t *testing.T) {
	f := &fakeNode{t: t, agentEnabled: false, resident: true, ctxTokens: 32768, nodeID: "node-off"}
	url := f.server().URL
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), testCfg(t), passingLocal(&localCalls), contracts(2), "spread", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 2 || localCalls.Load() != 2 || f.dispatches.Load() != 0 {
		t.Fatalf("summary=%+v local=%d dispatches=%d", sum, localCalls.Load(), f.dispatches.Load())
	}
	for _, pr := range results {
		if !strings.HasPrefix(pr.PlacementReason, "route=spread: no eligible remote — local (") {
			t.Errorf("reason = %q", pr.PlacementReason)
		}
	}
}

// TestRunRetriesFailedVerificationOnADifferentNode: local answers wrong
// (acceptance fails), the remote answers right → the published result is the
// remote's, marked as a recovered retry; both attempts were recorded.
func TestRunRetriesFailedVerificationOnADifferentNode(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	node, url := eligibleNode(t, "node-a", "qube from A")
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), testCfg(t), failingLocal(&localCalls), contracts(1), "local-then-retry-is-not-a-route", []string{url})
	if err == nil {
		t.Fatal("a bogus route must be refused")
	}
	results, sum, err = Run(context.Background(), testCfg(t), failingLocal(&localCalls), contracts(1), "spread", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	// spread with one subtask deals slot 0 = local → fails acceptance → retry on node-a.
	pr := results[0]
	if len(pr.AcceptanceFailures) != 0 || pr.Result.Deferred || pr.Err != "" {
		t.Fatalf("published result must be the clean retry, got %+v", pr)
	}
	if pr.Node != "node-a" || pr.RetriedOn != "node-a" || !strings.Contains(pr.RetryNote, "first attempt on") || !strings.Contains(pr.RetryNote, "failed_verification") {
		t.Fatalf("retry annotations wrong: node=%s retried_on=%s note=%q", pr.Node, pr.RetriedOn, pr.RetryNote)
	}
	if sum.Succeeded != 1 || sum.FailedVerification != 0 || sum.Retried != 1 || sum.RetryRecovered != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if localCalls.Load() != 1 || node.dispatches.Load() != 1 {
		t.Fatalf("attempts local=%d remote=%d, want 1/1", localCalls.Load(), node.dispatches.Load())
	}
}

// TestRunRetryRemoteFailureFallsBackToLocal: the remote answers wrong, local
// answers right → retry runs local and recovers.
func TestRunRetryRemoteFailureFallsBackToLocal(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	node, url := eligibleNode(t, "node-a", "wrong answer")
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), testCfg(t), passingLocal(&localCalls), contracts(1), "remote", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if !pr.ranLocal || len(pr.AcceptanceFailures) != 0 || pr.RetriedOn == "" || !strings.Contains(pr.RetryNote, "first attempt on node-a") {
		t.Fatalf("want a recovered local retry, got %+v", pr)
	}
	if sum.Succeeded != 1 || sum.Retried != 1 || sum.RetryRecovered != 1 || node.dispatches.Load() != 1 || localCalls.Load() != 1 {
		t.Fatalf("summary=%+v dispatches=%d local=%d", sum, node.dispatches.Load(), localCalls.Load())
	}
}

// TestRunRetryBothFailKeepsFirstAttemptAnnotated: when the retry also fails,
// the FIRST attempt is published with a note saying the retry failed too, and
// the summary counts the retry but not a recovery.
func TestRunRetryBothFailKeepsFirstAttemptAnnotated(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	_, url := eligibleNode(t, "node-a", "also wrong")
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), testCfg(t), failingLocal(&localCalls), contracts(1), "spread", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if !pr.ranLocal || len(pr.AcceptanceFailures) == 0 || pr.RetriedOn != "node-a" || !strings.Contains(pr.RetryNote, "retry on node-a also failed_verification") {
		t.Fatalf("want the first attempt annotated with the failed retry, got %+v", pr)
	}
	if sum.FailedVerification != 1 || sum.Succeeded != 0 || sum.Retried != 1 || sum.RetryRecovered != 0 {
		t.Fatalf("summary = %+v", sum)
	}
}

// TestRunRetryStaysInsideTimeoutBudget: the retry gets what the first attempt
// left of timeout_sec, and is skipped under the floor — timeout_sec remains the
// per-subtask EXECUTION budget the caller was told it is.
func TestRunRetryStaysInsideTimeoutBudget(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	node, url := eligibleNode(t, "node-a", "qube from A")
	slowWrongLocal := func(ctx context.Context, c core.AgentContract) (core.AgentWireResult, error) {
		time.Sleep(1200 * time.Millisecond)
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: "local-seat",
			Output: "no idea", Structured: json.RawMessage(`{"answer":"no idea"}`), StopReason: "done"}, nil
	}
	// budget 11 s: after a ~1.2 s first attempt, 9 s remain — under the 10 s floor → no retry
	c := contracts(1)
	c[0].TimeoutSec = 11
	results, sum, err := Run(context.Background(), testCfg(t), slowWrongLocal, c, "spread", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if pr.RetriedOn != "" || node.dispatches.Load() != 0 || sum.Retried != 0 || !strings.Contains(pr.RetryNote, "retry skipped") || !strings.Contains(pr.RetryNote, "timeout_sec budget") {
		t.Fatalf("want the retry skipped under the floor with a note, got %+v / %+v (dispatches %d)", pr, sum, node.dispatches.Load())
	}
	// budget 15 s: ≥ 12 s remain even if the first attempt (1.2 s sleep + a synchronous
	// delegation-log write) stalls for a couple of seconds on a loaded box — the retry
	// runs, and the contract it carries has the REMAINING budget, not the full 15
	var seen atomic.Int64
	node.onDispatch = func(jobID string, contract core.AgentContract) { seen.Store(int64(contract.TimeoutSec)) }
	c[0].TimeoutSec = 15
	results, sum, err = Run(context.Background(), testCfg(t), slowWrongLocal, c, "spread", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].RetriedOn != "node-a" || sum.RetryRecovered != 1 {
		t.Fatalf("want a recovered retry, got %+v / %+v", results[0], sum)
	}
	if got := seen.Load(); got <= 0 || got >= 15 {
		t.Fatalf("retry contract timeout_sec = %d, want the REMAINING budget (0 < n < 15)", got)
	}
}

// TestRunNoRetryWithoutADifferentNode: route=local with a failed verification
// has nowhere else to go — no retry, no annotation, the original result stands.
func TestRunNoRetryWithoutADifferentNode(t *testing.T) {
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), testCfg(t), failingLocal(&localCalls), contracts(1), "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].RetriedOn != "" || sum.Retried != 0 || localCalls.Load() != 1 || sum.FailedVerification != 1 {
		t.Fatalf("want exactly one un-retried failed attempt, got %+v / %+v (local calls %d)", results[0], sum, localCalls.Load())
	}
}

// TestRunTransportFailureIsNotRetried: a node refusing dispatch is a FAILURE,
// not a wrong answer, so it never enters the VERIFICATION retry (RetriedOn
// stays empty — another seat does not fix a broken wire). Since 0.101.0 it does
// enter RE-PLACEMENT, which is a different mechanism with a different bound;
// here there is no other node to re-place onto and route=remote never falls
// back to local, so the outcome is still one failure and zero local calls.
func TestRunTransportFailureIsNotRetried(t *testing.T) {
	f := &fakeNode{t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-a", dispatchStatus: 500}
	url := f.server().URL
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), testCfg(t), passingLocal(&localCalls), contracts(1), "remote", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err == "" || results[0].RetriedOn != "" || sum.Failed != 1 || sum.Retried != 0 || localCalls.Load() != 0 {
		t.Fatalf("want an un-retried transport failure, got %+v / %+v (local calls %d)", results[0], sum, localCalls.Load())
	}
}

// TestRunRemotesDefaultFromConfig: a call naming no remotes uses
// cfg.DelegateRemotes; a call's own list replaces it.
func TestRunRemotesDefaultFromConfig(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	cfgNode, cfgURL := eligibleNode(t, "cfg-node", "qube from config node")
	callNode, callURL := eligibleNode(t, "call-node", "qube from call node")
	cfg := testCfg(t)
	cfg.DelegateRemotes = []string{cfgURL}
	var localCalls atomic.Int64
	results, _, err := Run(context.Background(), cfg, passingLocal(&localCalls), contracts(1), "remote", nil)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Node != "cfg-node" || cfgNode.dispatches.Load() != 1 {
		t.Fatalf("want the config remote, got node=%s dispatches=%d", results[0].Node, cfgNode.dispatches.Load())
	}
	results, _, err = Run(context.Background(), cfg, passingLocal(&localCalls), contracts(1), "remote", []string{callURL})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Node != "call-node" || callNode.dispatches.Load() != 1 || cfgNode.dispatches.Load() != 1 {
		t.Fatalf("a call's remotes must REPLACE the config list: node=%s call=%d cfg=%d", results[0].Node, callNode.dispatches.Load(), cfgNode.dispatches.Load())
	}
}

// TestWireCarriesRetryFields: the published JSON names the retry.
func TestWireCarriesRetryFields(t *testing.T) {
	pr := PlacedResult{Node: "node-a", RetriedOn: "node-a", RetryNote: "first attempt on local failed_verification; this result is the retry", retryRecovered: true}
	w := WireResponse([]PlacedResult{pr}, Summary{Succeeded: 1, Retried: 1, RetryRecovered: 1}, nil)
	b, _ := json.Marshal(w)
	for _, want := range []string{`"retried":1`, `"retry_recovered":1`, `"retried_on":"node-a"`, `"retry_note":"first attempt on local`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("wire lacks %s: %s", want, b)
		}
	}
	clean, _ := json.Marshal(WireResponse([]PlacedResult{{Node: "x"}}, Summary{Succeeded: 1}, nil))
	if strings.Contains(string(clean), "retr") {
		t.Errorf("a run without retries must publish no retry fields: %s", clean)
	}
}
