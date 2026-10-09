// pin_retry_seatless_test.go: the retry note of a bare remote route, or of a reasonless remote hint, never names a pin
// (ADR 0078 decision 3), on a box with no agent seat as anywhere. runOne words the skipped retry of a reasoned remote pin
// with the pin, keyed on remotePinned(). A key on the route alone (r.route == "remote") survived every test, because the
// bare route's retry always had the local seat to go to and never reached those notes; a delegation client is the box
// where it has not, and there the widened key would publish "route=remote is pinned (pin_reason )" on a call that gave none.

package delegate

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestTheBareRemoteRoutesRetryNoteNamesNoPinOnASeatlessBox: one node, a seatless box, and a first attempt a retry would
// take up: a failed verification, whose skipped retry has no note of its own here, and a seat-down defer, whose note says
// no other node could take it. Neither says the route is pinned, and nothing ran on the box.
func TestTheBareRemoteRoutesRetryNoteNamesNoPinOnASeatlessBox(t *testing.T) {
	for _, call := range []struct {
		name string
		opts func() *RunOptions
	}{
		{"the bare remote route of a caller with no reason channel", func() *RunOptions { return &RunOptions{PinTally: NewPinTally()} }},
		{"a reasonless remote hint", func() *RunOptions { o, _ := countedOpts(""); return o }},
	} {
		for _, fa := range []struct {
			name  string
			first int // index into remotePinFirstAttempts
		}{{"a failed verification", 0}, {"a seat-down defer", 3}} {
			first := remotePinFirstAttempts[fa.first]
			t.Run(call.name+"/"+fa.name, func(t *testing.T) {
				compressPolls(t, 10*time.Millisecond, 2*time.Second)
				nodeA, urlA := eligibleNode(t, "node-a", "unused")
				scriptFirstJob(t, first.first, "verified by the second node", nodeA)
				var localCalls atomic.Int64
				results, sum, err := RunWith(t.Context(), seatlessCfg(t), passingLocal(&localCalls), []core.AgentContract{verifiedContract()}, "remote", []string{urlA}, call.opts())
				if err != nil {
					t.Fatal(err)
				}
				pr := results[0]
				if localCalls.Load() != 0 || pr.ranLocal || nodeA.dispatches.Load() != 1 || sum.Retried != 0 || !first.kept(pr) {
					t.Fatalf("local calls %d, ran local %v, node dispatches %d, summary %+v, first attempt kept %v: want the one attempt on the node and no retry (a seatless box has no seat to retry on); note=%q",
						localCalls.Load(), pr.ranLocal, nodeA.dispatches.Load(), sum, first.kept(pr), pr.RetryNote)
				}
				if strings.Contains(pr.RetryNote, "is pinned") || strings.Contains(pr.RetryNote, "pin_reason") || pr.PinReason != "" {
					t.Errorf("retry_note = %q, pin_reason %q: a call with no reason is not a reasoned pin, on a seatless box as anywhere", pr.RetryNote, pr.PinReason)
				}
				if SeatDownDefer(pr.Result) {
					want := "retry skipped: the seat on node-a went down and no other node could take the contract (every eligible node was already tried or is not eligible)"
					if pr.RetryNote != want {
						t.Errorf("retry_note = %q, want %q", pr.RetryNote, want)
					}
				} else if pr.RetryNote != "" {
					t.Errorf("retry_note = %q, want none: nothing was pinned, and the bare route's skipped retry says nothing else", pr.RetryNote)
				}
			})
		}
	}
}
