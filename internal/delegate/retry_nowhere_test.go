package delegate

// A retry the engine considered and could not place used to be published with an EMPTY retry_note unless the defer was one of
// four named shapes (a fence, a reservation, the call's deadline, a reasoned pin) or promised a second placement (seat-down,
// lost-root, forced-final tool call). A wrong answer or an abstention with nowhere to go read as a retry nobody considered; a
// research page whose retry was skipped for this reason had no note at all, so there was nothing to log and nothing to fix.
// Every skip now says why.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

func TestAWrongAnswerWithNowhereToRetryItSaysSo(t *testing.T) {
	t.Run("one remote node and no local seat", func(t *testing.T) {
		compressPolls(t, 10*time.Millisecond, 2*time.Second)
		nodeA, urlA := eligibleNode(t, "node-a", "unused")
		scriptFirstJob(t, func(node string) core.AgentWireResult { return wrongAnswerWire(node, 0) }, "unused", nodeA)
		results, sum, err := Run(context.Background(), seatlessCfg(t), neverLocal(t), []core.AgentContract{remoteContract()}, "spread", []string{urlA})
		if err != nil {
			t.Fatal(err)
		}
		note := results[0].RetryNote
		if sum.Retried != 0 || sum.FailedVerification != 1 {
			t.Fatalf("summary %+v, want the wrong answer published as failed verification with no retry", sum)
		}
		for _, want := range []string{"retry skipped", "no other node could take the contract", "node-a", "already tried"} {
			if !strings.Contains(note, want) {
				t.Errorf("retry_note = %q, want it to contain %q", note, want)
			}
		}
		if strings.Contains(note, "pinned") {
			t.Errorf("retry_note = %q: nothing was pinned", note)
		}
	})
	t.Run("route local", func(t *testing.T) {
		var calls atomic.Int64
		results, sum, err := Run(context.Background(), testCfg(t), failingLocal(&calls), []core.AgentContract{remoteContract()}, "local", nil)
		if err != nil {
			t.Fatal(err)
		}
		if note := results[0].RetryNote; sum.Retried != 0 || !strings.Contains(note, "retry skipped") || !strings.Contains(note, "route local places nothing on another node") {
			t.Fatalf("retried=%d note=%q, want the skip named: route local has no other node", sum.Retried, note)
		}
	})
}
