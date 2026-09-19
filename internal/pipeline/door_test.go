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
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
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
