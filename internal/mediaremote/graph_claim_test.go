package mediaremote

// The claim a whole-node graph call takes on a node (ticket.go: claimGraph), at the store: it excludes a ticket and another graph
// under the one directory lock, it is judged by its owner alone, and it lets the node go when its owner is gone for good.

import (
	"encoding/json"
	"sync"
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

// A graph and a ticket contending for one node at the same instant, from two processes (two queues over one state dir and a
// graph call that has no placer memory of the ticket): exactly one of them is sent. The claim is taken under the directory
// lock, so the read and the write are one step for both kinds.
func TestAGraphAndATicketNeverHoldOneNodeTogether(t *testing.T) {
	st := &store{dir: t.TempDir()}
	const base = "http://node-x.test:1"
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		holders []string
	)
	for round := 0; round < 40; round++ {
		holders = holders[:0]
		start := make(chan struct{})
		tk := ticketFile{ID: "mq-" + randHex(6), ReqKey: "k", Created: nowMs(), Owner: selfOwner(), Beat: nowMs(), State: stWaiting}
		gid := graphClaimID()
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if ok, _, err := st.claimNode(&tk, base, "node-x", stPlacing); err == nil && ok {
				mu.Lock()
				holders = append(holders, "ticket")
				mu.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if ok, _, err := st.claimGraph(gid, base); err == nil && ok {
				mu.Lock()
				holders = append(holders, "graph")
				mu.Unlock()
			}
		}()
		close(start)
		wg.Wait()
		if len(holders) != 1 {
			t.Fatalf("round %d: %d holders of one node: %v", round, len(holders), holders)
		}
		st.releaseNode(base, tk.ID)
		st.releaseNode(base, gid)
		if got := st.claims(); len(got) != 0 {
			t.Fatalf("round %d: both let go, the node is free: %+v", round, got)
		}
	}
}

// A graph's claim holds its node while its process lives, and for the dead-owner grace after the process ends (the crash window
// that matters is the seconds right after the send, before the node's own lease shows). Nothing else keeps it: a claim whose
// owner is dead and past the grace holds nothing, and the sweep takes it away.
func TestAGraphClaimHoldsWhileItsOwnerLivesAndForTheGraceAfter(t *testing.T) {
	st := &store{dir: t.TempDir()}
	const base = "http://node-y.test:2"
	prevAlive := ownerAliveFn
	alive := true
	ownerAliveFn = func(ownerID) bool { return alive }
	t.Cleanup(func() { ownerAliveFn = prevAlive })

	gid := graphClaimID()
	if ok, _, err := st.claimGraph(gid, base); err != nil || !ok {
		t.Fatalf("the graph claims an idle node: %v %v", ok, err)
	}
	if ok, h, _ := st.claimGraph(graphClaimID(), base); ok || h.Ticket != gid || h.Kind != claimKindGraph {
		t.Fatalf("a second graph is refused and told whose the node is: %v %+v", ok, h)
	}
	tk := ticketFile{ID: "mq-aaaaaaaaaaaa", ReqKey: "k", Created: nowMs(), Owner: selfOwner(), Beat: nowMs(), State: stWaiting}
	if ok, h, _ := st.claimNode(&tk, base, "node-y", stPlacing); ok || h.Ticket != gid {
		t.Fatalf("a ticket is refused too: %v %+v", ok, h)
	}
	if got := st.claims()[normBase(base)]; got.Ticket != gid {
		t.Fatalf("the claim is live while its owner lives: %+v", got)
	}

	alive = false // the process ended just now
	if got := st.claims()[normBase(base)]; got.Ticket != gid {
		t.Errorf("a dead owner's claim holds the node for the grace: %+v", got)
	}

	// The same claim, written long ago by a process that is gone, holds nothing.
	old := claim{Base: normBase(base), Ticket: gid, Kind: claimKindGraph, Owner: selfOwner(), At: nowMs() - (deadClaimGrace + time.Minute).Milliseconds()}
	writeClaimForTest(t, st, old)
	if got := st.claims(); len(got) != 0 {
		t.Errorf("a dead owner's claim past the grace holds nothing: %+v", got)
	}
	if ok, _, err := st.claimGraph(graphClaimID(), base); err != nil || !ok {
		t.Errorf("the node is free again for the next graph: %v %v", ok, err)
	}
	alive = true
	if ok, _, _ := st.claimGraph(graphClaimID(), base); ok {
		t.Error("the new graph's claim is live")
	}
	st.sweep(nil)
	if got := st.claims(); len(got) != 1 {
		t.Errorf("the sweep keeps a live graph's claim: %+v", got)
	}
}
