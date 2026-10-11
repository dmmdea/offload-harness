package mediaremote

// The ticket queue (P0 plan S2; ADR 0083): a busy image call waits for the first matching lane that frees ANYWHERE, with
// one answer that says where it stands and why, and no caller loop. Everything below runs against real fleet-node servers
// (their doors, job store and recipe advertisement) whose leases the test toggles, a delegator whose pipeline is a runner
// that answers the lane question and keeps places in a line, and the real ticket files in a temp state dir. Two queues
// over one state dir stand for two processes of one machine.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
)

// toggled is a node lease reader that reports a held media lease while b is set.
func toggled(b *atomic.Bool) func() gpulease.Info {
	return func() gpulease.Info {
		if b.Load() {
			return gpulease.Info{Held: true, Class: gpulease.ClassMedia, Epoch: 9, PID: 4242, Reason: "a bench render", ExpiresAt: time.Now().Add(5 * time.Minute)}
		}
		return gpulease.Info{}
	}
}

// qEnv is a delegator whose own lane is held and a roster of nodes that render the same recipe, each busy or idle at the
// test's say.
type qEnv struct {
	t     *testing.T
	cfg   config.Config
	nodes []*node
	busy  []*atomic.Bool
	lr    *keeperRunner
}

// newQEnv starts n matching nodes (node-a, node-b, ...). busy says whether they start busy; tune can change a node's
// options.
func newQEnv(t *testing.T, n int, busy bool, tune ...func(i int, o *nodeOpts)) *qEnv {
	t.Helper()
	e := &qEnv{t: t}
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	for i := 0; i < n; i++ {
		b := &atomic.Bool{}
		b.Store(busy)
		o := nodeOpts{lease: toggled(b)}
		for _, f := range tune {
			f(i, &o)
		}
		e.busy = append(e.busy, b)
		e.nodes = append(e.nodes, startImageNode(t, "node-"+string(rune('a'+i)), "hidream-o1", fams, o))
	}
	e.cfg = overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, e.nodes...)
	e.lr = newKeeperRunner(heldLane())
	return e
}

func (e *qEnv) free(i int) { e.busy[i].Store(false) }
func (e *qEnv) hold(i int) { e.busy[i].Store(true) }

// req is the request the tests send: the F52 shape, with a prompt that names it.
func (e *qEnv) req(prompt string, extra map[string]any) core.Request {
	r := overflowReq(e.t, "qwen-image-2.1-fast", extra)
	r.Input = prompt
	return r
}

// answer is the ticket's answer as a caller reads it.
type answer struct {
	Queued      bool         `json:"queued"`
	Scope       string       `json:"scope"`
	Ticket      string       `json:"ticket"`
	WaiterToken string       `json:"waiter_token"`
	Position    int          `json:"position"`
	Cluster     []ClusterRow `json:"cluster"`
	Local       *LocalLane   `json:"local"`
	Next        string       `json:"next"`
	State       string       `json:"state"`
	Node        string       `json:"node"`
}

func answerOf(t *testing.T, res core.Result) answer {
	t.Helper()
	if res.OK || !res.Deferred || res.DeferClass != core.DeferClassCapacity || res.Meta.ErrClass != core.ErrClassGPUQueued {
		t.Fatalf("want a queued (capacity) answer, got %+v", res)
	}
	var a answer
	if err := json.Unmarshal(res.Data, &a); err != nil {
		t.Fatalf("answer data %s: %v", res.Data, err)
	}
	if !a.Queued || a.Scope != "cluster" || !strings.HasPrefix(a.Ticket, "mq-") || a.WaiterToken != a.Ticket {
		t.Fatalf("not a cluster ticket answer: %s", res.Data)
	}
	return a
}

// ticketFiles lists the ticket files in the state dir that are still a ticket: a finished one whose result was taken stays on
// file for collectedKeep so a second caller waiting on it gets the result too, and is not a ticket any more (collectedFiles
// lists those).
func ticketFiles(t *testing.T, cfg config.Config) []string {
	t.Helper()
	st, _ := storeFor(cfg)
	ents, err := os.ReadDir(filepath.Join(cfg.StateDir, queueDirName))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "mq-") && strings.HasSuffix(e.Name(), ".json") {
			if tf, ok := st.read(strings.TrimSuffix(e.Name(), ".json")); ok && tf.Collected && terminalState(tf.State) {
				continue
			}
			out = append(out, e.Name())
		}
	}
	return out
}

func queueFiles(t *testing.T, cfg config.Config, prefix string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(cfg.StateDir, queueDirName))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// manual switches the scanner off and returns two queues over the machine's state dir, driven by hand with cycle: two
// processes of one machine, deterministically.
func manual(t *testing.T) (q1, q2 *queueRT) {
	t.Helper()
	prev := scannerAuto
	scannerAuto = false
	t.Cleanup(func() { scannerAuto = prev })
	q1, q2 = newQueueRT(selfOwner()), newQueueRT(selfOwner())
	t.Cleanup(func() { q1.stop(); q2.stop() })
	return q1, q2
}

// admitOn makes a ticket on q for the request (the lane must be busy), without waiting for it.
func admitOn(t *testing.T, q *queueRT, cfg config.Config, runner Runner, req core.Request) *liveTicket {
	t.Helper()
	res, lt := q.admit(context.Background(), cfg, runner, req, nil, Wait{})
	if lt == nil {
		t.Fatalf("no ticket was made: %+v", res)
	}
	return lt
}

func isDone(lt *liveTicket) bool {
	select {
	case <-lt.done:
		return true
	default:
		return false
	}
}

// ---- idempotence and the seed ---------------------------------------------------------------------------------------------

// Any re-send of the same request attaches to the live ticket: with no token, with its token, with a place token of the
// local line, and the answer is the same ticket every time. A request that differs in what it asks for is another
// ticket. This is what ends a caller's loop of resends.
func TestIdenticalResendAttachesToTheLiveTicket(t *testing.T) {
	e := newQEnv(t, 1, true)
	req := e.req("a red door", map[string]any{"seed": 7})
	first := answerOf(t, runDoor(e.cfg, e.lr, req, AtOnce))

	withToken := func(tok string) core.Request {
		r := req
		r.Params = map[string]any{}
		for k, v := range req.Params {
			r.Params[k] = v
		}
		r.Params["waiter_token"] = tok
		return r
	}
	for name, r := range map[string]core.Request{
		"the same request":            req,
		"with the ticket's token":     withToken(first.Ticket),
		"with a place token of a box": withToken("tk-abc12345"),
	} {
		if a := answerOf(t, runDoor(e.cfg, e.lr, r, AtOnce)); a.Ticket != first.Ticket {
			t.Errorf("%s: want ticket %s, got %s", name, first.Ticket, a.Ticket)
		}
	}
	if files := ticketFiles(t, e.cfg); len(files) != 1 {
		t.Errorf("ten resends are one ticket: %v", files)
	}
	if leaves := e.lr.leaveCalls(); len(leaves) != 1 {
		t.Errorf("a resend that attaches leaves no second place in the line: %d", len(leaves))
	}

	// What the request asks for decides identity: another seed, another prompt, another size are other jobs.
	other := map[string]core.Request{
		"another seed":   e.req("a red door", map[string]any{"seed": 8}),
		"another prompt": e.req("a blue door", map[string]any{"seed": 7}),
		"another size":   e.req("a red door", map[string]any{"seed": 7, "width": 1024}),
	}
	seen := map[string]bool{first.Ticket: true}
	for name, r := range other {
		a := answerOf(t, runDoor(e.cfg, e.lr, r, AtOnce))
		if seen[a.Ticket] {
			t.Errorf("%s must be its own ticket, got %s again", name, a.Ticket)
		}
		seen[a.Ticket] = true
	}
	if files := ticketFiles(t, e.cfg); len(files) != 4 {
		t.Errorf("four different requests, four tickets: %v", files)
	}
}

// The key is the request's content and nothing else: the waiter_token is not part of it, a number is the same number
// however a door spelled it, and the order of the parameters does not matter.
func TestRequestKeyIsTheContentWithoutTheToken(t *testing.T) {
	a := core.Request{Task: core.TaskGenerateImage, Input: "p", Params: map[string]any{"seed": 5, "width": 1024, "family": "f"}}
	b := core.Request{Task: core.TaskGenerateImage, Input: "p", Params: map[string]any{"family": "f", "width": float64(1024), "seed": json.Number("5"), "waiter_token": "mq-aaaaaaaa"}}
	if reqKey(a) != reqKey(b) {
		t.Errorf("the same content must have the same key:\n%s\n%s", reqKey(a), reqKey(b))
	}
	for name, c := range map[string]core.Request{
		"another prompt": {Task: core.TaskGenerateImage, Input: "q", Params: a.Params},
		"another task":   {Task: core.TaskGenerateVideo, Input: "p", Params: a.Params},
		"another param":  {Task: core.TaskGenerateImage, Input: "p", Params: map[string]any{"seed": 6, "width": 1024, "family": "f"}},
		"a param more":   {Task: core.TaskGenerateImage, Input: "p", Params: map[string]any{"seed": 5, "width": 1024, "family": "f", "refine": false}},
	} {
		if reqKey(a) == reqKey(c) {
			t.Errorf("%s must change the key", name)
		}
	}
	for _, ok := range []string{"mq-abcdef12", "mq-0123456789ab"} {
		if !validTicketID(ok) {
			t.Errorf("%q is a ticket id", ok)
		}
	}
	for _, bad := range []string{"", "tk-abcdef12", "mq-", "mq-../../x", "mq-ABCDEF12", "mq-abc", `mq-abcdef12\x`, "mq-abcdef12.json"} {
		if validTicketID(bad) {
			t.Errorf("%q must not be a ticket id (it becomes a path)", bad)
		}
	}
}

// A ticket fixes ONE seed when the caller gave none, and every send of the job carries it: the first node that bounces
// it, the node that finally renders it, the lane question and a local run all see the same number, so the composition
// does not depend on where the job landed. A seed the caller gave is the seed.
func TestTicketFixesOneSeed(t *testing.T) {
	bounce := func(i int, o *nodeOpts) {
		if i == 0 {
			o.deferAs, o.deferClass = "gpu busy: card(s) held by a media-class lease", core.ErrClassGPUBusy
		}
	}
	e := newQEnv(t, 2, false, bounce)
	req := e.req("a red door", nil)
	delete(req.Params, "seed")

	res := runBounded(e.cfg, e.lr, req, "auto", nil)
	if !res.OK || res.Meta.Node != "node-b" {
		t.Fatalf("node-a bounces the job and node-b renders it: %+v", res)
	}
	_, pa := posted(t, e.nodes[0])
	_, pb := posted(t, e.nodes[1])
	sa, sb := pa["seed"], pb["seed"]
	if sa == nil || sa != sb {
		t.Fatalf("a bounce and a re-placement render the same composition: seeds %v / %v", sa, sb)
	}
	seed := int(sa.(float64))
	if seed < 1 || seed > 1_000_000_000 {
		t.Errorf("a minted seed is in 1..1e9, like the pipeline's own: %d", seed)
	}
	e.lr.mu.Lock()
	probed := append([]core.Request(nil), e.lr.probed...)
	e.lr.mu.Unlock()
	for i, p := range probed {
		if got, _ := posInt(p.Params, "seed"); i > 0 && got != seed {
			t.Errorf("probe %d saw seed %d, the ticket's is %d", i, got, seed)
		}
	}

	// The seed the caller gave is the seed, and the key was taken before any seed was fixed: a resend without it attaches.
	given := newQEnv(t, 1, false)
	if r := runBounded(given.cfg, given.lr, given.req("p", map[string]any{"seed": 31337}), "auto", nil); !r.OK {
		t.Fatal(r)
	}
	if _, p := posted(t, given.nodes[0]); p["seed"] != float64(31337) {
		t.Errorf("a given seed travels as given: %v", p["seed"])
	}
}

// The ticket's seed is also the seed of the local run it ends up as.
func TestTicketSeedReachesTheLocalRun(t *testing.T) {
	e := newQEnv(t, 1, true)
	req := e.req("a red door", nil)
	delete(req.Params, "seed")
	answerOf(t, runDoor(e.cfg, e.lr, req, AtOnce))
	e.lr.set(core.LaneVerdict{Free: true})
	res := settle(t, e.cfg, e.lr, req, func(r core.Result) bool { return r.OK })
	if string(res.Data) != `{"local":true}` {
		t.Fatalf("the lane freed first: it ran here: %s", res.Data)
	}
	ran := e.lr.ranCalls()
	if len(ran) != 1 {
		t.Fatalf("one local run: %d", len(ran))
	}
	seed, ok := posInt(ran[0].Params, "seed")
	if !ok || seed < 1 {
		t.Errorf("the local run carries the ticket's seed: %v", ran[0].Params)
	}
}

// ---- attaching by token, and what a scan may not write --------------------------------------------------------------------

// Re-attaching to a ticket (by token or by resending) creates no job row, no card, no place in the line and no new ticket,
// however often it is done: only the ticket's dispatch does.
func TestReattachByTokenCreatesNoJobRowOrCard(t *testing.T) {
	rig := newPairRig(t, true, "node-a")
	e := newQEnv(t, 1, true)
	e.cfg.PairWorkloadsEnabled = true
	e.lr.rig = rig
	req := e.req("a red door", nil)
	a := answerOf(t, runDoor(e.cfg, e.lr, req, AtOnce))
	for i := 0; i < 6; i++ {
		r := req
		r.Params = map[string]any{"waiter_token": a.Ticket}
		for k, v := range req.Params {
			r.Params[k] = v
		}
		if got := answerOf(t, runDoor(e.cfg, e.lr, r, AtOnce)); got.Ticket != a.Ticket {
			t.Fatalf("re-attach %d: ticket %s, want %s", i, got.Ticket, a.Ticket)
		}
	}
	if rig.frameCount() != 0 || len(rig.rows()) != 0 {
		t.Errorf("no card and no ledger row: frames %d rows %d", rig.frameCount(), len(rig.rows()))
	}
	e.lr.mu.Lock()
	begins := e.lr.begins
	e.lr.mu.Unlock()
	if begins != 0 {
		t.Errorf("no attribution handle was begun: %d", begins)
	}
	if _, runs := e.lr.counts(); runs != 0 {
		t.Errorf("nothing ran here: %d", runs)
	}
	if n := len(ticketFiles(t, e.cfg)); n != 1 {
		t.Errorf("one ticket: %d", n)
	}
	if n := len(e.lr.leaveCalls()); n != 1 {
		t.Errorf("one place in the line: %d", n)
	}
	noPosts(t, "the node", e.nodes[0])
}

// A scan writes no PAIR card, no ledger row and no lease waiter (register C01: the first design of this loop polluted the
// PAIR workload list with a card per attempt). Thirty scans over a roster that is busy, gone and the wrong build leave
// exactly what the ticket's creation left: one place in the line, kept, and nothing else.
func TestScanNeverWritesCardsOrRows(t *testing.T) {
	rig := newPairRig(t, true, "node-a")
	e := newQEnv(t, 3, true)
	e.cfg.PairWorkloadsEnabled = true
	e.lr.rig = rig
	e.nodes[1].srv.Close() // gone
	q, _ := manual(t)
	lt := admitOn(t, q, e.cfg, e.lr, e.req("a red door", nil))
	place := lt.snapshot().Place
	for i := 0; i < 30; i++ {
		q.cycle(context.Background())
	}
	if rig.frameCount() != 0 || len(rig.rows()) != 0 {
		t.Errorf("a scan opens no card and writes no row: frames %d rows %d", rig.frameCount(), len(rig.rows()))
	}
	e.lr.mu.Lock()
	begins, runs := e.lr.begins, e.lr.runs
	e.lr.mu.Unlock()
	if begins != 0 || runs != 0 {
		t.Errorf("no handle begun, nothing run: %d %d", begins, runs)
	}
	if n := len(e.lr.leaveCalls()); n != 1 {
		t.Errorf("one place in the line, left at creation and not once per scan: %d", n)
	}
	if kept := e.lr.keptCount(place); kept < 25 {
		t.Errorf("the place is touched on every scan: %d", kept)
	}
	if e.lr.dropped(place) != 0 {
		t.Error("a waiting ticket keeps its place")
	}
	noPosts(t, "the busy node", e.nodes[0])
	noPosts(t, "the node of the wrong build", e.nodes[2])
}

// ---- order, in-use, seniority --------------------------------------------------------------------------------------------

// The oldest ticket of the machine starts first on a node, whichever process scans first, and a younger ticket does not take
// a node an older one waits for. An older ticket that has not been scanned yet is first in line for every node.
func TestTicketOldestFirstPerNode(t *testing.T) {
	e := newQEnv(t, 1, true)
	q1, q2 := manual(t)
	ctx := context.Background()
	lt1 := admitOn(t, q1, e.cfg, e.lr, e.req("first", nil))
	time.Sleep(5 * time.Millisecond)
	lt2 := admitOn(t, q2, e.cfg, e.lr, e.req("second", nil))

	// The younger ticket's process scans first, before the older one was ever scanned: it must not start.
	e.free(0)
	q2.cycle(ctx)
	noPosts(t, "the only node (an older ticket has not been scanned: it is first in line)", e.nodes[0])
	if row := lt2.snapshot().Cluster; len(row) != 1 || row[0].State != "free" || !strings.Contains(row[0].Why, lt1.t.ID) {
		t.Errorf("the younger ticket says the node is idle and whose turn it is: %+v", row)
	}

	// Both are scanned while the node is busy, then it frees: the younger one's scan still comes first and still must not start.
	e.hold(0)
	q1.cycle(ctx)
	q2.cycle(ctx)
	if m := lt1.snapshot().Matches; len(m) != 1 {
		t.Fatalf("the older ticket records the node it matches: %v", m)
	}
	e.free(0)
	q2.cycle(ctx)
	noPosts(t, "the only node (the older ticket matches it)", e.nodes[0])

	q1.cycle(ctx)
	waitFor(t, 5*time.Second, "the older ticket renders first", func() bool { return isDone(lt1) })
	q2.cycle(ctx)
	waitFor(t, 5*time.Second, "then the younger one", func() bool { return isDone(lt2) })
	e.nodes[0].runner.mu.Lock()
	var order []string
	for _, r := range e.nodes[0].runner.reqs {
		order = append(order, r.Input)
	}
	e.nodes[0].runner.mu.Unlock()
	if strings.Join(order, ",") != "first,second" {
		t.Errorf("the node rendered %v, want oldest first", order)
	}
}

// Two idle nodes, two tickets of one machine: the oldest takes the first node and the other takes the second in the same
// scan. An older ticket holds back a younger one only from the nodes the older one could itself be started on.
func TestOlderTicketDoesNotHoldANodeItCannotUse(t *testing.T) {
	// The jobs stay on their nodes until the test lets them go: a render that ended inside the scan would free the first node
	// again before the second ticket looked, and both jobs would honestly go there.
	hold := make(chan struct{})
	e := newQEnv(t, 2, true, func(_ int, o *nodeOpts) { o.hold = hold })
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release)
	q, _ := manual(t)
	lt1 := admitOn(t, q, e.cfg, e.lr, e.req("first", nil))
	time.Sleep(5 * time.Millisecond)
	lt2 := admitOn(t, q, e.cfg, e.lr, e.req("second", nil))
	e.free(0)
	e.free(1)
	q.cycle(context.Background())
	waitFor(t, 5*time.Second, "a job on each node", func() bool { return len(e.nodes[0].posts()) == 1 && len(e.nodes[1].posts()) == 1 })
	release()
	waitFor(t, 5*time.Second, "both tickets render", func() bool { return isDone(lt1) && isDone(lt2) })
	if len(e.nodes[0].posts()) != 1 || len(e.nodes[1].posts()) != 1 {
		t.Errorf("one job on each node: %d / %d", len(e.nodes[0].posts()), len(e.nodes[1].posts()))
	}
	if r := e.nodes[0].runner.last(); r.Input != "first" {
		t.Errorf("the oldest ticket took the first node: %q", r.Input)
	}
}

// A node that holds a placed ticket of this machine is IN USE for every scanner of the machine: a second process reads it
// from the state files, sends it nothing and says why, and gets the node once the first job is done and its claim is let go.
func TestPlacedTicketMarksNodeInUse(t *testing.T) {
	hold := make(chan struct{})
	e := newQEnv(t, 1, false, func(_ int, o *nodeOpts) { o.hold = hold })
	t.Cleanup(func() { close(hold) })
	q1, q2 := manual(t)
	ctx := context.Background()
	lt1 := admitOn(t, q1, e.cfg, e.lr, e.req("first", nil))
	time.Sleep(5 * time.Millisecond)
	lt2 := admitOn(t, q2, e.cfg, e.lr, e.req("second", nil))

	q1.cycle(ctx)
	waitFor(t, 5*time.Second, "the first job reaches the node", func() bool {
		return len(e.nodes[0].posts()) == 1 && lt1.snapshot().State == stPlaced
	})
	if files := queueFiles(t, e.cfg, "use-"); len(files) != 1 {
		t.Fatalf("a placed ticket holds one claim: %v", files)
	}

	q1.cycle(ctx) // the first process keeps its beat; nothing else moves
	q2.cycle(ctx)
	if n := len(e.nodes[0].posts()); n != 1 {
		t.Fatalf("the node is in use: the second process sent it a second job (%d POSTs)", n)
	}
	rows := lt2.snapshot().Cluster
	if len(rows) != 1 || rows[0].State != "in-use" || !strings.Contains(rows[0].Why, lt1.t.ID) {
		t.Fatalf("the second ticket is told the node is in use, by which ticket: %+v", rows)
	}

	hold <- struct{}{} // the first job finishes
	waitFor(t, 5*time.Second, "the first ticket finishes", func() bool { return isDone(lt1) })
	if files := queueFiles(t, e.cfg, "use-"); len(files) != 0 {
		t.Errorf("a finished job lets its node go: %v", files)
	}
	q2.cycle(ctx)
	waitFor(t, 5*time.Second, "the second job reaches the node", func() bool { return len(e.nodes[0].posts()) == 2 })
}

// Two processes scanning at the same instant never send one single-job node two tickets: the claim is taken under the
// directory lock, so the read and the write are one step. Each queue stands for one process, and a process has ONE scanner
// (scanLoop), which visits its tickets one after another: a queue's cycles are therefore run one after another here, the two
// queues against each other. (Eight goroutines per queue made the same ticket be stepped by two cycles at once, which no
// process does: one step could write the ticket's file with the state it had before its claim while the other process judged
// that claim, the claim read as dead, and the node was sent two jobs, 5 times in 150 runs under load, on the tree before the
// fix round as well as after it; serialized, 0 in 300.)
func TestTwoProcessesNeverDispatchTwoTicketsToOneSingleJobNode(t *testing.T) {
	hold := make(chan struct{})
	e := newQEnv(t, 1, false, func(_ int, o *nodeOpts) { o.hold = hold })
	t.Cleanup(func() { close(hold) })
	q1, q2 := manual(t)
	e.free(0)
	// The ordering rule would already keep the younger ticket back; it is turned off here so that only the claim stands
	// between the two sends, which is the last guard and the one that holds when the ordering information is stale.
	prev := honourOrder
	honourOrder = false
	t.Cleanup(func() { honourOrder = prev })
	admitOn(t, q1, e.cfg, e.lr, e.req("first", nil))
	admitOn(t, q2, e.cfg, e.lr, e.req("second", nil))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, q := range []*queueRT{q1, q2} {
		wg.Add(1)
		go func(q *queueRT) {
			defer wg.Done()
			<-start
			for i := 0; i < 8; i++ {
				q.cycle(context.Background())
			}
		}(q)
	}
	close(start)
	wg.Wait()
	waitFor(t, 5*time.Second, "a job reaches the node", func() bool { return len(e.nodes[0].posts()) >= 1 })
	time.Sleep(300 * time.Millisecond)
	if n := len(e.nodes[0].posts()); n != 1 {
		t.Fatalf("one idle single-job node was sent %d jobs by two processes", n)
	}
}

// A ticket keeps its place in this machine's own line: it is left once, at creation, with the ticket's arrival time; it is
// touched on every scan; it is let go when the ticket ends; a place that lapsed is left again with the SAME arrival time; and
// the local run that finally serves the ticket carries the place as its waiter_token.
func TestTicketKeepsItsPlaceInTheLocalLine(t *testing.T) {
	e := newQEnv(t, 1, true)
	q, _ := manual(t)
	before := time.Now()
	lt := admitOn(t, q, e.cfg, e.lr, e.req("a red door", nil))
	leaves := e.lr.leaveCalls()
	if len(leaves) != 1 || leaves[0].since.Before(before.Add(-time.Second)) || leaves[0].since.After(time.Now()) {
		t.Fatalf("one place, with the arrival time of the call: %+v", leaves)
	}
	place := lt.snapshot().Place
	if place == "" {
		t.Fatal("the ticket records its place")
	}
	for i := 0; i < 3; i++ {
		q.cycle(context.Background())
	}
	if kept := e.lr.keptCount(place); kept != 3 {
		t.Errorf("the place is touched once per scan: %d", kept)
	}

	// The place lapses (pruned, or spent by somebody): the next scan leaves it again, with the arrival it had.
	e.lr.lapse(place)
	q.cycle(context.Background())
	leaves = e.lr.leaveCalls()
	if len(leaves) != 2 || leaves[1].since.UnixMilli() != lt.snapshot().Created {
		t.Fatalf("a lapsed place is left again with the ticket's own arrival time: %+v (created %d)", leaves, lt.snapshot().Created)
	}
	place2 := lt.snapshot().Place

	// The lane frees: the local run carries the place.
	e.lr.set(core.LaneVerdict{Free: true})
	q.cycle(context.Background())
	waitFor(t, 5*time.Second, "the ticket runs here", func() bool { return isDone(lt) })
	ran := e.lr.ranCalls()
	if len(ran) != 1 || ran[0].Params["waiter_token"] != place2 {
		t.Fatalf("the local run spends the place through waiter_token: %+v", ran)
	}
	if e.lr.dropped(place2) != 1 {
		t.Errorf("the place is let go when the ticket is done: %v", e.lr.drops)
	}
}

// A box that keeps no places still queues, and the answer says there is no seniority in its own line.
func TestATicketOnABoxWithoutPlacesSaysSo(t *testing.T) {
	e := newQEnv(t, 1, true)
	e.lr.noPlace = core.ErrNoMediaPlace
	a := runDoor(e.cfg, e.lr, e.req("a red door", nil), AtOnce)
	_ = answerOf(t, a)
	if !strings.Contains(a.Reason, "no seniority") {
		t.Errorf("the answer says the ticket holds no place in the box's own line: %s", a.Reason)
	}
}

// ---- late binding --------------------------------------------------------------------------------------------------------

// Nothing is sent anywhere until a lane is free, so no job is parked on a node that frees later than another: when two nodes
// are busy and one frees, the job starts there, within a scan, with no call from the caller, and a later call with the
// token collects the verified result.
func TestLateBindingStartsOnFirstFreedLane(t *testing.T) {
	e := newQEnv(t, 2, true)
	req := e.req("a red door", nil)
	a := answerOf(t, runDoor(e.cfg, e.lr, req, AtOnce))
	time.Sleep(200 * time.Millisecond) // several scans
	noPosts(t, "node-a (busy)", e.nodes[0])
	noPosts(t, "node-b (busy)", e.nodes[1])

	e.free(1)
	waitFor(t, 5*time.Second, "the job starts on the node that freed", func() bool { return len(e.nodes[1].posts()) == 1 })
	noPosts(t, "node-a, still busy", e.nodes[0])

	collect := req
	collect.Params = map[string]any{"waiter_token": a.Ticket}
	for k, v := range req.Params {
		collect.Params[k] = v
	}
	res := settle(t, e.cfg, e.lr, collect, func(r core.Result) bool { return r.OK })
	if res.Meta.Node != "node-b" || !strings.Contains(res.Meta.Placement, "bench render of the spring set") {
		t.Errorf("the result says where it ran and why it left: %+v", res.Meta)
	}
	out := req.Params["out"].(string)
	if b, err := os.ReadFile(out); err != nil || string(b) != "IMG" {
		t.Errorf("the render lands at out, hash-checked: %q %v", b, err)
	}
	if _, runs := e.lr.counts(); runs != 0 {
		t.Errorf("the job did not also run here: %d", runs)
	}
	if n := len(ticketFiles(t, e.cfg)); n != 0 {
		t.Errorf("a collected ticket is gone: %d", n)
	}
}

// When the local lane frees first the ticket runs here, and the result is the local run's.
func TestLateBindingPrefersTheLocalLaneWhenItFreesFirst(t *testing.T) {
	e := newQEnv(t, 2, true)
	req := e.req("a red door", nil)
	answerOf(t, runDoor(e.cfg, e.lr, req, AtOnce))
	e.lr.set(core.LaneVerdict{Free: true})
	res := settle(t, e.cfg, e.lr, req, func(r core.Result) bool { return r.OK })
	if string(res.Data) != `{"local":true}` {
		t.Fatalf("it ran on this machine's lane: %s", res.Data)
	}
	noPosts(t, "node-a", e.nodes[0])
	noPosts(t, "node-b", e.nodes[1])
}

// ---- owners that die -------------------------------------------------------------------------------------------------------

// deadOwner is a process that is not running.
var deadOwner = ownerID{PID: 0x7ffffff0}

func TestADeadOwnerIsDeadByPidAndByAStaleBeat(t *testing.T) {
	if (ticketFile{Owner: deadOwner, Beat: nowMs()}).dead() == false {
		t.Fatal("a pid that is not running is a dead owner")
	}
	live := ticketFile{Owner: selfOwner(), Beat: nowMs()}
	if live.dead() {
		t.Fatal("this process is alive")
	}
	live.Beat = time.Now().Add(-ownerStale - time.Minute).UnixMilli()
	if !live.dead() {
		t.Error("an owner alive by pid that has not written its beat for ownerStale is hung: dead")
	}
	recycled := selfOwner()
	recycled.StartMs++ // the same pid, another process
	if (ticketFile{Owner: recycled, Beat: nowMs()}).dead() == false && recycled.StartMs != 1 {
		t.Error("a recycled pid is another process")
	}
}

// A ticket whose owner died after the job was accepted is adopted by the next identical call: it polls the recorded job and
// fetches it, never sends it again (one render, not two), and writes ONE ledger row and no second card.
func TestDeadOwnerPlacedTicketIsAdopted(t *testing.T) {
	rig := newPairRig(t, true, "node-a")
	e := newQEnv(t, 1, false)
	e.cfg.PairWorkloadsEnabled = true
	e.lr.rig = rig
	req := e.req("a red door", nil)
	n := e.nodes[0]

	// The job the dead process sent: really on the node, finished, its outputs waiting to be fetched.
	local, _, _ := localIdentity(e.cfg, "qwen-image-2.1-fast")
	jobID := "media-deadbeef00000001"
	body, _ := json.Marshal(map[string]any{"job_id": jobID, "task_type": taskImage, "payload": map[string]any{
		"prompt": req.Input, "family": "qwen-image-2.1", "seed": 424242, "width": 2048, "height": 2048, "recipe_digest": local.recipe.Digest()}})
	if err := post(context.Background(), e.cfg, n.srv.URL, "/fleet/dispatch", body); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the node finishes the job", func() bool {
		j, err := getJSON[jobWire](context.Background(), e.cfg, n.srv.URL+"/fleet/jobs/"+jobID)
		return err == nil && j.State == "done"
	})
	st, _ := storeFor(e.cfg)
	dead := ticketFile{ID: "mq-deadbeef0001", ReqKey: reqKey(req), Created: nowMs() - 60_000, Owner: deadOwner, Beat: nowMs() - 5000,
		Task: "generate_image", Out: req.Params["out"].(string), Seed: 424242, State: stPlaced, Node: "node-a", Base: n.srv.URL, Job: jobID,
		LaneWhy: "card(s) c2 held by a media-class lease"}
	if err := st.write(dead); err != nil {
		t.Fatal(err)
	}

	res := runDoor(e.cfg, e.lr, req, callWindow)
	if !res.OK || res.Meta.Node != "node-a" {
		t.Fatalf("the next identical call adopts the job and brings its result: %+v", res)
	}
	if got := len(n.posts()); got != 1 {
		t.Errorf("the request was sent once, by the process that died; the adopter only polls and fetches: %d POSTs", got)
	}
	if !strings.Contains(res.Meta.Placement, "taken over") {
		t.Errorf("the result says the job was taken over: %q", res.Meta.Placement)
	}
	if b, err := os.ReadFile(req.Params["out"].(string)); err != nil || string(b) != "IMG" {
		t.Errorf("the fetched render is at out: %q %v", b, err)
	}
	if rows := rig.rows(); len(rows) != 1 || rows[0].Deferred {
		t.Errorf("the adopted call writes one ledger row: %+v", rows)
	}
	if rig.frameCount() != 0 {
		t.Errorf("the adopted call opens no card of its own (the ended process's is swept as an orphan): %d frames", rig.frameCount())
	}
	if len(ticketFiles(t, e.cfg)) != 0 || len(queueFiles(t, e.cfg, "use-")) != 0 {
		t.Errorf("a collected ticket and its claim are gone")
	}
}

// A ticket whose owner died before anything was sent (waiting) or while it ran on this machine's lane (local) has nothing
// to adopt: the next scan of any process removes it and lets go of its place in the line. A placed ticket of a dead owner
// stays for adoption; a finished one nobody collected goes after doneKeep; a claim with no live owner goes.
func TestWaitingTicketOfDeadOwnerIsSwept(t *testing.T) {
	e := newQEnv(t, 1, true)
	st, _ := storeFor(e.cfg)
	old := nowMs() - 60_000
	files := []ticketFile{
		{ID: "mq-deadwaiting1", ReqKey: "k1", Created: old, Owner: deadOwner, Beat: old, State: stWaiting, Place: "tk-deadplace0001"},
		{ID: "mq-deadlocal001", ReqKey: "k2", Created: old, Owner: deadOwner, Beat: old, State: stLocal, Place: "tk-deadplace0002"},
		{ID: "mq-deadplaced01", ReqKey: "k3", Created: old, Owner: deadOwner, Beat: old, State: stPlaced, Node: "node-a", Base: e.nodes[0].srv.URL, Job: "media-x"},
	}
	for _, f := range files {
		if err := st.write(f); err != nil {
			t.Fatal(err)
		}
	}
	e.lr.placeFor("tk-deadplace0001", time.UnixMilli(old))
	e.lr.placeFor("tk-deadplace0002", time.UnixMilli(old))
	// A finished ticket nobody collected, long past doneKeep, and a claim whose owner is gone.
	stale := ticketFile{ID: "mq-donestale001", ReqKey: "k4", Created: old, Owner: deadOwner, Beat: old, State: stDone, Result: json.RawMessage(`{"ok":true}`)}
	prevKeep, prevOKKeep := doneKeep, doneOKKeep
	doneKeep, doneOKKeep = time.Millisecond, time.Millisecond
	t.Cleanup(func() { doneKeep, doneOKKeep = prevKeep, prevOKKeep })
	if err := st.write(stale); err != nil {
		t.Fatal(err)
	}
	cb, _ := json.Marshal(claim{Base: "http://gone.example:1", Ticket: "mq-nothing00001", Owner: deadOwner, At: old})
	if err := writeFileAtomic(st.claimPath("http://gone.example:1"), cb); err != nil {
		t.Fatal(err)
	}

	q, _ := manual(t)
	admitOn(t, q, e.cfg, e.lr, e.req("a live ticket", nil))
	time.Sleep(5 * time.Millisecond)
	q.cycle(context.Background())

	left := map[string]bool{}
	for _, f := range ticketFiles(t, e.cfg) {
		left[strings.TrimSuffix(f, ".json")] = true
	}
	for _, gone := range []string{"mq-deadwaiting1", "mq-deadlocal001", "mq-donestale001"} {
		if left[gone] {
			t.Errorf("%s must be swept", gone)
		}
	}
	if !left["mq-deadplaced01"] {
		t.Error("a placed ticket of a dead owner stays for the next identical call to adopt")
	}
	if e.lr.dropped("tk-deadplace0001") != 1 || e.lr.dropped("tk-deadplace0002") != 1 {
		t.Errorf("the places of the swept tickets are let go: %v", e.lr.drops)
	}
	if n := len(queueFiles(t, e.cfg, "use-")); n != 0 {
		t.Errorf("a claim with no live owner is removed: %d", n)
	}
}

// A ticket that was running on the dead owner's own lane cannot be polled: it is swept and the identical call starts over
// as a new ticket, which keeps the old one's arrival, and runs here once the lane frees.
func TestDeadOwnerLocalTicketIsSweptAndRerendered(t *testing.T) {
	e := newQEnv(t, 1, true)
	req := e.req("a red door", nil)
	st, _ := storeFor(e.cfg)
	arrived := nowMs() - 120_000
	if err := st.write(ticketFile{ID: "mq-deadlocal002", ReqKey: reqKey(req), Created: arrived, Owner: deadOwner, Beat: arrived, State: stLocal, Place: "tk-deadplace0003"}); err != nil {
		t.Fatal(err)
	}
	e.lr.placeFor("tk-deadplace0003", time.UnixMilli(arrived))

	a := answerOf(t, runDoor(e.cfg, e.lr, req, AtOnce))
	if a.Ticket == "mq-deadlocal002" {
		t.Fatal("the dead owner's ticket is not the new call's")
	}
	if _, ok := st.read("mq-deadlocal002"); ok {
		t.Error("the dead owner's local ticket is swept")
	}
	if e.lr.dropped("tk-deadplace0003") != 1 {
		t.Errorf("its place is let go: %v", e.lr.drops)
	}
	if got := st.list(); len(got) != 1 || got[0].Created != arrived {
		t.Errorf("the new ticket keeps the arrival of the one it replaces: %+v (want %d)", got, arrived)
	}
	e.lr.set(core.LaneVerdict{Free: true})
	if res := settle(t, e.cfg, e.lr, req, func(r core.Result) bool { return r.OK }); string(res.Data) != `{"local":true}` {
		t.Fatalf("it renders here, once: %s", res.Data)
	}
	if _, runs := e.lr.counts(); runs != 1 {
		t.Errorf("one run: %d", runs)
	}
}

// ---- the CLI ---------------------------------------------------------------------------------------------------------------

// The CLI blocks until the job is done, tells a human where the ticket stands, and an interrupt cancels a ticket that sent
// nothing: no job anywhere, no ticket on file, the place let go.
func TestCLIBlocksAndCancelsUndispatched(t *testing.T) {
	e := newQEnv(t, 1, true)
	req := e.req("a red door", nil)
	// The human is told where the ticket started by the ticket itself, not by a poll that might happen to see it.
	prevPoll := attachPoll
	attachPoll = time.Hour
	t.Cleanup(func() { attachPoll = prevPoll })

	// Blocks: nothing frees, so the call does not return, and a lane that frees ends it with the result.
	var lines []string
	var lm sync.Mutex
	done := make(chan core.Result, 1)
	go func() {
		done <- RunWith(context.Background(), e.cfg, e.lr, req, "auto", nil, Wait{Window: -1, Notify: func(s string) {
			lm.Lock()
			lines = append(lines, s)
			lm.Unlock()
		}})
	}()
	select {
	case res := <-done:
		t.Fatalf("the CLI blocks while every lane is busy: %+v", res)
	case <-time.After(400 * time.Millisecond):
	}
	waitFor(t, 5*time.Second, "the human is told it is queued", func() bool {
		lm.Lock()
		defer lm.Unlock()
		return len(lines) > 0 && strings.Contains(lines[0], "queued")
	})
	e.free(0)
	select {
	case res := <-done:
		if !res.OK || res.Meta.Node != "node-a" {
			t.Fatalf("the freed node ran it: %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the CLI never returned")
	}
	lm.Lock()
	if last := lines[len(lines)-1]; !strings.Contains(last, "started on") {
		t.Errorf("the human is told when it started: %v", lines)
	}
	for i := 1; i < len(lines); i++ {
		if lines[i] == lines[i-1] {
			t.Errorf("the human is told once per change of standing, not once per scan: %v", lines)
			break
		}
	}
	lm.Unlock()
	if n := len(ticketFiles(t, e.cfg)); n != 0 {
		t.Errorf("the call that took the result took the ticket with it: %d file(s)", n)
	}

	// Ctrl-C before anything was sent cancels the ticket.
	e.hold(0)
	ctx, cancel := context.WithCancel(context.Background())
	req2 := e.req("another door", nil)
	cdone := make(chan core.Result, 1)
	go func() { cdone <- RunWith(ctx, e.cfg, e.lr, req2, "auto", nil, Wait{Window: -1}) }()
	waitFor(t, 5*time.Second, "the second ticket exists", func() bool { return len(ticketFiles(t, e.cfg)) == 1 })
	place := ""
	for _, lc := range e.lr.leaveCalls() {
		place = lc.token
	}
	cancel()
	select {
	case res := <-cdone:
		if res.OK || !res.Deferred || !strings.Contains(res.Reason, "canceled") {
			t.Fatalf("the interrupted call says its ticket was canceled: %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the interrupt never returned")
	}
	if n := len(ticketFiles(t, e.cfg)); n != 0 {
		t.Errorf("the canceled ticket is gone: %d", n)
	}
	if e.lr.dropped(place) != 1 {
		t.Errorf("its place in the line is let go: %v", e.lr.drops)
	}
	if n := len(e.nodes[0].posts()); n != 1 {
		t.Errorf("nothing was sent for the canceled ticket (the one POST is the first job's): %d", n)
	}
	defaultQ.mu.Lock()
	live := len(defaultQ.live)
	defaultQ.mu.Unlock()
	if live != 0 {
		t.Errorf("the queue holds no ticket: %d", live)
	}
}

// An MCP door that is canceled does not cancel the ticket: it outlives the call.
func TestADetachedCallThatEndsLeavesTheTicketRunning(t *testing.T) {
	e := newQEnv(t, 1, true)
	req := e.req("a red door", nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan core.Result, 1)
	go func() { done <- RunWith(ctx, e.cfg, e.lr, req, "auto", nil, Wait{Window: time.Minute, Detach: true}) }()
	waitFor(t, 5*time.Second, "the ticket exists", func() bool { return len(ticketFiles(t, e.cfg)) == 1 })
	cancel()
	res := <-done
	if res.OK || !strings.Contains(res.Reason, "keeps running") {
		t.Fatalf("the call says the ticket goes on: %+v", res)
	}
	if n := len(ticketFiles(t, e.cfg)); n != 1 {
		t.Errorf("the ticket is still on file: %d", n)
	}
	e.free(0)
	if res := settle(t, e.cfg, e.lr, req, func(r core.Result) bool { return r.OK }); res.Meta.Node != "node-a" {
		t.Errorf("it still runs when a lane frees: %+v", res)
	}
}

// ---- the answer -----------------------------------------------------------------------------------------------------------

// The answer names EVERY roster node, in config order, with its state and why: an idle node a ticket of this machine is
// using (in-use), a node that holds a lease (busy, and whose), one that renders another build (not-capable, and which key
// differs) and one that is gone (unreachable), each since when; the ticket, the position, this machine's own lane, and the
// instruction not to call again.
func TestAnswerNamesEveryRosterNode(t *testing.T) {
	fams := map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}
	inUse := startImageNode(t, "node-a", "hidream-o1", fams, nodeOpts{})
	busy := startImageNode(t, "node-b", "hidream-o1", fams, nodeOpts{lease: leased()})
	other := startImageNode(t, "node-c", "hidream-o1", map[string]string{"qwen-image-2.1": block(fp4Ckpt, "")}, nodeOpts{})
	gone := startImageNode(t, "node-d", "hidream-o1", fams, nodeOpts{})
	gone.srv.Close()
	cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, inUse, busy, other, gone)
	lr := newKeeperRunner(heldLane())

	// node-a is in use by a ticket of this machine: a placed ticket with a live owner and a claim.
	st, _ := storeFor(cfg)
	user := ticketFile{ID: "mq-inuse0000001", ReqKey: "other", Created: nowMs() - 5000, Owner: selfOwner(), Beat: nowMs(), State: stPlaced, Node: "node-a", Base: inUse.srv.URL, Job: "media-x"}
	if ok, _, err := st.claimNode(&user, inUse.srv.URL, "node-a", stPlaced); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}

	res := runDoor(cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil), AtOnce)
	a := answerOf(t, res)
	if a.Position != 1 || !strings.Contains(a.Next, "do not call again") || !strings.Contains(a.Next, "waiter_token="+a.Ticket) {
		t.Errorf("position / next: %d %q", a.Position, a.Next)
	}
	if len(a.Cluster) != 4 {
		t.Fatalf("one row per roster node: %+v", a.Cluster)
	}
	want := []struct{ state, node string }{{"in-use", "node-a"}, {"busy", "node-b"}, {"not-capable", "node-c"}, {"unreachable", "127.0.0.1"}}
	for i, w := range want {
		r := a.Cluster[i]
		if r.State != w.state || !strings.Contains(r.Node, w.node) || r.Why == "" {
			t.Errorf("row %d = %+v, want %s naming %s", i, r, w.state, w.node)
		}
		if _, err := time.Parse(time.RFC3339, r.Since); err != nil {
			t.Errorf("row %d: since %q is not a time: %v", i, r.Since, err)
		}
	}
	if !strings.Contains(a.Cluster[0].Why, "mq-inuse0000001") {
		t.Errorf("in-use says whose job: %s", a.Cluster[0].Why)
	}
	if h := a.Cluster[1].Holder; h == nil || h.Class != "media" || h.Reason != "a bench render" || h.Epoch != 9 || h.RemainingSec < 200 {
		t.Errorf("busy says what holds the node: %+v", a.Cluster[1].Holder)
	}
	if len(a.Cluster[2].Differs) != 1 || !strings.HasPrefix(a.Cluster[2].Differs[0], "imagegen_ckpt: ") {
		t.Errorf("not-capable says which key differs: %v", a.Cluster[2].Differs)
	}
	if a.Local == nil || a.Local.State != "busy" || a.Local.Holder == nil || a.Local.Holder.Reason != "bench render of the spring set" {
		t.Errorf("this machine's own lane is named: %+v", a.Local)
	}
}

// ---- the roster ------------------------------------------------------------------------------------------------------------

// An MCP session never reloads its config, so a node added to the config file since the session began must join it: the
// ticket re-reads delegate_remotes from the file when the file's mtime changes, and nothing else of the session's config moves.
func TestRosterReReadWithoutRestart(t *testing.T) {
	e := newQEnv(t, 2, true)
	file := filepath.Join(t.TempDir(), "config.json")
	write := func(remotes ...string) {
		b, _ := json.Marshal(map[string]any{"delegate_remotes": remotes, "media_dir": "elsewhere", "gpu_wait_ms": 1})
		if err := os.WriteFile(file, b, 0o644); err != nil {
			t.Fatal(err)
		}
		bump := time.Now().Add(time.Duration(len(remotes)) * 3 * time.Second)
		_ = os.Chtimes(file, bump, bump)
	}
	write(e.nodes[0].srv.URL)
	cfg := e.cfg
	cfg.DelegateRemotes = []string{e.nodes[0].srv.URL} // the session started with the first node only
	req := e.req("a red door", nil)
	a := answerOf(t, RunWith(context.Background(), cfg, e.lr, req, "auto", nil, Wait{Window: AtOnce, Detach: true, RosterFile: file}))
	if len(a.Cluster) != 1 {
		t.Fatalf("the session's roster is one node: %+v", a.Cluster)
	}

	// The second node is added to the file; it is idle. The running session picks it up and the job starts there.
	e.free(1)
	write(e.nodes[0].srv.URL, e.nodes[1].srv.URL)
	waitFor(t, 5*time.Second, "the node added to the file runs the job", func() bool { return len(e.nodes[1].posts()) == 1 })
	noPosts(t, "the node the session started with (busy)", e.nodes[0])

	// Nothing else moved: the call's config still carries its own token and window.
	got := effectiveCfg(cfg, file)
	if got.FleetAuthToken != "tok" || got.GPUWaitMs != cfg.GPUWaitMs || got.MediaDir != cfg.MediaDir || got.StateDir != cfg.StateDir {
		t.Errorf("only delegate_remotes is re-read: %+v", got)
	}
	if len(got.DelegateRemotes) != 2 {
		t.Errorf("the roster is the file's: %v", got.DelegateRemotes)
	}
	// A file that cannot be read as JSON keeps the roster the session has; a missing path changes nothing.
	if err := os.WriteFile(file, []byte(`{ "delegate_remotes": [`), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(file, later, later)
	if kept := effectiveCfg(cfg, file); len(kept.DelegateRemotes) != 1 {
		t.Errorf("a half-written file must not empty or change a running session's fleet: %v", kept.DelegateRemotes)
	}
	if same := effectiveCfg(cfg, ""); len(same.DelegateRemotes) != 1 {
		t.Errorf("no file, no change: %v", same.DelegateRemotes)
	}
}

// ---- tokens that are not the pipeline's -------------------------------------------------------------------------------------

// A ticket's token is the queue's alone: the pipeline's lease line never sees an mq- token (it would read it as a new
// arrival and say nothing), a token of another request is refused by name, and a token that names nothing is a new arrival.
func TestMQTokenNeverReachesThePipeline(t *testing.T) {
	e := newQEnv(t, 1, true)
	req := e.req("a red door", nil)
	a := answerOf(t, runDoor(e.cfg, e.lr, req, AtOnce))

	with := func(r core.Request, tok string) core.Request {
		p := map[string]any{"waiter_token": tok}
		for k, v := range r.Params {
			p[k] = v
		}
		r.Params = p
		return r
	}
	answerOf(t, runDoor(e.cfg, e.lr, with(req, a.Ticket), AtOnce))
	// Another request with this ticket's token: refused, by name.
	res := runDoor(e.cfg, e.lr, with(e.req("a different door", nil), a.Ticket), AtOnce)
	if res.OK || res.DeferClass != core.DeferClassContract || !strings.Contains(res.Reason, a.Ticket) {
		t.Errorf("a token of another request is a contract refusal that names it: %+v", res)
	}
	// A token that names nothing: a new arrival, and its own ticket.
	fresh := answerOf(t, runDoor(e.cfg, e.lr, with(e.req("a third door", nil), "mq-000000000abc"), AtOnce))
	if fresh.Ticket == "mq-000000000abc" || fresh.Ticket == a.Ticket {
		t.Errorf("an unknown token starts a new ticket: %+v", fresh)
	}
	// A malformed one too, and none of them reached the lane question or a run.
	answerOf(t, runDoor(e.cfg, e.lr, with(e.req("a fourth door", nil), "mq-../../etc"), AtOnce))
	e.lr.mu.Lock()
	probed := append([]core.Request(nil), e.lr.probed...)
	e.lr.mu.Unlock()
	for _, p := range probed {
		if tok, _ := p.Params["waiter_token"].(string); strings.HasPrefix(tok, "mq-") {
			t.Errorf("the pipeline was asked about a request carrying a ticket token: %v", p.Params)
		}
	}
	for _, p := range e.lr.ranCalls() {
		if tok, _ := p.Params["waiter_token"].(string); strings.HasPrefix(tok, "mq-") {
			t.Errorf("the pipeline ran a request carrying a ticket token: %v", p.Params)
		}
	}
}

// ---- bounces and growth ----------------------------------------------------------------------------------------------------

// A node that takes the job and passes it back is left alone for a while, and the ticket goes on to the next node at the next
// scan: one node per scan, no limit on how many nodes it ends up trying (the S1 call gave up after three and went to the
// local queue; the ticket waits for a lane instead).
func TestABouncedAttemptGoesBackToWaitingAndTheNextScanTriesTheNextNode(t *testing.T) {
	bounce := func(i int, o *nodeOpts) {
		if i < 4 {
			o.deferAs, o.deferClass = "gpu busy: card(s) held by a media-class lease", core.ErrClassGPUBusy
		}
	}
	e := newQEnv(t, 5, false, bounce)
	pinPlacerClock(t)
	res := runBounded(e.cfg, e.lr, e.req("a red door", nil), "auto", nil)
	if !res.OK || res.Meta.Node != "node-e" {
		t.Fatalf("the fifth node serves it: %+v", res)
	}
	for i := 0; i < 4; i++ {
		if n := len(e.nodes[i].posts()); n != 1 {
			t.Errorf("node %d was sent the job once: %d", i, n)
		}
	}
	if _, runs := e.lr.counts(); runs != 0 {
		t.Errorf("the job never ran here: %d", runs)
	}
}

// The state of the queue does not grow with scans: the same files, the same ticket file size, no temp files, no claims, no
// goroutines per scan; and when the ticket is done everything is gone and the scanner has ended.
func TestQueueStateAndGoroutinesDoNotGrowWithScans(t *testing.T) {
	e := newQEnv(t, 3, true)
	e.nodes[1].srv.Close()
	q, _ := manual(t)
	lt := admitOn(t, q, e.cfg, e.lr, e.req("a red door", nil))
	for i := 0; i < 10; i++ {
		q.cycle(context.Background())
	}
	files := func() []string {
		ents, _ := os.ReadDir(filepath.Join(e.cfg.StateDir, queueDirName))
		st, _ := storeFor(e.cfg)
		var names []string
		for _, f := range ents {
			if f.Name() == lockName { // the lock file stays for good (queuefs.go)
				continue
			}
			if tf, ok := st.read(strings.TrimSuffix(f.Name(), ".json")); ok && tf.Collected && terminalState(tf.State) {
				continue // a taken result stays collectedKeep for the other callers waiting on it
			}
			names = append(names, f.Name())
		}
		return names
	}
	size := func() int64 {
		fi, err := os.Stat(filepath.Join(e.cfg.StateDir, queueDirName, lt.t.ID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	baseFiles, baseSize, baseG := files(), size(), runtime.NumGoroutine()
	for i := 0; i < 300; i++ {
		q.cycle(context.Background())
	}
	if got := files(); strings.Join(got, ",") != strings.Join(baseFiles, ",") {
		t.Errorf("the queue directory is the same after 300 scans: %v / %v", baseFiles, got)
	}
	if got := size(); got > baseSize+64 {
		t.Errorf("the ticket file does not grow with scans: %d -> %d bytes", baseSize, got)
	}
	if g := runtime.NumGoroutine(); g > baseG+3 {
		t.Errorf("no goroutine per scan: %d -> %d", baseG, g)
	}
	lt.mu.Lock()
	memo, rows := len(lt.rowSince), len(lt.t.Cluster)
	lt.mu.Unlock()
	if memo > 4 || rows != 3 {
		t.Errorf("the per-node memory is bounded by the roster: %d memos, %d rows", memo, rows)
	}
	placer.mu.Lock()
	nodes := len(placer.nodes)
	placer.mu.Unlock()
	if nodes > 3 {
		t.Errorf("the placer remembers at most the roster: %d", nodes)
	}

	// The ticket ends: everything goes, and the real scanner of a queue exits when it has nothing to look at.
	e.free(0)
	q.cycle(context.Background())
	waitFor(t, 5*time.Second, "the ticket is done", func() bool { return isDone(lt) })
	q.deliver(lt)
	if got := files(); len(got) != 0 {
		t.Errorf("nothing is left on disk: %v", got)
	}

	prev := scannerAuto
	scannerAuto = true
	defer func() { scannerAuto = prev }()
	auto := freshQueue(t)
	runDoor(e.cfg, e.lr, e.req("another door", nil), AtOnce)
	e.free(2)
	waitFor(t, 10*time.Second, "the second ticket renders", func() bool {
		auto.mu.Lock()
		defer auto.mu.Unlock()
		return len(auto.live) == 0
	})
	waitFor(t, 5*time.Second, "the scanner ends with its last ticket", func() bool {
		auto.mu.Lock()
		defer auto.mu.Unlock()
		return !auto.scanning
	})
	waitFor(t, 5*time.Second, "the place keeper ends with its scanner", func() bool { return auto.keepers.Load() == 0 })
}

// ---- the claim, the lane, the memory -----------------------------------------------------------------------------------

// A node is claimed by one ticket at a time, under the directory lock: a second ticket is refused and told whose it is; the
// holder's own claim is its to keep; a claim whose owner is gone, or whose ticket has no job in flight, holds nothing; and a
// released node is free.
func TestClaimNodeIsExclusiveAndLetsGoOfTheDead(t *testing.T) {
	st := &store{dir: filepath.Join(t.TempDir(), queueDirName)}
	const base = "http://node.example:18811"
	mk := func(id string, owner ownerID) ticketFile {
		return ticketFile{ID: id, ReqKey: id, Created: nowMs(), Owner: owner, Beat: nowMs(), State: stWaiting}
	}
	a, b := mk("mq-aaaaaaaaaaaa", selfOwner()), mk("mq-bbbbbbbbbbbb", selfOwner())
	if ok, _, err := st.claimNode(&a, base, "node-x", stPlacing); err != nil || !ok {
		t.Fatalf("the first ticket claims an unclaimed node: %v %v", ok, err)
	}
	if a.State != stPlacing || a.Base != base {
		t.Errorf("the claim saves the ticket as placing on the node: %+v", a)
	}
	ok, holder, err := st.claimNode(&b, base+"/", "node-x", stPlacing) // the same node, spelled with a slash
	if err != nil || ok || holder.Ticket != a.ID {
		t.Fatalf("a second ticket is refused and told whose the node is: ok=%v holder=%+v err=%v", ok, holder, err)
	}
	if b.State != stWaiting || b.Base != "" {
		t.Errorf("a refused ticket is not changed: %+v", b)
	}
	if again, _, _ := st.claimNode(&a, base, "node-x", stPlacing); !again {
		t.Error("the holder keeps its own claim")
	}
	if got := st.claims(); len(got) != 1 || got[base].Ticket != a.ID {
		t.Errorf("one live claim: %+v", got)
	}

	// The holder's job ends (its ticket is no longer in flight): the claim holds nothing.
	a.State = stDone
	if err := st.write(a); err != nil {
		t.Fatal(err)
	}
	if len(st.claims()) != 0 {
		t.Error("a claim whose ticket has no job in flight is not live")
	}
	if ok, _, _ := st.claimNode(&b, base, "node-x", stPlacing); !ok {
		t.Fatal("the node is free once the job is done")
	}
	// The holder's process is gone: the claim holds nothing either.
	dead, _ := json.Marshal(claim{Base: normBase(base), Ticket: b.ID, Owner: deadOwner, At: nowMs()})
	if err := writeFileAtomic(st.claimPath(base), dead); err != nil {
		t.Fatal(err)
	}
	c := mk("mq-cccccccccccc", selfOwner())
	// Its process just died: the job it sent is still running on the node, which shows its lease only once the job is admitted,
	// so the node stays in use for the grace.
	if ok, holder, _ := st.claimNode(&c, base, "node-x", stPlacing); ok || holder.Ticket != b.ID {
		t.Fatalf("the node of an owner that died a moment ago is still in use: ok=%v holder=%+v", ok, holder)
	}
	// Long after, the node's own health speaks for it.
	b.Beat = time.Now().Add(-deadClaimGrace - time.Minute).UnixMilli()
	if err := st.write(b); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := st.claimNode(&c, base, "node-x", stPlacing); !ok {
		t.Fatal("the node of an owner that died long ago is free")
	}
	st.releaseNode(base, "mq-bbbbbbbbbbbb") // not the holder: nothing happens
	if len(st.claims()) != 1 {
		t.Error("only the holder lets a node go")
	}
	st.releaseNode(base, c.ID)
	// The three ticket files and the lock file, which stays for good (an operating-system file lock is on the open file, and a
	// file that came and went would let a late contender lock a new one while an earlier holder still had the old: queuefs.go).
	if files, _ := os.ReadDir(st.dir); len(files) != 4 {
		var names []string
		for _, f := range files {
			names = append(names, f.Name())
		}
		t.Errorf("a released node leaves no claim behind (the three tickets and the lock file): %v", names)
	}
}

// Only the oldest ticket takes the local lane in one scan; the others wait for the next. (Two runs that both start would
// each wait the lease line's window, and the second would report a lost race.)
func TestOnlyOneTicketTakesTheLocalLanePerScan(t *testing.T) {
	e := newQEnv(t, 1, true)
	q, _ := manual(t)
	lt1 := admitOn(t, q, e.cfg, e.lr, e.req("first", nil))
	time.Sleep(5 * time.Millisecond)
	lt2 := admitOn(t, q, e.cfg, e.lr, e.req("second", nil))
	forgetFirstReading(lt1, lt2)
	e.lr.set(core.LaneVerdict{Free: true})
	q.cycle(context.Background())
	waitFor(t, 5*time.Second, "the oldest ticket runs here", func() bool { return isDone(lt1) })
	if runs := e.lr.ranCalls(); len(runs) != 1 || runs[0].Input != "first" {
		t.Fatalf("one run in the scan, the oldest ticket's: %+v", runs)
	}
	if isDone(lt2) {
		t.Error("the younger ticket waits for the next scan")
	}
	q.cycle(context.Background())
	waitFor(t, 5*time.Second, "then the younger one", func() bool { return isDone(lt2) })
}

// The queue directory is kept tidy: a temp file of a write that never finished is swept, and the row memory forgets a node
// that left the roster.
func TestSweepRemovesDebrisAndTheRowMemoryForgetsAGoneNode(t *testing.T) {
	e := newQEnv(t, 2, true)
	st, _ := storeFor(e.cfg)
	if err := os.MkdirAll(st.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for _, name := range []string{"mq-aaaaaaaaaaaa.json.1.abcd.tmp", "mq-garbage00001.json"} {
		p := filepath.Join(st.dir, name)
		if err := os.WriteFile(p, []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, old, old)
	}
	q, _ := manual(t)
	lt := admitOn(t, q, e.cfg, e.lr, e.req("a red door", nil))
	q.cycle(context.Background())
	if left := queueFiles(t, e.cfg, "mq-aaaaaaaaaaaa"); len(left) != 0 {
		t.Errorf("a stale temp file is swept: %v", left)
	}
	if left := queueFiles(t, e.cfg, "mq-garbage"); len(left) != 0 {
		t.Errorf("a ticket file that does not parse is swept: %v", left)
	}

	lt.mu.Lock()
	before := len(lt.rowSince)
	lt.mu.Unlock()
	if before != 3 { // two nodes and this machine's own lane
		t.Fatalf("the memory holds the two nodes and the local lane: %d", before)
	}
	lt.cfg.DelegateRemotes = lt.cfg.DelegateRemotes[:1]
	q.cycle(context.Background())
	lt.mu.Lock()
	after := len(lt.rowSince)
	lt.mu.Unlock()
	if after != 2 {
		t.Errorf("a node that left the roster is forgotten: %d", after)
	}
}

// forgetFirstReading drops the lane reading a ticket made when it was created, which its first scan would otherwise reuse for
// a couple of seconds: a test that changes the lane right after creating the ticket wants the scan to look again.
func forgetFirstReading(lts ...*liveTicket) {
	for _, lt := range lts {
		lt.mu.Lock()
		lt.first = nil
		lt.mu.Unlock()
	}
}

// The lane was free when the question was asked and taken before the run got it (a race the question cannot rule out): the
// run came back with nothing run ("gpu busy / queued"), and the ticket goes back to waiting with its place, then runs when
// the lane really frees. It is not a failure and not a result.
func TestALostRaceOnTheLocalLaneGoesBackToWaiting(t *testing.T) {
	e := newQEnv(t, 1, true)
	lost := core.Deferf("gpu queued: card(s) c2 held", "", core.Meta{ErrClass: core.ErrClassGPUQueued})
	lost.DeferClass = core.DeferClassCapacity
	e.lr.local = lost
	q, _ := manual(t)
	lt := admitOn(t, q, e.cfg, e.lr, e.req("a red door", nil))
	place := lt.snapshot().Place
	forgetFirstReading(lt)
	e.lr.set(core.LaneVerdict{Free: true})
	q.cycle(context.Background())
	waitFor(t, 5*time.Second, "the run comes back", func() bool { return len(e.lr.ranCalls()) == 1 })
	waitFor(t, 5*time.Second, "the ticket is waiting again", func() bool {
		s := lt.snapshot()
		lt.mu.Lock()
		running := lt.running
		lt.mu.Unlock()
		return s.State == stWaiting && !running
	})
	if isDone(lt) {
		t.Fatal("a lost race is not the ticket's result")
	}
	if e.lr.dropped(place) != 0 {
		t.Error("the ticket keeps its place through a lost race")
	}
	e.lr.mu.Lock()
	e.lr.local = core.Result{OK: true, Data: json.RawMessage(`{"local":true}`)}
	e.lr.mu.Unlock()
	// The refusal paused the lane for this ticket (TestALostRaceOnTheLocalLaneIsNotARetryLoop...): a cycle inside the pause
	// does not ask it, and one after the pause does.
	lt.mu.Lock()
	if !time.Now().Before(lt.lostUntil) || lt.lostN != 1 {
		lt.mu.Unlock()
		t.Fatalf("a refused run pauses the lane for the ticket: lostN=%d until=%s", lt.lostN, lt.lostUntil)
	}
	lt.mu.Unlock()
	q.cycle(context.Background())
	if runs := len(e.lr.ranCalls()); runs != 1 {
		t.Fatalf("inside the pause the lane is not asked again: %d runs", runs)
	}
	lt.mu.Lock()
	lt.lostUntil = time.Now().Add(-time.Second)
	lt.mu.Unlock()
	q.cycle(context.Background())
	waitFor(t, 5*time.Second, "the second run serves it", func() bool { return isDone(lt) })
	if !lt.res.OK {
		t.Errorf("the second run is the result: %+v", lt.res)
	}
}

// An owner says it is alive by writing its beat on every scan, so one that is hung (alive by pid, silent for ownerStale) can
// be told from one that is working.
func TestAnOwnerWritesItsBeatOnEveryScan(t *testing.T) {
	e := newQEnv(t, 1, true)
	q, _ := manual(t)
	lt := admitOn(t, q, e.cfg, e.lr, e.req("a red door", nil))
	st, _ := storeFor(e.cfg)
	t0, _ := st.read(lt.t.ID)
	time.Sleep(20 * time.Millisecond)
	q.cycle(context.Background())
	t1, _ := st.read(lt.t.ID)
	if t1.Beat <= t0.Beat {
		t.Errorf("the beat moves on every scan: %d -> %d", t0.Beat, t1.Beat)
	}
	// A ticket whose local run is in flight is still beating.
	lt.mu.Lock()
	lt.running, lt.t.State = true, stLocal
	lt.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	q.cycle(context.Background())
	t2, _ := st.read(lt.t.ID)
	if t2.Beat <= t1.Beat {
		t.Errorf("a ticket that is running keeps beating: %d -> %d", t1.Beat, t2.Beat)
	}
	lt.mu.Lock()
	lt.running, lt.t.State = false, stWaiting
	lt.mu.Unlock()
}

// A ticket whose box keeps no place asks again after a while, not on every scan.
func TestAPlaceThatCannotBeHadIsAskedForAgainAfterAWhileNotEveryScan(t *testing.T) {
	e := newQEnv(t, 1, true)
	e.lr.noPlace = core.ErrNoMediaPlace
	q, _ := manual(t)
	lt := admitOn(t, q, e.cfg, e.lr, e.req("a red door", nil))
	for i := 0; i < 12; i++ {
		q.cycle(context.Background())
	}
	e.lr.km.Lock()
	asked := e.lr.asked
	e.lr.km.Unlock()
	if asked != 1 {
		t.Fatalf("the box said it keeps no place; twelve scans must not ask again: %d asks", asked)
	}
	// After the retry interval it asks again, and a box that can now keep one gives the ticket its place.
	lt.mu.Lock()
	lt.placeTried = time.Now().Add(-2 * placeRetry)
	lt.mu.Unlock()
	e.lr.km.Lock()
	e.lr.noPlace = nil
	e.lr.km.Unlock()
	q.cycle(context.Background())
	if lt.snapshot().Place == "" {
		t.Error("a box that can keep a place now gives the ticket one")
	}
}

// Two processes that get the same request at the same instant make ONE ticket: the check and the write are one step under the
// directory lock, so neither passes the other's request by.
func TestTwoProcessesAskingAtOnceMakeOneTicket(t *testing.T) {
	e := newQEnv(t, 1, true)
	q1, q2 := manual(t)
	// Nobody scans in this test, so the call that attaches to the other process's ticket must not wait for a first scan.
	prev := firstScanBound
	firstScanBound = 20 * time.Millisecond
	t.Cleanup(func() { firstScanBound = prev })
	for round := 0; round < 25; round++ {
		req := e.req("a red door", map[string]any{"seed": 1000 + round})
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, q := range []*queueRT{q1, q2} {
			wg.Add(1)
			go func(q *queueRT) {
				defer wg.Done()
				<-start
				q.admit(context.Background(), e.cfg, e.lr, req, nil, Wait{Window: AtOnce, Detach: true})
			}(q)
		}
		close(start)
		wg.Wait()
		st, _ := storeFor(e.cfg)
		n := 0
		for _, tk := range st.list() {
			if tk.Seed == 1000+round {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("round %d: two processes asking at once made %d tickets for one request", round, n)
		}
	}
}

// A call whose window ends while the job is already on a node is told where it runs, not that it is queued, and that it
// need not call again.
func TestAnAnswerWhileTheJobRunsSaysWhereItRuns(t *testing.T) {
	hold := make(chan struct{})
	e := newQEnv(t, 1, false, func(_ int, o *nodeOpts) { o.hold = hold })
	t.Cleanup(func() { close(hold) })
	req := e.req("a red door", nil)
	res := settle(t, e.cfg, e.lr, req, func(r core.Result) bool { return strings.Contains(r.Reason, "is running on node-a") })
	a := answerOf(t, res)
	if (a.State != stPlaced && a.State != stPlacing && a.State != stFetching) || a.Node != "node-a" {
		t.Errorf("the answer carries the state and the node: %+v", a)
	}
	if !strings.Contains(res.Reason, "remote job media-") || !strings.Contains(res.Reason, "do not call again") {
		t.Errorf("the answer names the job and says not to call again: %s", res.Reason)
	}
	hold <- struct{}{}
	if r := settle(t, e.cfg, e.lr, req, func(r core.Result) bool { return r.OK }); r.Meta.Node != "node-a" {
		t.Errorf("collected: %+v", r.Meta)
	}
}

// The ticket of a dead owner, named by its token: nothing was sent, so it is swept, and the call starts over as a new ticket that
// keeps the arrival of the one it replaces.
func TestADeadOwnersTicketNamedByTokenGivesItsArrivalToTheNewOne(t *testing.T) {
	e := newQEnv(t, 1, true)
	req := e.req("a red door", nil)
	st, _ := storeFor(e.cfg)
	arrived := nowMs() - 300_000
	if err := st.write(ticketFile{ID: "mq-deadnamed0001", ReqKey: reqKey(req), Created: arrived, Owner: deadOwner, Beat: arrived, State: stWaiting}); err != nil {
		t.Fatal(err)
	}
	named := req
	named.Params = map[string]any{"waiter_token": "mq-deadnamed0001"}
	for k, v := range req.Params {
		named.Params[k] = v
	}
	a := answerOf(t, runDoor(e.cfg, e.lr, named, AtOnce))
	if a.Ticket == "mq-deadnamed0001" {
		t.Fatal("the dead owner's ticket is not the new call's")
	}
	if got := st.list(); len(got) != 1 || got[0].Created != arrived {
		t.Errorf("the new ticket keeps the arrival of the swept one: %+v (want %d)", got, arrived)
	}
}

// A finished ticket is collected by its token wherever the collecting call runs, including under a lease its process inherited
// (which has no lane of its own to ask about): the ticket's result does not depend on one.
func TestATicketIsCollectedByTokenEvenUnderAnInheritedLease(t *testing.T) {
	e := newQEnv(t, 1, true)
	req := e.req("a red door", nil)
	st, _ := storeFor(e.cfg)
	done, _ := json.Marshal(core.Result{OK: true, Data: json.RawMessage(`{"image_path":"x.png"}`), Meta: core.Meta{Node: "node-a"}})
	if err := st.write(ticketFile{ID: "mq-collectme0001", ReqKey: reqKey(req), Created: nowMs(), Owner: deadOwner, Beat: nowMs(), State: stDone, Result: done}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GPU_LEASE_DIR", t.TempDir())
	named := req
	named.Params = map[string]any{"waiter_token": "mq-collectme0001"}
	for k, v := range req.Params {
		named.Params[k] = v
	}
	res := runDoor(e.cfg, e.lr, named, AtOnce)
	if !res.OK || res.Meta.Node != "node-a" || !strings.Contains(string(res.Data), "x.png") {
		t.Fatalf("the finished ticket's result is handed over: %+v", res)
	}
	if n := len(ticketFiles(t, e.cfg)); n != 0 {
		t.Errorf("a collected ticket is gone: %d", n)
	}
	if _, runs := e.lr.counts(); runs != 0 {
		t.Errorf("collecting asks no lane: %d runs", runs)
	}
}

// The miss of a node is logged once per backoff step, not once per scan: a node inside its pause is not probed, so it says
// nothing. (The pauses are 5, 15, 60 and then 300 seconds, chosen.)
func TestADeadNodeIsLoggedOncePerBackoffStepNotPerScan(t *testing.T) {
	e := newQEnv(t, 1, true)
	cfg := e.cfg
	cfg.DelegateRemotes = append([]string{"http://127.0.0.1:9"}, cfg.DelegateRemotes...) // nothing listens on port 9
	now := pinPlacerClock(t)
	var mu sync.Mutex
	var lines []string
	prev := queueLog
	queueLog = func(l string) {
		mu.Lock()
		lines = append(lines, l)
		mu.Unlock()
	}
	t.Cleanup(func() { queueLog = prev })
	q, _ := manual(t)
	admitOn(t, q, cfg, e.lr, e.req("a red door", nil))
	logged := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, l := range lines {
			if strings.Contains(l, ":9") && strings.Contains(l, "not probing it again for") {
				n++
			}
		}
		return n
	}
	for _, step := range []struct {
		advance time.Duration
		want    int
		note    string
	}{
		{0, 1, "the first miss is logged"},
		{4 * time.Second, 1, "inside the 5 s pause: not probed, not logged"},
		{2 * time.Second, 2, "past 5 s: probed and missed again, logged"},
		{14 * time.Second, 2, "inside the 15 s pause"},
		{2 * time.Second, 3, "past 15 s"},
		{59 * time.Second, 3, "inside the 60 s pause"},
		{2 * time.Second, 4, "past 60 s"},
		{299 * time.Second, 4, "inside the 300 s pause"},
		{2 * time.Second, 5, "past 300 s"},
	} {
		*now = now.Add(step.advance)
		for i := 0; i < 3; i++ { // several scans inside one instant of the pinned clock
			q.cycle(context.Background())
		}
		if got := logged(); got != step.want {
			t.Fatalf("%s: %d line(s), want %d (%v)", step.note, got, step.want, lines)
		}
	}
}

// A miss the shared roster cache replays (another lane of the process found the node dead a moment ago) is not this lane's own
// failure: it is neither logged nor counted towards the node's pause, or one outage would push the pause up a step per lane.
func TestAMissReplayedFromTheNegativeCacheIsNeitherLoggedNorCounted(t *testing.T) {
	e := newQEnv(t, 1, true)
	pinPlacerClock(t)
	const gone = "http://127.0.0.1:9"
	prev := probeRoster
	probeRoster = func(_ context.Context, remotes []string, _ string, _ time.Duration) []rosterprobe.Reading {
		var out []rosterprobe.Reading
		for i, b := range remotes {
			out = append(out, rosterprobe.Reading{Member: rosterprobe.Member{Index: i, Base: b}, Err: errors.New("dial tcp: connection refused"), Source: rosterprobe.SourceNegative})
		}
		return out
	}
	t.Cleanup(func() { probeRoster = prev })
	var mu sync.Mutex
	var lines []string
	prevLog := queueLog
	queueLog = func(l string) {
		mu.Lock()
		lines = append(lines, l)
		mu.Unlock()
	}
	t.Cleanup(func() { queueLog = prevLog })
	local, _, _ := localIdentity(e.cfg, "qwen-image-2.1-fast")
	_, rows := candidatesFor(context.Background(), e.cfg, []string{gone}, local, need{})
	if len(rows) != 1 || rows[0].row.State != "unreachable" {
		t.Fatalf("the node reads as unreachable in the answer: %+v", rows)
	}
	if _, backed := placer.backedOff(gone); backed {
		t.Error("a replayed miss does not start the node's own pause")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 0 {
		t.Errorf("a replayed miss is not logged: %v", lines)
	}
}
