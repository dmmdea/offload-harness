package delegate

// A node names itself two ways. /fleet/health publishes the id fleet-serve resolved (--node-id, else fleet_node_id, else the
// hostname), and every placement text, progress line and queue claim uses it. The result envelope the node stamps on its own
// answer used to carry another name for a node launched with --node-id (the installers' form): the OS hostname. The delegator
// copied the envelope into results[].node, so one result said one node in its `node` and another in its `placement`.
//
// results[].node is the id the placement used. results[].node_reported (additive, omitempty) is the id the node's own result
// carried, present only when it differs. These tests use placeholder names throughout: "node-id-aaaa" is what the node's health
// publishes and "host-bbbb" is what its result envelope says.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

const (
	nidHealth   = "node-id-aaaa" // the id the node publishes in /fleet/health
	nidEnvelope = "host-bbbb"    // the id the node stamped on its own result
)

// divergentNode is an eligible fleet node that publishes healthID and completes every job with a result envelope naming
// envelopeID. The output satisfies remoteContract's acceptance.
func divergentNode(t *testing.T, healthID, envelopeID string) (*fakeNode, string) {
	t.Helper()
	return acceptingNode(t, healthID, "zorblax from the divergent node", func(f *fakeNode) {
		f.pollByJob = func(jobID string, n int64) (map[string]any, int) {
			w := remoteWire("zorblax from the divergent node", `{"answer":"zorblax from the divergent node"}`)
			w.NodeID = envelopeID
			return doneWire(t, w), http.StatusOK
		}
	})
}

// delegationLogNodes returns the `node` of every delegation-log line cfg's run wrote.
func delegationLogNodes(t *testing.T, cfg config.Config) []string {
	t.Helper()
	dir := filepath.Join(cfg.BaseDir(), "delegation-log")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("delegation-log dir: %v", err)
	}
	var nodes []string
	for _, e := range entries {
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatal(rerr)
		}
		for _, ln := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if strings.TrimSpace(ln) == "" {
				continue
			}
			var line struct {
				Node string `json:"node"`
			}
			if err := json.Unmarshal([]byte(ln), &line); err != nil {
				t.Fatalf("delegation-log line: %v (%s)", err, ln)
			}
			nodes = append(nodes, line.Node)
		}
	}
	return nodes
}

// wireRowOf publishes results through WireResponse and returns the first row as the keys the wire carries (the assertions
// read the JSON, not the struct).
func wireRowOf(t *testing.T, results []PlacedResult, sum Summary) map[string]any {
	t.Helper()
	raw, err := json.Marshal(WireResponse(results, sum, nil))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || len(got.Results) == 0 {
		t.Fatalf("response %s: %v", raw, err)
	}
	return got.Results[0]
}

// The defect: a node whose health says one id and whose result says another. The result names the node by the id the
// placement used, keeps the envelope untouched, and publishes the envelope id beside it.
func TestARemoteResultNamesTheNodeByTheIdItsHealthPublishes(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, url := divergentNode(t, nidHealth, nidEnvelope)
	cfg := testCfg(t)

	results, sum, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 || sum.Succeeded != 1 {
		t.Fatalf("results=%d summary=%+v, want one success", len(results), sum)
	}
	pr := results[0]
	// Controls, green before the change: the placement named the health id, and the node's envelope is delivered as it came.
	if !strings.Contains(pr.PlacementReason, nidHealth) {
		t.Errorf("placement = %q, want it to name %s (the id the node publishes)", pr.PlacementReason, nidHealth)
	}
	if pr.Result.NodeID != nidEnvelope {
		t.Errorf("result envelope node_id = %q, want %q untouched: the delegator reports what a node said", pr.Result.NodeID, nidEnvelope)
	}
	// The change: the result and its placement name the node alike.
	if pr.Node != nidHealth {
		t.Errorf("results[].node = %q, want %q: the id the placement used, not the id the node stamped on its result", pr.Node, nidHealth)
	}
	row := wireRowOf(t, results, sum)
	if row["node"] != nidHealth {
		t.Errorf("wire node = %v, want %q", row["node"], nidHealth)
	}
	if row["node_reported"] != nidEnvelope {
		t.Errorf("wire node_reported = %v, want %q: the id the node's own result carried", row["node_reported"], nidEnvelope)
	}
	// The delegation log carries the same node name the response does.
	if got := delegationLogNodes(t, cfg); len(got) != 1 || got[0] != nidHealth {
		t.Errorf("delegation-log node = %v, want [%s]", got, nidHealth)
	}
}

// The fallback the change keeps: a node whose health published no id is named by its result, as it always was. Green before
// the change and after it; it turns red under an unconditional overwrite of the node by the (empty) placement id.
func TestAResultFromANodeWhoseHealthNamesNoIdKeepsTheEnvelopeId(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, url := divergentNode(t, "", nidEnvelope)

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 || sum.Succeeded != 1 {
		t.Fatalf("results=%d summary=%+v, want one success", len(results), sum)
	}
	if results[0].Node != nidEnvelope {
		t.Fatalf("results[].node = %q, want %q: health named no id, so the result's own is the only name there is", results[0].Node, nidEnvelope)
	}
	if _, present := wireRowOf(t, results, sum)["node_reported"]; present {
		t.Errorf("node_reported is on a row whose node IS the envelope id: nothing differs, so nothing is reported")
	}
}

// One job's dispatch marker and its finished row name the node by the same id (the marker already used the health id; the
// finished row used the envelope's).
func TestTheStartedMarkerAndTheFinishedRowNameTheNodeAlike(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, url := divergentNode(t, nidHealth, nidEnvelope)
	cfg := testCfg(t)

	if _, _, err := Run(t.Context(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{url}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var started, finished []ledger.Entry
	for _, e := range readRows(t, cfg.LedgerPath) {
		if e.Task != "agent_delegate" {
			continue
		}
		if e.Phase == ledger.PhaseStarted {
			started = append(started, e)
		} else {
			finished = append(finished, e)
		}
	}
	if len(started) != 1 || len(finished) != 1 {
		t.Fatalf("agent_delegate rows: %d started marker(s), %d finished row(s), want one of each", len(started), len(finished))
	}
	tierNode := func(e ledger.Entry) string { return strings.SplitN(e.ModelTier, ":", 2)[0] }
	if got := tierNode(started[0]); got != nidHealth {
		t.Errorf("started marker model_tier node = %q, want %q (control: the marker names the placement id)", got, nidHealth)
	}
	if got := tierNode(finished[0]); got != nidHealth {
		t.Errorf("finished row model_tier node = %q, want %q: the same node the marker named", got, nidHealth)
	}
}

// The schema-miss rescue is handed the node that wrote the answer; the node it names is the placement id.
func TestTheSchemaMissRescueIsHandedThePlacementId(t *testing.T) {
	wire := legacyRepackStall()
	// The node's re-pack was never sent (class budget), so the origin is handed (a seat that failed its re-pack is not).
	wire.SchemaMiss, wire.DeferClass = true, core.DeferClassBudget
	wire.Reason = "structured re-pack skipped: 4 s left to the call's deadline buys 403 tokens < the answer's 1064"
	wire.NodeID = nidEnvelope // rescueNode's health publishes "fake-node"
	o, ok := originHanded(t, wire)
	if !ok {
		t.Fatalf("the rescue was handed no origin: the fixture is not the case under test")
	}
	if o.Seat != wire.Seat {
		t.Errorf("origin seat = %q, want %q (control: the origin is the node that wrote the answer)", o.Seat, wire.Seat)
	}
	if o.Node != "fake-node" {
		t.Errorf("origin node = %q, want %q: the id the placement used, not the one the result carried (%s)", o.Node, "fake-node", nidEnvelope)
	}
}

// A subtask re-placed after a refusal flows through the same block, so it carries the same rule.
func TestAReplacedResultAlsoNamesThePlacementId(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	_, fullURL := refusingNode(t, "node-full", http.StatusServiceUnavailable, nil)
	_, url := divergentNode(t, nidHealth, nidEnvelope)

	results, _, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{remoteContract()}, "remote", []string{fullURL, url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results=%d, want one", len(results))
	}
	if results[0].Replacements != 1 {
		t.Fatalf("replacements=%d, want 1 (control: the re-placement path ran)", results[0].Replacements)
	}
	if results[0].Node != nidHealth {
		t.Errorf("results[].node = %q, want %q after a re-placement", results[0].Node, nidHealth)
	}
}

// A local run is named by its placement when the placement carries a name of its own: an endpoint that is another box's
// engine is that box's work (register C-58), and the runner's envelope (which knows nothing of the endpoint) does not
// overrule it. A run on this box's own engine keeps the runner's envelope name, as it always did.
func TestALocalRunNamesTheNodeByThePlacementNotByTheRunnersEnvelope(t *testing.T) {
	runner := LocalRunner(func(ctx context.Context, ac core.AgentContract, _ LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "done", Seat: "a2-pool", NodeID: "envelope-local"}, nil
	})
	run := func(t *testing.T, endpoint string) PlacedResult {
		t.Helper()
		cfg := testCfg(t)
		cfg.Endpoint = endpoint
		cfg.AgentModel = "a2-pool"
		res, sum, err := RunWith(context.Background(), cfg, runner, []core.AgentContract{{Goal: "say done"}}, "local", nil, nil)
		if err != nil || sum.Succeeded != 1 || len(res) != 1 {
			t.Fatalf("RunWith: err=%v succeeded=%d results=%d", err, sum.Succeeded, len(res))
		}
		return res[0]
	}

	t.Run("an endpoint that is another box's engine names that box", func(t *testing.T) {
		pr := run(t, "http://node-b:18797")
		if !strings.Contains(pr.PlacementReason, "attributed there") {
			t.Fatalf("placement = %q, want the endpoint host named as the engine's box (control)", pr.PlacementReason)
		}
		if pr.Node != "node-b" {
			t.Errorf("node = %q, want the endpoint's host node-b: the runner's envelope name (envelope-local) does not know the engine is another box's", pr.Node)
		}
	})

	t.Run("this box's own engine keeps the runner's envelope name", func(t *testing.T) {
		pr := run(t, "http://127.0.0.1:11434")
		if strings.Contains(pr.PlacementReason, "attributed there") {
			t.Fatalf("placement = %q: a loopback endpoint is not another box (control)", pr.PlacementReason)
		}
		if pr.Node != "envelope-local" {
			t.Errorf("node = %q, want envelope-local: nothing in this placement names a box other than the runner's own", pr.Node)
		}
	})
}
