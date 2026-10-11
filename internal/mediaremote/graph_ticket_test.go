package mediaremote

// How a whole-node graph and the image tickets of one machine share its nodes (the release that carries both the ticket queue,
// ADR 0083, and the graph overflow, ADR 0082 Amendment 1). Neither branch tested the two together; the review of the merge
// found four ways they met badly, and these tests pin the answers:
//
//   - a node a graph has a job on is IN USE for every scanner of the machine, as a node a ticket has a job on is: the graph takes
//     the same claim, under the same directory lock, so no ticket and no other graph is sent a node the graph is running on;
//   - a graph does not take a node an OLDER waiting ticket of the machine is first in line for, by the ticket's own rule;
//   - a lane the pipeline tried to read and could not is not a free lane for a graph either: the fleet is looked at first;
//   - a node's refusal of a graph at its door is not called "full".

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediacap"
)

// newMixedEnv is a qEnv (image nodes that bind the qwen family and hold the shared weight files, one card each) whose
// delegator also has a graph lane: the same fleet serves image tickets and whole-node graphs.
func newMixedEnv(t *testing.T, n int, busy bool, tune ...func(i int, o *nodeOpts)) *qEnv {
	t.Helper()
	e := newQEnv(t, n, busy, tune...)
	stubLocalRoutes(t, configured("generate_image", "run_graph", mediacap.ImageFamilyRoute("qwen-image-2.1-fast")))
	return e
}

// waitPosts waits until the node has received n dispatches.
func waitPosts(t *testing.T, what string, nd *node, n int) {
	t.Helper()
	waitFor(t, 10*time.Second, what, func() bool { return len(nd.posts()) >= n })
}

// A ticket never sends a node a graph of this machine is running on. The graph's job shows as a lease on the node only once the
// node admitted it, so in the first seconds the node reads idle: the claim the graph holds is what tells the ticket's scan
// the node is in use. Before the graph took a claim, the ticket was sent a node that already had a job and the node received
// two POSTs (the review's experiment: [run-graph image-gen]).
func TestATicketNeverSendsANodeAGraphHasAJobOn(t *testing.T) {
	hold := make(chan struct{})
	e := newMixedEnv(t, 1, false, func(_ int, o *nodeOpts) { o.hold = hold })
	t.Cleanup(func() { close(hold) })
	q, _ := manual(t)
	ctx := context.Background()

	graphDone := make(chan core.Result, 1)
	go func() {
		graphDone <- Run(ctx, e.cfg, &laneRunner{verdict: heldLane()}, graphReqOf(t, krea2Graph, nil), "auto", nil)
	}()
	waitPosts(t, "the graph reaches the node", e.nodes[0], 1)

	lt := admitOn(t, q, e.cfg, e.lr, e.req("an image", nil))
	q.cycle(ctx)
	time.Sleep(300 * time.Millisecond) // a send that wrongly started would have reached the node by now
	if n := len(e.nodes[0].posts()); n != 1 {
		t.Fatalf("the node has a graph running and was sent %d jobs in all (ticket %s)", n, lt.t.ID)
	}
	rows := lt.snapshot().Cluster
	if len(rows) != 1 || rows[0].State != "in-use" || !strings.Contains(rows[0].Why, "graph") {
		t.Fatalf("the ticket is told the node is in use, by a graph call: %+v", rows)
	}
	if st := lt.snapshot().State; st != stWaiting {
		t.Errorf("the ticket keeps waiting: %s", st)
	}

	hold <- struct{}{} // the graph's job finishes
	select {
	case r := <-graphDone:
		if !r.OK {
			t.Fatalf("the graph: %+v", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the graph never ended")
	}
	st, _ := storeFor(e.cfg)
	if got := st.claims(); len(got) != 0 {
		t.Errorf("a finished graph lets its node go: %+v", got)
	}
	q.cycle(ctx)
	waitPosts(t, "the node is the ticket's once the graph is done", e.nodes[0], 2)
}

// Two graph calls from two processes never stack on one node: the claim file is the machine-wide cover, the in-process hold is
// only the first line. A second PROCESS is a placer with no memory (resetPlacer), so only the claim can keep it off.
func TestTwoProcessesNeverStackTwoGraphsOnOneNode(t *testing.T) {
	hold := make(chan struct{})
	n := startGraphNodeWith(t, "node-a", nodeOpts{hold: hold}, krea2File)
	t.Cleanup(func() { close(hold) })
	cfg := graphClient(t, n)

	first := make(chan core.Result, 1)
	go func() {
		first <- Run(context.Background(), cfg, &laneRunner{verdict: heldLane()}, graphReqOf(t, krea2Graph, nil), "auto", nil)
	}()
	waitPosts(t, "the first graph reaches the node", n, 1)

	resetPlacer() // the other process: it has never heard of the first graph
	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	bound, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	res := Run(bound, cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
	if probes, runs := lr.counts(); probes != 1 || runs != 1 {
		t.Fatalf("the second graph waits in its own lane: probes %d runs %d (%+v)", probes, runs, res)
	}
	if got := len(n.posts()); got != 1 {
		t.Errorf("the node was sent a second graph while it had one: %d POSTs", got)
	}
	row, ok := rowFor(clusterOf(t, res), "node-a")
	if !ok || row.State != "in-use" || !strings.Contains(row.Why, "graph") {
		t.Errorf("the answer says a graph call of this machine is running there: %+v", row)
	}
	hold <- struct{}{}
	select {
	case r := <-first:
		if !r.OK {
			t.Fatalf("the first graph: %+v", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the first graph never ended")
	}
}

// A graph yields to the tickets of the machine that are older than it and still waiting, by the ticket's own rule: an older
// ticket is first in line for the nodes it matched in its last scan, and for every node while it has not been scanned. Without
// this a graph that arrived last was sent a node a ticket had waited for.
func TestAGraphLeavesANodeAnOlderTicketIsFirstInLineFor(t *testing.T) {
	t.Run("the older ticket matched the node", func(t *testing.T) {
		e := newMixedEnv(t, 1, true) // busy: the ticket waits and records the node it matches
		q, _ := manual(t)
		ctx := context.Background()
		lt := admitOn(t, q, e.cfg, e.lr, e.req("an image", nil))
		q.cycle(ctx)
		if m := lt.snapshot().Matches; len(m) != 1 {
			t.Fatalf("the ticket recorded the node it matches: %v", m)
		}
		e.free(0) // the node frees; the ticket's next scan has not come yet

		lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
		res := Run(ctx, e.cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
		noPosts(t, "the node the older ticket is first in line for", e.nodes[0])
		if probes, runs := lr.counts(); probes != 1 || runs != 1 {
			t.Fatalf("the graph waits in its own lane: probes %d runs %d (%+v)", probes, runs, res)
		}
		row, ok := rowFor(clusterOf(t, res), "node-a")
		if !ok || row.State != "free" || !strings.Contains(row.Why, lt.t.ID) || !strings.Contains(row.Why, "first in line") {
			t.Errorf("the answer names the ticket whose turn it is: %+v", row)
		}
		// The ticket's own scan then takes the node, as it would have without the graph.
		q.cycle(ctx)
		waitPosts(t, "the older ticket is sent the node", e.nodes[0], 1)
	})

	t.Run("the older ticket has not been scanned yet", func(t *testing.T) {
		e := newMixedEnv(t, 1, false)
		q, _ := manual(t)
		lt := admitOn(t, q, e.cfg, e.lr, e.req("an image", nil)) // made, never scanned: first in line for every node
		lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
		res := Run(context.Background(), e.cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
		noPosts(t, "the only node (an older ticket has not been scanned)", e.nodes[0])
		if _, runs := lr.counts(); runs != 1 {
			t.Fatalf("the graph waits in its own lane: %+v", res)
		}
		if row, ok := rowFor(clusterOf(t, res), "node-a"); !ok || row.State != "free" || !strings.Contains(row.Why, lt.t.ID) {
			t.Errorf("the answer names the ticket whose turn it is: %+v", row)
		}
	})

	t.Run("an older ticket does not hold a node it cannot use", func(t *testing.T) {
		a := startImageNode(t, "node-a", "hidream-o1", map[string]string{"qwen-image-2.1": block(int8Ckpt, "")}, nodeOpts{lease: leased()})
		b := startGraphNode(t, "node-b", []string{krea2File}, 7.96) // holds the graph's file, binds no image family
		cfg := overflowClient(t, "krea2", map[string]string{"qwen-image-2.1-fast": block(int8Ckpt, "")}, a, b)
		stubLocalRoutes(t, configured("generate_image", "run_graph", mediacap.ImageFamilyRoute("qwen-image-2.1-fast")))
		q, _ := manual(t)
		ctx := context.Background()
		lr0 := newKeeperRunner(heldLane())
		req := overflowReq(t, "qwen-image-2.1-fast", nil)
		admitOn(t, q, cfg, lr0, req)
		q.cycle(ctx) // scanned: it matches node-a only
		lr := &laneRunner{verdict: heldLane()}
		res := Run(ctx, cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
		if !res.OK || res.Meta.Node != "node-b" {
			t.Fatalf("the graph goes to the node no older ticket matches: %+v", res)
		}
		noPosts(t, "node-a", a)
	})
}

// A lane the pipeline tried to read and could not (the card table does not read inside its bound, the lease directory does not
// open) is not a free lane for a graph either: the card table is slowest to read exactly when a heavy render holds the card,
// so a graph that took the unread lane for a free one would never leave the box. The fleet is looked at, and the graph runs on
// this machine's lane only when no node takes it.
func TestAGraphWithAnUnreadLaneStillLooksAtTheFleet(t *testing.T) {
	unread := core.LaneVerdict{Free: true, Unjudged: true,
		Why: "not judged: the card table could not be read (context deadline exceeded); the call reads it itself"}

	t.Run("a node that holds the files takes it", func(t *testing.T) {
		n := startGraphNode(t, "node-a", []string{krea2File}, 7.96)
		cfg := graphClient(t, n)
		lr := &laneRunner{verdict: unread}
		res := Run(context.Background(), cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
		if !res.OK || res.Meta.Node != "node-a" {
			t.Fatalf("the node serves the graph: %+v", res)
		}
		if _, runs := lr.counts(); runs != 0 {
			t.Errorf("a graph a node served is not also run here: %d runs", runs)
		}
		if !strings.Contains(res.Meta.Placement, "could not be judged") {
			t.Errorf("the placement says the lane could not be judged, not that it was busy: %q", res.Meta.Placement)
		}
	})

	t.Run("no node takes it, so it runs here", func(t *testing.T) {
		n := startGraphNode(t, "node-a", []string{"models/vae/other.safetensors"}, 7.96) // lacks the file: 503 at its door
		cfg := graphClient(t, n)
		lr := &laneRunner{verdict: unread}
		res := Run(context.Background(), cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
		if !res.OK {
			t.Fatalf("the local run is the answer: %+v", res)
		}
		if probes, runs := lr.counts(); probes != 1 || runs != 1 {
			t.Errorf("one question, one run here: probes %d runs %d", probes, runs)
		}
		if len(n.posts()) != 1 {
			t.Errorf("the node that lacks the file was asked once and refused at its door: %d POSTs", len(n.posts()))
		}
	})

	t.Run("a lane that is simply free runs here and asks the fleet nothing", func(t *testing.T) {
		n := startGraphNode(t, "node-a", []string{krea2File}, 7.96)
		cfg := graphClient(t, n)
		lr := &laneRunner{verdict: core.LaneVerdict{Free: true}}
		res := Run(context.Background(), cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
		if !res.OK {
			t.Fatalf("%+v", res)
		}
		if _, runs := lr.counts(); runs != 1 {
			t.Errorf("a free lane runs the graph here: %d runs", runs)
		}
		untouched(t, "the node", n)
	})
}

// A node that refuses a graph at its door because it lacks a file it names (a 503) is not full: the cause the placer keeps for
// the pause, which an image ticket's next scan reads as the node's row, must not say so.
func TestAGraphsRefusalAtTheDoorIsNotCalledAFullNode(t *testing.T) {
	n := startGraphNode(t, "node-a", []string{"models/vae/other.safetensors"}, 7.96)
	cfg := graphClient(t, n)
	pinPlacerClock(t)
	for i := 0; i < 2; i++ {
		lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
		res := Run(context.Background(), cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
		rows := clusterOf(t, res)
		if len(rows) != 1 {
			t.Fatalf("call %d: one row: %+v", i, rows)
		}
		if strings.Contains(rows[0].Why, "full") {
			t.Errorf("call %d: a missing-file refusal is not a full node: %q", i, rows[0].Why)
		}
		if i == 1 && !strings.Contains(rows[0].Why, "not offered a call") {
			t.Errorf("the second call finds the node paused: %q", rows[0].Why)
		}
	}
	if pz, backed := placer.backedOff(n.srv.URL); !backed || strings.Contains(pz.cause, "full") {
		t.Errorf("the pause's cause: %+v", pz)
	}
}
