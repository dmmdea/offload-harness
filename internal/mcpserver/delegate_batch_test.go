package mcpserver

// D-111 part (1): one bad subtask no longer rejects the valid ones in the same call.
//
// Before, handleAgentDelegate returned ONE flat defer for the whole call the moment any
// subtask failed intake ("subtask 0: ..."), so a valid sibling never ran: a caller that put a
// scratch file in one subtask's context_paths lost the other subtask's answer too. Intake now
// prepares each subtask on its own. A rejected subtask is a contract-class defer filed at its
// own position (the caller's mistake, not a broken stack), the valid ones go to the runner,
// and the call is only refused as a whole when nothing in it is valid or when it is over the
// subtask cap. offload_research follows the same rule per source.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/research"
)

// batchRunner is a local seat that records every contract it was handed and answers each
// with a clean digest naming its goal (and a structured `summary`, which a research schema asks for).
type batchRunner struct {
	mu    sync.Mutex
	goals []string
}

func (b *batchRunner) run(_ context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
	b.mu.Lock()
	b.goals = append(b.goals, c.Goal)
	b.mu.Unlock()
	return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box", Seat: "fake-seat",
		Output: "answered " + c.Goal, Structured: json.RawMessage(`{"summary":"digest"}`), StopReason: "done"}, nil
}

func (b *batchRunner) ran() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.goals...)
}

// batchCall marshals a call whose read_root is a fresh directory (so a context_paths entry
// naming a file that is not in it is a deterministic intake rejection).
func batchCall(t *testing.T, subtasks ...map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"subtasks": subtasks, "route": "local", "read_root": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func resultsOf(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, _ := m["results"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		row, _ := r.(map[string]any)
		out = append(out, row)
	}
	return out
}

// TestAgentDelegateRunsTheValidSubtaskWhenASiblingIsRejectedAtIntake is the acceptance shape:
// subtask 0 names a file that does not exist, subtask 1 is valid. The runner runs once, for
// subtask 1; the answer carries two results in input order, the rejection first.
func TestAgentDelegateRunsTheValidSubtaskWhenASiblingIsRejectedAtIntake(t *testing.T) {
	run := &batchRunner{}
	s := delegateTestServer(t, run.run)
	res, err := s.handleAgentDelegate(context.Background(), callReq(batchCall(t,
		map[string]any{"goal": "read the missing file", "context_paths": []string{"no-such-scratch-file.txt"}},
		map[string]any{"goal": "answer the valid one"},
	)))
	if err != nil {
		t.Fatal(err)
	}
	if got := run.ran(); len(got) != 1 || got[0] != "answer the valid one" {
		t.Fatalf("the runner ran %q, want exactly the valid subtask", got)
	}
	m := decodeResult(t, res)
	rs := resultsOf(t, m)
	if len(rs) != 2 {
		t.Fatalf("results = %v, want one per input subtask, in input order", m["results"])
	}
	reason, _ := rs[0]["reason"].(string)
	if rs[0]["deferred"] != true || !strings.HasPrefix(reason, "subtask 0:") || !strings.Contains(reason, "no-such-scratch-file.txt") {
		t.Errorf("results[0] = %v, want a defer whose reason starts \"subtask 0:\" and names the path", rs[0])
	}
	if rs[1]["deferred"] == true || rs[1]["failed"] == true || rs[1]["output"] != "answered answer the valid one" {
		t.Errorf("results[1] = %v, want the valid subtask's answer", rs[1])
	}
	sum, _ := m["summary"].(map[string]any)
	if sum["succeeded"] != float64(1) || sum["deferred"] != float64(1) || sum["infrastructure"] != float64(0) || sum["failed"] != float64(0) {
		t.Errorf("summary = %v, want succeeded 1, deferred 1, infrastructure 0", sum)
	}
	if res.IsError {
		t.Error("IsError = true: one subtask delivered, and a partial result is not a failed call")
	}
	if !strings.HasPrefix(res.Content[0].(*mcp.TextContent).Text, `{"summary"`) {
		t.Error("the body must still lead with the summary")
	}
}

// TestAgentDelegateRejectedSubtaskIsContractClassAndNotAnError: the rejection is the CALLER's
// mistake, so it is a contract-class defer: it stays out of summary.infrastructure and of
// lost_to_stack, it is not a failure, and it is not placed anywhere. The lints still line up
// with the ORIGINAL indices: the valid subtask is index 1 and carries its own lint.
func TestAgentDelegateRejectedSubtaskIsContractClassAndNotAnError(t *testing.T) {
	run := &batchRunner{}
	s := delegateTestServer(t, run.run)
	res, err := s.handleAgentDelegate(context.Background(), callReq(batchCall(t,
		map[string]any{"goal": "read the missing file", "context_paths": []string{"gone.txt"}},
		map[string]any{
			"goal":       "answer the valid one",
			"context":    []map[string]any{{"name": "doc.txt", "text": "some words"}},
			"acceptance": []string{"contains:a-name-the-doc-never-wrote"},
		},
	)))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	rs := resultsOf(t, m)
	if len(rs) != 2 {
		t.Fatalf("results = %v", m["results"])
	}
	if rs[0]["defer_class"] != core.DeferClassContract {
		t.Errorf("results[0].defer_class = %v, want %q", rs[0]["defer_class"], core.DeferClassContract)
	}
	if rs[0]["failed"] == true {
		t.Errorf("results[0] = %v: an intake rejection is a defer, not a failure", rs[0])
	}
	if _, placed := rs[0]["node"]; placed && rs[0]["node"] != "" {
		t.Errorf("results[0].node = %v: nothing was placed", rs[0]["node"])
	}
	sum, _ := m["summary"].(map[string]any)
	if sum["infrastructure"] != float64(0) || sum["lost_to_stack"] != nil {
		t.Errorf("summary = %v: a contract-class defer is not a broken stack", sum)
	}
	if lint, _ := rs[0]["acceptance_lint"].([]any); len(lint) != 0 {
		t.Errorf("results[0].acceptance_lint = %v, want none: the lint of subtask 1 must not slide onto the rejected subtask 0", lint)
	}
	if lint, _ := rs[1]["acceptance_lint"].([]any); len(lint) == 0 {
		t.Errorf("results[1] = %v, want its own UNGROUNDED acceptance lint at its original index", rs[1])
	}
	if res.IsError {
		t.Error("IsError = true for a call that delivered a result")
	}
}

// TestAgentDelegateAllSubtasksRejectedKeepsTheFlatDeferNamingEveryIndex: nothing valid means
// nothing to run; the call keeps today's flat defer, and it now names every bad index, not
// only the first.
func TestAgentDelegateAllSubtasksRejectedKeepsTheFlatDeferNamingEveryIndex(t *testing.T) {
	s := delegateTestServer(t, noLocalRun(t))
	res, err := s.handleAgentDelegate(context.Background(), callReq(batchCall(t,
		map[string]any{"goal": "first", "context_paths": []string{"gone-one.txt"}},
		map[string]any{"goal": "   "},
		map[string]any{"goal": "third", "context_paths": []string{"gone-three.txt"}},
	)))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	reason, _ := m["reason"].(string)
	if m["deferred"] != true || m["results"] != nil {
		t.Fatalf("result = %v, want the flat defer (deferred true, no results)", m)
	}
	for _, want := range []string{"subtask 0:", "gone-one.txt", "subtask 1:", "goal", "subtask 2:", "gone-three.txt"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason = %q, want it to name %q", reason, want)
		}
	}
}

// TestAgentDelegateKeepsTheEightSubtaskCapWhenSomeAreRejected: the cap is read off the INPUT,
// so nine subtasks with one bad one are refused for the cap exactly as nine good ones are, and
// the runner never runs. The rejection does not buy the call a place under the cap.
func TestAgentDelegateKeepsTheEightSubtaskCapWhenSomeAreRejected(t *testing.T) {
	s := delegateTestServer(t, noLocalRun(t))
	subtasks := []map[string]any{{"goal": "bad", "context_paths": []string{"gone.txt"}}}
	for i := 1; i < 9; i++ {
		subtasks = append(subtasks, map[string]any{"goal": fmt.Sprintf("good %d", i)})
	}
	res, err := s.handleAgentDelegate(context.Background(), callReq(batchCall(t, subtasks...)))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	reason, _ := m["reason"].(string)
	if m["deferred"] != true || !strings.Contains(reason, "9 subtasks exceeds the max of 8") {
		t.Fatalf("result = %v, want the cap refusal (9 subtasks exceeds the max of 8)", m)
	}
	if m["results"] != nil {
		t.Errorf("result carries results %v: the cap refuses the whole call", m["results"])
	}
}

// TestAgentDelegateWithEightSubtasksAndOneRejectedStillRuns: the boundary the cap test leaves
// open. Eight subtasks are within the cap whether or not some of them are rejected.
func TestAgentDelegateWithEightSubtasksAndOneRejectedStillRuns(t *testing.T) {
	run := &batchRunner{}
	s := delegateTestServer(t, run.run)
	subtasks := []map[string]any{{"goal": "bad", "context_paths": []string{"gone.txt"}}}
	for i := 1; i < 8; i++ {
		subtasks = append(subtasks, map[string]any{"goal": fmt.Sprintf("good %d", i)})
	}
	res, err := s.handleAgentDelegate(context.Background(), callReq(batchCall(t, subtasks...)))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	sum, _ := m["summary"].(map[string]any)
	if len(run.ran()) != 7 || sum["succeeded"] != float64(7) || sum["deferred"] != float64(1) {
		t.Fatalf("ran %d, summary %v, want 7 run, 7 succeeded and 1 deferred", len(run.ran()), sum)
	}
	rs := resultsOf(t, m)
	for i, r := range rs {
		want := fmt.Sprintf("answered good %d", i)
		if i == 0 {
			if r["deferred"] != true {
				t.Errorf("results[0] = %v, want the rejection", r)
			}
			continue
		}
		if r["output"] != want {
			t.Errorf("results[%d].output = %v, want %q (input order, rejection spliced at its own index)", i, r["output"], want)
		}
	}
}

// TestResearchOneBadSourceDoesNotDeferTheOthers: the per-source rule. Page 1 is larger than
// the contract context cap (the fetch seam hands the door a page its own fetcher would have
// cut), so its contract is rejected at intake; pages 0 and 2 are digested, and the rejected
// page keeps its place in results and in result_sources.
func TestResearchOneBadSourceDoesNotDeferTheOthers(t *testing.T) {
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
	// A caller acceptance that is shape-only draws the intake lint on every page that has a contract.
	args := strings.Replace(researchArgs, `"route":"local"`, `"acceptance":["nonempty:summary"],"route":"local"`, 1)
	res, err := s.handleResearch(context.Background(), callReq(args))
	if err != nil {
		t.Fatal(err)
	}
	if len(run.ran()) != 2 {
		t.Fatalf("the runner ran %d contracts, want the two valid pages", len(run.ran()))
	}
	m := decodeResult(t, res)
	rs := resultsOf(t, m)
	if len(rs) != 3 {
		t.Fatalf("results = %v, want one per usable page", m["results"])
	}
	reason, _ := rs[1]["reason"].(string)
	if rs[1]["deferred"] != true || rs[1]["defer_class"] != core.DeferClassContract || !strings.HasPrefix(reason, "source 1:") {
		t.Errorf("results[1] = %v, want a contract defer whose reason starts \"source 1:\"", rs[1])
	}
	for _, i := range []int{0, 2} {
		if rs[i]["deferred"] == true || rs[i]["failed"] == true {
			t.Errorf("results[%d] = %v, want a delivered digest", i, rs[i])
		}
		if lint, _ := rs[i]["acceptance_lint"].([]any); len(lint) == 0 {
			t.Errorf("results[%d] = %v, want its own acceptance lint at its own position", i, rs[i])
		}
	}
	if lint, _ := rs[1]["acceptance_lint"].([]any); len(lint) != 0 {
		t.Errorf("results[1].acceptance_lint = %v, want none: a valid page's lint must not slide onto the rejected one", lint)
	}
	if rsrc, _ := m["result_sources"].([]any); len(rsrc) != 3 {
		t.Errorf("result_sources = %v, want the three page indices kept in step with results", m["result_sources"])
	}
	sum, _ := m["summary"].(map[string]any)
	if sum["succeeded"] != float64(2) || sum["deferred"] != float64(1) || sum["infrastructure"] != float64(0) {
		t.Errorf("summary = %v, want succeeded 2, deferred 1, infrastructure 0", sum)
	}
	if res.IsError {
		t.Error("IsError = true on a research result that delivered two digests")
	}
}

// TestResearchRejectedSourceIsNamedByItsPlaceInTheURLList: a page whose fetch failed has no
// contract, so the third url is the SECOND contract. The rejection is named by the url's own
// place ("source 2"), the number the caller reads in `sources` and `result_sources`, not by the
// contract's place.
func TestResearchRejectedSourceIsNamedByItsPlaceInTheURLList(t *testing.T) {
	run := &batchRunner{}
	s := delegateTestServer(t, run.run)
	s.researchFetch = func(_ context.Context, urls []string, _ research.Options) []research.Fetched {
		out := make([]research.Fetched, len(urls))
		for i, u := range urls {
			text := "A short page about widgets."
			switch i {
			case 0:
				out[i] = research.Fetched{URL: u, Err: "connection refused"}
				continue
			case 2:
				text = strings.Repeat("w", core.AgentContextMaxBytes+1024)
			}
			out[i] = research.Fetched{URL: u, Status: 200, Title: "Page", Text: text, Bytes: len(text), TextBytes: len(text)}
		}
		return out
	}
	res, err := s.handleResearch(context.Background(), callReq(researchArgs))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	rs := resultsOf(t, m)
	if len(rs) != 2 {
		t.Fatalf("results = %v, want one per usable page (the refused fetch has none)", m["results"])
	}
	reason, _ := rs[1]["reason"].(string)
	if !strings.HasPrefix(reason, "source 2:") {
		t.Errorf("results[1].reason = %q, want it to start \"source 2:\" (the url's place, not the contract's)", reason)
	}
	if rsrc, _ := m["result_sources"].([]any); len(rsrc) != 2 || rsrc[0] != float64(1) || rsrc[1] != float64(2) {
		t.Errorf("result_sources = %v, want [1 2]", m["result_sources"])
	}
}

// TestResearchEverySourceRejectedKeepsTheFlatDefer: nothing valid, nothing to run; the flat
// defer names every bad source and still carries the sources list.
func TestResearchEverySourceRejectedKeepsTheFlatDefer(t *testing.T) {
	s := delegateTestServer(t, noLocalRun(t))
	s.researchFetch = func(_ context.Context, urls []string, _ research.Options) []research.Fetched {
		out := make([]research.Fetched, len(urls))
		for i, u := range urls {
			text := strings.Repeat("w", core.AgentContextMaxBytes+1024)
			out[i] = research.Fetched{URL: u, Status: 200, Title: "Page", Text: text, Bytes: len(text), TextBytes: len(text)}
		}
		return out
	}
	res, err := s.handleResearch(context.Background(), callReq(researchArgs))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	reason, _ := m["reason"].(string)
	if m["deferred"] != true || m["results"] != nil || m["sources"] == nil {
		t.Fatalf("result = %v, want the flat defer with the sources", m)
	}
	for _, want := range []string{"source 0:", "source 1:", "source 2:"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason = %q, want it to name %q", reason, want)
		}
	}
}
