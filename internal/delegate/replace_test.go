// replace_test.go pins RE-PLACEMENT ON REFUSAL: a node that answers a dispatch
// with anything other than the one 202 ack — or that the delegator cannot reach
// at all — must not end the subtask. Before this, `retryable` returned false for
// any result carrying an Err, so a `503 queue full` from the first-choice node
// was TERMINAL: no other node was tried, local was never asked, and the work was
// simply not done on a healthy fleet.
//
// It also pins the companion half — capacity-aware placement — because the
// cheapest refusal is the one that never happens: a node already at its
// published admission ceiling is a node that WILL answer 503, and it must not
// win the placement over one with headroom.

package delegate

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// refusingNode is a fleet node that passes the health gate (so placement really
// does choose it) and REFUSES every dispatch with the given status. tune runs
// BEFORE the listener starts: every field the handler goroutines read must be
// written before the server exists, or the write races the first request.
func refusingNode(t *testing.T, id string, status int, tune func(*fakeNode)) (*fakeNode, string) {
	t.Helper()
	f := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: id,
		dispatchStatus: status,
		pollState:      func(int64) (map[string]any, int) { return nil, http.StatusNotFound },
	}
	if tune != nil {
		tune(f)
	}
	return f, f.server().URL
}

// acceptingNode is an eligible fleet node that completes every job with output.
// Same one-literal-then-listen discipline as refusingNode.
func acceptingNode(t *testing.T, id, output string, tune func(*fakeNode)) (*fakeNode, string) {
	t.Helper()
	f := &fakeNode{t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: id}
	f.pollByJob = func(jobID string, n int64) (map[string]any, int) {
		w := remoteWire(output, `{"answer":"`+output+`"}`)
		w.NodeID = id
		return doneWire(t, w), http.StatusOK
	}
	if tune != nil {
		tune(f)
	}
	return f, f.server().URL
}

// TestRunReplacesA503RefusalOnAnotherNode is the defect itself: the
// first-choice node is full, a sibling is idle, and the subtask must land on
// the sibling instead of being reported as failed work nobody ran.
func TestRunReplacesA503RefusalOnAnotherNode(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	full, fullURL := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	idle, idleURL := acceptingNode(t, "node-idle", "qube from the idle node", nil)

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{fullURL, idleURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Succeeded: 1, Replaced: 1, ReplacementRecovered: 1}) {
		t.Fatalf("summary = %+v, want one success recovered by re-placement", sum)
	}
	if results[0].Node != "node-idle" {
		t.Fatalf("node = %q, want the work re-placed on node-idle", results[0].Node)
	}
	if results[0].Replacements != 1 {
		t.Fatalf("replacements = %d, want 1", results[0].Replacements)
	}
	note := results[0].ReplacementNote
	if !strings.Contains(note, "node-full") || !strings.Contains(note, "503") {
		t.Fatalf("replacement_note = %q, want it to name node-full and its 503", note)
	}
	// The refusing node is asked ONCE. dispatchAttempts exists for transport
	// DOUBT; an answered 503 is not doubt, and re-asking the same node is not
	// re-placement.
	if got := full.dispatches.Load(); got != 1 {
		t.Fatalf("refusing node saw %d dispatches, want exactly 1", got)
	}
	if got := idle.dispatches.Load(); got != 1 {
		t.Fatalf("replacement node saw %d dispatches, want exactly 1", got)
	}
}

// TestRunUnreachableNodeIsReplaced: a node that answers health but drops the
// dispatch connection is unreachable RIGHT NOW, not wrong about the contract.
// Both dispatch attempts fail at transport level (that bounded retry is
// preserved), and the subtask then moves to a node that can take it.
func TestRunUnreachableNodeIsReplaced(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	dead := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-dead",
		killOnDispatch: true,
		pollState:      func(int64) (map[string]any, int) { return nil, http.StatusNotFound },
	}
	deadURL := dead.server().URL
	live, liveURL := acceptingNode(t, "node-live", "qube from the live node", nil)

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{deadURL, liveURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Succeeded: 1, Replaced: 1, ReplacementRecovered: 1}) {
		t.Fatalf("summary = %+v, want one success recovered by re-placement", sum)
	}
	if results[0].Node != "node-live" {
		t.Fatalf("node = %q, want the work re-placed on node-live", results[0].Node)
	}
	// dispatchAttempts (2) is a SEPARATE mechanism and must survive: the
	// unreachable node is POSTed to twice under the same job id before the
	// delegator concludes it cannot be reached.
	if got := dead.dispatches.Load(); got != dispatchAttempts {
		t.Fatalf("unreachable node saw %d dispatches, want dispatchAttempts=%d", got, dispatchAttempts)
	}
	if got := live.dispatches.Load(); got != 1 {
		t.Fatalf("replacement node saw %d dispatches, want exactly 1", got)
	}
}

// TestRunRefusalFallsBackToLocalWhenNoRemoteIsLeft: the "then local" half. With
// route=auto, the GPU lease held and the ONE eligible remote refusing, the work
// must run on the local seat — queued-local beats work nobody did.
func TestRunRefusalFallsBackToLocalWhenNoRemoteIsLeft(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	full, fullURL := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)

	leaseDir := filepath.Join(t.TempDir(), "lease")
	m, err := gpulease.OpenAt(leaseDir, "")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "delegate-replace-test", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()

	cfg := testCfg(t)
	cfg.GPULockPath = leaseDir

	var localCalls atomic.Int64
	results, sum, err := Run(t.Context(), cfg, passingLocal(&localCalls),
		[]core.AgentContract{remoteContract()}, "auto", []string{fullURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Succeeded: 1, Replaced: 1, ReplacementRecovered: 1}) {
		t.Fatalf("summary = %+v, want the local seat to have recovered the refusal", sum)
	}
	if localCalls.Load() != 1 {
		t.Fatalf("local runner called %d times, want 1", localCalls.Load())
	}
	if got := full.dispatches.Load(); got != 1 {
		t.Fatalf("refusing node saw %d dispatches, want exactly 1", got)
	}
	if !strings.Contains(results[0].PlacementReason, "re-placed") {
		t.Fatalf("placement = %q, want it to say the subtask was re-placed", results[0].PlacementReason)
	}
}

// TestRunExhaustedFleetIsADistinctFailureNotADefer: every eligible node refused
// and route=remote never falls back to local. The outcome must be a FAILURE
// with its own sentence — not a manufactured defer (no seat ever saw the
// contract, so there is no report to author on a node's behalf), and not
// confusable with 0.100.0's "the node accepted it and never started it".
func TestRunExhaustedFleetIsADistinctFailureNotADefer(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	a, aURL := refusingNode(t, "node-a", http.StatusServiceUnavailable, nil)
	b, bURL := refusingNode(t, "node-b", http.StatusServiceUnavailable, nil)

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{aURL, bURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Failed: 1, Replaced: 1}) {
		t.Fatalf("summary = %+v, want one FAILURE after an exhausted fleet", sum)
	}
	pr := results[0]
	if pr.Result.Deferred {
		t.Fatal("an exhausted fleet must never manufacture a defer — no seat saw this contract")
	}
	if !strings.HasPrefix(pr.Err, replacementExhaustedPrefix) {
		t.Fatalf("err = %q, want the stable %q prefix", pr.Err, replacementExhaustedPrefix)
	}
	for _, want := range []string{"node-a", "node-b", "503"} {
		if !strings.Contains(pr.Err, want) {
			t.Fatalf("err = %q, want it to name %q", pr.Err, want)
		}
	}
	// Distinct from the two deadline sentences 0.100.0 already produces.
	for _, forbidden := range []string{"queue deadline", "poll deadline"} {
		if strings.Contains(pr.Err, forbidden) {
			t.Fatalf("err = %q must not read as %q — a refused job never started anywhere", pr.Err, forbidden)
		}
	}
	if a.dispatches.Load() != 1 || b.dispatches.Load() != 1 {
		t.Fatalf("dispatches a=%d b=%d, want each node asked exactly once",
			a.dispatches.Load(), b.dispatches.Load())
	}
}

// TestRun400ClassRefusalIsNeverReplaced: a 400 is the node rejecting THIS
// REQUEST. The delegator hands the next node byte-identical bytes and the same
// bearer, so re-placing only collects the same answer again while spending the
// contract's budget. It must stop at the first node.
func TestRun400ClassRefusalIsNeverReplaced(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"400 malformed/unsupported", http.StatusBadRequest},
		{"401 token mismatch", http.StatusUnauthorized},
		{"403 agent lane requires a token", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad, badURL := refusingNode(t, "node-bad", tc.status, nil)
			good, goodURL := acceptingNode(t, "node-good", "qube from the good node", nil)

			results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t),
				[]core.AgentContract{remoteContract()}, "remote", []string{badURL, goodURL})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if sum != (Summary{Failed: 1}) {
				t.Fatalf("summary = %+v, want one failure and NO re-placement", sum)
			}
			if got := good.dispatches.Load(); got != 0 {
				t.Fatalf("second node saw %d dispatches, want 0 — a request-class refusal recurs everywhere", got)
			}
			if bad.dispatches.Load() != 1 {
				t.Fatalf("refusing node saw %d dispatches, want 1", bad.dispatches.Load())
			}
			if results[0].Replacements != 0 {
				t.Fatalf("replacements = %d, want 0", results[0].Replacements)
			}
		})
	}
}

// TestRunReplacementNeverExceedsTheContractBudget: re-placement lives INSIDE
// the subtask's own timeout_sec, exactly as the verification retry does. The
// replacement node must be handed what is LEFT of the budget, never a fresh
// copy of it — the caller was told that number is the per-subtask wall ceiling.
func TestRunReplacementNeverExceedsTheContractBudget(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	// The refusing node spends real wall clock before refusing, so "what is
	// left" is provably smaller than the original budget rather than equal to
	// it by rounding.
	_, slowURL := refusingNode(t, "node-slow", http.StatusServiceUnavailable, func(f *fakeNode) {
		f.dispatchHook = func(int64) int {
			time.Sleep(1200 * time.Millisecond)
			return http.StatusServiceUnavailable
		}
	})
	var got atomic.Int64
	_, takerURL := acceptingNode(t, "node-taker", "qube from the taker", func(f *fakeNode) {
		f.onDispatch = func(_ string, c core.AgentContract) { got.Store(int64(c.TimeoutSec)) }
	})

	contract := remoteContract() // TimeoutSec: 30
	_, sum, err := Run(t.Context(), testCfg(t), neverLocal(t),
		[]core.AgentContract{contract}, "remote", []string{slowURL, takerURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v, want the re-placement to have succeeded", sum)
	}
	replaced := int(got.Load())
	if replaced <= 0 || replaced >= contract.TimeoutSec {
		t.Fatalf("re-placed contract carried timeout_sec=%d, want 0 < t < %d (the remaining budget, never a fresh one)",
			replaced, contract.TimeoutSec)
	}
	if replaced > contract.TimeoutSec-1 {
		t.Fatalf("re-placed timeout_sec=%d does not reflect the ~1.2s already spent", replaced)
	}
}

// TestRunReplacementIsBounded: a fleet where every node refuses must not be
// walked forever. The bound is on RE-PLACEMENTS, and exhausting it produces the
// same honest sentence, naming the bound rather than pretending nothing else
// existed.
func TestRunReplacementIsBounded(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	var nodes []*fakeNode
	var urls []string
	for _, id := range []string{"r1", "r2", "r3", "r4", "r5"} {
		n, u := refusingNode(t, id, http.StatusServiceUnavailable, nil)
		nodes = append(nodes, n)
		urls = append(urls, u)
	}
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", urls)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Failed != 1 {
		t.Fatalf("summary = %+v, want one failure", sum)
	}
	var asked int
	for _, n := range nodes {
		asked += int(n.dispatches.Load())
	}
	want := 1 + maxRemoteReplacements
	if asked != want {
		t.Fatalf("%d nodes were dispatched to, want %d (the first choice plus maxRemoteReplacements=%d)",
			asked, want, maxRemoteReplacements)
	}
	if results[0].Replacements != maxRemoteReplacements {
		t.Fatalf("replacements = %d, want maxRemoteReplacements=%d", results[0].Replacements, maxRemoteReplacements)
	}
	if !strings.Contains(results[0].Err, "bound") {
		t.Fatalf("err = %q, want the exhausted message to name the bound it hit", results[0].Err)
	}
}

// --- capacity-aware placement -------------------------------------------

// capacityRemote builds an eligible remote carrying a full capacity
// advertisement.
func capacityRemote(id string, depth, queued, running, maxConcurrent, maxDepth int) NodeView {
	v := eligibleRemote()
	v.NodeID = id
	v.QueueDepth = depth
	v.JobsQueued = queued
	v.JobsRunning = running
	v.MaxConcurrentJobs = maxConcurrent
	v.MaxQueueDepth = maxDepth
	return v
}

// TestPlaceIsCapacityAware: queue_depth alone is meaningless without the limit
// it is measured against. A node at its published admission ceiling WILL answer
// 503, and must lose to any node that is not provably full — however deep that
// node's backlog is.
func TestPlaceIsCapacityAware(t *testing.T) {
	st := schemaSubtask()
	cases := []struct {
		name    string
		remotes []NodeView
		want    string
	}{
		{
			name: "a node at its ceiling loses to a deeper node that never published one",
			remotes: []NodeView{
				capacityRemote("full", 1, 1, 0, 1, 1),
				capacityRemote("deep-but-open", 500, 500, 0, 0, 0),
			},
			want: "deep-but-open",
		},
		{
			name: "a node at its ceiling loses even when it is listed first and shallower",
			remotes: []NodeView{
				capacityRemote("full", 2, 2, 0, 1, 2),
				capacityRemote("roomy", 5, 1, 4, 8, 32),
			},
			want: "roomy",
		},
		{
			name: "a provably free execution slot beats a shallower node whose workers are all busy",
			remotes: []NodeView{
				capacityRemote("all-busy", 2, 1, 1, 1, 32),
				capacityRemote("free-slot", 4, 0, 4, 8, 32),
			},
			want: "free-slot",
		},
		{
			name: "both at their ceiling: the existing lowest-queue_depth rule still decides",
			remotes: []NodeView{
				capacityRemote("full-deep", 9, 9, 0, 1, 9),
				capacityRemote("full-shallow", 2, 2, 0, 1, 2),
			},
			want: "full-shallow",
		},
		{
			name: "no capacity numbers at all: lowest queue_depth, exactly as before",
			remotes: []NodeView{
				capacityRemote("deep", 3, 0, 0, 0, 0),
				capacityRemote("shallow", 1, 0, 0, 0, 0),
			},
			want: "shallow",
		},
		{
			name: "an idle node that publishes nothing still beats a loaded node that does",
			remotes: []NodeView{
				capacityRemote("idle-old", 0, 0, 0, 0, 0),
				capacityRemote("loaded-new", 6, 2, 4, 8, 32),
			},
			want: "idle-old",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Place(st, localNode(), tc.remotes, true)
			if got.NodeID != tc.want {
				t.Fatalf("Place chose %q, want %q", got.NodeID, tc.want)
			}
		})
	}
}

// TestRunPlacementPrefersHeadroomOverASaturatedNode drives the same rule
// through the real engine: the saturated node must never be dispatched to while
// a node with headroom exists, so the refusal never happens at all.
func TestRunPlacementPrefersHeadroomOverASaturatedNode(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	// The fixture ISOLATES the saturation key. The two nodes are deliberately
	// equal on every other ranking input — both provably start a job at once —
	// and the saturated one is listed FIRST and is SHALLOWER, so under the old
	// lowest-queue_depth rule it won every time and answered 503 every time.
	// Only "it is at its published admission ceiling" can move the placement.
	saturated, satURL := acceptingNode(t, "node-saturated", "qube from the saturated node", func(f *fakeNode) {
		f.queueDepth, f.jobsQueued, f.jobsRunning = 1, 0, 1
		f.maxConcurrentJobs, f.maxQueueDepth = 4, 1 // 1 of 1 admitted: the next dispatch is a 503
	})
	_, roomyURL := acceptingNode(t, "node-roomy", "qube from the roomy node", func(f *fakeNode) {
		f.queueDepth, f.jobsQueued, f.jobsRunning = 4, 0, 4
		f.maxConcurrentJobs, f.maxQueueDepth = 8, 32 // 4 of 32 admitted, 4 of 8 workers busy
	})

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{satURL, roomyURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Succeeded: 1}) {
		t.Fatalf("summary = %+v, want a clean success with no re-placement at all", sum)
	}
	if results[0].Node != "node-roomy" {
		t.Fatalf("node = %q, want node-roomy — a node at its admission ceiling is a poor choice", results[0].Node)
	}
	if got := saturated.dispatches.Load(); got != 0 {
		t.Fatalf("the saturated node saw %d dispatches, want 0", got)
	}
}

// TestFetchNodeViewDecodesMaxQueueDepth: the capacity advertisement is only
// usable if all four fields arrive. max_queue_depth is the one the saturation
// rule is built on, and it was the one field nodeview.go did not decode.
func TestFetchNodeViewDecodesMaxQueueDepth(t *testing.T) {
	body := `{"node_id":"cap-node","queue_depth":5,"jobs_queued":3,"jobs_running":2,
	          "max_concurrent_jobs":2,"max_queue_depth":7,
	          "agent_enabled":true,"agent_seat":"s","agent_seat_resident":true,"agent_ctx_tokens":8192}`
	srv := healthServer(t, body, nil)
	v, err := FetchNodeView(t.Context(), srv.URL, "")
	if err != nil {
		t.Fatalf("FetchNodeView: %v", err)
	}
	// Every number differs, so a decoder wired to the wrong key is caught.
	if v.QueueDepth != 5 || v.JobsQueued != 3 || v.JobsRunning != 2 ||
		v.MaxConcurrentJobs != 2 || v.MaxQueueDepth != 7 {
		t.Fatalf("capacity decoded as depth=%d queued=%d running=%d maxconc=%d maxdepth=%d, want 5/3/2/2/7",
			v.QueueDepth, v.JobsQueued, v.JobsRunning, v.MaxConcurrentJobs, v.MaxQueueDepth)
	}
}

// TestReplaceableRefusalDrawsTheLineOnWhoTheAnswerIsAbout states the rule
// directly, so the line is a pinned decision rather than an accident of the
// switch's shape.
func TestReplaceableRefusalDrawsTheLineOnWhoTheAnswerIsAbout(t *testing.T) {
	replace := []int{
		0,   // never reached the node at all
		404, // this address does not serve /fleet/dispatch
		408, 409, 429,
		500, 502, 503, 504,
		599, // an unknown 5xx is still about that node
	}
	terminal := []int{400, 401, 403, 405, 413, 415, 422, 451}
	for _, s := range replace {
		if !replaceableRefusal(s) {
			t.Errorf("status %d must be re-placeable — it is about that node, right now", s)
		}
	}
	for _, s := range terminal {
		if replaceableRefusal(s) {
			t.Errorf("status %d must be terminal — the next node is handed identical bytes", s)
		}
	}
}

// TestRunReplacementDoesNotDisturbTheRedispatchPath: maxRedispatches is a
// SEPARATE bounded retry (a poll 404, same job id, same node) and re-placement
// must not fold into it or duplicate it. A node that loses the job and then
// answers normally still completes on that node, with no re-placement recorded.
func TestRunReplacementDoesNotDisturbTheRedispatchPath(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node := &fakeNode{t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-forgetful"}
	node.pollState = func(n int64) (map[string]any, int) {
		if n == 1 {
			return map[string]any{"status": "error", "error": "unknown job"}, http.StatusNotFound
		}
		w := remoteWire("qube answer", `{"answer":"42"}`)
		w.NodeID = "node-forgetful"
		return doneWire(t, w), http.StatusOK
	}
	other, otherURL := acceptingNode(t, "node-other", "qube from the other node", nil)
	srv := node.server()

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{srv.URL, otherURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Succeeded: 1}) {
		t.Fatalf("summary = %+v, want a plain success via the 404 re-dispatch", sum)
	}
	if got := node.dispatches.Load(); got != 2 {
		t.Fatalf("dispatches = %d, want 2 (initial + the 404-triggered redispatch, same node)", got)
	}
	if got := other.dispatches.Load(); got != 0 {
		t.Fatalf("the sibling saw %d dispatches; a poll 404 is not a refusal and must not re-place", got)
	}
	if results[0].Replacements != 0 {
		t.Fatalf("replacements = %d, want 0", results[0].Replacements)
	}
}

// TestWireResponsePublishesReplacement: the caller reading JSON must be able to
// see that a subtask was re-placed and which node refused it — otherwise a
// fleet shedding load looks byte-identical to a healthy one.
func TestWireResponsePublishesReplacement(t *testing.T) {
	out := WireResponse(
		[]PlacedResult{{Node: "node-b", Replacements: 1, ReplacementNote: "node-a refused: 503"}},
		Summary{Succeeded: 1, Replaced: 1, ReplacementRecovered: 1}, nil)
	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"replaced":1`, `"replacement_recovered":1`, `"replacements":1`, `"replacement_note":"node-a refused: 503"`} {
		if !strings.Contains(string(blob), want) {
			t.Fatalf("wire = %s, want it to carry %s", blob, want)
		}
	}
	// A run with no re-placement must publish byte-identically to before.
	clean, err := json.Marshal(WireResponse([]PlacedResult{{Node: "n"}}, Summary{Succeeded: 1}, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"replaced", "replacement"} {
		if strings.Contains(string(clean), forbidden) {
			t.Fatalf("a clean run published %s, want the fields omitted entirely", clean)
		}
	}
}
