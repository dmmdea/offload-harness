package delegate

// results[].node_reported: the id the executing node stamped on its OWN result, published beside results[].node (the id the
// placement used) only when the two differ. omitempty: a result whose ids agree is byte-identical to one published before the
// field existed. It is stamped in one place, WireResponse, from NodeReported.

import (
	"encoding/json"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

func TestWireResponseStampsNodeReportedOnlyWhenTheNodeCalledItselfSomethingElse(t *testing.T) {
	for _, tc := range []struct {
		name         string
		node, stamp  string
		wantReported string // "" = the key must be absent
	}{
		{"the node called itself something else", "n1", "h1", "h1"},
		{"equal ids", "n1", "n1", ""},
		{"ids that differ only in case", "Node-B", "node-b", ""},
		{"the result carries no id", "n1", "", ""},
		{"no node took the subtask (the deadline-cleared or exhausted row)", "", "h1", ""},
		{"a queue-lane row, where the placement id IS the claimant's envelope id", "sim-node", "sim-node", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := PlacedResult{Node: tc.node, Seat: "seat-a", PlacementReason: "route=remote forced → " + tc.node, ranBase: "http://192.0.2.10:8080",
				Result: core.AgentWireResult{Output: "ok", NodeID: tc.stamp}}
			raw, err := json.Marshal(WireResponse([]PlacedResult{pr}, Summary{}, nil))
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Results []map[string]json.RawMessage `json:"results"`
			}
			if err := json.Unmarshal(raw, &got); err != nil || len(got.Results) != 1 {
				t.Fatalf("response %s: %v", raw, err)
			}
			row := got.Results[0]
			// Control on every row: the node key is the placement's, whatever the stamp says.
			var node string
			if err := json.Unmarshal(row["node"], &node); err != nil || node != tc.node {
				t.Fatalf("node = %s (%v), want %q", row["node"], err, tc.node)
			}
			rawRep, present := row["node_reported"]
			if tc.wantReported == "" {
				if present {
					t.Fatalf("node_reported = %s on a row where the node's own id adds nothing: the key must be absent", rawRep)
				}
				return
			}
			if !present {
				t.Fatalf("node_reported is absent, want %q: the node's own result named it by another id", tc.wantReported)
			}
			var rep string
			if err := json.Unmarshal(rawRep, &rep); err != nil || rep != tc.wantReported {
				t.Fatalf("node_reported = %s (%v), want %q", rawRep, err, tc.wantReported)
			}
		})
	}
}

// NodeReported is also what the routed agent_run door reads (the mcpserver response map), so it is exercised directly.
func TestNodeReportedReadsTheSameRuleAsTheWire(t *testing.T) {
	pr := PlacedResult{Node: "n1", Result: core.AgentWireResult{NodeID: "h1"}}
	if got := NodeReported(pr); got != "h1" {
		t.Errorf("NodeReported = %q, want h1", got)
	}
	pr.Result.NodeID = "N1"
	if got := NodeReported(pr); got != "" {
		t.Errorf("NodeReported = %q for an id that differs in case only, want none", got)
	}
}
