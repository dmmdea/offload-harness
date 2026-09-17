package delegate

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// TestRetryCarriesAnExplicitRemainderNeverTheAutoMarker (register D-03): the
// first attempt of an unsized contract goes out with timeout_auto — the node
// sizes its wall — and the retry goes out with what is LEFT: an explicit
// number, no marker, because a retry's budget is the delegator's remainder,
// not a second node's estimate. The remainder also proves the delegator held
// the CAP open for the auto contract: a delegator that budgeted it at the wire
// default would hand the retry at most ~300 s.
func TestRetryCarriesAnExplicitRemainderNeverTheAutoMarker(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "wrong answer") // fails acceptance -> retryable
	nodeB, urlB := eligibleNode(t, "node-b", "qube from B")  // passes acceptance
	var seenA, seenB atomic.Value
	nodeA.onDispatch = func(_ string, c core.AgentContract) { seenA.Store(c) }
	nodeB.onDispatch = func(_ string, c core.AgentContract) { seenB.Store(c) }
	auto := remoteContract()
	auto.TimeoutSec, auto.TimeoutAuto = core.AgentTimeoutSecDefault, true
	// The local seat is fenced (the D-94 shape) so the retry has exactly one
	// place to go: node-b. Placement is not this test's subject; the wire is.
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "5070 bench", Exclusive: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{auto}, "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Retried != 1 || results[0].RetriedOn != "node-b" {
		t.Fatalf("the retry must land on node-b: retried=%d retried_on=%q note=%q", sum.Retried, results[0].RetriedOn, results[0].RetryNote)
	}
	first, _ := seenA.Load().(core.AgentContract)
	retry, _ := seenB.Load().(core.AgentContract)
	if !first.TimeoutAuto || first.TimeoutSec != core.AgentTimeoutSecDefault {
		t.Fatalf("first attempt: auto=%v timeout=%d, want the marker on the wire default %d", first.TimeoutAuto, first.TimeoutSec, core.AgentTimeoutSecDefault)
	}
	if retry.TimeoutAuto || retry.TimeoutSec <= 0 || retry.TimeoutSec > core.AgentTimeoutSecCap {
		t.Fatalf("retry: auto=%v timeout=%d, want an explicit remainder inside the cap %d", retry.TimeoutAuto, retry.TimeoutSec, core.AgentTimeoutSecCap)
	}
	if retry.TimeoutSec <= core.AgentTimeoutSecDefault {
		t.Fatalf("retry remainder %d s: the delegator budgeted the auto contract at the default, not the cap", retry.TimeoutSec)
	}
}
