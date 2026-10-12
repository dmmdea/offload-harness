package mcpserver

// D-111 fix round: what a mixed call SAYS about itself.
//
// Intake splices a refused subtask back among the results at its own position, so the call
// answers one result per subtask sent. Two other things the call says had to follow:
//   - the progress notifications number subtasks by the caller's position and count the
//     refused ones, so "subtask K of N" points at results[K-1] and "N of M done" can reach M;
//   - a call the engine fails as a whole (nothing ran) still tells the caller which subtask
//     it also refused, so the mistake the server already knows about is not left for the
//     second try.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/research"
)

// TestAgentDelegateProgressOfAMixedCallUsesTheCallersNumbering: [fast, refused, slow]. The engine
// is handed two contracts, and used to announce the third subtask as "subtask 2 of 2" and its
// finish as "2 of 2 done" for a call that returned three results. The caller's numbers are the
// input positions, the total is the three sent, and the refused one counts as done from the start.
func TestAgentDelegateProgressOfAMixedCallUsesTheCallersNumbering(t *testing.T) {
	s := deadlineServer(t, 30, slowThenFast)
	res, log := callOverMCP(t, s, "agent_delegate", map[string]any{
		"subtasks": []any{
			map[string]any{"goal": "fast one"},
			map[string]any{"goal": "refused at intake", "context_paths": []string{"no-such-scratch-file.txt"}},
			map[string]any{"goal": "slow one"},
		},
		"route": "local", "read_root": t.TempDir(),
	}, "tok-mixed")
	if res.IsError {
		t.Fatalf("the call itself failed: %+v", res)
	}
	msgs := progressMessages(log.all())
	if len(msgs) < 5 {
		t.Fatalf("got %d progress notification(s), want an opening one plus a start and a finish per run subtask: %v", len(msgs), msgs)
	}
	if !strings.Contains(msgs[0], "delegating 3 subtask(s)") || !strings.Contains(msgs[0], "1 refused at intake") {
		t.Errorf("opening = %q, want it to count the three subtasks sent and name the one refused", msgs[0])
	}
	for _, m := range msgs {
		if strings.Contains(m, "of 2") {
			t.Errorf("notification %q counts the %d subtasks the engine was handed, not the 3 the caller sent: %v", m, 2, msgs)
		}
		if strings.Contains(m, "subtask 2 of 3") {
			t.Errorf("notification %q is about position 2, which was refused at intake and never ran: %v", m, msgs)
		}
	}
	for _, want := range []string{"subtask 1 of 3 started", "subtask 3 of 3 started", "subtask 1 of 3 finished", "subtask 3 of 3 finished"} {
		if !hasMessage(msgs, want) {
			t.Errorf("no notification says %q: %v", want, msgs)
		}
	}
	// The refused subtask is done from the start: the fast one finishing is the second of three.
	if !hasMessage(msgs, "subtask 1 of 3 finished") || !hasMessage(msgs, "2 of 3 done") {
		t.Errorf("want the first finish to read \"2 of 3 done\" (one refused, one finished): %v", msgs)
	}
	if last := msgs[len(msgs)-1]; !strings.Contains(last, "subtask 3 of 3 finished") || !strings.Contains(last, "3 of 3 done") {
		t.Errorf("the last notification %q should report the third subtask finished and all three done", last)
	}
}

// The reporter counts the refused subtasks as done from the start, before the engine has said
// anything: a call waiting in a capacity queue sends heartbeats before its first event, and
// "0 of 3" would deny the answer the call already holds for the refused one.
func TestAProgressReporterCountsTheRefusedSubtasksFromTheStart(t *testing.T) {
	p := &progressReporter{total: 3, begun: time.Now()}
	p.noteRefused(1)
	if got := p.heartbeat(); !strings.Contains(got, "1 of 3 subtasks done") {
		t.Errorf("heartbeat = %q, want the refused subtask counted: \"1 of 3 subtasks done\"", got)
	}
	if got := p.opening(); !strings.Contains(got, "delegating 3 subtask(s), 1 refused at intake") {
		t.Errorf("opening = %q, want the three sent and the one refused named", got)
	}
	clean := &progressReporter{total: 2, begun: time.Now()}
	clean.noteRefused(0)
	if got := clean.opening(); strings.Contains(got, "refused") {
		t.Errorf("opening = %q mentions a refusal on a call that had none", got)
	}
}

// A call with nothing refused is exactly what it was: the same numbers, no mention of intake.
func TestAgentDelegateProgressOfACleanCallIsUnchanged(t *testing.T) {
	s := deadlineServer(t, 30, slowThenFast)
	_, log := callOverMCP(t, s, "agent_delegate", twoSubtasks, "tok-clean")
	msgs := progressMessages(log.all())
	if len(msgs) == 0 || !strings.HasPrefix(msgs[0], "delegating 2 subtask(s); call deadline in") {
		t.Fatalf("opening = %v, want the plain two-subtask opening", msgs)
	}
	for _, m := range msgs {
		if strings.Contains(m, "refused") {
			t.Errorf("notification %q mentions a refusal on a call that had none", m)
		}
	}
	if last := msgs[len(msgs)-1]; !strings.Contains(last, "2 of 2") {
		t.Errorf("last notification %q should report 2 of 2", last)
	}
}

func hasMessage(msgs []string, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// TestAgentDelegateEngineErrorStillNamesTheIntakeRejection: the engine refuses the whole call
// (a route it does not know), so nothing runs and there is no result to splice the rejection
// into. The caller fixes the route and resubmits; the subtask the server already knew was
// malformed must not be left for the second try.
func TestAgentDelegateEngineErrorStillNamesTheIntakeRejection(t *testing.T) {
	run := &batchRunner{}
	s := delegateTestServer(t, run.run)
	res, err := s.handleAgentDelegate(context.Background(), callReq(
		`{"subtasks":[{"goal":"valid one"},{"goal":"refused","context_paths":["no-such-scratch-file.txt"]}],"route":"bogus","read_root":`+jsonString(t.TempDir())+`}`))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	reason, _ := m["reason"].(string)
	if m["deferred"] != true || !strings.Contains(reason, `route "bogus" not recognized`) {
		t.Fatalf("result = %v, want the whole-call defer carrying the engine's own error", m)
	}
	if !strings.Contains(reason, "subtask 1:") || !strings.Contains(reason, "no-such-scratch-file.txt") {
		t.Errorf("reason = %q, want it to also name subtask 1 and its path: refused at intake", reason)
	}
	if strings.Contains(reason, "subtask 0:") {
		t.Errorf("reason = %q names subtask 0, which was valid", reason)
	}
	if got := run.ran(); len(got) != 0 {
		t.Errorf("the runner ran %q on a call the engine refused", got)
	}
}

// A call with nothing refused keeps the engine's error exactly as it was.
func TestAgentDelegateEngineErrorWithNothingRefusedIsUnchanged(t *testing.T) {
	run := &batchRunner{}
	s := delegateTestServer(t, run.run)
	res, err := s.handleAgentDelegate(context.Background(), callReq(
		`{"subtasks":[{"goal":"valid one"}],"route":"bogus","read_root":`+jsonString(t.TempDir())+`}`))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	reason, _ := m["reason"].(string)
	if reason != `delegate: route "bogus" not recognized (want auto, spread, local, remote, or queue)` {
		t.Errorf("reason = %q, want the engine's own error and nothing appended", reason)
	}
}

// TestResearchEngineErrorStillNamesTheIntakeRejection: the same for offload_research, where the
// refused page is named by its place in the url list.
func TestResearchEngineErrorStillNamesTheIntakeRejection(t *testing.T) {
	run := &batchRunner{}
	s := delegateTestServer(t, run.run)
	s.researchFetch = func(_ context.Context, urls []string, _ research.Options) []research.Fetched {
		out := make([]research.Fetched, len(urls))
		for i, u := range urls {
			text := "A short page about widgets."
			if i == 1 {
				text = strings.Repeat("w", core.AgentContextMaxBytes+1024)
			}
			out[i] = research.Fetched{URL: u, Status: 200, Title: "Page", Text: text, Bytes: len(text), TextBytes: len(text)}
		}
		return out
	}
	args := strings.Replace(researchArgs, `"route":"local"`, `"route":"bogus"`, 1)
	res, err := s.handleResearch(context.Background(), callReq(args))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	reason, _ := m["reason"].(string)
	if m["deferred"] != true || !strings.Contains(reason, `route "bogus" not recognized`) {
		t.Fatalf("result = %v, want the whole-call defer carrying the engine's own error", m)
	}
	if !strings.Contains(reason, "source 1:") {
		t.Errorf("reason = %q, want it to also name source 1: refused at intake", reason)
	}
	if got := run.ran(); len(got) != 0 {
		t.Errorf("the runner ran %q on a call the engine refused", got)
	}
}
