package mcpserver

// Register C-79 at the agent_run door. The loop now ends a run on a tool's report
// that the workspace root is gone, so the door answers `deferred` with the
// reason, where it used to return the seat's "I could not read the file" as a
// finished answer. The door files no defer_class for loop errors in general; a lost
// root is the one it classes, and only when the read root existed as the call came
// in: a read_root the caller mistyped passes agent.Build (which only makes the path
// absolute) and fails the same way at the first tool call, so the report alone cannot
// tell a typo from a root taken under the run.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// readNotesCall is the seat's first turn: it reads the one document the run was given.
const readNotesCall = `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"notes.md\"}"}}]},"finish_reason":"tool_calls"}]}`

func TestAgentRunDefersWhenItsReadRootVanishesMidRun(t *testing.T) {
	const seat = "agent-pool"
	root := filepath.Join(t.TempDir(), "docs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("the answer is 42"), 0o644); err != nil {
		t.Fatal(err)
	}
	var turns atomic.Int64
	srv, _ := coherenceSeatServer(t, seat, func(map[string]any) string {
		if turns.Add(1) == 1 {
			_ = os.RemoveAll(root) // the root goes while the seat decides what to read
			return readNotesCall
		}
		return `{"choices":[{"message":{"role":"assistant","content":"I could not read the file."},"finish_reason":"stop"}]}`
	})
	defer srv.Close()
	s := New(pipeline.New(coherenceCfg(t, srv.URL, seat, "off"), nil, nil, nil))

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":4,"timeout_sec":60}`, root)))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("a run whose root vanished answered instead of deferring: %v", m)
	}
	reason, _ := m["reason"].(string)
	if !strings.HasPrefix(reason, core.RootGoneReason) || !strings.Contains(reason, root) {
		t.Errorf("reason = %q, want it to open with %q and name the root %q", reason, core.RootGoneReason, root)
	}
	if n := turns.Load(); n != 1 {
		t.Errorf("the seat was asked %d times, want 1: the run ends on the tool's report", n)
	}
	if steps, _ := m["steps"].(float64); steps != 1 {
		t.Errorf("steps = %v, want 1", m["steps"])
	}
	if m["defer_class"] != string(core.DeferClassInfrastructure) {
		t.Errorf("defer_class = %v, want %q: the root existed when the call came in, and the box lost it", m["defer_class"], core.DeferClassInfrastructure)
	}
}

// A read_root that never existed is the caller's typo. Build lets it through and the first
// tool call fails exactly as it does for a root that vanished, so the report cannot tell the
// two apart: the door asked whether the root was there when the call came in, and claims no
// infrastructure fault for one that never was.
func TestAgentRunWithAReadRootThatNeverExistedClaimsNoInfrastructureFault(t *testing.T) {
	const seat = "agent-pool"
	root := filepath.Join(t.TempDir(), "typo") // never created
	var turns atomic.Int64
	srv, _ := coherenceSeatServer(t, seat, func(map[string]any) string {
		if turns.Add(1) == 1 {
			return readNotesCall
		}
		return `{"choices":[{"message":{"role":"assistant","content":"I could not read the file."},"finish_reason":"stop"}]}`
	})
	defer srv.Close()
	s := New(pipeline.New(coherenceCfg(t, srv.URL, seat, "off"), nil, nil, nil))

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":4,"timeout_sec":60}`, root)))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("a run that could not open its read root answered: %v", m)
	}
	reason, _ := m["reason"].(string)
	if !strings.HasPrefix(reason, core.RootGoneReason) || !strings.Contains(reason, root) {
		t.Errorf("reason = %q, want it to open with %q and name the root %q", reason, core.RootGoneReason, root)
	}
	if m["defer_class"] == string(core.DeferClassInfrastructure) {
		t.Errorf("defer_class = %v: a read_root that never existed is the caller's typo, not a fault of this box", m["defer_class"])
	}
}

// The class is the root loss's own. Any other loop error at this door keeps filing none, even
// with a read root that exists (here a parser mismatch: the seat answers with a tool call
// written out as text), so an existing root does not turn every failure into a fault of the box.
func TestAgentRunFilesNoInfrastructureClassForALoopErrorThatIsNotARootLoss(t *testing.T) {
	const seat = "agent-pool"
	root := filepath.Join(t.TempDir(), "docs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	srv, _ := coherenceSeatServer(t, seat, func(map[string]any) string {
		return `{"choices":[{"message":{"role":"assistant","content":"<tool_call>{}</tool_call>"},"finish_reason":"stop"}]}`
	})
	defer srv.Close()
	s := New(pipeline.New(coherenceCfg(t, srv.URL, seat, "off"), nil, nil, nil))

	res, err := s.handleAgentRun(context.Background(), callReq(fmt.Sprintf(
		`{"goal":"what is the answer","read_root":%q,"max_steps":4,"timeout_sec":60}`, root)))
	if err != nil {
		t.Fatalf("handleAgentRun: %v", err)
	}
	m := decodeResult(t, res)
	reason, _ := m["reason"].(string)
	if m["deferred"] != true || strings.HasPrefix(reason, core.RootGoneReason) {
		t.Fatalf("premise: the run must defer on a loop error that is not a root loss, got %v", m)
	}
	if m["defer_class"] != nil {
		t.Errorf("defer_class = %v for a loop error that is not a root loss (reason %q), want none: the door files no class for loop errors in general", m["defer_class"], reason)
	}
}
