package mediaremote

// A whole-node graph is a ticket (ADR 0083 Amendment 1; it supersedes decision 12). A graph call whose local lane is busy waits
// for the first lane that frees ANYWHERE exactly as an image call does: one file per request, one answer that says per node why
// not, the arrival time kept in this machine's own line, a job that is adopted when its owner dies, and the claim file that keeps
// two processes of a machine from putting two graphs on one node. What differs is what makes the call the same call (the graph's
// CONTENT, not the path it was read from), who may take it (the graph rules of ADR 0082 Amendment 1), and how a node's "I do not
// hold this file" is read (a refusal of THIS ticket at the node's door, not a full node). Everything below runs against real
// fleet-node servers and the real ticket files, as queue_test.go does for images.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// gEnv is a delegator whose own lane is held and a roster of single-card graph nodes, each busy or idle at the test's say.
type gEnv struct {
	t     *testing.T
	cfg   config.Config
	nodes []*node
	busy  []*atomic.Bool
	lr    *keeperRunner
}

func newGEnv(t *testing.T) *gEnv { return &gEnv{t: t, lr: newKeeperRunner(heldLane())} }

// node adds a node that holds files, busy or idle to start with.
func (e *gEnv) node(id string, busy bool, o nodeOpts, files ...string) *node {
	b := &atomic.Bool{}
	b.Store(busy)
	o.lease = toggled(b)
	n := startGraphNodeWith(e.t, id, o, files...)
	e.nodes = append(e.nodes, n)
	e.busy = append(e.busy, b)
	return n
}

// start makes the delegator once the roster is complete.
func (e *gEnv) start() *gEnv {
	e.cfg = graphClient(e.t, e.nodes...)
	return e
}

func (e *gEnv) free(i int) { e.busy[i].Store(false) }
func (e *gEnv) hold(i int) { e.busy[i].Store(true) }

func (e *gEnv) req(graph string, extra map[string]any) core.Request {
	return graphReqOf(e.t, graph, extra)
}

// resend is the request again the way a second call would build it: the same content, read from a file of its own (what an
// inline graph_json becomes at the MCP door), with the same out_dir unless extra says otherwise.
func resend(e *gEnv, req core.Request, graph string, extra map[string]any) core.Request {
	p := map[string]any{"out_dir": req.Params["out_dir"]}
	for k, v := range extra {
		p[k] = v
	}
	return e.req(graph, p)
}

// graphAnswer is the answer of a graph call whose window ended: the ticket's.
func graphAnswer(t *testing.T, res core.Result) answer {
	t.Helper()
	return answerOf(t, res)
}

// ---- identity -------------------------------------------------------------------------------------------------------------

// A graph call whose lane is busy is a TICKET, and the same graph sent again attaches to it however it reaches this machine:
// with no token, with its token, with a place token of the local line, and read from a file of its own. That last is the MCP
// door's graph_json, which is written to a temp file with a name of its own on every call: a key made of the path would make
// every resend a new job. What the graph, its manifest and its parameters say is what decides identity.
func TestABusyGraphIsATicketAndTheSameGraphAttachesToIt(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	req := e.req(krea2Graph, nil)

	res := runDoor(e.cfg, e.lr, req, AtOnce)
	first := graphAnswer(t, res)
	if first.Position != 1 || len(first.Cluster) != 1 || first.Cluster[0].State != "busy" || !strings.Contains(first.Cluster[0].Why, "bench render") {
		t.Fatalf("the answer says where the ticket stands and why: %+v", first)
	}
	if first.Local == nil || first.Local.State != "busy" {
		t.Errorf("the answer says what this machine's lane is doing: %+v", first.Local)
	}
	if _, runs := e.lr.counts(); runs != 0 {
		t.Errorf("a ticket does not run the graph in this machine's line while it waits: %d runs", runs)
	}
	if rows := Queue(e.cfg); len(rows) != 1 || rows[0].Task != string(core.TaskRunGraph) || rows[0].State != stWaiting || !rows[0].Place {
		t.Errorf("offload_status lists the graph ticket, holding a place in the local line: %+v", rows)
	}

	withToken := func(tok string) core.Request {
		return resend(e, req, krea2Graph, map[string]any{"waiter_token": tok})
	}
	for name, r := range map[string]core.Request{
		"the same graph from a file of its own": resend(e, req, krea2Graph, nil),
		"with the ticket's token":               withToken(first.Ticket),
		"with a place token of the local line":  withToken("tk-abc12345"),
		"the very same request":                 req,
	} {
		if a := graphAnswer(t, runDoor(e.cfg, e.lr, r, AtOnce)); a.Ticket != first.Ticket {
			t.Errorf("%s: want ticket %s, got %s", name, first.Ticket, a.Ticket)
		}
	}
	if files := ticketFiles(t, e.cfg); len(files) != 1 {
		t.Errorf("five sends of one graph are one ticket: %v", files)
	}
	if leaves := e.lr.leaveCalls(); len(leaves) != 1 {
		t.Errorf("a resend that attaches leaves no second place in the line: %d", len(leaves))
	}

	// What the graph asks for decides identity: its content, its manifest, where the outputs go, the steering of the placement.
	manifest := writeFile(t, t.TempDir(), "m.json", []byte(`{"models":[]}`))
	other := map[string]core.Request{
		"another graph":        resend(e, req, plainGraph, nil),
		"another out_dir":      e.req(krea2Graph, nil),
		"a manifest":           resend(e, req, krea2Graph, map[string]any{"manifest_path": manifest}),
		"another min_vram_gb":  resend(e, req, krea2Graph, map[string]any{"min_vram_gb": 4}),
		"another model_family": resend(e, req, krea2Graph, map[string]any{"model_family": "ltx25"}),
		"another reserve_vram": resend(e, req, krea2Graph, map[string]any{"reserve_vram": "0.5"}),
	}
	seen := map[string]bool{first.Ticket: true}
	for name, r := range other {
		a := graphAnswer(t, runDoor(e.cfg, e.lr, r, AtOnce))
		if seen[a.Ticket] {
			t.Errorf("%s must be its own ticket, got %s again", name, a.Ticket)
		}
		seen[a.Ticket] = true
	}
	if files := ticketFiles(t, e.cfg); len(files) != 1+len(other) {
		t.Errorf("%d different graphs, %d tickets: %v", 1+len(other), len(ticketFiles(t, e.cfg)), files)
	}

	// A manifest is its content too: the same manifest from a file of its own attaches, an edited one is another job.
	m1 := writeFile(t, t.TempDir(), "a.json", []byte(`{"models":[{"name":"x"}]}`))
	m2 := writeFile(t, t.TempDir(), "b.json", []byte(`{"models":[{"name":"x"}]}`))
	m3 := writeFile(t, t.TempDir(), "c.json", []byte(`{"models":[{"name":"y"}]}`))
	withManifest := graphAnswer(t, runDoor(e.cfg, e.lr, resend(e, req, krea2Graph, map[string]any{"manifest_path": m1}), AtOnce))
	if a := graphAnswer(t, runDoor(e.cfg, e.lr, resend(e, req, krea2Graph, map[string]any{"manifest_path": m2}), AtOnce)); a.Ticket != withManifest.Ticket {
		t.Errorf("the same manifest from another file is the same ticket: %s vs %s", a.Ticket, withManifest.Ticket)
	}
	if a := graphAnswer(t, runDoor(e.cfg, e.lr, resend(e, req, krea2Graph, map[string]any{"manifest_path": m3}), AtOnce)); a.Ticket == withManifest.Ticket {
		t.Errorf("another manifest is another ticket: %s", a.Ticket)
	}

	// A token names ITS request: sent with another graph it is refused by name, and nothing is made or collected.
	wrong := runDoor(e.cfg, e.lr, resend(e, req, plainGraph, map[string]any{"waiter_token": first.Ticket}), AtOnce)
	if wrong.OK || wrong.DeferClass != core.DeferClassContract || !strings.Contains(wrong.Reason, "different request") {
		t.Errorf("a token of another request is refused by name: %+v", wrong)
	}
}

// The key of a graph is its content, not the path: two files with one body are one request, an edited file is another, and the
// waiter_token is no part of it. A graph that cannot be read has no content to hash and keeps the path (the lane that runs it
// reports the contract), so a key can always be made.
func TestAGraphsKeyIsItsContent(t *testing.T) {
	dir := t.TempDir()
	a := core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": writeFile(t, dir, "a.json", []byte(krea2Graph)), "out_dir": "o"}}
	b := core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": writeFile(t, dir, "b.json", []byte(krea2Graph)), "out_dir": "o", "waiter_token": "mq-aaaaaaaa"}}
	if reqKey(a) != reqKey(b) {
		t.Error("one body in two files is one request, token or not")
	}
	before := reqKey(a)
	c := core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": writeFile(t, dir, "a.json", []byte(plainGraph)), "out_dir": "o"}}
	if before == reqKey(c) || before == reqKey(a) {
		t.Error("the same path with another body is another request")
	}
	layout := `{
  "4": {"class_type": "KSampler", "inputs": {"model": ["3", 0]}},
  "3": {"inputs": {"unet_name": "krea2_turbo_bf16.safetensors"}, "class_type": "UNETLoader"}
}`
	reformatted := core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": writeFile(t, dir, "r.json", []byte(layout)), "out_dir": "o"}}
	writeFile(t, dir, "a.json", []byte(krea2Graph))
	if reqKey(reformatted) != reqKey(a) {
		t.Error("the same graph with another layout and key order is the same request")
	}
	changed := core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": writeFile(t, dir, "n.json", []byte(`{"1":{"class_type":"KSampler","inputs":{"seed":2}}}`)), "out_dir": "o"}}
	unchanged := core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": writeFile(t, dir, "p.json", []byte(plainGraph)), "out_dir": "o"}}
	if reqKey(changed) == reqKey(unchanged) {
		t.Error("a changed number in the graph is another request")
	}
	missing := core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": filepath.Join(dir, "gone.json"), "out_dir": "o"}}
	other := core.Request{Task: core.TaskRunGraph, Params: map[string]any{"graph_path": filepath.Join(dir, "gone-too.json"), "out_dir": "o"}}
	if reqKey(missing) == reqKey(other) {
		t.Error("a graph that cannot be read keeps its path in the key")
	}
	img := core.Request{Task: core.TaskGenerateImage, Input: "p", Params: map[string]any{"graph_path": a.Params["graph_path"]}}
	if reqKey(img) == reqKey(core.Request{Task: core.TaskRunGraph, Input: "p", Params: map[string]any{"graph_path": a.Params["graph_path"]}}) {
		t.Error("the task is part of the key")
	}
}

// Two processes asking for the same graph at once make one ticket, as for an image.
func TestTwoProcessesAskingForOneGraphAtOnceMakeOneTicket(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	q1, q2 := manual(t)
	prev := firstScanBound
	firstScanBound = 20 * time.Millisecond
	t.Cleanup(func() { firstScanBound = prev })
	for round := 0; round < 15; round++ {
		out := filepath.Join(t.TempDir(), "outs")
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, q := range []*queueRT{q1, q2} {
			wg.Add(1)
			go func(q *queueRT) {
				defer wg.Done()
				req := e.req(krea2Graph, map[string]any{"out_dir": out}) // each process reads the graph from a file of its own
				<-start
				q.admit(context.Background(), e.cfg, e.lr, req, nil, Wait{Window: AtOnce, Detach: true})
			}(q)
		}
		close(start)
		wg.Wait()
		st, _ := storeFor(e.cfg)
		n := 0
		for _, tk := range st.list() {
			if tk.Out == out {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("round %d: two processes asking at once made %d tickets for one graph", round, n)
		}
	}
	// The process that lost the race made copies of the graph for a ticket that does not exist: they are gone, one per ticket remains.
	st, _ := storeFor(e.cfg)
	if tickets, copies := len(st.list()), len(queueFiles(t, e.cfg, "body-")); copies != tickets {
		t.Errorf("%d tickets, %d graph copies: the loser of each race removed its own", tickets, copies)
	}
}

// ---- late binding ---------------------------------------------------------------------------------------------------------

// Nothing is sent anywhere until a lane is free there; when one frees the graph starts on it within a scan, with no call from
// the caller, and the result says which node ran it and what held this machine's lane. The harness gives a graph no seed (the
// graph owns its own), puts no family or digest on the wire, and the claim and the ticket are gone once the result is taken.
func TestAGraphTicketRunsOnTheFirstNodeThatFrees(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.node("node-b", true, nodeOpts{}, krea2File)
	e.start()
	req := e.req(krea2Graph, map[string]any{"reserve_vram": "0.5"})
	graphAnswer(t, runDoor(e.cfg, e.lr, req, AtOnce))
	time.Sleep(200 * time.Millisecond) // several scans
	noPosts(t, "node-a (busy)", e.nodes[0])
	noPosts(t, "node-b (busy)", e.nodes[1])

	e.free(1)
	res := settle(t, e.cfg, e.lr, req, func(r core.Result) bool { return r.OK })
	if res.Meta.Node != "node-b" {
		t.Fatalf("the graph ran on the node that freed: %+v", res.Meta)
	}
	if !strings.Contains(res.Meta.Placement, "this machine's graph lane is not free") || !strings.Contains(res.Meta.Placement, "bench render of the spring set") || !strings.Contains(res.Meta.Placement, "node-b") {
		t.Errorf("the placement says where the graph ran and what held the local lane: %q", res.Meta.Placement)
	}
	noPosts(t, "node-a, still busy", e.nodes[0])
	if ps := e.nodes[1].posts(); len(ps) != 1 || ps[0].path != "/fleet/dispatch" {
		t.Fatalf("one tokenless dispatch: %+v", ps)
	}
	p := graphPayload(t, e.nodes[1])
	if string(p["graph"]) != krea2Graph || string(p["reserve_vram"]) != `"0.5"` {
		t.Errorf("the graph travels inline as it is: %v", p)
	}
	for _, k := range []string{"seed", "family", "recipe_digest", "out_dir", "min_vram_gb", "devices", "waiter_token"} {
		if _, has := p[k]; has {
			t.Errorf("%s must not travel with a graph: %v", k, p)
		}
	}
	if b, err := os.ReadFile(filepath.Join(req.Params["out_dir"].(string), "graph-b.mp4")); err != nil || string(b) != "GB" {
		t.Errorf("the secondary output lands in out_dir, hash-checked: %q %v", b, err)
	}
	if _, unverified := decode(t, res)["unverified"]; unverified {
		t.Errorf("the node published the files' hashes, so the result is verified: %s", res.Data)
	}
	if _, runs := e.lr.counts(); runs != 0 {
		t.Errorf("the graph did not also run here: %d", runs)
	}
	if n := len(ticketFiles(t, e.cfg)); n != 0 || len(queueFiles(t, e.cfg, "use-")) != 0 {
		t.Errorf("a collected ticket and its claim are gone: %d tickets", n)
	}
}

// ---- seniority in this machine's own line ---------------------------------------------------------------------------------

// A graph ticket keeps its place in this machine's line as an image ticket does: it is left once, with the call's arrival time,
// touched on every scan, and spent by the local run that finally serves the ticket through waiter_token. The local run is
// given no seed, and the place is let go when the ticket is done. A graph that arrives already holding a place of the local line
// brings it into the ticket: the place is resumed (its id and arrival), not left a second time.
func TestAGraphTicketKeepsItsPlaceInTheLocalLine(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	q, _ := manual(t)
	before := time.Now()
	lt := admitOn(t, q, e.cfg, e.lr, e.req(krea2Graph, nil))
	leaves := e.lr.leaveCalls()
	if len(leaves) != 1 || leaves[0].req.Task != core.TaskRunGraph || leaves[0].since.Before(before.Add(-time.Second)) || leaves[0].since.After(time.Now()) {
		t.Fatalf("one place, for the graph, with the arrival time of the call: %+v", leaves)
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
	e.lr.set(core.LaneVerdict{Free: true})
	q.cycle(context.Background())
	waitFor(t, 5*time.Second, "the ticket runs here", func() bool { return isDone(lt) })
	ran := e.lr.ranCalls()
	if len(ran) != 1 || ran[0].Params["waiter_token"] != place {
		t.Fatalf("the local run spends the place through waiter_token: %+v", ran)
	}
	if _, has := ran[0].Params["seed"]; has {
		t.Errorf("a graph is not given a seed by the queue: %v", ran[0].Params)
	}
	if e.lr.dropped(place) != 1 {
		t.Errorf("the place is let go when the ticket is done: %v", e.lr.drops)
	}
	noPosts(t, "node-a", e.nodes[0])
}

func TestAGraphThatArrivesWithAPlaceBringsItIntoTheTicket(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	arrived := time.Now().Add(-40 * time.Second)
	e.lr.placeFor("tk-abc12345", arrived)
	res := runDoor(e.cfg, e.lr, e.req(krea2Graph, map[string]any{"waiter_token": "tk-abc12345"}), AtOnce)
	a := graphAnswer(t, res)
	leaves := e.lr.leaveCalls()
	if len(leaves) != 1 || leaves[0].token != "tk-abc12345" || leaves[0].req.Task != core.TaskRunGraph {
		t.Fatalf("the place the graph held is resumed, not left again: %+v", leaves)
	}
	st, _ := storeFor(e.cfg)
	if tk, ok := st.read(a.Ticket); !ok || tk.Place != "tk-abc12345" || tk.Created != arrived.UnixMilli() {
		t.Errorf("the ticket holds the place the graph brought, and the arrival it left with: %+v (arrived %d)", tk, arrived.UnixMilli())
	}
	if _, runs := e.lr.counts(); runs != 0 {
		t.Errorf("the graph did not run in the local line behind its own place: %d", runs)
	}
}

// A box that keeps no places still queues a graph, and the answer says there is no seniority in its own line.
func TestAGraphTicketOnABoxWithoutPlacesSaysSo(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	e.lr.noPlace = core.ErrNoMediaPlace
	res := runDoor(e.cfg, e.lr, e.req(krea2Graph, nil), AtOnce)
	_ = graphAnswer(t, res)
	if !strings.Contains(res.Reason, "no seniority") {
		t.Errorf("the answer says the ticket holds no place in the box's own line: %s", res.Reason)
	}
}

// ---- owners that die ------------------------------------------------------------------------------------------------------

// The graph a process sent to a node and then died: the next identical call adopts the job. It polls the recorded job and
// fetches its outputs and never sends the graph again; the result says the job was taken over and names the graph lane; one
// ledger row is written and no card of its own is opened.
func TestADeadOwnersPlacedGraphIsAdopted(t *testing.T) {
	rig := newPairRig(t, true, "node-a")
	e := newGEnv(t)
	n := e.node("node-a", false, nodeOpts{}, krea2File)
	e.start()
	e.cfg.PairWorkloadsEnabled = true
	e.lr.rig = rig
	req := e.req(krea2Graph, nil)

	// The job the dead process sent: really on the node, finished, its outputs waiting to be fetched.
	pl, err := plan(req)
	if err != nil {
		t.Fatal(err)
	}
	jobID := "media-deadbeef00000002"
	body, _ := json.Marshal(map[string]any{"job_id": jobID, "task_type": pl.fleetTask, "payload": pl.payload})
	if err := post(context.Background(), e.cfg, n.srv.URL, "/fleet/dispatch", body); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the node finishes the job", func() bool {
		j, err := getJSON[jobWire](context.Background(), e.cfg, n.srv.URL+"/fleet/jobs/"+jobID)
		return err == nil && j.State == "done"
	})
	st, _ := storeFor(e.cfg)
	dead := ticketFile{ID: "mq-deadbeef0003", ReqKey: reqKey(req), Created: nowMs() - 60_000, Owner: deadOwner, Beat: nowMs() - 5000,
		Task: string(core.TaskRunGraph), Out: req.Params["out_dir"].(string), State: stPlaced, Node: "node-a", Base: n.srv.URL, Job: jobID,
		LaneWhy: "card(s) c2 held by a media-class lease"}
	if err := st.write(dead); err != nil {
		t.Fatal(err)
	}

	res := runDoor(e.cfg, e.lr, resend(e, req, krea2Graph, nil), callWindow)
	if !res.OK || res.Meta.Node != "node-a" {
		t.Fatalf("the next identical call adopts the job and brings its result: %+v", res)
	}
	if got := len(n.posts()); got != 1 {
		t.Errorf("the graph was sent once, by the process that died; the adopter only polls and fetches: %d POSTs", got)
	}
	if !strings.Contains(res.Meta.Placement, "taken over") || !strings.Contains(res.Meta.Placement, "graph lane") {
		t.Errorf("the result says the job was taken over, and from which lane: %q", res.Meta.Placement)
	}
	if b, err := os.ReadFile(filepath.Join(req.Params["out_dir"].(string), "graph-b.mp4")); err != nil || string(b) != "GB" {
		t.Errorf("the fetched output is in out_dir: %q %v", b, err)
	}
	if rows := rig.rows(); len(rows) != 1 || rows[0].Deferred {
		t.Errorf("the adopted call writes one ledger row: %+v", rows)
	}
	if rig.frameCount() != 0 {
		t.Errorf("the adopted call opens no card of its own: %d frames", rig.frameCount())
	}
	if len(ticketFiles(t, e.cfg)) != 0 || len(queueFiles(t, e.cfg, "use-")) != 0 {
		t.Errorf("a collected ticket and its claim are gone")
	}
	if _, runs := e.lr.counts(); runs != 0 {
		t.Errorf("the adopted graph did not run here: %d", runs)
	}
}

// ---- identity is decided at the node's door -------------------------------------------------------------------------------

// A node that does not hold a file the graph names answers 503 at its door, before any job exists. That is the node saying "not
// this graph", not "I am full": the ticket marks the node not-capable for ITSELF, in words that name the file, sets no pause on
// the node (another graph, or an image, is offered it at once), does not send it the graph again at every scan, and goes on to
// the next lane in the same breath. A node that frees is taken by the ticket as any other.
func TestAFileRefusalIsNotCapableForThatTicketAndNeverFull(t *testing.T) {
	e := newGEnv(t)
	lacks := e.node("node-a", false, nodeOpts{}, "models/vae/other.safetensors")
	holds := e.node("node-b", true, nodeOpts{}, krea2File)
	e.start()
	pinPlacerClock(t)
	req := e.req(krea2Graph, nil)
	graphAnswer(t, runDoor(e.cfg, e.lr, req, AtOnce))
	waitFor(t, 5*time.Second, "the graph is offered to the node that lacks the file", func() bool { return len(lacks.posts()) == 1 })
	time.Sleep(300 * time.Millisecond) // several scans

	if n := len(lacks.posts()); n != 1 {
		t.Errorf("a node that refused this graph is not sent it again at every scan: %d POSTs", n)
	}
	a := graphAnswer(t, runDoor(e.cfg, e.lr, req, AtOnce))
	row, ok := rowFor(a.Cluster, "node-a")
	if !ok || row.State != "not-capable" {
		t.Fatalf("a missing file makes the node not-capable for this ticket: %+v", row)
	}
	if !strings.Contains(row.Why, "krea2_turbo_bf16.safetensors") {
		t.Errorf("the row names the file: %+v", row)
	}
	for _, wrong := range []string{"full", "left alone", "asked to be left", "last dispatch"} {
		if strings.Contains(row.Why, wrong) {
			t.Errorf("a file refusal is not a busy node (%q): %+v", wrong, row)
		}
	}
	if pz, backed := placer.backedOff(normBase(lacks.srv.URL)); backed {
		t.Errorf("a file refusal sets no pause on the node: %+v", pz)
	}
	if r, ok := rowFor(a.Cluster, "node-b"); !ok || r.State != "busy" {
		t.Errorf("the other node is busy, and the ticket waits for it: %+v", r)
	}

	// For THAT ticket: another graph that the same node does hold is offered to it at once.
	otherGraph := `{"1":{"class_type":"VAELoader","inputs":{"vae_name":"other.safetensors"}}}`
	res := settle(t, e.cfg, e.lr, e.req(otherGraph, nil), func(r core.Result) bool { return r.OK })
	if res.Meta.Node != "node-a" {
		t.Errorf("a graph the node holds runs there with no pause in the way: %+v", res.Meta)
	}

	// The first graph goes to the node that frees, and the node that lacks the file never sees it twice.
	e.free(1)
	res = settle(t, e.cfg, e.lr, req, func(r core.Result) bool { return r.OK })
	if res.Meta.Node != "node-b" {
		t.Fatalf("the graph ran on the node that holds the file: %+v", res.Meta)
	}
	if n := len(holds.posts()); n != 1 {
		t.Errorf("one dispatch to the node that holds it: %d", n)
	}
	if n := len(lacks.posts()); n != 2 { // the refusal, and the other graph that it ran
		t.Errorf("the node that lacks the file saw this graph once (and the other graph): %d POSTs", n)
	}
}

// A refusal is a fact about the node on the day it was given: a ticket that waits for hours asks the node again after
// notCapableAgain (an operator who placed the file wants it found), and the row counts the time down.
func TestATicketAsksANodeThatRefusedItAgainAfterAWhile(t *testing.T) {
	e := newGEnv(t)
	lacks := e.node("node-a", false, nodeOpts{}, "models/vae/other.safetensors")
	e.node("node-b", true, nodeOpts{}, krea2File)
	e.start()
	now := pinPlacerClock(t)
	req := e.req(krea2Graph, nil)
	graphAnswer(t, runDoor(e.cfg, e.lr, req, AtOnce))
	waitFor(t, 5*time.Second, "the first refusal", func() bool { return len(lacks.posts()) == 1 })
	a := settle(t, e.cfg, e.lr, req, rowIs("node-a", "not-capable"))
	row, _ := rowFor(answerOf(t, a).Cluster, "node-a")
	if !strings.Contains(row.Why, "asks it again in 5m0s") {
		t.Errorf("the row says when the ticket asks again: %+v", row)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(lacks.posts()); n != 1 {
		t.Fatalf("not asked again inside the interval: %d POSTs", n)
	}

	// The interval passes (the node, which still lacks the file, answers the second ask as it did the first: a second POST is the
	// proof that the ticket asked) with no call from the caller.
	*now = now.Add(notCapableAgain + time.Second)
	waitFor(t, 5*time.Second, "the ticket asks the node again", func() bool { return len(lacks.posts()) == 2 })
}

// With the node's refusal first in the roster and a node that holds the file after it, the graph is not delayed by the
// refusal: it goes to the next node in the same scan chain, with no pause between the two.
func TestAFileRefusalMovesTheGraphOnAtOnce(t *testing.T) {
	e := newGEnv(t)
	lacks := e.node("node-a", false, nodeOpts{}, "models/vae/other.safetensors")
	e.node("node-b", false, nodeOpts{}, krea2File)
	e.start()
	pinPlacerClock(t)
	start := time.Now()
	res := runBounded(e.cfg, e.lr, e.req(krea2Graph, nil), "auto", nil)
	if !res.OK || res.Meta.Node != "node-b" {
		t.Fatalf("the node that holds the file ran it: %+v", res)
	}
	if n := len(lacks.posts()); n != 1 {
		t.Errorf("the refusing node was asked once: %d", n)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("a refusal at the door costs no wait (took %s)", took)
	}
}

// The refusal marker is the node's own word, and this client reads exactly that word.
func TestTheNodesRefusalWordIsTheOneTheClientReads(t *testing.T) {
	if refusalNotCapable != fleetnode.RefusalNotCapable {
		t.Errorf("the client reads %q and the node writes %q", refusalNotCapable, fleetnode.RefusalNotCapable)
	}
}

// ---- two processes, one node ----------------------------------------------------------------------------------------------

// Two processes of one machine, each with a graph ticket, both read one idle single-card node and both scan at once: the claim
// is taken under the directory lock, so the node is sent ONE graph. (The per-process hold of the one-shot path could not do
// that; the order rule is turned off so that only the claim stands between the two sends.) Each queue stands for one process, and a
// process has ONE scanner (scanLoop), which visits its tickets one after another: a queue's cycles are therefore run one after
// another here, the two queues against each other. Eight goroutines per queue made the same ticket be stepped by two cycles at once,
// which no process does: one step could write the ticket's file with the state it had before its claim (waiting, no node) while the
// other process judged that claim, the claim read as dead, and the node was sent two graphs (13 runs of 30 on one Windows box, with no
// product code involved). Ignoring a live holder in claimNode still makes this test fail.
func TestTwoProcessesNeverStackTwoGraphsOnOneNode(t *testing.T) {
	hold := make(chan struct{})
	e := newGEnv(t)
	n := e.node("node-a", false, nodeOpts{hold: hold}, krea2File)
	e.start()
	t.Cleanup(func() { close(hold) })
	q1, q2 := manual(t)
	prev := honourOrder
	honourOrder = false
	t.Cleanup(func() { honourOrder = prev })
	admitOn(t, q1, e.cfg, e.lr, e.req(krea2Graph, nil))
	admitOn(t, q2, e.cfg, e.lr, e.req(krea2Graph, nil))
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
	waitFor(t, 5*time.Second, "a graph reaches the node", func() bool { return len(n.posts()) >= 1 })
	time.Sleep(300 * time.Millisecond)
	if got := len(n.posts()); got != 1 {
		t.Fatalf("one idle single-card node was sent %d graphs by two processes", got)
	}
}

// The node a graph ticket has a job on is in use for every scanner of the machine: a second process sends it nothing and says
// whose job is there, and gets the node once the first job is done.
func TestAPlacedGraphMarksItsNodeInUseForTheOtherProcess(t *testing.T) {
	hold := make(chan struct{})
	e := newGEnv(t)
	n := e.node("node-a", false, nodeOpts{hold: hold}, krea2File)
	e.start()
	t.Cleanup(func() { close(hold) })
	q1, q2 := manual(t)
	ctx := context.Background()
	lt1 := admitOn(t, q1, e.cfg, e.lr, e.req(krea2Graph, nil))
	time.Sleep(5 * time.Millisecond)
	lt2 := admitOn(t, q2, e.cfg, e.lr, e.req(krea2Graph, map[string]any{"reserve_vram": "0.5"}))

	q1.cycle(ctx)
	waitFor(t, 5*time.Second, "the first graph reaches the node", func() bool { return len(n.posts()) == 1 && lt1.snapshot().State == stPlaced })
	q2.cycle(ctx)
	if got := len(n.posts()); got != 1 {
		t.Fatalf("the node is in use: the second process sent it a second graph (%d POSTs)", got)
	}
	rows := lt2.snapshot().Cluster
	if len(rows) != 1 || rows[0].State != "in-use" || !strings.Contains(rows[0].Why, lt1.t.ID) {
		t.Fatalf("the second ticket is told the node is in use, by which ticket: %+v", rows)
	}
	hold <- struct{}{}
	waitFor(t, 5*time.Second, "the first graph finishes", func() bool { return isDone(lt1) })
	q2.cycle(ctx)
	waitFor(t, 5*time.Second, "the second graph reaches the node", func() bool { return len(n.posts()) == 2 })
}

// ---- order across kinds ---------------------------------------------------------------------------------------------------

// A graph no longer bypasses the queue: a node that an OLDER image ticket of the machine matches is that ticket's, and the graph
// ticket that arrived after it leaves the node alone and says whose turn it is, even when the graph's process scans first.
func TestAGraphDoesNotTakeANodeAnOlderImageTicketIsFirstInLineFor(t *testing.T) {
	e := newQEnv(t, 1, true)
	stubLocalRoutes(t, configured("generate_image", mediacap.ImageFamilyRoute("qwen-image-2.1-fast"), "run_graph"))
	q1, q2 := manual(t)
	ctx := context.Background()
	img := admitOn(t, q1, e.cfg, e.lr, e.req("first", nil))
	time.Sleep(5 * time.Millisecond)
	gr := admitOn(t, q2, e.cfg, e.lr, graphReqOf(t, krea2Graph, nil))

	e.free(0)
	q2.cycle(ctx) // the graph's process scans first
	noPosts(t, "the node (an older image ticket has not been scanned: it is first in line)", e.nodes[0])
	rows := gr.snapshot().Cluster
	if len(rows) != 1 || rows[0].State != "free" || !strings.Contains(rows[0].Why, img.t.ID) {
		t.Fatalf("the graph says the node is idle and whose turn it is: %+v", rows)
	}
	q1.cycle(ctx)
	waitFor(t, 5*time.Second, "the older image renders first", func() bool { return isDone(img) })
	q2.cycle(ctx)
	waitFor(t, 5*time.Second, "then the graph", func() bool { return isDone(gr) })
	if got := len(e.nodes[0].posts()); got != 2 {
		t.Errorf("both reached the node, in order: %d POSTs", got)
	}
}

// ---- a graph that stays on this machine -----------------------------------------------------------------------------------

// A graph that reads an input file is never offered to a node (a node can match such a file only by name, so a same-named file of
// other bytes would run in its place), but it IS a ticket: one per request, a place in this machine's line, a position, an answer
// that says why the fleet is not asked, and the local run when this machine's lane frees. It reads no node at all.
func TestAGraphThatReadsAnInputFileIsALocalOnlyTicketThatKeepsItsPlace(t *testing.T) {
	e := newGEnv(t)
	n := e.node("node-a", false, nodeOpts{}, krea2File, "input/ref.png") // idle, and it holds a same-named file
	e.start()
	req := e.req(refGraph, nil)
	res := runDoor(e.cfg, e.lr, req, AtOnce)
	a := graphAnswer(t, res)
	if len(a.Cluster) != 0 {
		t.Errorf("no node was asked, so there is no per-node row: %+v", a.Cluster)
	}
	note, _ := decode(t, res)["fleet_note"].(string)
	for _, want := range []string{"ref.png", "by name", "did not offer the graph to the fleet"} {
		if !strings.Contains(note, want) || !strings.Contains(res.Reason, want) {
			t.Errorf("the answer says why the fleet is not asked (%q is missing): note %q reason %q", want, note, res.Reason)
		}
	}
	time.Sleep(300 * time.Millisecond) // several scans
	untouched(t, "the idle node that holds a same-named file", n)
	if leaves := e.lr.leaveCalls(); len(leaves) != 1 {
		t.Errorf("the ticket holds one place in this machine's line: %d", len(leaves))
	}
	if again := graphAnswer(t, runDoor(e.cfg, e.lr, resend(e, req, refGraph, nil), AtOnce)); again.Ticket != a.Ticket {
		t.Errorf("a resend is the same ticket: %s vs %s", again.Ticket, a.Ticket)
	}
	if rows := Queue(e.cfg); len(rows) != 1 || rows[0].Task != string(core.TaskRunGraph) || rows[0].Why == "" || !rows[0].Place {
		t.Errorf("offload_status lists it with its reason and its place: %+v", rows)
	}

	// This machine's lane frees: the ticket runs the graph here, through its place, and the fleet was never read.
	e.lr.set(core.LaneVerdict{Free: true})
	out := settle(t, e.cfg, e.lr, req, func(r core.Result) bool { return r.OK })
	if string(out.Data) != `{"local":true}` {
		t.Fatalf("the graph ran on this machine's lane: %s", out.Data)
	}
	ran := e.lr.ranCalls()
	if len(ran) != 1 || !strings.HasPrefix(str(ran[0].Params, "waiter_token"), "tk-") {
		t.Errorf("the local run spends the ticket's place: %+v", ran)
	}
	if _, has := ran[0].Params["seed"]; has {
		t.Errorf("a graph is not given a seed by the queue: %v", ran[0].Params)
	}
	untouched(t, "the node, still", n)
}

// A local-only ticket never moves, however long the lane stays busy and however many nodes would take the graph by name: the
// fleet is not read for it in any scan. It ends with the other tickets' ceiling and not before.
func TestALocalOnlyGraphTicketIsNeverMovedAndHoldsNoNodeAgainOthers(t *testing.T) {
	e := newGEnv(t)
	n := e.node("node-a", false, nodeOpts{}, krea2File, "input/ref.png")
	e.start()
	q1, q2 := manual(t)
	ctx := context.Background()
	local := admitOn(t, q1, e.cfg, e.lr, e.req(refGraph, nil))
	time.Sleep(5 * time.Millisecond)
	younger := admitOn(t, q2, e.cfg, e.lr, e.req(krea2Graph, nil)) // a graph with no input file, younger
	for i := 0; i < 5; i++ {
		q1.cycle(ctx)
	}
	if m := local.snapshot().Matches; len(m) != 0 {
		t.Errorf("a ticket that stays here matches no node, so it holds none against a younger one: %v", m)
	}
	q2.cycle(ctx)
	waitFor(t, 5*time.Second, "the younger graph runs on the node the local-only one cannot use", func() bool { return isDone(younger) })
	if len(n.posts()) != 1 {
		t.Errorf("the node ran the younger graph: %d POSTs", len(n.posts()))
	}
	if s := local.snapshot(); s.State != stWaiting || !s.Scanned || s.Position != 1 {
		t.Errorf("the local-only ticket still waits for this machine's lane, first in its line: %+v", s)
	}
}

// What a graph asks of a node (the default VRAM floor reads the graph's height) is read ONCE, when the ticket is made, and the ticket
// goes on with what it read. The ticket runs from its own copy of the graph (body-<id>-graph.json), which the caller's file can no
// longer take away, but the copy is a file in a directory other processes sweep and an operator may clear, and a floor that vanished
// with it would send a Wan graph to the 8 GB node it was kept off. So both files are removed here: removing the caller's alone cannot
// tell a need read once from a need read at every scan (the copy is still there to be read), which is how the mutant "re-read at
// every scan" came to survive this test once the ticket kept copies.
func TestAGraphsNeedIsReadOnceWhenTheTicketIsMade(t *testing.T) {
	e := newGEnv(t)
	small := e.node("node-small", true, nodeOpts{}, krea2File)
	e.start()
	req := e.req(graphWith(1280), map[string]any{"model_family": "wan22"})
	a := graphAnswer(t, runDoor(e.cfg, e.lr, req, AtOnce))
	if r, ok := rowFor(a.Cluster, "node-small"); !ok || r.State != "not-capable" || !strings.Contains(r.Why, "chosen") {
		t.Fatalf("the 8 GB node is kept off a Wan graph at 720p and above by the default floor: %+v", r)
	}
	st, _ := storeFor(e.cfg)
	ownCopy := st.bodyPath(a.Ticket, "graph")
	if _, err := os.Stat(ownCopy); err != nil {
		t.Fatalf("control: the ticket keeps its own copy of the graph, the file a scan would read the need from: %v", err)
	}
	for _, gone := range []string{req.Params["graph_path"].(string), ownCopy} {
		if err := os.Remove(gone); err != nil {
			t.Fatal(err)
		}
	}
	e.free(0)
	time.Sleep(300 * time.Millisecond) // several scans
	noPosts(t, "the 8 GB node (the floor was read when the ticket was made)", small)
	if rows := Queue(e.cfg); len(rows) != 1 || !strings.Contains(rows[0].Why, "chosen") {
		t.Errorf("the ticket still names the default floor: %+v", rows)
	}
}

// ---- the doors ------------------------------------------------------------------------------------------------------------

// The CLI's `run-graph` IS the wait: it blocks while every lane is busy, tells the human once per change of standing where the ticket
// is, returns the result when a lane frees, and a first Ctrl-C before anything was sent cancels the ticket and lets go of its place
// (a job already on a node cannot be recalled, ADR 0064). The same ticket is the MCP door's, which only answers inside its window.
func TestTheGraphCLIBlocksAndCancelsUndispatched(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	prevPoll := attachPoll
	attachPoll = time.Hour // the human is told by the ticket itself, not by a poll that might happen to see it
	t.Cleanup(func() { attachPoll = prevPoll })

	var lines []string
	var lm sync.Mutex
	done := make(chan core.Result, 1)
	go func() {
		done <- RunWith(context.Background(), e.cfg, e.lr, e.req(krea2Graph, nil), "auto", nil, Wait{Window: -1, Notify: func(s string) {
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
			t.Fatalf("the freed node ran the graph: %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the CLI never returned")
	}
	lm.Lock()
	if last := lines[len(lines)-1]; !strings.Contains(last, "started on") {
		t.Errorf("the human is told when it started: %v", lines)
	}
	lm.Unlock()

	// Ctrl-C before anything was sent cancels the ticket and lets go of its place.
	e.hold(0)
	ctx, cancel := context.WithCancel(context.Background())
	cdone := make(chan core.Result, 1)
	go func() { cdone <- RunWith(ctx, e.cfg, e.lr, e.req(plainGraph, nil), "auto", nil, Wait{Window: -1}) }()
	waitFor(t, 5*time.Second, "the second ticket exists", func() bool { return len(ticketFiles(t, e.cfg)) == 1 })
	place := ""
	for _, lc := range e.lr.leaveCalls() {
		place = lc.token
	}
	cancel()
	select {
	case res := <-cdone:
		if res.OK || !res.Deferred || !strings.Contains(res.Reason, "canceled before anything was sent") {
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
}

// An MCP door that ends does not end the graph's ticket: it outlives the call, and still runs when a lane frees.
func TestAGraphDoorThatEndsLeavesTheTicketRunning(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File)
	e.start()
	req := e.req(krea2Graph, nil)
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

// The delegator reads the node's refusal word and the sentence from the body it wrote, and nothing from a body it did not: a proxy's
// page, a body with another word, or a sentence of unbounded length (the body is the node's to write) makes a plain 503.
func TestPostReadsTheNodesRefusalWordAndABoundedSentence(t *testing.T) {
	long := "a" + strings.Repeat("é", 700) // two bytes each, after one: the cut at 600 bytes falls inside a character
	for name, c := range map[string]struct {
		status       int
		body         string
		refusal, why string
		maxWhy       int
	}{
		"the node's word and sentence":   {503, `{"status":"error","error":"the graph loads 1 file this node does not hold: a.safetensors","refusal":"not_capable"}`, "not_capable", "a.safetensors", 600},
		"another word":                   {503, `{"status":"error","error":"queue full","refusal":"other"}`, "other", "queue full", 600},
		"no word (a full node)":          {503, `{"status":"error","error":"queue full (3 jobs)"}`, "", "queue full", 600},
		"a proxy's page":                 {503, `<html>busy</html>`, "", "", 600},
		"a sentence of unbounded length": {503, `{"error":"` + long + `","refusal":"not_capable"}`, "not_capable", "aé", 603},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		err := post(context.Background(), config.Config{}, srv.URL, "/fleet/dispatch", []byte(`{}`))
		srv.Close()
		var pe *postError
		if !errors.As(err, &pe) {
			t.Fatalf("%s: want a postError, got %v", name, err)
		}
		if pe.status != c.status || pe.refusal != c.refusal || !strings.Contains(pe.detail, c.why) || len(pe.detail) > c.maxWhy {
			t.Errorf("%s: status %d refusal %q detail %q (%d bytes), want refusal %q containing %q within %d bytes", name, pe.status, pe.refusal, pe.detail, len(pe.detail), c.refusal, c.why, c.maxWhy)
		}
		if !utf8.ValidString(pe.detail) {
			t.Errorf("%s: the cut sentence splits a character: %q", name, pe.detail)
		}
	}
}

// ---- a lane the pipeline could not read -----------------------------------------------------------------------------------------

// A graph's lane that could not be READ (the card table is slowest to read exactly when a heavy render holds the card) is not a free
// lane for a graph either: the call is a ticket, the fleet is looked at in the same scan, and an idle matching node takes it. This
// is the loaded-box shape the live acceptance sets up; the pipeline half (the probe says Unjudged) is held in the pipeline package.
func TestAnUnreadLaneStillLooksAtTheFleetForAGraph(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", false, nodeOpts{}, krea2File) // idle
	e.start()
	e.lr.set(unjudgedLane())
	e.lr.local = core.Deferf("gpu queued: card(s) c2 held", "", core.Meta{ErrClass: core.ErrClassGPUQueued})
	res := runDoor(e.cfg, e.lr, e.req(krea2Graph, nil), callWindow)
	if !res.OK || res.Meta.Node != "node-a" {
		t.Fatalf("the idle node takes a graph whose lane could not be read: %+v", res)
	}
	if !strings.Contains(res.Meta.Placement, "could not be judged") || !strings.Contains(res.Meta.Placement, "graph lane") {
		t.Errorf("the placement says the graph lane could not be judged, not that it was busy: %q", res.Meta.Placement)
	}
	if _, runs := e.lr.counts(); runs != 0 {
		t.Errorf("no run on this machine's lane when a node took the graph: %d", runs)
	}
}

// With no node able to take it, a graph whose lane could not be read runs here, in the scan that found no node, as it always did.
func TestAnUnreadLaneWithNoNodeRunsAGraphHere(t *testing.T) {
	e := newGEnv(t)
	e.node("node-a", true, nodeOpts{}, krea2File) // busy
	e.start()
	e.lr.set(unjudgedLane())
	res := runDoor(e.cfg, e.lr, e.req(krea2Graph, nil), callWindow)
	if !res.OK || string(res.Data) != `{"local":true}` {
		t.Fatalf("the graph runs on this machine's lane: %+v", res)
	}
	if _, runs := e.lr.counts(); runs != 1 {
		t.Errorf("one run: %d", runs)
	}
}

// ---- claim and order across the two kinds, both directions -------------------------------------------------------------------
//
// The 0.180.0 integration fix round made the one-shot graph call take the ticket's per-node claim and yield to older waiting
// tickets, and tested it as a graph CALL beside image tickets. A graph is a ticket now (ADR 0083 Amendment 1), so these are the same
// guarantees held on the ticket path: one claim and one order for both kinds, whichever kind arrives first and whichever process scans
// first. (TestAGraphTicketLeavesANodeAnotherTicketHasAJobOn is the graph-leaves-an-image's-node direction of the claim.)

// An image ticket is never sent a node a graph ticket of the machine has a job on. The graph's job shows as a lease on the node only
// once the node admitted it, so in the first seconds the node reads idle: the claim the graph ticket holds is what tells the other
// process's scan the node is in use. Without the claim the node receives two POSTs.
func TestAnImageTicketNeverSendsANodeAGraphTicketHasAJobOn(t *testing.T) {
	hold := make(chan struct{})
	e := newQEnv(t, 1, false, func(_ int, o *nodeOpts) { o.hold = hold })
	stubLocalRoutes(t, configured("generate_image", mediacap.ImageFamilyRoute("qwen-image-2.1-fast"), "run_graph"))
	t.Cleanup(func() { close(hold) })
	q1, q2 := manual(t)
	ctx := context.Background()

	gr := admitOn(t, q1, e.cfg, e.lr, graphReqOf(t, krea2Graph, nil))
	q1.cycle(ctx)
	waitFor(t, 5*time.Second, "the graph reaches the node", func() bool { return len(e.nodes[0].posts()) == 1 && gr.snapshot().State == stPlaced })

	img := admitOn(t, q2, e.cfg, e.lr, e.req("an image", nil))
	q2.cycle(ctx)
	time.Sleep(300 * time.Millisecond) // a send that wrongly started would have reached the node by now
	if n := len(e.nodes[0].posts()); n != 1 {
		t.Fatalf("the node has a graph running and was sent %d jobs in all (image ticket %s)", n, img.t.ID)
	}
	rows := img.snapshot().Cluster
	if len(rows) != 1 || rows[0].State != "in-use" || !strings.Contains(rows[0].Why, gr.t.ID) {
		t.Fatalf("the image ticket is told the node is in use, by the graph ticket: %+v", rows)
	}
	if st := img.snapshot().State; st != stWaiting {
		t.Errorf("the image ticket keeps waiting: %s", st)
	}

	hold <- struct{}{} // the graph's job finishes
	waitFor(t, 30*time.Second, "the graph ends", func() bool { return isDone(gr) })
	if got := mustStore(t, e.cfg).claims(); len(got) != 0 {
		t.Errorf("a finished graph lets its node go: %+v", got)
	}
	q2.cycle(ctx)
	waitFor(t, 5*time.Second, "the node is the image ticket's once the graph is done", func() bool { return len(e.nodes[0].posts()) == 2 })
}

func mustStore(t *testing.T, cfg config.Config) *store {
	t.Helper()
	st, ok := storeFor(cfg)
	if !ok {
		t.Fatal("the test machine has a queue")
	}
	return st
}

// A graph ticket yields to the tickets of the machine that are older than it and still waiting, by the ticket's own rule: an older
// ticket is first in line for the nodes it matched in its last scan, and for every node while it has not been scanned. (The "not
// scanned yet" case is TestAGraphDoesNotTakeANodeAnOlderImageTicketIsFirstInLineFor.) Without the rule a graph that arrived last was
// sent a node a ticket had waited for.
func TestAGraphTicketLeavesANodeAnOlderTicketIsFirstInLineFor(t *testing.T) {
	t.Run("the older ticket matched the node at its last scan", func(t *testing.T) {
		e := newQEnv(t, 1, true) // busy: the image ticket waits and records the node it matches
		stubLocalRoutes(t, configured("generate_image", mediacap.ImageFamilyRoute("qwen-image-2.1-fast"), "run_graph"))
		q1, q2 := manual(t)
		ctx := context.Background()
		img := admitOn(t, q1, e.cfg, e.lr, e.req("an image", nil))
		q1.cycle(ctx)
		if m := img.snapshot().Matches; len(m) != 1 {
			t.Fatalf("the image ticket recorded the node it matches: %v", m)
		}
		time.Sleep(5 * time.Millisecond)
		gr := admitOn(t, q2, e.cfg, e.lr, graphReqOf(t, krea2Graph, nil))
		e.free(0) // the node frees; the image ticket's next scan has not come yet

		q2.cycle(ctx) // the graph's process scans first
		noPosts(t, "the node the older ticket is first in line for", e.nodes[0])
		rows := gr.snapshot().Cluster
		if len(rows) != 1 || rows[0].State != "free" || !strings.Contains(rows[0].Why, img.t.ID) || !strings.Contains(rows[0].Why, "first in line") {
			t.Fatalf("the graph's answer names the ticket whose turn it is: %+v", rows)
		}
		q1.cycle(ctx) // the older ticket's own scan then takes the node, as it would have without the graph
		waitFor(t, 5*time.Second, "the older image ticket is sent the node", func() bool { return len(e.nodes[0].posts()) >= 1 })
	})

	t.Run("an older ticket does not hold a node it cannot use", func(t *testing.T) {
		a := startImageNode(t, "node-a", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{lease: leased()})
		b := startGraphNode(t, "node-b", []string{krea2File}, 7.96) // holds the graph's file, binds no image family
		cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, a, b)
		stubLocalRoutes(t, configured("generate_image", mediacap.ImageFamilyRoute("qwen-image-2.1-fast"), "run_graph"))
		q1, q2 := manual(t)
		ctx := context.Background()
		lr := newKeeperRunner(heldLane())
		img := admitOn(t, q1, cfg, lr, overflowReq(t, "qwen-image-2.1-fast", nil))
		q1.cycle(ctx) // scanned: it matches node-a only, and node-a is busy
		if m := img.snapshot().Matches; len(m) != 1 {
			t.Fatalf("the image ticket matches one node: %v", m)
		}
		time.Sleep(5 * time.Millisecond)
		gr := admitOn(t, q2, cfg, lr, graphReqOf(t, krea2Graph, nil))
		q2.cycle(ctx)
		waitFor(t, 5*time.Second, "the graph goes to the node no older ticket matches", func() bool { return isDone(gr) })
		if res := gr.snapshot(); res.State != stDone && res.State != stPlaced {
			t.Errorf("the graph ticket ran: %+v", res)
		}
		if len(b.posts()) != 1 {
			t.Errorf("node-b took the graph: %d POSTs", len(b.posts()))
		}
		noPosts(t, "node-a", a)
	})
}

// The same rule the other way round: an image ticket that arrived after a waiting graph ticket yields to it.
func TestAnImageTicketLeavesANodeAnOlderGraphTicketIsFirstInLineFor(t *testing.T) {
	e := newQEnv(t, 1, true) // busy: the graph ticket waits and records the node it matches
	stubLocalRoutes(t, configured("generate_image", mediacap.ImageFamilyRoute("qwen-image-2.1-fast"), "run_graph"))
	q1, q2 := manual(t)
	ctx := context.Background()
	gr := admitOn(t, q1, e.cfg, e.lr, graphReqOf(t, krea2Graph, nil))
	q1.cycle(ctx)
	if m := gr.snapshot().Matches; len(m) != 1 {
		t.Fatalf("the graph ticket recorded the node it matches: %v", m)
	}
	time.Sleep(5 * time.Millisecond)
	img := admitOn(t, q2, e.cfg, e.lr, e.req("an image", nil))
	e.free(0)

	q2.cycle(ctx) // the image's process scans first
	noPosts(t, "the node the older graph ticket is first in line for", e.nodes[0])
	rows := img.snapshot().Cluster
	if len(rows) != 1 || rows[0].State != "free" || !strings.Contains(rows[0].Why, gr.t.ID) || !strings.Contains(rows[0].Why, "first in line") {
		t.Fatalf("the image ticket's answer names the graph ticket whose turn it is: %+v", rows)
	}
	q1.cycle(ctx)
	waitFor(t, 5*time.Second, "the older graph ticket is sent the node", func() bool { return len(e.nodes[0].posts()) >= 1 })
}

// A 503 for a graph that carries no refusal word is the node's own state (a full queue, a route that is not configured), not a file it
// lacks, which is refused WITH the word. The ticket does not mark such a node not-capable; the placer pauses it as it does for any
// refusal at the door, and the cause it keeps for the pause does not call the node full (the node may be anything but).
func TestAGraphsWordlessRefusalIsTheNodesStateAndNotAMissingFile(t *testing.T) {
	e := newGEnv(t)
	refuses := e.node("node-a", false, nodeOpts{}, krea2File)
	e.node("node-b", true, nodeOpts{}, krea2File) // busy: the ticket has a reason to wait
	e.start()
	refuses.refusePost = 503
	pinPlacerClock(t)
	req := e.req(krea2Graph, nil)
	graphAnswer(t, runDoor(e.cfg, e.lr, req, AtOnce))
	waitFor(t, 5*time.Second, "the graph is offered to the node that refuses", func() bool { return len(refuses.posts()) >= 1 })

	res := settle(t, e.cfg, e.lr, req, rowIs("node-a", "refused"))
	row, _ := rowFor(answerOf(t, res).Cluster, "node-a")
	if !strings.Contains(row.Why, "answered 503") {
		t.Errorf("the row says what the node answered: %+v", row)
	}
	if r, ok := rowFor(answerOf(t, res).Cluster, "node-a"); ok && r.State == "not-capable" {
		t.Errorf("a refusal with no word is not a missing file: %+v", r)
	}
	pz, backed := placer.backedOff(normBase(refuses.srv.URL))
	if !backed {
		t.Fatal("a refusal at the door pauses the node, as it always did")
	}
	if !strings.Contains(pz.cause, "at its door") || strings.Contains(pz.cause, "full") {
		t.Errorf("the cause kept for a graph's wordless 503 does not call the node full: %q", pz.cause)
	}
}
