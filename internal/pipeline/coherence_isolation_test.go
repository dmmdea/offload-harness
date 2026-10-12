package pipeline

// The incoherent-seat memo (coherence.go) is one table for the whole test process: a broken verdict is filed under the endpoint and
// the seat that earned it and kept for coherenceMemoTTL. A fake server's address is a loopback port the OS may hand to a later fake,
// so a test that earned a broken verdict and left it behind could meet it in an unrelated test that took the WARM path, which defers
// on a remembered verdict without asking the seat anything. agentFake.server drops what its server earned when the test that asked
// for it ends; this file holds that rule and the helper it uses.

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// forgetCoherenceOf drops every incoherent-seat verdict filed under one endpoint, whichever seat it names: forgetCoherence takes one
// exact seat, and a fake server can serve another through agentFake.seatName. The endpoint is matched as a whole (with or without a
// path after it), so a server on :1234 never takes the entries of one on :12345 with it.
func forgetCoherenceOf(endpoint string) {
	coherenceMemo.Lock()
	defer coherenceMemo.Unlock()
	for key := range coherenceMemo.broken {
		ep, _, found := strings.Cut(key, " seat=")
		if found && (ep == endpoint || strings.HasPrefix(ep, endpoint+"/")) {
			delete(coherenceMemo.broken, key)
		}
	}
}

// A test that earns a broken verdict on its fake server (a cold load that answers the probe with token-0 spam, the shape
// TestRunAgentTaskDefersAnIncoherentSeatAfterColdLoad and TestNoWallIsReportedUntilTheWallStarts both produce) must not leave it
// behind. The subtest runs the cold load, so its server's cleanup has run by the time t.Run returns; the checks after it are about
// what is left. The verdict is asserted to be there INSIDE the subtest, because "nothing remembered afterwards" proves nothing about
// a run that never remembered anything.
func TestAnIncoherentSeatVerdictDoesNotOutliveTheTestThatEarnedIt(t *testing.T) {
	broken := CoherenceVerdict{Ran: true, Broken: true, Note: core.IncoherentSeatReason + "spam"}
	var url string
	t.Cleanup(func() { forgetCoherence(url+"0", agentTestSeat) }) // the neighbour planted below, whatever happens
	t.Run("a cold load that answers with spam", func(t *testing.T) {
		fake := coldLoadFake(func(int64) string { return degenerateChat() })
		srv := fake.server(t)
		defer srv.Close()
		url = srv.URL
		wire := decodeWire(t, coherenceTestPipeline(t, srv.URL, 30, "").Run(context.Background(), agentTestRequest(t, testContract())))
		if !wire.Deferred || !strings.HasPrefix(wire.Reason, core.IncoherentSeatReason) {
			t.Fatalf("test premise: the cold load must defer as an incoherent seat, got deferred=%v reason=%q", wire.Deferred, wire.Reason)
		}
		if _, ok := RecallIncoherentSeat(srv.URL, agentTestSeat); !ok {
			t.Fatal("test premise: the broken verdict is not remembered while the test runs, so the checks after it would pass vacuously")
		}
		// A fake that serves another seat files its verdict under the same endpoint with that seat's name...
		RememberCoherence(srv.URL, "another-seat", broken)
		// ...and an address that only starts like this one is another server's, whose verdict stays.
		RememberCoherence(srv.URL+"0", agentTestSeat, broken)
	})
	for _, seat := range []string{agentTestSeat, "another-seat"} {
		if note, ok := RecallIncoherentSeat(url, seat); ok {
			t.Errorf("the verdict on seat %q outlived the test that earned it, and a later test on this address would defer on it: %q", seat, note)
		}
	}
	if _, ok := RecallIncoherentSeat(url+"0", agentTestSeat); !ok {
		t.Error("the cleanup also took the verdict of an endpoint that merely starts with this server's address")
	}
}
