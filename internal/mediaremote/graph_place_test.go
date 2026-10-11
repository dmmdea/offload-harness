package mediaremote

// The busy-lane path of a whole-node graph in the release that carries both the ticket queue (ADR 0083) and the graph
// overflow (ADR 0082 Amendment 1). An image call has ONE busy-lane path, the ticket. A graph keeps the one-shot overflow
// and, when no node admits it, waits in this machine's own lane: it is not a ticket, it keeps no place in the cluster queue
// and is not idempotent by content. These tests pin that edge, and the three small pieces S2 removed from the image path that
// the graph path needs (the in-process hold, the bound of three nodes, and the machine-wide claim of a node). graph_ticket_test.go
// pins how a graph and the tickets share the nodes.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// startGraphNodeWith is startGraphNode with the node's other options (a held job, a deferring pipeline).
func startGraphNodeWith(t *testing.T, id string, o nodeOpts, files ...string) *node {
	t.Helper()
	tree := graphTreeWith(t, files...)
	o.id, o.version, o.healthEdit = id, fixtureVersion, withCards(7.96)
	o.cfg = func(c *config.Config) { c.ComfyDir = tree }
	return startNode(t, o)
}

// A graph whose lane is busy and that no node admits is not a ticket: it waits in the local lane exactly as 0.179.0 left it
// (one question, one run, the caller's request untouched), no ticket file is made, nothing is queued for the scanner, and a
// repeat of the same graph is a second call, not the first one's ticket. The graph does take a claim on the node it sends a job
// to (the claim file is not a ticket), and lets it go when the call ends.
func TestAGraphIsNotATicket(t *testing.T) {
	n := startGraphNode(t, "node-a", []string{"models/vae/other.safetensors"}, 7.96) // lacks the file: 503 at its door
	cfg := graphClient(t, n)
	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	req := graphReqOf(t, krea2Graph, nil)

	res := Run(context.Background(), cfg, lr, req, "auto", nil)
	if probes, runs := lr.counts(); probes != 1 || runs != 1 {
		t.Fatalf("the graph waits in its own lane once: probes %d runs %d (%+v)", probes, runs, res)
	}
	ran := lr.ranCalls()[0]
	if _, has := ran.Params["seed"]; has {
		t.Errorf("a graph is not given a seed by the queue: %v", ran.Params)
	}
	if tok := str(ran.Params, "waiter_token"); tok != "" {
		t.Errorf("the local run carries no ticket token: %q", tok)
	}
	if rows := clusterOf(t, res); len(rows) != 1 || rows[0].State != "refused" {
		t.Errorf("the answer says per node why not: %+v", rows)
	}
	if strings.Contains(string(res.Data), `"ticket"`) || strings.Contains(res.Reason, "mq-") {
		t.Errorf("the answer is the local lane's own, not a ticket's: %s / %s", res.Data, res.Reason)
	}
	if got := Queue(cfg); len(got) != 0 {
		t.Errorf("no ticket was made: %+v", got)
	}
	// The directory lock file stays for good (queuefs.go); no ticket and no claim is left behind.
	for _, prefix := range []string{"mq-", "use-"} {
		if files := queueFiles(t, cfg, prefix); len(files) != 0 {
			t.Errorf("nothing of the graph is left under the queue directory (%s*): %v", prefix, files)
		}
	}

	// The same graph again is a second call: a second question, a second run.
	Run(context.Background(), cfg, lr, req, "auto", nil)
	if probes, runs := lr.counts(); probes != 2 || runs != 2 {
		t.Errorf("a repeat of a graph is not attached to the first one: probes %d runs %d", probes, runs)
	}
}

// Two graph calls of one process that both read a node idle must not both send it a job: the second would wait out the node's
// whole window in the node's own queue and come back as a bounce. The node a graph has a job on is held for it, and the second
// goes on to its own lane and says why. (S1's test for the image path, kept for the path that still has the hold. Where the queue
// directory opens, which is every machine but one whose state root the lease refuses, the claim file answers first and the row says
// so, graph_ticket_test.go; the in-process hold is the whole of the cover on a machine without a queue, which is what this test runs
// on: its state root is one the lease refuses.)
func TestTwoGraphCallsDoNotStackOnOneNode(t *testing.T) {
	hold := make(chan struct{})
	n := startGraphNodeWith(t, "node-b", nodeOpts{hold: hold}, krea2File)
	t.Cleanup(func() { close(hold) })
	cfg := graphClient(t, n)
	cfg.StateDir = filepath.Join(t.TempDir(), "Google Drive", "state") // a cloud-synced root is refused: no queue directory, so no claim file, and only the in-process hold stands between the two calls

	first := make(chan core.Result, 1)
	go func() {
		first <- Run(context.Background(), cfg, &laneRunner{verdict: heldLane()}, graphReqOf(t, krea2Graph, nil), "auto", nil)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(n.posts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(n.posts()) != 1 {
		t.Fatal("the first call never reached the node")
	}

	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	// A bound on the second call, so that a node that WAS sent a second job (the node holds both until it is released below)
	// ends this test in seconds instead of hanging it.
	second, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	res := Run(second, cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
	if probes, runs := lr.counts(); probes != 1 || runs != 1 {
		t.Fatalf("the second graph waits in its own lane: probes %d runs %d (%+v)", probes, runs, res)
	}
	if len(n.posts()) != 1 {
		t.Errorf("the node was sent a second job while it had one: %d POSTs", len(n.posts()))
	}
	row, ok := rowFor(clusterOf(t, res), "node-b")
	if !ok || row.State != "busy" || !strings.Contains(row.Why, "another call of this machine") {
		t.Errorf("the answer says why: %+v", row)
	}

	hold <- struct{}{}
	select {
	case r := <-first:
		if !r.OK {
			t.Fatalf("the first call: %+v", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the first call never ended")
	}
	if !placer.hold(n.srv.URL) {
		t.Error("the node must be free for the next call once the first is done")
	}
	placer.release(n.srv.URL)
}

// A graph is sent to at most three nodes: a fourth idle node that would also bounce is named and not tried. Without the bound
// a roster of bouncing nodes parks the call for each node's whole window in turn.
func TestAGraphIsSentToAtMostThreeNodes(t *testing.T) {
	busy := nodeOpts{deferAs: "gpu busy: card(s) held by a media-class lease", deferClass: core.ErrClassGPUBusy}
	var nodes []*node
	for _, id := range []string{"node-a", "node-b", "node-c", "node-d"} {
		nodes = append(nodes, startGraphNodeWith(t, id, busy, krea2File))
	}
	cfg := graphClient(t, nodes...)
	pinPlacerClock(t)
	lr := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	res := Run(context.Background(), cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
	if probes, runs := lr.counts(); probes != 1 || runs != 1 {
		t.Fatalf("every node bounced, so the graph runs here once: probes %d runs %d", probes, runs)
	}
	total := 0
	for _, n := range nodes {
		total += len(n.posts())
	}
	if total != maxGraphPlacements {
		t.Fatalf("a graph is sent to at most %d nodes: %d POSTs in all", maxGraphPlacements, total)
	}
	noPosts(t, "the fourth node", nodes[3])
	rows := clusterOf(t, res)
	if len(rows) != 4 {
		t.Fatalf("one row per roster node: %+v", rows)
	}
	for i, want := range []string{"bounced", "bounced", "bounced", "skipped"} {
		if rows[i].State != want {
			t.Errorf("row %d (%s) = %s, want %s: %+v", i, rows[i].Node, rows[i].State, want, rows[i])
		}
	}
	if !strings.Contains(rows[3].Why, "at most 3 nodes") {
		t.Errorf("the fourth row says why it was not tried: %+v", rows[3])
	}
}

// A node a ticket of this machine has a job on is in use, whatever its health says yet (a job shows as a lease only once the
// node admitted it). A graph leaves it to that ticket and says so, and takes the claim of the node it does use only for the
// length of its call: the ticket's claim is neither taken nor changed.
func TestAGraphLeavesANodeATicketHasAJobOn(t *testing.T) {
	inUse := startGraphNode(t, "node-a", []string{krea2File}, 7.96)
	idle := startGraphNode(t, "node-b", []string{krea2File}, 7.96)
	cfg := graphClient(t, inUse, idle)

	st, ok := storeFor(cfg)
	if !ok {
		t.Fatal("the test machine has a state dir")
	}
	tk := ticketFile{ID: "mq-aaaaaaaaaaaa", ReqKey: "k", Created: nowMs(), Owner: selfOwner(), Beat: nowMs(), State: stWaiting}
	if got, _, err := st.claimNode(&tk, inUse.srv.URL, "node-a", stPlaced); err != nil || !got {
		t.Fatalf("the ticket claims the node: %v %v", got, err)
	}
	before := st.claims()

	lr := &laneRunner{verdict: heldLane()}
	res := Run(context.Background(), cfg, lr, graphReqOf(t, krea2Graph, nil), "auto", nil)
	if !res.OK || res.Meta.Node != "node-b" {
		t.Fatalf("the graph goes to the node no ticket is using: %+v", res)
	}
	noPosts(t, "the node a ticket has a job on", inUse)
	if after := st.claims(); len(after) != len(before) || after[normBase(inUse.srv.URL)].Ticket != tk.ID {
		t.Errorf("a graph neither takes over nor changes a ticket's claim, and lets its own go: before %+v after %+v", before, after)
	}

	// With only that node on the roster, the graph waits in its own lane and the row says whose the node is.
	only := graphClient(t, inUse)
	lr2 := &laneRunner{verdict: heldLane(), local: queuedLocal()}
	st2, _ := storeFor(only)
	tk2 := ticketFile{ID: "mq-bbbbbbbbbbbb", ReqKey: "k2", Created: nowMs(), Owner: selfOwner(), Beat: nowMs(), State: stWaiting}
	if got, _, err := st2.claimNode(&tk2, inUse.srv.URL, "node-a", stPlaced); err != nil || !got {
		t.Fatalf("the ticket claims the node: %v %v", got, err)
	}
	res2 := Run(context.Background(), only, lr2, graphReqOf(t, krea2Graph, nil), "auto", nil)
	if _, runs := lr2.counts(); runs != 1 {
		t.Fatalf("with no node free the graph runs in its own lane: %+v", res2)
	}
	row, ok := rowFor(clusterOf(t, res2), "node-a")
	if !ok || row.State != "in-use" || !strings.Contains(row.Why, tk2.ID) {
		t.Errorf("the row names the ticket that uses the node: %+v", row)
	}
}
