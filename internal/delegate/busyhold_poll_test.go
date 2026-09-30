package delegate

import (
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// ADR 0061 end to end, delegator side: a node that HOLDS a silent request
// because its seat's engine is working for others (phase "queued") reports no
// token progress for minutes. It publishes, on entering the hold, the time left
// to the run's ceiling as its allowance — and the delegator must keep polling
// on that basis past its own poll budget, collecting the result when it comes.
// The control arm is what 0.143.0's first draft published (the hold's short
// poll interval): the delegator gives the job up while the node still holds it.
func TestDelegatorKeepsPollingARunTheNodeHolds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		allowanceMs int64
		wantDone    bool
	}{
		{"hold publishes the time left to the ceiling", 3000, true},
		{"control: a hold that publishes its poll interval is abandoned", 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compressPolls(t, 5*time.Millisecond, 100*time.Millisecond)
			old := pollSecond
			pollSecond = 10 * time.Millisecond // a 30 s contract polls for 300 ms + the grace
			t.Cleanup(func() { pollSecond = old })
			holdStart := time.Now().UnixMilli()
			doneAfter := time.Now().Add(1500 * time.Millisecond) // 3x the contract's own poll budget
			node := &fakeNode{
				t: t, token: "sekrit", agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
				pollState: func(n int64) (map[string]any, int) {
					if time.Now().After(doneAfter) {
						return doneWire(t, remoteWire("the held answer", `{"answer":"42"}`)), 200
					}
					return map[string]any{"state": "running", "progress": map[string]any{
						"phase": "queued", "last_progress_ms": holdStart, "allowance_ms": tc.allowanceMs, "ceiling_sec": 300}}, 200
				},
			}
			srv := node.server()
			defer srv.Close()
			cfg := testCfg(t)
			cfg.FleetAuthToken = "sekrit"
			contract := remoteContract()
			contract.Acceptance = []string{"contains:held answer", "nonempty:answer"}
			res, sum, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{srv.URL})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			got := sum.Succeeded == 1 && len(res) == 1 && !res[0].Result.Deferred
			if got != tc.wantDone {
				reason := ""
				if len(res) == 1 {
					reason = res[0].Result.Reason
				}
				t.Fatalf("succeeded=%d deferred=%v reason=%q, want done=%v", sum.Succeeded, len(res) == 1 && res[0].Result.Deferred, reason, tc.wantDone)
			}
		})
	}
}
