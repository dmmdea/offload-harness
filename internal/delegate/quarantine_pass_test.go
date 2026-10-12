package delegate

// A node is quarantined for answering about the wrong document. The tripwire (research.AnchorCheck) is lexical, and a
// lexical tripwire misfires on an honest answer now and then (9 of 254 research answers in one corpus, none off-document), so
// "two failures in 30 minutes" read as "this node is broken" for a node that had passed 48 of 51. One node was out of the
// session's placement for 30 minutes that way while the rest of the fleet stood at "queue full". A node that is off-document
// FAILS MORE OFTEN THAN IT PASSES; one that merely tripped twice does not, so the quarantine now needs strikes to outnumber
// the passes of the same window.

import (
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

const fpCheck = "regex:(?i)(?P<docanchor>setpts|atempo|timestamps)"

func fingerprintContract() core.AgentContract {
	c := remoteContract()
	c.Acceptance = []string{fpCheck, "nonempty:answer"}
	return c
}

func TestAQuarantineNeedsMoreStrikesThanPasses(t *testing.T) {
	q := NewQuarantine(10 * time.Minute)
	base := "http://node-a:18811"
	for i := 0; i < 3; i++ {
		q.Pass(base)
	}
	if q.Strike(base) || q.Strike(base) || q.Blocked(base) {
		t.Fatal("two strikes against three passes quarantined the node: it answers about the right document most of the time")
	}
	if q.Strike(base) || q.Blocked(base) {
		t.Fatal("three strikes against three passes quarantined the node: strikes must OUTNUMBER the passes")
	}
	if !q.Strike(base) || !q.Blocked(base) {
		t.Fatal("four strikes against three passes did not quarantine the node")
	}
}

func TestAnUnprovenNodeIsQuarantinedOnTheSecondStrikeAsBefore(t *testing.T) {
	q := NewQuarantine(10 * time.Minute)
	base := "http://node-c:18811"
	if q.Strike(base) || q.Blocked(base) {
		t.Fatal("one strike is a strike, not a block")
	}
	if !q.Strike(base) || !q.Blocked(base) {
		t.Fatal("the second strike of a node with no passes must quarantine it, as it always did")
	}
}

func TestPassesAgeOutWithTheWindow(t *testing.T) {
	q := NewQuarantine(10 * time.Minute)
	now := time.Now()
	q.now = func() time.Time { return now }
	base := "http://node-a:18811"
	for i := 0; i < 5; i++ {
		q.Pass(base)
	}
	now = now.Add(11 * time.Minute) // every pass is older than the window
	q.Strike(base)
	if !q.Strike(base) || !q.Blocked(base) {
		t.Fatal("passes older than the window still shielded the node")
	}
}

func TestANilQuarantineAndAnEmptyBaseRecordNothing(t *testing.T) {
	var q *Quarantine
	q.Pass("http://x") // must not panic
	NewQuarantine(0).Pass("")
}

// noteFingerprint is the runner's reading of one result: a contract that carries the fingerprint check and PASSED it is a
// pass for the node; one that failed it is a strike; one that carries no such check says nothing about the node.
func TestNoteFingerprintCountsPassesAndStrikes(t *testing.T) {
	q := NewQuarantine(10 * time.Minute)
	r := &runner{quarantine: q}
	base := "http://node-a:18811"
	withCheck, without := fingerprintContract(), remoteContract()

	for i := 0; i < 3; i++ {
		r.noteFingerprint(base, withCheck, nil) // passed
	}
	r.noteFingerprint(base, withCheck, []string{fpCheck + ": pattern did not match output"})
	r.noteFingerprint(base, withCheck, []string{fpCheck + ": pattern did not match output"})
	if q.Blocked(base) || r.quarantined.Load() != 0 {
		t.Fatal("two failures after three passes quarantined a node that answers about the right document")
	}
	// a contract without the check, passing or failing, neither shields nor strikes
	for i := 0; i < 10; i++ {
		r.noteFingerprint(base, without, nil)
	}
	r.noteFingerprint(base, without, []string{"contains:zorblax: not found in output"})
	r.noteFingerprint(base, withCheck, []string{fpCheck + ": pattern did not match output"})
	r.noteFingerprint(base, withCheck, []string{fpCheck + ": pattern did not match output"})
	if !q.Blocked(base) || r.quarantined.Load() != 1 {
		t.Fatalf("blocked=%v flips=%d: ten results with no fingerprint check must not have counted as passes", q.Blocked(base), r.quarantined.Load())
	}
}

// The wiring: a remote result that carried the fingerprint check is a pass or a strike for the node that served it.
func TestARemoteResultIsNotedAgainstTheNodesQuarantine(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	q := NewQuarantine(time.Hour)
	_, good := acceptingNode(t, "node-a", "the clip shows setpts and atempo in its filter graph", nil)
	_, off := acceptingNode(t, "node-b", "the latest stable Go release is newer than that", nil)
	run := func(url string) {
		t.Helper()
		cfg := testCfg(t)
		cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true}) // the retry has no local seat to go to
		if _, _, err := RunWith(t.Context(), cfg, neverLocal(t), []core.AgentContract{fingerprintContract()}, "remote", []string{url}, &RunOptions{Quarantine: q}); err != nil {
			t.Fatal(err)
		}
	}
	run(good)
	run(off)
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.passes[good]) != 1 || len(q.strikes[good]) != 0 {
		t.Errorf("node that answered about the page: %d pass(es), %d strike(s), want 1 and 0", len(q.passes[good]), len(q.strikes[good]))
	}
	if len(q.strikes[off]) != 1 || len(q.passes[off]) != 0 {
		t.Errorf("node that answered about another document: %d strike(s), %d pass(es), want 1 and 0", len(q.strikes[off]), len(q.passes[off]))
	}
}
