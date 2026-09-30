// research_route_test.go: register C-76 - a research digest is routed by the
// SHAPE of its contract, never by the words of the caller's question; and the
// second chance for a failed digest queues for a busy seat instead of being
// skipped (INV-4: a busy seat is a place in line).

package delegate

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// digestContract is what offload_research builds for one fetched page: the
// research door, one context document, an output schema - and the caller's own
// question as the goal, whatever words it happens to use.
func digestContract(goal string) core.AgentContract {
	c := verifiedContract()
	c.Goal = goal
	c.Door = "offload_research"
	c.Context = []core.ContextDoc{{Name: "01-example.txt", Text: "the page text"}}
	return c
}

// researchGoals are questions a caller really asks of a page. Every one of them
// carries a word explanationRe reads as "reasoning".
var researchGoals = []string{
	"Summarize the architecture of this project and list its main modules",
	"explain how the retry path interacts with the queue",
	"what does the page say about why the build fails",
	"compare the two designs described here and list the differences",
	"trace the request through the modules and name each one",
}

func TestResearchDigestIsMechanicalWhateverTheGoalSays(t *testing.T) {
	for _, goal := range researchGoals {
		st := Subtask{Contract: digestContract(goal), EstTokens: 100}
		if got := inferKind(st); got != KindMechanical {
			t.Errorf("inferKind(research digest %q) = %v, want mechanical — the page holds the answer, whatever the question's words", goal, got)
		}
		if kind, rule := shapeOf(st); kind != KindMechanical || rule != "research-digest" {
			t.Errorf("shapeOf(%q) = %v/%q, want mechanical/research-digest (the placement reason must name the rule that fired)", goal, kind, rule)
		}
		// The guard: the SAME words on any other door are still read from the goal.
		other := st
		other.Contract.Door = "agent_delegate"
		if got := inferKind(other); got != KindReasoning {
			t.Errorf("inferKind(agent_delegate %q) = %v, want reasoning — only the research door's shape is structural", goal, got)
		}
	}
	// And the shape means one page and a schema: a research-door contract without
	// them is read from its goal as before.
	two := digestContract(researchGoals[0])
	two.Context = append(two.Context, core.ContextDoc{Name: "02-example.txt", Text: "another page"})
	if got := inferKind(Subtask{Contract: two}); got != KindReasoning {
		t.Errorf("a two-document research contract is not a page digest; inferKind = %v, want the goal's own reading", got)
	}
	bare := digestContract(researchGoals[0])
	bare.OutputSchema = nil
	if got := inferKind(Subtask{Contract: bare}); got != KindReasoning {
		t.Errorf("a schema-less research contract is not a page digest; inferKind = %v, want the goal's own reading", got)
	}
}

// TestResearchDigestsAreDealtByShapeNotByWords: three digests whose questions are
// full of reasoning words, three seats of different windows. The deal ranks a
// mechanical contract by expected completion and then the SMALLEST adequate seat
// (the roomy seat stays free for work that needs the room); the words used to send
// every one of them to the roomiest seat first.
func TestResearchDigestsAreDealtByShapeNotByWords(t *testing.T) {
	deal3 := func(door string) []string {
		r := fitRunner(fitBigRemote, fitMidRemote, fitSmallRemote)
		r.spreadLease = gpulease.Info{Held: true, Class: gpulease.ClassText} // the local seat is out of the rotation
		cs := make([]core.AgentContract, 3)
		for i := range cs {
			cs[i] = digestContract("explain how the architecture works and why it was built this way")
			cs[i].Door = door
		}
		slots := r.dealSpread(cs, fitLocal())
		out := make([]string, len(slots))
		for i, sl := range slots {
			out[i] = sl.view.NodeID
		}
		return out
	}
	if got, want := deal3("offload_research"), []string{"small-remote", "mid-remote", "big-remote"}; !equalStrings(got, want) {
		t.Fatalf("research digests dealt %v, want %v — routed by contract shape (mechanical: smallest adequate seat first)", got, want)
	}
	if got, want := deal3("agent_delegate"), []string{"big-remote", "mid-remote", "small-remote"}; !equalStrings(got, want) {
		t.Fatalf("the same goals on agent_delegate dealt %v, want %v — reasoning words still rank the roomiest seat first there", got, want)
	}
}

// ---- the second chance queues for a busy seat -----------------------------

// TestRunRetryQueuesForABusySeatInsteadOfSkipping: the first attempt failed
// verification and the retry seat is running another job. The retry used to be
// skipped on the spot ("a shared seat would only slow both") - the opposite of
// INV-4. It now waits in line for the seat, runs on it the moment it has room, and
// recovers the digest.
func TestRunRetryQueuesForABusySeatInsteadOfSkipping(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	var busyNow atomic.Int64
	busyNow.Store(1)
	var freedAt, retryAt atomic.Int64
	node, url := eligibleNode(t, "node-a", "verified from A")
	node.maxConcurrentJobs = 1
	node.jobsRunningFn = func() int { return int(busyNow.Load()) }
	node.queueDepthFn = func() int { return int(busyNow.Load()) }
	node.onDispatch = func(string, core.AgentContract) { retryAt.Store(time.Now().UnixNano()) }
	go func() {
		time.Sleep(300 * time.Millisecond)
		freedAt.Store(time.Now().UnixNano())
		busyNow.Store(0)
	}()
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 10

	var localCalls atomic.Int64
	results, sum, err := RunWith(t.Context(), cfg, failingLocal(&localCalls), []core.AgentContract{verifiedContract()}, "spread", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if node.dispatches.Load() != 1 || sum.Retried != 1 || sum.RetryRecovered != 1 || pr.RetriedOn != "node-a" {
		t.Fatalf("dispatches=%d summary=%+v retried_on=%q note=%q — the retry must wait for the seat and then run on it", node.dispatches.Load(), sum, pr.RetriedOn, pr.RetryNote)
	}
	if retryAt.Load() == 0 || freedAt.Load() == 0 || retryAt.Load() < freedAt.Load() {
		t.Fatalf("the retry reached the seat before it had room (retry %d, freed %d) — two runs would have shared it", retryAt.Load(), freedAt.Load())
	}
}

// TestRunRetryQueueExpiresAsASkipNamingTheWait: a seat that never frees inside the
// placement wait still skips the retry - the first attempt's answer stands - and
// the note says how long the retry waited in line.
func TestRunRetryQueueExpiresAsASkipNamingTheWait(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := eligibleNode(t, "node-a", "verified from A")
	node.jobsRunning, node.queueDepth, node.maxConcurrentJobs = 1, 1, 1 // busy for good
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 1

	var localCalls atomic.Int64
	start := time.Now()
	results, sum, err := RunWith(t.Context(), cfg, failingLocal(&localCalls), []core.AgentContract{verifiedContract()}, "spread", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if node.dispatches.Load() != 0 || sum.Retried != 0 {
		t.Fatalf("dispatches=%d retried=%d — a seat that never freed must not receive the retry", node.dispatches.Load(), sum.Retried)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("gave up after %s, want the retry to have waited out the 1 s placement wait", elapsed)
	}
	for _, want := range []string{"retry skipped", "already running another job", "after waiting", "in line"} {
		if !strings.Contains(pr.RetryNote, want) {
			t.Errorf("retry_note = %q, want it to contain %q", pr.RetryNote, want)
		}
	}
}
