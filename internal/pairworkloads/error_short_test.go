package pairworkloads

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// TestPairErrorIsTheShortFormOfAStoredReason: PAIR's card error field has always
// held at most the 120 bytes the ledger used to cut a reason to. The ledger now
// stores the whole reason (up to 4096 bytes, ADR 0064), and PAIR must not start
// receiving it.
func TestPairErrorIsTheShortFormOfAStoredReason(t *testing.T) {
	long := strings.Repeat("queue deadline after 5m0s: the node accepted the job but never started it — ", 10) // 770 bytes
	e := &Emitter{}
	ev := e.FromLedger(ledger.Entry{TS: 100, Task: "agent_delegate", ModelTier: "node-a:seat", Deferred: true, Reason: long})
	if ev.State != "failed" {
		t.Fatalf("state = %q, want failed", ev.State)
	}
	if ev.Error != ledger.ShortReason(long) || len(ev.Error) > 120 {
		t.Fatalf("error = %d bytes %q, want the short form (at most 120 bytes)", len(ev.Error), ev.Error)
	}
	// A deferred row with no reason still says something.
	if ev := e.FromLedger(ledger.Entry{TS: 101, Task: "agent_delegate", Deferred: true}); ev.Error != "deferred" {
		t.Fatalf("error = %q, want the placeholder %q", ev.Error, "deferred")
	}
}
