package fleetview

import (
	"strings"
	"testing"
)

func TestRenderTopShowsNodesJobsErrors(t *testing.T) {
	o := Overview{At: 1, DelegationEnabled: true, Nodes: []Node{
		{Base: "http://node-a:18811", NodeID: "lenovo-ampere6", Reachable: true, AgentSeat: "qwen3.5-4b-agent", AgentResident: true, GpuUtil: 7, GpuUtilKnown: true, VramFree: 4.1, VramTotal: 6, HostCPU: 3, RamUsed: 10, RamTotal: 64, JobsRunning: 0, JobsQueued: 0,
			Jobs: []map[string]any{{"id": "agd-1", "task": "agent-run", "state": "done", "wall_ms": float64(900)}}},
		{Base: "http://node-b:18811", Reachable: false, ProbeError: "dial refused"},
	}, Errors: []Error{{At: 1, Severity: "error", Node: "node-b", Source: "probe", Message: "dial refused"}}}
	out := RenderTop(o, 120)
	for _, want := range []string{"lenovo-ampere6", "7%", "4.1/6.0", "qwen3.5-4b-agent", "agd-1", "dial refused", "1/2 reachable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
