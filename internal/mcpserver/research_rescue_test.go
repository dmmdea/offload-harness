package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/research"
)

// offload_research hands the delegate engine the delegator's rescue. The
// research lane is where the finished answers whose re-pack failed came from
// (every research page is a schema contract), and nothing exercised the handler
// end to end: dropping `Rescue:` from its RunOptions kept the suite green.
func TestOfferResearchRescuesAFinishedAnswerOnTheDelegator(t *testing.T) {
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	cfg.AgentDelegationEnabled = true
	s := New(pipeline.New(cfg, nil, nil, nil))
	const page = "The driven path uses shared memory transfers between processes.\nThe engine copies every buffer through pinned staging memory, which is slower than the shared handle path."
	s.researchFetch = func(ctx context.Context, urls []string, opt research.Options) []research.Fetched {
		return []research.Fetched{{URL: urls[0], FinalURL: urls[0], Title: "MP", Text: page}}
	}
	// The node's loop finished; only its structuring failed (the flagged defer).
	s.localAgent = func(ctx context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion, NodeID: "n", Seat: "seat-x",
			Output:   "The page says pinned staging memory is slower than the shared handle path.",
			Deferred: true, DeferClass: core.DeferClassInfrastructure, SchemaMiss: true, StopReason: "done",
			Reason: "structured re-pack unreachable: stalled: no progress for 120s in repack",
		}, nil
	}
	var rescues atomic.Int64
	s.rescue = func(ctx context.Context, c core.AgentContract, output string, budget time.Duration) (delegate.Rescued, error) {
		rescues.Add(1)
		return delegate.Rescued{
			Structured: json.RawMessage(`{"key_facts":["pinned staging memory is slower"],"numbers":[],"quotes":[],"verdict":"ok"}`),
			Seat:       "delegator-seat", How: "one re-pack completion",
		}, nil
	}

	res, err := s.handleResearch(context.Background(), callReq(`{"goal":"Which transfer path is slower?","urls":["https://docs.example.com/mp/"],"route":"local"}`))
	if err != nil {
		t.Fatal(err)
	}
	if rescues.Load() != 1 {
		t.Fatalf("rescue calls = %d, want the research door to hand the engine the rescue (one finished answer)", rescues.Load())
	}
	m := decodeResult(t, res)
	if summary, _ := m["summary"].(map[string]any); summary["succeeded"] != float64(1) {
		t.Fatalf("summary = %v, want the rescued digest counted as a success", m["summary"])
	}
	results, _ := m["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %v", m["results"])
	}
	r0, _ := results[0].(map[string]any)
	if note, _ := r0["repack_note"].(string); !strings.Contains(note, "rescued on delegator-seat") {
		t.Fatalf("repack_note = %q, want the rescue named", r0["repack_note"])
	}
}
