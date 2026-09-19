package pairworkloads

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// TestFromLedgerAcceleratorRowNamesDeviceAndNode pins the card a forwarded NPU
// call produces: the ledger records "<node>:<device>" (E-04), and the card must
// run on that node and show the device — not "llamacpp on this box" with the
// pair as its model. A local call and a failed forward keep their tier verbatim
// and no node.
func TestFromLedgerAcceleratorRowNamesDeviceAndNode(t *testing.T) {
	e := &Emitter{}
	cases := []struct {
		row        ledger.Entry
		wantModel  string
		wantEngine string
		wantNode   string
		wantState  string
	}{
		{
			row:        ledger.Entry{TS: 100, Task: "classify", ModelTier: "node-b:coral-edgetpu", LatencyMs: 516},
			wantModel:  "coral-edgetpu",
			wantEngine: "coral-edgetpu",
			wantNode:   "node-b",
			wantState:  "completed",
		},
		{
			row:        ledger.Entry{TS: 101, Task: "classify", ModelTier: "coral-edgetpu", LatencyMs: 3},
			wantModel:  "coral-edgetpu",
			wantEngine: "coral-edgetpu",
			wantNode:   "",
			wantState:  "completed",
		},
		{
			row:        ledger.Entry{TS: 102, Task: "classify", ModelTier: "coral-edgetpu@fleet", Deferred: true, Reason: "node leased"},
			wantModel:  "coral-edgetpu@fleet",
			wantEngine: "coral-edgetpu",
			wantNode:   "",
			wantState:  "failed",
		},
		{
			row:        ledger.Entry{TS: 103, Task: "face_detect", ModelTier: "node-c:hailo-8l", LatencyMs: 40},
			wantModel:  "hailo-8l",
			wantEngine: "hailo-8l",
			wantNode:   "node-c",
			wantState:  "completed",
		},
		// A text row whose tier happens to hold a colon is not split: only
		// accelerator rows carry the "<node>:<device>" convention here.
		{
			row:        ledger.Entry{TS: 104, Task: "summarize", ModelTier: "a:b", LatencyMs: 10},
			wantModel:  "a:b",
			wantEngine: "llamacpp",
			wantNode:   "",
			wantState:  "completed",
		},
	}
	for _, c := range cases {
		ev := e.FromLedger(c.row)
		if ev.Model != c.wantModel || ev.Engine != c.wantEngine || ev.Node != c.wantNode || ev.State != c.wantState {
			t.Errorf("FromLedger(%q %q) = model %q engine %q node %q state %q; want %q %q %q %q",
				c.row.Task, c.row.ModelTier, ev.Model, ev.Engine, ev.Node, ev.State,
				c.wantModel, c.wantEngine, c.wantNode, c.wantState)
		}
	}
}
