package mcpserver

// The incoherent-seat memo (pipeline.RememberCoherence) is one table for the whole test process, keyed by endpoint and seat, and
// the agent_run door both fills it (its post-warm probe) and reads it (a warm run defers on a remembered verdict without asking the
// seat anything). A fake server's address is a loopback port the OS may hand to a later fake, so a test that earned a broken
// verdict and left it behind could meet it in an unrelated test. coherenceSeatServer drops the verdict of the seat it serves when
// the test that asked for it ends.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// The cold load of TestAgentRunDefersAnIncoherentSeat, in a subtest: its server's cleanup has run by the time t.Run returns, and
// the checks after it are about what is left. The verdict is asserted to be there INSIDE the subtest, because "nothing remembered
// afterwards" proves nothing about a run that never remembered anything.
func TestAnIncoherentSeatVerdictDoesNotOutliveTheTestThatEarnedIt(t *testing.T) {
	const seat = "agent-pool"
	var url string
	t.Run("a cold load that answers with spam", func(t *testing.T) {
		srv, _ := coherenceSeatServer(t, seat, func(body map[string]any) string {
			if isProbeBody(body) {
				return `{"choices":[{"message":{"role":"assistant","content":"<tool_call>` + strings.Repeat("!", 40) + `"},"finish_reason":"length"}]}`
			}
			return `{"choices":[{"message":{"role":"assistant","content":"The answer is 42."},"finish_reason":"stop"}]}`
		})
		defer srv.Close()
		url = srv.URL
		s := New(pipeline.New(coherenceCfg(t, srv.URL, seat, ""), nil, nil, nil))
		res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
			`{"goal":"what is the answer","read_root":%q,"max_steps":2,"timeout_sec":60}`, t.TempDir())))
		if err != nil {
			t.Fatalf("handleAgentRun: %v", err)
		}
		m := decodeResult(t, res)
		if reason, _ := m["reason"].(string); m["deferred"] != true || !strings.HasPrefix(reason, core.IncoherentSeatReason) {
			t.Fatalf("test premise: the cold load must defer as an incoherent seat, got %v", m)
		}
		if _, ok := pipeline.RecallIncoherentSeat(srv.URL, seat); !ok {
			t.Fatal("test premise: the broken verdict is not remembered while the test runs, so the check after it would pass vacuously")
		}
	})
	if note, ok := pipeline.RecallIncoherentSeat(url, seat); ok {
		t.Errorf("the verdict on seat %q outlived the test that earned it, and a later test on this address would defer on it: %q", seat, note)
	}
}
