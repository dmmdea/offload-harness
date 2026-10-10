package mediaremote

// The ticket queue (P0 plan S2; ADR 0083): a busy image call waits for the first matching lane that frees ANYWHERE, with
// one answer that says where it stands and why, and no caller loop. Everything below runs against real fleet-node servers
// (their doors, job store and recipe advertisement) whose leases the test toggles, a delegator whose pipeline is a runner
// that answers the lane question and keeps places in a line, and the real ticket files in a temp state dir. Two queues
// over one state dir stand for two processes of one machine.

import (
	"context"
	"encoding/json"
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

// ticketFiles lists the ticket files in the state dir.
func ticketFiles(t *testing.T, cfg config.Config) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(cfg.StateDir, queueDirName))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "mq-") && strings.HasSuffix(e.Name(), ".json") {
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

	res := Run(context.Background(), e.cfg, e.lr, req, "auto", nil)
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
	if r := Run(context.Background(), given.cfg, given.lr, given.req("p", map[string]any{"seed": 31337}), "auto", nil); !r.OK {
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
	e := newQEnv(t, 2, true)
	q, _ := manual(t)
	lt1 := admitOn(t, q, e.cfg, e.lr, e.req("first", nil))
	time.Sleep(5 * time.Millisecond)
	lt2 := admitOn(t, q, e.cfg, e.lr, e.req("second", nil))
	e.free(0)
	e.free(1)
	q.cycle(context.Background())
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
// directory lock, so the read and the write are one step.
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
	for i := 0; i < 8; i++ {
		for _, q := range []*queueRT{q1, q2} {
			wg.Add(1)
			go func(q *queueRT) {
				defer wg.Done()
				<-start
				q.cycle(context.Background())
			}(q)
		}
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
	prevKeep := doneKeep
	doneKeep = time.Millisecond
	t.Cleanup(func() { doneKeep = prevKeep })
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
	lm.Unlock()

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
	res := Run(context.Background(), e.cfg, e.lr, e.req("a red door", nil), "auto", nil)
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
		var names []string
		for _, f := range ents {
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
}
