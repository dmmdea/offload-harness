package mediaremote

// What the one-shot placement of a whole-node graph (S1's overflow, S4's graph rules) guaranteed, held on the ticket that replaced
// it (ADR 0083 Amendment 1). The one-shot path had a hold in the process (two graph calls of one process never stacked on one
// node), a read of the claim files, and a bound of three nodes per call. The hold became the claim file, which also stops two
// PROCESSES (the hold could not), the read became the ticket's own rule for every node, and the bound is gone with the one-shot
// call: a ticket waits for the next lane instead of giving up after three. The graph_ticket_test.go file holds the tests of
// what is new; these are the old guarantees, so that none of them is lost with the code that made them.

import (
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

// Two graph calls of one process that both read a node idle must not both send it a job: the second would sit in the node's own
// queue for its whole window and come back as a bounce. The node a graph has a job on is claimed for it, the second ticket
// leaves it alone and says whose job is there, and it gets the node when the first is done. (The test the one-shot path had for
// its in-process hold, on the claim that replaced it.)
func TestTwoGraphCallsOfOneProcessDoNotStackOnOneNode(t *testing.T) {
	hold := make(chan struct{})
	e := newGEnv(t)
	n := e.node("node-a", false, nodeOpts{hold: hold}, krea2File)
	e.start()
	t.Cleanup(func() { close(hold) })

	first := e.req(krea2Graph, nil)
	a := graphAnswer(t, runDoor(e.cfg, e.lr, first, AtOnce))
	waitFor(t, 10*time.Second, "the first call reaches the node", func() bool { return len(n.posts()) == 1 })

	second := e.req(krea2Graph, map[string]any{"reserve_vram": "0.5"})
	res := settle(t, e.cfg, e.lr, second, rowIs("node-a", "in-use"))
	time.Sleep(200 * time.Millisecond) // several scans
	if got := len(n.posts()); got != 1 {
		t.Fatalf("the node was sent a second job while it had one: %d POSTs (%+v)", got, res)
	}
	row, ok := rowFor(clusterOf(t, res), "node-a")
	if !ok || !strings.Contains(row.Why, a.Ticket) {
		t.Errorf("the answer says whose job is there: %+v", row)
	}

	hold <- struct{}{}
	if r := settle(t, e.cfg, e.lr, first, func(r core.Result) bool { return r.OK }); r.Meta.Node != "node-a" {
		t.Fatalf("the first call: %+v", r)
	}
	waitFor(t, 10*time.Second, "the second graph reaches the node once the first is done", func() bool { return len(n.posts()) == 2 })
	hold <- struct{}{}
	if r := settle(t, e.cfg, e.lr, second, func(r core.Result) bool { return r.OK }); r.Meta.Node != "node-a" {
		t.Fatalf("the second call: %+v", r)
	}
}

// A graph is no longer sent to at most three nodes: it waits for the next lane. Four nodes that each take the graph and pass it
// back (another job holds their card) leave it waiting, and the fifth, which serves it, gets it; each of the four saw it once.
// (The bound of three kept a call from parking for every bouncing node's whole window; a ticket parks nowhere, because a node
// that passed it back is left alone and the scan looks at the next.)
func TestABouncingGraphIsNotBoundedToThreeNodes(t *testing.T) {
	e := newGEnv(t)
	for i, id := range []string{"node-a", "node-b", "node-c", "node-d", "node-e"} {
		o := nodeOpts{}
		if i < 4 {
			o.deferAs, o.deferClass = "gpu busy: card(s) held by a media-class lease", core.ErrClassGPUBusy
		}
		e.node(id, false, o, krea2File)
	}
	e.start()
	pinPlacerClock(t)
	res := runBounded(e.cfg, e.lr, e.req(krea2Graph, nil), "auto", nil)
	if !res.OK || res.Meta.Node != "node-e" {
		t.Fatalf("the fifth node serves it: %+v", res)
	}
	for i := 0; i < 4; i++ {
		if n := len(e.nodes[i].posts()); n != 1 {
			t.Errorf("node %d was sent the graph once: %d", i, n)
		}
	}
	if _, runs := e.lr.counts(); runs != 0 {
		t.Errorf("the graph never ran here: %d", runs)
	}
}

// A node a ticket of this machine has a job on is in use, whatever its health says yet (a job shows as a lease only once the node
// admitted it). A graph leaves it to that ticket and says so; it reads the claim and never writes one.
func TestAGraphTicketLeavesANodeAnotherTicketHasAJobOn(t *testing.T) {
	e := newGEnv(t)
	inUse := e.node("node-a", false, nodeOpts{}, krea2File)
	idle := e.node("node-b", false, nodeOpts{}, krea2File)
	e.start()
	st, _ := storeFor(e.cfg)
	tk := ticketFile{ID: "mq-aaaaaaaaaaaa", ReqKey: "k", Created: nowMs(), Owner: selfOwner(), Beat: nowMs(), State: stWaiting}
	if got, _, err := st.claimNode(&tk, inUse.srv.URL, "node-a", stPlaced); err != nil || !got {
		t.Fatalf("the ticket claims the node: %v %v", got, err)
	}

	res := runBounded(e.cfg, e.lr, e.req(krea2Graph, nil), "auto", nil)
	if !res.OK || res.Meta.Node != "node-b" {
		t.Fatalf("the graph goes to the node no ticket is using: %+v", res)
	}
	noPosts(t, "the node a ticket has a job on", inUse)
	if len(idle.posts()) != 1 {
		t.Errorf("the other node ran it: %d POSTs", len(idle.posts()))
	}
	if c := st.claims()[normBase(inUse.srv.URL)]; c.Ticket != tk.ID {
		t.Errorf("a graph neither takes nor changes the claim of another ticket: %+v", c)
	}

	// With only that node on the roster the graph waits, and the row says whose the node is.
	only := newGEnv(t)
	busyNode := only.node("node-a", false, nodeOpts{}, krea2File)
	only.start()
	st2, _ := storeFor(only.cfg)
	tk2 := ticketFile{ID: "mq-bbbbbbbbbbbb", ReqKey: "k2", Created: nowMs(), Owner: selfOwner(), Beat: nowMs(), State: stWaiting}
	if got, _, err := st2.claimNode(&tk2, busyNode.srv.URL, "node-a", stPlaced); err != nil || !got {
		t.Fatalf("the ticket claims the node: %v %v", got, err)
	}
	a := graphAnswer(t, runDoor(only.cfg, only.lr, only.req(krea2Graph, nil), AtOnce))
	row, ok := rowFor(a.Cluster, "node-a")
	if !ok || row.State != "in-use" || !strings.Contains(row.Why, tk2.ID) {
		t.Errorf("the row names the ticket that uses the node: %+v", row)
	}
	noPosts(t, "the claimed node", busyNode)
}
