// deal_width_test.go: a call is as wide as its deal (ADR 0076).
//
// RunWith's semaphore was the constant runConcurrency (4). The joint deal could commit two subtasks to each of
// four nodes, and only four goroutines ran, so each node saw about one: a 12-page research call used four of the
// fleet's sixteen slots, and it was cut into chunks of eight that ran one after the other. The width is now the
// sum of what the deal committed to each place (dealParallelism), and RunBatched deals up to sixteen subtasks at
// once.
//
// The peak tests do not measure how fast a loaded runner schedules goroutines. Every fleet node holds its jobs
// until the fan-out has put `want` of them open at once (or a generous limit passes, which is what a regression
// pays), so reaching the peak is the proof and a stall can only delay it.

package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// fanProbe watches a fan-out from the fleet's side: how many jobs are open at once, in all and per node, where
// "open" is dispatched and not yet answered.
type fanProbe struct {
	want  int64         // the number of open jobs at which every node lets its jobs finish
	limit time.Duration // the longest a job is held for want: what a regression pays, never what a pass does

	mu       sync.Mutex
	open     int64
	peak     int64
	nodeOpen map[string]int64
	nodePeak map[string]int64
	jobs     map[string]*probeJob
	reached  chan struct{}
	once     sync.Once
}

type probeJob struct {
	node     string
	at       time.Time
	released bool
}

func newFanProbe(want int64, limit time.Duration) *fanProbe {
	return &fanProbe{want: want, limit: limit, nodeOpen: map[string]int64{}, nodePeak: map[string]int64{}, jobs: map[string]*probeJob{}, reached: make(chan struct{})}
}

// dispatched opens a job. A re-ack of a job already open (the node's idempotent 202) opens nothing.
func (p *fanProbe) dispatched(node, jobID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, dup := p.jobs[jobID]; dup {
		return
	}
	p.jobs[jobID] = &probeJob{node: node, at: time.Now()}
	p.open++
	p.nodeOpen[node]++
	p.peak = max(p.peak, p.open)
	p.nodePeak[node] = max(p.nodePeak[node], p.nodeOpen[node])
	if p.open >= p.want {
		p.once.Do(func() { close(p.reached) })
	}
}

// finished reports whether the job may answer now, and closes it the first time it may.
func (p *fanProbe) finished(jobID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	j := p.jobs[jobID]
	if j == nil {
		return true
	}
	if !j.released {
		select {
		case <-p.reached:
		default:
			if time.Since(j.at) < p.limit {
				return false
			}
		}
		j.released = true
		p.open--
		p.nodeOpen[j.node]--
	}
	return true
}

// peaks is the most jobs that were open at once, in all and per node.
func (p *fanProbe) peaks() (peak int64, perNode map[string]int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	perNode = make(map[string]int64, len(p.nodePeak))
	for k, v := range p.nodePeak {
		perNode[k] = v
	}
	return p.peak, perNode
}

// node is a fleet node that publishes `ceiling` execution slots (0 = it publishes none, as a node older than 0.100.0)
// and answers every job once the probe lets it.
func (p *fanProbe) node(t *testing.T, id string, ceiling int) (*fakeNode, string) {
	t.Helper()
	return acceptingNode(t, id, "answer from "+id, func(f *fakeNode) {
		f.maxConcurrentJobs, f.maxQueueDepth = ceiling, 2*ceiling
		f.onDispatch = func(jobID string, _ core.AgentContract) { p.dispatched(id, jobID) }
		inner := f.pollByJob
		f.pollByJob = func(jobID string, n int64) (map[string]any, int) {
			if !p.finished(jobID) {
				return map[string]any{"state": "running"}, http.StatusOK
			}
			return inner(jobID, n)
		}
	})
}

// pages is n plain contracts whose goals differ, so a local seat can tell them apart.
func pages(n int) []core.AgentContract {
	out := make([]core.AgentContract, n)
	for i := range out {
		out[i] = plainContract()
		out[i].Goal = "answer page " + string(rune('A'+i))
	}
	return out
}

// TestACallIsAsWideAsItsDeal is the defect (the diagnosis' F01, scratch proof S3): 8 subtasks, route=remote, 4 nodes that
// each publish 4 execution slots. The deal commits every subtask to a free slot and 16 are free; the call opened four at a
// time. Each node also keeps to its own ceiling: the wider call never puts more on a node than the node says it runs.
func TestACallIsAsWideAsItsDeal(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	logs := captureLog(t)
	probe := newFanProbe(8, 5*time.Second)
	var urls []string
	for _, id := range []string{"n1", "n2", "n3", "n4"} {
		_, url := probe.node(t, id, 4)
		urls = append(urls, url)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	_, sum, err := RunWith(ctx, testCfg(t), neverLocal(t), pages(8), "remote", urls, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 8 {
		t.Fatalf("summary %+v, want all 8 subtasks to succeed", sum)
	}
	peak, perNode := probe.peaks()
	if peak < 8 {
		t.Errorf("peak jobs open fleet-wide = %d, want 8: the call ran %d at a time with 16 free execution slots", peak, runConcurrency)
	}
	for id, n := range perNode {
		if n > 4 {
			t.Errorf("node %s held %d jobs open, want at most the 4 it publishes", id, n)
		}
	}
	if want := "fan-out width 8 for 8 subtask(s), sized from the remote deal"; !strings.Contains(logs.String(), want) {
		t.Errorf("log = %q, want a line saying %q: an operator asking why 8 jobs were open needs it", logs.String(), want)
	}
}

// TestACallThatIsFourWideLogsNoWidth: the width line is for a call that is not the old four. route=local has no deal, so five
// local subtasks run four at a time and say nothing about it.
func TestACallThatIsFourWideLogsNoWidth(t *testing.T) {
	logs := captureLog(t)
	var calls atomic.Int64
	_, sum, err := RunWith(t.Context(), testCfg(t), passingLocal(&calls), contracts(5), "local", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 5 || calls.Load() != 5 {
		t.Fatalf("summary %+v, local runs %d, want 5 of each", sum, calls.Load())
	}
	if strings.Contains(logs.String(), "fan-out width") {
		t.Errorf("log = %q, want no width line for a call that kept the old four", logs.String())
	}
}

// TestRunBatchedDealsTwelvePagesAsOneBatchAcrossTheFleet: an offload_research call of 12 pages over 3 nodes that each run 4
// is one deal and one batch, and all 12 pages are open at once. It used to be chunks of 8 and 4, the second waiting for the
// slowest page of the first.
func TestRunBatchedDealsTwelvePagesAsOneBatchAcrossTheFleet(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	probe := newFanProbe(12, 5*time.Second)
	var urls []string
	for _, id := range []string{"n1", "n2", "n3"} {
		_, url := probe.node(t, id, 4)
		urls = append(urls, url)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	_, sum, err := RunBatched(ctx, testCfg(t), neverLocal(t), pages(12), "remote", urls, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 12 || sum.Batches != 1 {
		t.Fatalf("summary %+v, want 12 successes in ONE batch", sum)
	}
	peak, perNode := probe.peaks()
	if peak < 12 {
		t.Errorf("peak jobs open fleet-wide = %d, want all 12 pages open at once", peak)
	}
	for id, n := range perNode {
		if n > 4 {
			t.Errorf("node %s held %d jobs open, want at most the 4 it publishes", id, n)
		}
	}
}

// TestANodeThatPublishesNoCeilingIsHeldToFourWhenTheCallsOtherLegsFinish: the width counts a node that publishes no
// ceiling as at most four, but the semaphore is one pool for the whole call. Here 12 pages are dealt over a node of four
// that answers at once and a node that publishes nothing and holds its jobs. When the fast node's legs finish they free
// their share of the width. Without a bound of its own, the unknown node then held every page dealt to it, up to 8 at
// once. The process gate holds it to four (admissionCeiling), and the pages it turns away wait in line for the first node
// that frees.
func TestANodeThatPublishesNoCeilingIsHeldToFourWhenTheCallsOtherLegsFinish(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	fast := newFanProbe(1, 5*time.Second)            // releases every job as soon as one is open
	hold := newFanProbe(1000, 1500*time.Millisecond) // never reaches its peak, so each job is held 1.5 s
	_, fastURL := fast.node(t, "n1", 4)
	_, oldURL := hold.node(t, "old", 0)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 30 // production waits for room; the test default (-1) defers at once
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	results, sum, err := RunBatched(ctx, cfg, neverLocal(t), pages(12), "remote", []string{fastURL, oldURL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 12 {
		for i, res := range results {
			if res.Result.StopReason != "final" {
				t.Logf("page %d on %q: stop %q, err %q, placement %q", i, res.Node, res.Result.StopReason, res.Err, res.PlacementReason)
			}
		}
		t.Fatalf("summary %+v, want all 12 pages to succeed", sum)
	}
	if _, perNode := hold.peaks(); perNode["old"] > int64(runConcurrency) {
		t.Errorf("the node that publishes no ceiling held %d jobs open at once, want at most %d", perNode["old"], runConcurrency)
	}
}

// TestRunBatchedSpreadsAResearchCallOverTheLocalSeatAndTheFleet: the door's default route. 12 pages over the local seat
// and three nodes of 4 slots are dealt three each, the local seat inside its run-cap room of 4, and all 12 are open at once.
func TestRunBatchedSpreadsAResearchCallOverTheLocalSeatAndTheFleet(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	probe := newFanProbe(12, 5*time.Second)
	var urls []string
	for _, id := range []string{"n1", "n2", "n3"} {
		_, url := probe.node(t, id, 4)
		urls = append(urls, url)
	}
	local := LocalRunner(func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		probe.dispatched("local", c.Goal)
		for !probe.finished(c.Goal) {
			select {
			case <-ctx.Done():
				return core.AgentWireResult{}, ctx.Err()
			case <-time.After(2 * time.Millisecond):
			}
		}
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: "local-seat",
			Output: "answer from the local seat", Structured: json.RawMessage(`{"answer":"local"}`), StopReason: "done"}, nil
	})
	cfg := testCfg(t)
	cfg.FleetMaxConcurrentJobs = 4
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	_, sum, err := RunBatched(ctx, cfg, local, pages(12), "spread", urls, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 12 || sum.Batches != 1 {
		t.Fatalf("summary %+v, want 12 successes in ONE batch", sum)
	}
	peak, perNode := probe.peaks()
	if peak < 12 {
		t.Errorf("peak jobs open = %d (%v), want all 12 pages open at once", peak, perNode)
	}
	for id, n := range perNode {
		if n > 4 {
			t.Errorf("%s held %d jobs open, want at most the 4 it can take at once", id, n)
		}
	}
}

// remoteSlotOf builds a dealt slot for the width table: a remote dial base that publishes `ceiling` (0 = none).
func remoteSlotOf(base string, ceiling int) spreadSlot {
	v := fitBigRemote
	v.MaxConcurrentJobs = ceiling
	return spreadSlot{placement: placement{view: v, base: base}}
}

// localSlotOf is a slot dealt to the local seat.
func localSlotOf() spreadSlot { return spreadSlot{placement: placement{view: fitLocal()}} }

func slotsOf(parts ...[]spreadSlot) []spreadSlot {
	var out []spreadSlot
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func repeatSlot(s spreadSlot, n int) []spreadSlot {
	out := make([]spreadSlot, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// TestDealParallelismSumsWhatTheDealCommittedToEachPlace is the width table. Each row is a deal and the width the call gets.
func TestDealParallelismSumsWhatTheDealCommittedToEachPlace(t *testing.T) {
	wait := spreadSlot{capacityWait: true, placement: placement{view: fitLocal()}}
	reserved := spreadSlot{reserved: true, placement: placement{view: fitLocal()}}
	for _, tc := range []struct {
		name  string
		route string
		deal  []spreadSlot
		room  int
		want  int
	}{
		{"four nodes that publish a ceiling, two each", "remote", slotsOf(
			repeatSlot(remoteSlotOf("http://n1", 4), 2), repeatSlot(remoteSlotOf("http://n2", 4), 2),
			repeatSlot(remoteSlotOf("http://n3", 4), 2), repeatSlot(remoteSlotOf("http://n4", 4), 2)), 0, 8},
		{"twelve pages over three nodes of four", "auto", slotsOf(
			repeatSlot(remoteSlotOf("http://n1", 4), 4), repeatSlot(remoteSlotOf("http://n2", 4), 4), repeatSlot(remoteSlotOf("http://n3", 4), 4)), 0, 12},
		{"one node that publishes none, dealt eight, keeps the bound of four", "remote", repeatSlot(remoteSlotOf("http://old", 0), 8), 0, runConcurrency},
		{"two nodes that publish none, four each, keep four EACH (eight, not four)", "remote", slotsOf(
			repeatSlot(remoteSlotOf("http://old1", 0), 4), repeatSlot(remoteSlotOf("http://old2", 0), 4)), 0, 8},
		{"a node that publishes a ceiling beside one that publishes none", "auto", slotsOf(
			repeatSlot(remoteSlotOf("http://new", 6), 6), repeatSlot(remoteSlotOf("http://old", 0), 6)), 0, 6 + runConcurrency},
		{"the local seat counts its run-cap room, not what it was dealt", "auto", slotsOf(
			repeatSlot(localSlotOf(), 8), repeatSlot(remoteSlotOf("http://n1", 4), 4)), 6, 6 + 4},
		{"a local seat dealt less than its room counts what it was dealt", "spread", slotsOf(
			repeatSlot(localSlotOf(), 3), repeatSlot(remoteSlotOf("http://n1", 4), 4)), 6, 3 + 4},
		{"a local seat past its room falls back to the floor", "auto", repeatSlot(localSlotOf(), 8), 2, runConcurrency},
		{"a seat with no run cap counts the floor, not all it was dealt", "auto", repeatSlot(localSlotOf(), 12), unlimitedHeadroom, runConcurrency},
		{"a seat with no run cap beside a node of four counts the floor plus the node", "spread", slotsOf(
			repeatSlot(localSlotOf(), 8), repeatSlot(remoteSlotOf("http://n1", 4), 4)), unlimitedHeadroom, runConcurrency + 4},
		{"overflow in the capacity wait counts nothing (its slot carries the local view, and the seat has room)", "auto", slotsOf(
			repeatSlot(remoteSlotOf("http://n1", 6), 6), repeatSlot(wait, 3)), 6, 6},
		{"a slot a lease reserves counts nothing", "spread", slotsOf(
			repeatSlot(remoteSlotOf("http://n1", 6), 5), repeatSlot(reserved, 3)), 4, 5},
		{"a small deal keeps the floor", "remote", repeatSlot(remoteSlotOf("http://n1", 4), 2), 0, runConcurrency},
		{"an empty deal keeps the floor", "auto", nil, 4, runConcurrency},
	} {
		r := &runner{route: tc.route, dealRoom: tc.room}
		if tc.route == "spread" {
			r.spreadDeal = tc.deal
		} else {
			r.autoDeal = tc.deal
		}
		if got := r.dealParallelism(); got != tc.want {
			t.Errorf("%s: width = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestACallWithNoDealKeepsTheConstant: route=local has no deal to size the call from, so a deal that happens to sit
// on the runner is ignored and the call keeps runConcurrency. (route=queue has no width at all: runWith hands its
// subtasks to the pull holders and returns before the semaphore exists.)
func TestACallWithNoDealKeepsTheConstant(t *testing.T) {
	r := &runner{route: "local", dealRoom: 8, autoDeal: repeatSlot(remoteSlotOf("http://n1", 8), 8), spreadDeal: repeatSlot(remoteSlotOf("http://n1", 8), 8)}
	if got := r.dealParallelism(); got != runConcurrency {
		t.Fatalf("route=local width = %d, want %d: nothing sizes a call that has no deal", got, runConcurrency)
	}
}

// TestTheLocalSeatCountsOnlyItsRunCapRoomInTheWidth: 8 subtasks all dealt to the local seat (no remote could take any of
// them), whose run-cap line takes 2. The seat's own FIFO is the line they would wait in, and holding a run slot for that wait
// is what the constant bounded: the call stays at runConcurrency. The seat holds each run until 5 are in flight at once, which
// the correct width never allows, so the run time is the limit and the peak is the assertion.
func TestTheLocalSeatCountsOnlyItsRunCapRoomInTheWidth(t *testing.T) {
	for _, route := range []string{"spread", "auto"} {
		t.Run(route, func(t *testing.T) {
			var mu sync.Mutex
			var inflight, peak int
			reached := make(chan struct{})
			var once sync.Once
			local := LocalRunner(func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
				mu.Lock()
				inflight++
				peak = max(peak, inflight)
				if inflight >= runConcurrency+1 {
					once.Do(func() { close(reached) })
				}
				mu.Unlock()
				select {
				case <-reached:
				case <-time.After(300 * time.Millisecond):
				case <-ctx.Done():
				}
				mu.Lock()
				inflight--
				mu.Unlock()
				return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: "local-seat",
					Output: "answered locally", Structured: json.RawMessage(`{"answer":"x"}`), StopReason: "done"}, nil
			})
			cfg := testCfg(t)
			cfg.FleetMaxConcurrentJobs = 2
			_, sum, err := RunWith(t.Context(), cfg, local, pages(8), route, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if sum.Succeeded != 8 {
				t.Fatalf("summary %+v, want all 8 run on the local seat", sum)
			}
			mu.Lock()
			defer mu.Unlock()
			if peak > runConcurrency {
				t.Fatalf("route=%s: %d local runs in flight at once, want at most %d: the seat's run cap leaves room for 2", route, peak, runConcurrency)
			}
		})
	}
}

// ---- R1's rules, at a width other than four --------------------------------------------------------------

// TestAWaitHoldingARunSlotOfAWideCallKeepsItsTTLWhileASubtaskIsUnstarted is ADR 0073 decision 9 with six slots instead of four:
// seven subtasks, one node that publishes six slots and never has room. The deal opens six, the seventh waits behind them for a
// slot, and the six that hold slots wait only the TTL and say why; the seventh, started when the first slot freed, has
// nothing behind it and waits for the call's horizon. heldSlotWait (callwait_slots_test.go) is the body the four-slot test
// shares, and says what each assertion proves and when a run is skipped as the runner's stall.
func TestAWaitHoldingARunSlotOfAWideCallKeepsItsTTLWhileASubtaskIsUnstarted(t *testing.T) {
	heldSlotWait(t, pages(7), func(f *fakeNode) { f.maxConcurrentJobs, f.maxQueueDepth = 6, 12 })
}

// TestASubtaskBehindAWideCallIsNotStartedInsideTheReserve is decision 10 with six slots: seven subtasks over two nodes that
// publish three each, whose jobs take 1 s, a 2 s call and a 1.2 s reserve. Six start at once; the seventh gets a slot when the
// first finishes, with about a second left, no more than a placed job needs, and is not started.
func TestASubtaskBehindAWideCallIsNotStartedInsideTheReserve(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	withCallReserve(t, 1200*time.Millisecond)
	probe := newFanProbe(1<<30, time.Second) // never reached: every job takes its second
	var urls []string
	var nodes []*fakeNode
	for _, id := range []string{"n1", "n2"} {
		n, url := probe.node(t, id, 3)
		nodes = append(nodes, n)
		urls = append(urls, url)
	}
	results, sum, _ := runWithin(t, 10*time.Second, testCfg(t), neverLocal(t), pages(7), "remote", urls, deadlineIn(2*time.Second), nil)

	if got := nodes[0].dispatches.Load() + nodes[1].dispatches.Load(); got != 6 {
		t.Fatalf("the nodes were sent %d jobs, want 6: the seventh subtask got its slot inside the reserve and must not begin", got)
	}
	if sum.Succeeded != 6 || sum.Deferred != 1 {
		t.Fatalf("summary %+v, want the six started subtasks to succeed and the seventh to defer", sum)
	}
	last := results[6]
	if !last.Result.Deferred || last.Result.DeferClass != core.DeferClassCapacity || !last.Unplaced || last.deadlineCut {
		t.Fatalf("last result %+v (cut %v), want an unplaced capacity defer, not the call-deadline cut", last.Result, last.deadlineCut)
	}
	if !strings.Contains(last.Result.Reason, "not started: the call's deadline left no room") {
		t.Errorf("reason = %q, want it to say the call's deadline left no room", last.Result.Reason)
	}
}

// TestAWideCallUnwindsAtTheDeadline is ADR 0065 with eight subtasks open at once on two nodes: the call returns at its
// deadline with every subtask published as the call-deadline defer, naming the whole call's count, and stops polling (at
// most one poll in flight per job when it returns).
func TestAWideCallUnwindsAtTheDeadline(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	probe := newFanProbe(1<<30, time.Hour) // jobs never finish
	var urls []string
	var nodes []*fakeNode
	for _, id := range []string{"n1", "n2"} {
		n, url := probe.node(t, id, 4)
		nodes = append(nodes, n)
		urls = append(urls, url)
	}
	results, sum, elapsed := runWithin(t, 10*time.Second, testCfg(t), neverLocal(t), pages(8), "remote", urls, deadlineIn(time.Second), nil)

	if elapsed > 5*time.Second {
		t.Fatalf("RunWith returned after %s, want about the 1 s deadline plus a short unwind", elapsed)
	}
	if sum.Deferred != 8 {
		t.Fatalf("summary %+v, want all 8 published as call-deadline defers", sum)
	}
	if peak, _ := probe.peaks(); peak <= runConcurrency {
		t.Fatalf("peak jobs open = %d, want more than %d open when the deadline cut them: the call was not wide", peak, runConcurrency)
	}
	for i, pr := range results {
		if !strings.HasPrefix(pr.Result.Reason, deadlinePrefix+"8 unfinished") {
			t.Errorf("result %d reason = %q, want the call-deadline defer counting the whole call (8 unfinished)", i, pr.Result.Reason)
		}
	}
	before := nodes[0].polls.Load() + nodes[1].polls.Load()
	time.Sleep(200 * time.Millisecond)
	if after := nodes[0].polls.Load() + nodes[1].polls.Load(); after-before > 8 {
		t.Fatalf("the nodes were polled %d more time(s) after RunWith returned: outstanding jobs are still being polled", after-before)
	}
}

// ---- route=auto sends an idle seat's overflow to the fleet (the diagnosis' F02), end to end ------------------------------

// TestAutoRunSendsTheOverflowOfAnIdleSeatToTheFleet: 8 subtasks on route=auto, an idle local seat whose run cap is 4, one node
// that publishes 4 slots. The idle seat runs 4 and the node 4, and all 8 are open at once: the local share is part of the width
// too. Without the roster read the deal has no remote to send the overflow to and the seat gets all 8 (the deal-level tests
// cannot see that gate).
func TestAutoRunSendsTheOverflowOfAnIdleSeatToTheFleet(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	probe := newFanProbe(8, 5*time.Second)
	node, url := probe.node(t, "node-a", 4)
	var localRuns atomic.Int64
	local := LocalRunner(func(ctx context.Context, c core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		localRuns.Add(1)
		probe.dispatched("local", c.Goal)
		for !probe.finished(c.Goal) {
			select {
			case <-ctx.Done():
				return core.AgentWireResult{}, ctx.Err()
			case <-time.After(2 * time.Millisecond):
			}
		}
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: "local-seat",
			Output: "answered locally", Structured: json.RawMessage(`{"answer":"x"}`), StopReason: "done"}, nil
	})
	cfg := testCfg(t)
	cfg.FleetMaxConcurrentJobs = 4
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	results, sum, err := RunWith(ctx, cfg, local, pages(8), "auto", []string{url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 8 || localRuns.Load() != 4 || node.dispatches.Load() != 4 {
		t.Fatalf("summary %+v, local runs %d, node dispatches %d, want 8 successes split 4 and 4", sum, localRuns.Load(), node.dispatches.Load())
	}
	for i, pr := range results[:4] {
		if !pr.ranLocal {
			t.Errorf("subtask %d ran on %s, want the idle seat: it wins the first 4", i, pr.Node)
		}
	}
	for i, pr := range results[4:] {
		if pr.ranLocal || !strings.Contains(pr.PlacementReason, "the idle local seat's run-cap line is spent by this deal") {
			t.Errorf("subtask %d: ran local %v, placement reason %q, want the node and the spent line named", i+4, pr.ranLocal, pr.PlacementReason)
		}
	}
	if peak, perNode := probe.peaks(); peak < 8 {
		t.Errorf("peak jobs open = %d (%v), want all 8 open at once: 4 on the idle seat and 4 on the node", peak, perNode)
	}
}

// TestAutoRunDoesNotReadTheFleetWhileTheIdleSeatsLineTakesEverySubtask: the roster is read for an idle seat only when the call
// has more subtasks than the seat's line takes. 4 subtasks against a cap of 4 read no node's health, as an idle box never did.
func TestAutoRunDoesNotReadTheFleetWhileTheIdleSeatsLineTakesEverySubtask(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	node, url := acceptingNode(t, "node-a", "zorblax from A", func(f *fakeNode) { f.maxConcurrentJobs, f.maxQueueDepth = 4, 8 })
	cfg := testCfg(t)
	cfg.FleetMaxConcurrentJobs = 4
	var localCalls atomic.Int64
	_, sum, err := Run(t.Context(), cfg, passingLocal(&localCalls), contracts(4), "auto", []string{url})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 4 || localCalls.Load() != 4 || node.dispatches.Load() != 0 {
		t.Fatalf("summary %+v, local runs %d, node dispatches %d, want all 4 on the idle seat", sum, localCalls.Load(), node.dispatches.Load())
	}
	if got := node.healths.Load(); got != 0 {
		t.Fatalf("the node's health was read %d time(s), want none: the seat's line takes every subtask", got)
	}
}
