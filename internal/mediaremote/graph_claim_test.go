package mediaremote

// The claim a 0.180.0 process wrote for a whole-node graph call (ticket.go: claimKindGraph), read by this release. A busy graph is a
// ticket now and takes a ticket's claim, so nothing in this release writes this kind; but the processes of one machine are not all
// restarted at a deploy (an MCP server keeps its process for days), and a 0.180.0 process that is running a graph on a node holds the
// node by this claim alone. A scanner that did not honour it would send the node a second job. The claim is judged by its owner alone,
// it excludes a ticket, and it lets the node go when its owner is gone for good.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// writeClaimForTest writes a claim record as a process that has since ended would have left it.
func writeClaimForTest(t *testing.T, st *store, c claim) {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(st.claimPath(c.Base), b); err != nil {
		t.Fatal(err)
	}
}

// A graph call's claim of an older release holds its node while its process lives, and, once its process is gone, for the dead-owner
// grace counted from the claim (the call writes no beat; the window that matters is the seconds right after the send, before the
// node's own lease shows). Nothing else keeps it: a claim whose owner is dead and older than the grace holds nothing, and the sweep
// takes it away. While it holds, a ticket's claim on the same node is refused and told whose the node is.
func TestAnOlderReleasesGraphClaimHoldsWhileItsOwnerLivesAndForTheGraceAfter(t *testing.T) {
	st := &store{dir: t.TempDir()}
	const base = "http://node-y.test:2"
	prevAlive := ownerAliveFn
	alive := true
	ownerAliveFn = func(ownerID) bool { return alive }
	t.Cleanup(func() { ownerAliveFn = prevAlive })

	const gid = "gr-aaaaaaaaaaaa"
	writeClaimForTest(t, st, claim{Base: normBase(base), Ticket: gid, Kind: claimKindGraph, Owner: selfOwner(), At: nowMs()})
	tk := ticketFile{ID: "mq-aaaaaaaaaaaa", ReqKey: "k", Created: nowMs(), Owner: selfOwner(), Beat: nowMs(), State: stWaiting}
	if ok, h, _ := st.claimNode(&tk, base, "node-y", stPlacing); ok || h.Ticket != gid || h.Kind != claimKindGraph {
		t.Fatalf("a ticket is refused the node and told whose it is: %v %+v", ok, h)
	}
	if got := st.claims()[normBase(base)]; got.Ticket != gid || !strings.Contains(got.words(), "graph call") {
		t.Fatalf("the claim is live while its owner lives, and its words name a graph call: %+v / %q", got, got.words())
	}

	alive = false // the process ended just now
	if got := st.claims()[normBase(base)]; got.Ticket != gid {
		t.Errorf("a dead owner's claim holds the node for the grace: %+v", got)
	}

	// The same claim, written long ago by a process that is gone, holds nothing.
	writeClaimForTest(t, st, claim{Base: normBase(base), Ticket: gid, Kind: claimKindGraph, Owner: selfOwner(), At: nowMs() - (deadClaimGrace + time.Minute).Milliseconds()})
	if got := st.claims(); len(got) != 0 {
		t.Errorf("a dead owner's claim past the grace holds nothing: %+v", got)
	}
	if ok, _, err := st.claimNode(&tk, base, "node-y", stPlacing); err != nil || !ok {
		t.Errorf("the node is free again for the next ticket: %v %v", ok, err)
	}

	// A live owner's claim survives the sweep; a dead one's, past the grace, does not.
	st.releaseNode(base, tk.ID)
	alive = true
	writeClaimForTest(t, st, claim{Base: normBase(base), Ticket: gid, Kind: claimKindGraph, Owner: selfOwner(), At: nowMs()})
	st.sweep(nil)
	if got := st.claims(); len(got) != 1 {
		t.Errorf("the sweep keeps a live graph call's claim: %+v", got)
	}
}

// The scan of a ticket honours it: an idle node that a 0.180.0 process is running a graph on is in use, whatever its health says yet
// (the job shows as a lease only once the node admitted it), the row says a graph call is there, and the node is the ticket's once the
// call lets it go.
func TestATicketNeverSendsANodeAnOlderReleasesGraphCallHasAJobOn(t *testing.T) {
	e := newQEnv(t, 1, false) // the lane is held, the node idle
	st, ok := storeFor(e.cfg)
	if !ok {
		t.Fatal("the test machine has a state dir")
	}
	base := e.nodes[0].srv.URL
	const gid = "gr-bbbbbbbbbbbb"
	writeClaimForTest(t, st, claim{Base: normBase(base), Ticket: gid, Kind: claimKindGraph, Owner: selfOwner(), At: nowMs()})
	q, _ := manual(t)
	ctx := context.Background()
	lt := admitOn(t, q, e.cfg, e.lr, e.req("an image", nil))
	q.cycle(ctx)
	time.Sleep(300 * time.Millisecond) // a send that wrongly started would have reached the node by now
	noPosts(t, "the node a graph call of an older release is running on", e.nodes[0])
	rows := lt.snapshot().Cluster
	if len(rows) != 1 || rows[0].State != "in-use" || !strings.Contains(rows[0].Why, "graph call") {
		t.Fatalf("the ticket is told the node is in use, by a graph call: %+v", rows)
	}
	if s := lt.snapshot().State; s != stWaiting {
		t.Errorf("the ticket keeps waiting: %s", s)
	}

	st.releaseNode(base, gid) // the older process's graph call ended
	q.cycle(ctx)
	waitFor(t, 5*time.Second, "the node is the ticket's once the graph call is done", func() bool { return len(e.nodes[0].posts()) == 1 })
}
