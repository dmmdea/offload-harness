package delegate

// Pins for the guarantees of ADR 0065 decisions 2 and 3 that a mutation battery
// over the whole-call deadline found UNPINNED: a one-line regression in each place
// below left the suite green. Every test here passes on the code as it stands and
// fails when its one line is undone — the failure each one names is the user-visible
// consequence, not the mechanism.
//
// The clocks are compressed to milliseconds, so the assertions are about which
// outcome was published and what was recorded, never about how long anything took
// (beyond the loose bounds the deadline itself implies).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// TestClientCancelBeforeTheDeadlineIsNotTheCallDeadline: the caller going away is
// not the call deadline. The client cancels long before the deadline; the seat takes
// longer to unwind than the deadline's own allowance would give it. Every
// subtask reports its own cancelled outcome and the call WAITS for the slow unwind —
// it does not abandon the seat and publish "call deadline reached; ... did not stop".
func TestClientCancelBeforeTheDeadlineIsNotTheCallDeadline(t *testing.T) {
	cfg := testCfg(t)
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		<-ctx.Done()
		time.Sleep(700 * time.Millisecond) // a slow unwind: longer than the 250 ms allowance
		return cancelledLoop(), nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	results, _, err := RunWith(ctx, cfg, local, []core.AgentContract{{Goal: "slow one"}}, "local", nil, deadlineIn(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if reason := results[0].Result.Reason; strings.HasPrefix(reason, callDeadlinePrefix) {
		t.Fatalf("a client cancel was published as the call deadline: %q", reason)
	}
	if elapsed := time.Since(start); elapsed < 650*time.Millisecond {
		t.Fatalf("returned after %s, before the cancelled seat had unwound (700ms): the call abandoned it as if the deadline had passed", elapsed)
	}
}

// TestClientCancelWhileAwaitingARunSlotIsNotTheCallDeadline: the client cancels while
// subtasks still wait for a run slot. Those subtasks never start, and say the CALL'S
// CONTEXT ended — not that a deadline (an hour away) passed.
func TestClientCancelWhileAwaitingARunSlotIsNotTheCallDeadline(t *testing.T) {
	cfg := testCfg(t)
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		<-ctx.Done()
		return cancelledLoop(), nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(200*time.Millisecond, cancel)
	contracts := make([]core.AgentContract, runConcurrency+2)
	for i := range contracts {
		contracts[i] = core.AgentContract{Goal: "slow one"}
	}
	results, _, err := RunWith(ctx, cfg, local, contracts, "local", nil, deadlineIn(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	sawNeverStarted := 0
	for i, pr := range results {
		if strings.HasPrefix(pr.Result.Reason, callDeadlinePrefix) {
			t.Fatalf("result %d claims the call deadline passed after a plain client cancel (it was an hour away): %q", i, pr.Result.Reason)
		}
		if strings.HasPrefix(pr.Err, "canceled: the call's context ended before this subtask started") {
			sawNeverStarted++
		}
	}
	if sawNeverStarted != 2 {
		t.Fatalf("%d result(s) say the call's context ended before they started, want the 2 that never got a slot", sawNeverStarted)
	}
}

// TestQueueRouteClientCancelIsAFailureNotTheCallDeadline: the same rule on
// route=queue. A cancelled poll with the deadline an hour away is the failure it
// always was, not unfinished work "cut by the call deadline".
func TestQueueRouteClientCancelIsAFailureNotTheCallDeadline(t *testing.T) {
	holder := queueHolder(t)
	cfg := config.Config{FleetQueueHolder: holder.url, StateDir: t.TempDir()}
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(200*time.Millisecond, cancel)
	results, sum, err := RunWith(ctx, cfg, nil, []core.AgentContract{queueContract("which shipment?")}, "queue", nil, deadlineIn(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failed != 1 || strings.HasPrefix(results[0].Result.Reason, callDeadlinePrefix) {
		t.Fatalf("a client cancel with the deadline an hour away was published as the call deadline: summary %+v reason %q", sum, results[0].Result.Reason)
	}
}

// queueHolderFixture is an in-process route=queue holder.
type queueHolderFixture struct {
	q   *fleetqueue.Queue
	url string
}

// queueHolder starts a holder nobody claims from unless the test does.
func queueHolder(t *testing.T) *queueHolderFixture {
	t.Helper()
	q, err := fleetqueue.Open(t.TempDir() + "/q.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	mux := http.NewServeMux()
	fleetqueue.Mount(mux, q, func(*http.Request) bool { return true })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &queueHolderFixture{q: q, url: srv.URL}
}

// queueContract is a queue-lane contract (it must carry an output schema).
func queueContract(goal string) core.AgentContract {
	return core.AgentContract{Goal: goal, OutputSchema: json.RawMessage(`{"properties":{"shipment_id":{"type":"string"}}}`), TimeoutSec: 30}
}

// hangNode is a bare fleet node whose named legs never answer.
func hangNode(t *testing.T, hang map[string]bool) string {
	t.Helper()
	mux := http.NewServeMux()
	stall := func(w http.ResponseWriter, r *http.Request) {
		// Read the body first: the server only notices the client hanging up on a request
		// whose body has been consumed, and a handler that never noticed would hold the
		// test's listener open for the whole stall.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		if hang["health"] {
			stall(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id": "node-a", "queue_depth": 0, "agent_seat": "remote-seat", "agent_ctx_tokens": 32768,
			"agent_seat_resident": true, "agent_enabled": true, "jobs_queued": 0, "jobs_running": 0,
		})
	})
	mux.HandleFunc("POST /fleet/dispatch", func(w http.ResponseWriter, r *http.Request) {
		if hang["dispatch"] {
			stall(w, r)
			return
		}
		var env struct {
			JobID string `json:"job_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": env.JobID, "status": "accepted"})
	})
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if hang["poll"] {
			stall(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": r.PathValue("id"), "state": "running"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestDeadlineDuringAFleetLegIsADeferNotAFailure: the deadline lands while a fleet
// call is IN FLIGHT — the health probe, the dispatch, or a poll never answers. Each
// is published as the call-deadline defer and never as a failure. The dispatch leg is
// the one that used to go wrong: its cancelled POST reads as a refused dispatch, and a
// result still marked refused is re-placed and ends as "placement refused" — the
// deadline turned into summary.failed (and, with nothing else succeeded, an error
// flag).
func TestDeadlineDuringAFleetLegIsADeferNotAFailure(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	for _, leg := range []string{"health", "dispatch", "poll"} {
		t.Run(leg, func(t *testing.T) {
			url := hangNode(t, map[string]bool{leg: true})
			results, sum, _ := runWithin(t, 8*time.Second, testCfg(t), neverLocal(t),
				[]core.AgentContract{remoteContract()}, "remote", []string{url}, deadlineIn(400*time.Millisecond), nil)
			pr := results[0]
			if sum.Failed != 0 || pr.Err != "" || !strings.HasPrefix(pr.Result.Reason, deadlinePrefix+"1 unfinished") {
				t.Fatalf("a %s leg in flight at the deadline was not published as the call-deadline defer: summary %+v err %q reason %q", leg, sum, pr.Err, pr.Result.Reason)
			}
		})
	}
}

// TestALateFrameFromAnAbandonedSubtaskDoesNotReachPair: through RunWith. A seat that
// ignores its context is abandoned; when it finally answers, its PAIR frame must be
// dropped, not emitted after the run has drained its emitter (an Add racing the last
// Done of a Wait panics the process). The frames at return and after the late answer
// are the same.
func TestALateFrameFromAnAbandonedSubtaskDoesNotReachPair(t *testing.T) {
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
	t.Cleanup(unblock)
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		if isSlow(c) {
			<-release // deaf to ctx
		}
		return localOK(), nil
	}
	runWithin(t, 4*time.Second, cfg, local,
		[]core.AgentContract{{Goal: "fast one"}, {Goal: "slow one"}}, "local", nil, deadlineIn(300*time.Millisecond), unblock)
	atReturn := len(c.snapshot())
	unblock()
	time.Sleep(600 * time.Millisecond)
	if after := len(c.snapshot()); after != atReturn {
		t.Fatalf("%d PAIR frame(s) were emitted after RunWith returned: the late frame of an abandoned subtask was not dropped", after-atReturn)
	}
}

// TestPairInflightFrameIsFencedOnceTheRunIsShut: the queued / running frame is behind
// the same fence as the terminal one (the terminal frame has its own test). The
// control shows an open run emits it.
func TestPairInflightFrameIsFencedOnceTheRunIsShut(t *testing.T) {
	pairAppDir(t)
	c := &pairCapture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	defer srv.Close()
	cfg := testCfg(t)
	cfg.PairWorkloadsEnabled = true
	cfg.PairWorkloadsEndpoint = srv.URL
	r := &runner{cfg: cfg, pair: pairworkloads.New(pairworkloads.FromConfig(cfg))}
	var open PlacedResult
	r.pairInflight(&open, "agd-open", "node-a", nil, "seat-m", "running", false) // control: an open run emits
	r.pair.Wait()
	if n := len(c.snapshot()); n != 1 {
		t.Fatalf("control: %d frame(s) from an open run, want 1", n)
	}
	r.shutPair()
	var late PlacedResult
	r.pairInflight(&late, "agd-late", "node-a", nil, "seat-m", "running", false)
	r.pair.Wait()
	waitABit()
	if n := len(c.snapshot()); n != 1 {
		t.Fatalf("%d frame(s) after the run was shut, want only the control's 1: the in-flight emit is not fenced", n)
	}
}

// passRows is how many ledger rows say a subtask finished and passed.
func passRows(t *testing.T, path string) int {
	t.Helper()
	rows, err := ledger.ReadAll(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, row := range rows {
		if row.AcceptanceResult == "pass" {
			n++
		}
	}
	return n
}

// TestAnAbandonedSubtasksLateAnswerStillReachesTheLedger: ADR 0065 decision 3 says the
// abandoned subtask's own rows are still recorded and the ledger closes after the last
// such goroutine returns. A seat that ignores its context is released after the call
// returned; its real, finished answer must then be a row. The ledger handle held for it
// is the only thing that lets it be (closed under it, the row is silently lost).
func TestAnAbandonedSubtasksLateAnswerStillReachesTheLedger(t *testing.T) {
	cfg := testCfg(t)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		if isSlow(c) {
			<-release // deaf to ctx
		}
		return localOK(), nil
	}
	_, sum, _ := runWithin(t, 4*time.Second, cfg, local,
		[]core.AgentContract{{Goal: "fast one"}, {Goal: "slow one"}}, "local", nil, deadlineIn(300*time.Millisecond), unblock)
	if sum != (Summary{Succeeded: 1, Deferred: 1}) {
		t.Fatalf("summary %+v, want one success and one abandoned subtask", sum)
	}
	if n := passRows(t, cfg.LedgerPath); n != 1 {
		t.Fatalf("%d pass row(s) at return, want only the finished subtask's", n)
	}
	unblock()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end) && passRows(t, cfg.LedgerPath) < 2; time.Sleep(20 * time.Millisecond) {
	}
	if n := passRows(t, cfg.LedgerPath); n != 2 {
		t.Fatalf("%d pass row(s) after the abandoned seat answered, want 2: its own row was lost (the ledger closed under its goroutine)", n)
	}
	time.Sleep(100 * time.Millisecond) // let the goroutine finish before the temp dir goes
}

// TestUnwindAllowanceIsATwentiethClampedToItsBounds: after the deadline cooperating
// subtasks get a twentieth of the time that was left, never less than 250 ms (a
// compressed test clock) and never more than 10 s (the live check is "call wall <=
// deadline + 30 s"). An unclamped allowance would either abandon a healthy unwind or let
// an hour-long deadline hold the call for minutes.
func TestUnwindAllowanceIsATwentiethClampedToItsBounds(t *testing.T) {
	for _, tc := range []struct {
		left time.Duration
		want time.Duration
	}{
		{100 * time.Millisecond, 250 * time.Millisecond}, // a compressed clock: the floor
		{40 * time.Second, 2 * time.Second},              // a twentieth
		{1500 * time.Second, 10 * time.Second},           // the production default: the ceiling
		{time.Hour, 10 * time.Second},
	} {
		c := newCallDeadline(context.Background(), &RunOptions{Deadline: time.Now().Add(tc.left)}, 1)
		lo, hi := tc.want-tc.want/10-20*time.Millisecond, tc.want+tc.want/10+20*time.Millisecond
		if c.grace < lo || c.grace > hi {
			t.Errorf("deadline in %s: unwind allowance = %s, want about %s", tc.left, c.grace, tc.want)
		}
	}
}

// TestUnfinishedCountIsFrozenAtTheFirstLook: every reason one call publishes states
// the same number. A subtask that finishes for REAL in the unwind, after another
// subtask's reason was published, must not change what a later reason says. The order is
// forced through the progress callback (it fires once a result is on the board and
// counted), not through sleeps.
func TestUnfinishedCountIsFrozenAtTheFirstLook(t *testing.T) {
	cfg := testCfg(t)
	aDone, bDone := make(chan struct{}), make(chan struct{})
	var onceA, onceB sync.Once
	opts := deadlineIn(400 * time.Millisecond)
	opts.OnProgress = func(ev ProgressEvent) {
		if ev.Kind != "finished" {
			return
		}
		switch ev.Index {
		case 0:
			onceA.Do(func() { close(aDone) })
		case 1:
			onceB.Do(func() { close(bDone) })
		}
	}
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		<-ctx.Done()
		switch {
		case strings.Contains(c.Goal, "cut-a"):
			return cancelledLoop(), nil // cut first: its reason freezes the count at 3
		case strings.Contains(c.Goal, "late-b"):
			<-aDone
			return localOK(), nil // finishes for REAL after a's reason was published
		default:
			<-bDone
			return cancelledLoop(), nil // cut after b was answered: unfrozen it would say 2
		}
	}
	results, _, _ := runWithin(t, 4*time.Second, cfg, local,
		[]core.AgentContract{{Goal: "cut-a"}, {Goal: "late-b"}, {Goal: "cut-c"}}, "local", nil, opts, nil)
	a, c := results[0].Result.Reason, results[2].Result.Reason
	if results[1].Result.Deferred || !strings.HasPrefix(a, deadlinePrefix+"3 unfinished") || !strings.HasPrefix(c, deadlinePrefix+"3 unfinished") {
		t.Fatalf("reasons %q / %q: every reason of one call must state the same count (frozen at the first look)", a, c)
	}
}

// TestLocalRunCutMidRunStaysPlacedAndNamed: a local run the deadline cancels WAS
// placed — it ran on this box's seat — so its result names the node and the seat and
// says what it was doing, rather than "had not been placed on a seat" (which is for work
// no node ever took).
func TestLocalRunCutMidRunStaysPlacedAndNamed(t *testing.T) {
	cfg := testCfg(t)
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		<-ctx.Done()
		return cancelledLoop(), nil
	}
	results, _, _ := runWithin(t, 4*time.Second, cfg, local,
		[]core.AgentContract{{Goal: "slow one"}}, "local", nil, deadlineIn(300*time.Millisecond), nil)
	pr := results[0]
	if pr.Unplaced || pr.Node == "" || !strings.Contains(pr.Result.Reason, "still running on the local seat") {
		t.Fatalf("a local run cut mid-run must stay a placed, named result that says so: unplaced=%v node=%q reason=%q", pr.Unplaced, pr.Node, pr.Result.Reason)
	}
}

// TestQueueRouteCountsEveryCutJob: two queued jobs nobody claims, both cut. The count
// in each reason is the whole call's (2), and the summary moves both from failed to
// deferred.
func TestQueueRouteCountsEveryCutJob(t *testing.T) {
	holder := queueHolder(t)
	cfg := config.Config{FleetQueueHolder: holder.url, StateDir: t.TempDir()}
	results, sum, _ := runWithin(t, 4*time.Second, cfg, nil,
		[]core.AgentContract{queueContract("which shipment one?"), queueContract("which shipment two?")}, "queue", nil, deadlineIn(300*time.Millisecond), nil)
	for i, pr := range results {
		if !strings.HasPrefix(pr.Result.Reason, deadlinePrefix+"2 unfinished") {
			t.Fatalf("result %d reason %q, want the whole call's count (2 unfinished)", i, pr.Result.Reason)
		}
	}
	if sum != (Summary{Deferred: 2}) {
		t.Fatalf("summary %+v, want both jobs deferred", sum)
	}
}

// TestNeverStartedRowCarriesTheJobIDTheCallerWasGiven: a page that never started is
// still answered, and its ledger row is the one the caller was told about: the same job
// id, the same call-deadline wording. A row under a different id could not be found from
// the result.
func TestNeverStartedRowCarriesTheJobIDTheCallerWasGiven(t *testing.T) {
	cfg := testCfg(t)
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		if isSlow(c) {
			<-ctx.Done()
			return cancelledLoop(), nil
		}
		return localOK(), nil
	}
	contracts := make([]core.AgentContract, 9)
	for i := range contracts {
		contracts[i] = core.AgentContract{Goal: "fast one"}
	}
	contracts[7] = core.AgentContract{Goal: "slow one"}
	res, _, err := RunBatched(t.Context(), cfg, local, contracts, "local", nil, deadlineIn(400*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := ledger.ReadAll(cfg.LedgerPath)
	found := false
	for _, row := range rows {
		if row.JobID == res[8].JobID {
			found = true
			if !strings.HasPrefix(row.Reason, deadlinePrefix+"2 unfinished") {
				t.Fatalf("the never-started page's row reads %q, want the call-deadline wording", row.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("no ledger row carries the job id (%q) the caller was given for the never-started page", res[8].JobID)
	}
}

// TestTheDeadlineContextCarriesTheCause: the context every placement and seat runs under
// ends with the exported cause, so an error that names it ("call deadline reached") is
// the deadline's, and a transport error wrapping it can be told from a caller cancel.
func TestTheDeadlineContextCarriesTheCause(t *testing.T) {
	cfg := testCfg(t)
	var cause error
	local := func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		<-ctx.Done()
		cause = context.Cause(ctx)
		return cancelledLoop(), nil
	}
	runWithin(t, 4*time.Second, cfg, local, []core.AgentContract{{Goal: "slow one"}}, "local", nil, deadlineIn(300*time.Millisecond), nil)
	if cause != ErrCallDeadline {
		t.Fatalf("context.Cause = %v, want ErrCallDeadline", cause)
	}
}

// TestWithdrawRequestTargetIsTheCleanJobRoute: the withdraw URL is built from the node's
// dial base, and a base written with a trailing slash must not become
// "//fleet/jobs/{id}". A Go ServeMux quietly cleans that path, so the node here is a bare
// handler that records the RAW request target — the form a proxy or a stricter node in
// front of the fleet port would see.
func TestWithdrawRequestTargetIsTheCleanJobRoute(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, inner := remoteRunningForeverServer(t)
	var mu sync.Mutex
	var targets []string
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			targets = append(targets, r.RequestURI)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	results, _, _ := runWithin(t, 4*time.Second, testCfg(t), neverLocal(t),
		[]core.AgentContract{remoteGoal("slow one")}, "remote", []string{front.URL + "/"}, deadlineIn(300*time.Millisecond), nil)
	mu.Lock()
	defer mu.Unlock()
	if want := "/fleet/jobs/" + results[0].JobID; len(targets) != 1 || targets[0] != want {
		t.Fatalf("withdraw request target(s) %q for a remote configured as %q, want exactly [%q]", targets, front.URL+"/", want)
	}
}

// oneRefusalThenNoRoom is a node with room at deal time that refuses its first dispatch
// (503) and then advertises no room, so the capacity wait never re-enters attempt(): it
// ends by the CONTEXT, into settle, with exactly one attempt behind it. Deterministic (no
// timer race decides which path cuts).
func oneRefusalThenNoRoom(t *testing.T) string {
	t.Helper()
	_, url := refusingNode(t, "node-full", http.StatusServiceUnavailable, func(f *fakeNode) {
		f.maxConcurrentJobs, f.maxQueueDepth = 1, 2
		f.jobsRunningFn = func() int {
			if f.dispatches.Load() >= 1 {
				return 1
			}
			return 0
		}
		f.queueDepthFn = func() int {
			if f.dispatches.Load() >= 1 {
				return 2
			}
			return 0
		}
	})
	return url
}

// TestAWaitCutAfterARefusalIsStillTheCallDeadline: a subtask whose first dispatch was
// refused (503) and whose capacity wait the call then ends is published as the
// call-deadline defer, exactly as a wait with no attempt behind it is.
func TestAWaitCutAfterARefusalIsStillTheCallDeadline(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 50*time.Millisecond)
	url := oneRefusalThenNoRoom(t)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 30
	results, _, _ := runWithin(t, 5*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{remoteContract()}, "remote", []string{url}, deadlineIn(400*time.Millisecond), nil)
	r := results[0].Result
	if !strings.HasPrefix(r.Reason, deadlinePrefix+"1 unfinished") || r.DeferClass != core.DeferClassBudget {
		t.Fatalf("a wait cut by the call deadline after a refusal was published as class %q reason %q", r.DeferClass, r.Reason)
	}
}

// TestAttemptGuardCutNamesTheDeadlineInThePlacement: the attempt guard (nothing starts
// once the call is over) publishes a result whose placement narration also opens with
// the deadline marker, so the corpus row's placement column agrees with its reason.
func TestAttemptGuardCutNamesTheDeadlineInThePlacement(t *testing.T) {
	r := &runner{cfg: testCfg(t), local: func(context.Context, core.AgentContract, LocalOptions) (core.AgentWireResult, error) {
		return localOK(), nil
	}, route: "local", call: pastDeadline(1)}
	pr := r.attempt(t.Context(), 0, core.AgentContract{Goal: "say done"}, nil)
	if !strings.HasPrefix(pr.PlacementReason, callDeadlinePrefix) {
		t.Fatalf("placement narration = %q, want it to open with the call-deadline marker", pr.PlacementReason)
	}
}
