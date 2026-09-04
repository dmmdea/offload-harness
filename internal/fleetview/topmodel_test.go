package fleetview

import (
	"strings"
	"testing"
)

// TestRenderTopJobsSortByAcceptedAtAcrossNodes exercises the cross-node
// interleaving the review flagged: node A contributes t=100 and t=300, node
// B contributes t=200 — accepted_at is a real unix timestamp shared across
// nodes (unlike wall_ms, a per-job duration), so the merged JOBS feed must
// come out newest-first regardless of which node it came from: 300, 200,
// 100. Also asserts wall_ms:1500 renders as "1.5s".
func TestRenderTopJobsSortByAcceptedAtAcrossNodes(t *testing.T) {
	o := Overview{Nodes: []Node{
		{NodeID: "node-a", Reachable: true, Jobs: []map[string]any{
			{"id": "a-old", "task": "task-oldest", "state": "done", "accepted_at": float64(100)},
			{"id": "a-new", "task": "task-newest", "state": "done", "accepted_at": float64(300), "wall_ms": float64(1500)},
		}},
		{NodeID: "node-b", Reachable: true, Jobs: []map[string]any{
			{"id": "b-mid", "task": "task-middle", "state": "done", "accepted_at": float64(200)},
		}},
	}}
	out := RenderTop(o, 120)

	newIdx := strings.Index(out, "task-newest")
	midIdx := strings.Index(out, "task-middle")
	oldIdx := strings.Index(out, "task-oldest")
	if newIdx < 0 || midIdx < 0 || oldIdx < 0 {
		t.Fatalf("missing a job task name in:\n%s", out)
	}
	if !(newIdx < midIdx && midIdx < oldIdx) {
		t.Fatalf("expected order task-newest(300), task-middle(200), task-oldest(100); got indices %d, %d, %d in:\n%s", newIdx, midIdx, oldIdx, out)
	}
	if !strings.Contains(out, "1.5s") {
		t.Fatalf("missing wall_ms 1.5s rendering in:\n%s", out)
	}
}

func TestRenderTopShowsNodesJobsErrors(t *testing.T) {
	o := Overview{At: 1, DelegationEnabled: true, Nodes: []Node{
		{Base: "http://node-a:18811", NodeID: "lenovo-ampere6", Reachable: true, AgentSeat: "qwen3.5-4b-agent", AgentResident: true, GpuUtil: 7, GpuUtilKnown: true, VramFree: 4.1, VramTotal: 6, HostCPU: 3, RamUsed: 10, RamTotal: 64, JobsRunning: 0, JobsQueued: 0,
			Jobs: []map[string]any{{"id": "agd-1", "task": "agent-run", "state": "done", "wall_ms": float64(900)}}},
		{Base: "http://node-b:18811", Reachable: false, ProbeError: "dial refused"},
	}, Errors: []Error{{At: 1, Severity: "error", Node: "node-b", Source: "probe", Message: "dial refused"}}}
	out := RenderTop(o, 120)
	// NOTE: "agd-1" (the job id) is intentionally not asserted here — the
	// review's Critical finding #3 dropped the id column from JOBS in favor
	// of "age node task model state wall" (matching ui.html), so "agent-run"
	// (the job's task, still rendered) stands in for the "a job row
	// appears" assertion the original brief's id check was making.
	for _, want := range []string{"lenovo-ampere6", "7%", "4.1/6.0", "qwen3.5-4b-agent", "agent-run", "dial refused", "1/2 reachable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
