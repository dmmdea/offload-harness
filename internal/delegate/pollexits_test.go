package delegate

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The poll loop has exits the intent ledger must not close as "terminal observed":
// an intent records a dispatch the node ACKED, and closing it says this process saw
// the job end. Two exits closed it having seen nothing of the kind, and closing it
// took away the only path (the recovery pass, ADR 0028) that could still collect
// work the node might hold or finish. ADR 0064 decision 6 says why for the recovery
// pass: a refusal of THIS process's credentials is a fact about the caller, not about
// the job. It holds for a live poll as much as for a recovery one.

// TestRunAPoll401LeavesTheIntentOpen: a node that acked the job and then answers
// every poll 401 (a token rotated mid-run, a config that lost it) has told the
// delegator nothing about the job. The intent stays open so a process holding the
// right token can still collect it, and no withdraw is sent: it would carry the same
// refused token.
func TestRunAPoll401LeavesTheIntentOpen(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 20*time.Millisecond)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-rotated",
		pollState: func(int64) (map[string]any, int) {
			return map[string]any{"error": "unauthorized"}, http.StatusUnauthorized
		},
	}
	probe := &withdrawProbe{answer: confirmsWithdrawal} // must never be asked
	url := probe.front(t, node.server()).URL

	cfg := testCfg(t)
	results, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{withdrawContract()}, "remote", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r := results[0]; !strings.HasPrefix(r.Err, "poll: 401") {
		t.Fatalf("err = %q, want the poll 401 failure", r.Err)
	}
	jobID, _ := node.lastJobID.Load().(string)
	closed, open := intentNotes(t, cfg.StateDir)
	if !open[jobID] {
		t.Fatalf("the intent for %s was closed as %q on a poll 401: the node may still hold the job, and recovery could no longer look for it", jobID, closed[jobID])
	}
	if got := probe.deletes.Load(); got != 0 {
		t.Fatalf("%d withdraw(s) sent after a 401: the same refused token would be refused again", got)
	}
}

// TestRunAnUnownedPollDeadlineDoesNotCloseTheIntent: the node acked the job and then
// never answered a poll in a way that said it held it (here: 502 to every one). The
// failure is unchanged, but nobody observed the job end, so the intent must not say
// so: the give-up asks the node to take the job back like any other, and only a
// confirmation closes the intent (as withdrawn); anything else leaves it open.
func TestRunAnUnownedPollDeadlineDoesNotCloseTheIntent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		answer     func(n int64, id string) (int, map[string]any)
		wantClosed string // "" = the intent must stay open
	}{
		{"the node has no withdraw route", nil, ""},
		{"the node confirms the withdrawal", confirmsWithdrawal, intentNoteWithdrawn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compressPolls(t, 20*time.Millisecond, 100*time.Millisecond)
			node := &fakeNode{
				t: t, agentEnabled: true, resident: true, ctxTokens: 32768, nodeID: "node-behind-a-proxy",
				pollState: func(int64) (map[string]any, int) {
					return map[string]any{"error": "bad gateway"}, http.StatusBadGateway
				},
			}
			probe := &withdrawProbe{answer: tc.answer}
			url := probe.front(t, node.server()).URL

			cfg := testCfg(t)
			contract := withdrawContract()
			contract.TimeoutSec = 1
			results, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{contract}, "remote", []string{url})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			r := results[0]
			if r.Result.Deferred || !strings.HasPrefix(r.Err, "poll deadline") || !strings.Contains(r.Err, "never reported owning the job") {
				t.Fatalf("result = deferred %v err %q, want the unowned poll-deadline FAILURE, unchanged", r.Result.Deferred, r.Err)
			}
			if got := probe.deletes.Load(); got != 1 {
				t.Fatalf("withdraw attempts = %d, want exactly one best-effort try: the node acked the job, so it may hold it", got)
			}
			jobID, _ := node.lastJobID.Load().(string)
			closed, open := intentNotes(t, cfg.StateDir)
			if tc.wantClosed == "" {
				if !open[jobID] {
					t.Fatalf("the intent was closed as %q with nothing observed and no withdrawal confirmed: recovery could no longer find the job", closed[jobID])
				}
				if !strings.Contains(r.Err, "; withdraw not confirmed: HTTP 405: the node has no withdraw route") {
					t.Fatalf("err = %q, want it to say why the withdraw was not confirmed", r.Err)
				}
				return
			}
			if strings.Contains(r.Err, "withdraw not confirmed") {
				t.Fatalf("err = %q reports an unconfirmed withdraw although the node confirmed it", r.Err)
			}
			if closed[jobID] != tc.wantClosed || len(open) != 0 {
				t.Fatalf("intent closed as %q (open=%v), want %q", closed[jobID], open, tc.wantClosed)
			}
		})
	}
}
