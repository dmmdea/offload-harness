// Register A-102: every cascade row in the live ledger carried no door, so
// nobody could tell whether a cascade call came in through an MCP tool, a
// hand-run CLI command or fleet dispatch. Run now carries Request.Door into
// the result meta and entryFrom maps it onto the ledger row. These tests pin
// both halves of that path, plus the additive guarantee: a caller that stamps
// no door must publish a row byte-identical to the pre-A-102 one.
package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

func TestRunCarriesTheDoorIntoMetaAndTheLedgerRow(t *testing.T) {
	srv, _ := cascadeSeenModels(t)
	defer srv.Close()
	cfg := cascadeTestCfg(srv, config.Default())
	p := cascadePipeline(t, srv, cfg)

	res := p.Run(context.Background(), core.Request{
		Task:  core.TaskSummarize,
		Input: summaryInput,
		Door:  "offload_summarize",
	})
	if !res.OK {
		t.Fatalf("defer: %s", res.Reason)
	}
	if res.Meta.Door != "offload_summarize" {
		t.Fatalf("meta.Door = %q, want the caller's door offload_summarize", res.Meta.Door)
	}
	row := entryFrom(core.TaskSummarize, res.Meta, false, len(summaryInput))
	if row.Door != "offload_summarize" {
		t.Fatalf("ledger row door = %q, want offload_summarize", row.Door)
	}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	if !strings.Contains(string(b), `"door":"offload_summarize"`) {
		t.Fatalf("serialized row lacks the door column: %s", b)
	}
}

// A door is documentary and OPTIONAL: an un-stamped call must still carry no
// `door` key at all, so a node one release behind reads the row unchanged.
func TestRunWithoutADoorOmitsTheColumn(t *testing.T) {
	srv, _ := cascadeSeenModels(t)
	defer srv.Close()
	cfg := cascadeTestCfg(srv, config.Default())
	p := cascadePipeline(t, srv, cfg)

	res := p.Run(context.Background(), core.Request{Task: core.TaskSummarize, Input: summaryInput})
	if !res.OK {
		t.Fatalf("defer: %s", res.Reason)
	}
	if res.Meta.Door != "" {
		t.Fatalf("meta.Door = %q, want empty for an un-stamped call", res.Meta.Door)
	}
	b, err := json.Marshal(entryFrom(core.TaskSummarize, res.Meta, false, len(summaryInput)))
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	if strings.Contains(string(b), `"door"`) {
		t.Fatalf("un-stamped row must omit the door column: %s", b)
	}
}

// The request itself must stay wire-compatible in the other direction too: a
// door-less request serializes without the key, so a node one release behind
// decodes a delegator's request unchanged.
func TestRequestDoorIsAdditiveOnTheWire(t *testing.T) {
	b, err := json.Marshal(core.Request{Task: core.TaskSummarize, Input: "x"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if strings.Contains(string(b), `"door"`) {
		t.Fatalf("door-less request must omit the key: %s", b)
	}
	var back core.Request
	if err := json.Unmarshal([]byte(`{"task":"summarize","input":"x","door":"cli:summarize"}`), &back); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if back.Door != "cli:summarize" {
		t.Fatalf("decoded door = %q, want cli:summarize", back.Door)
	}
}

// An agent contract's row names the door that admitted the contract, on the
// box that runs it: the door travels on the wire (core.AgentContract.Door) and
// RunAgentContract hands it to the run's request, so a node's ledger row for a
// dispatched contract names the delegator's surface (register A-102 (e), review
// finding: the agent doors are 98 % of the ledger's wall and were still door-less).
func TestRunAgentContractStampsTheContractsDoorOnItsRow(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	p, home := agentContractPipeline(t, srv.URL)
	ledgerPath := filepath.Join(home, "ledger.jsonl")
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	p.led = led
	defer func() { p.led = nil; _ = led.Close() }()

	contract := testContract()
	contract.Depth = 0
	contract.Door = "agent_delegate"
	wire, err := p.RunAgentContract(context.Background(), contract, AgentContractOptions{})
	if err != nil || wire.Deferred {
		t.Fatalf("run: err=%v wire=%+v", err, wire)
	}
	_ = led.Close()
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e ledger.Entry
		if json.Unmarshal([]byte(line), &e) != nil || e.Task != string(core.TaskAgentRun) {
			continue
		}
		found = true
		if e.Door != "agent_delegate" {
			t.Fatalf("agent row door = %q, want the contract's door", e.Door)
		}
	}
	if !found {
		t.Fatalf("no agent_run row in the ledger: %s", raw)
	}
}
