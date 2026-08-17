// THE cross-package delegation test.
//
// Every other test in this arc fakes its neighbour: the MCP handler fakes the
// local runner, delegate.Run fakes the whole node over httptest, fleetnode
// fakes the pipeline with a Runner stub, and the pipeline fakes the seat. Each
// layer is green against its own idea of the layer below, and NOTHING proves
// two of them agree — rename jobWire's `data` field and every one of those
// suites stays green while every real delegation fails.
//
// So this test fakes exactly ONE thing, the leaf llama seat, and runs
// production code the whole way down:
//
//	handleAgentDelegate (MCP)
//	  → delegate.PrepareContract + delegate.Run (placement, dispatch, poll,
//	    delegator-side acceptance)
//	    → real HTTP → the REAL fleetnode mux (auth, envelope decode,
//	      buildAgentRun's materialization, the Jobs store, the poll endpoint)
//	      → a REAL pipeline.Pipeline → runAgentTask → agent.Build's loop
//	        → the faked seat
//
// It is the first and only place that checks the three claims each layer
// merely asserts about the other: that the node a result NAMES is the node
// health advertised and the node that actually ran it; that the quarantine
// holds end to end (transcript content produced on the node never reaches the
// caller's bytes); and that a materialized job leaves nothing on disk.

package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	integrationSeat  = "fleet-agent-seat"
	integrationNode  = "integration-node"
	integrationToken = "fleet-integration-token"

	// markerTurn is emitted ONLY inside a tool turn on the remote node — an
	// intermediate assistant message that exists nowhere but that node's
	// transcript. Quarantine (§S2, "no transcript crosses the wire") means it
	// must never appear in the delegator's response bytes. Nothing but an
	// end-to-end run can check that: the wire types make it structurally
	// impossible, and "structurally impossible" is exactly the kind of claim
	// that quietly stops being true.
	markerTurn = "QUARANTINE-TRANSCRIPT-7f3a91"
	// markerDoc rides in a context doc the node's loop READS through the
	// read_file tool, so it lands in a tool RESULT. The delegator sent it, so
	// its absence proves a narrower thing than markerTurn — but it is the half
	// an operator worries about (context bytes echoed back at N× the cost).
	markerDoc = "QUARANTINE-TOOLRESULT-91be22"

	// finalAnswer is the only node-side text that is ALLOWED to cross back.
	finalAnswer = "The fleet node answered: 42."
)

// integrationSeatServer is the one fake: an OpenAI-compatible seat.
//   - /v1/models publishes the roster the node's residency probe and
//     runAgentTask's own seat check read.
//   - /v1/chat/completions answers the agent LOOP (requests carrying "tools")
//     and the structured re-pack (requests carrying "grammar"), told apart by
//     shape exactly as internal/pipeline's own fake does.
//
// Everything else (/props, /tokenize) 404s, which every consumer fails open on.
func integrationSeatServer(t *testing.T, loopCalls *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, integrationSeat)
		case "/v1/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "application/json")
			if _, hasTools := body["tools"]; hasTools {
				switch loopCalls.Add(1) {
				case 1:
					// Turn 1: an assistant message whose CONTENT carries the
					// marker, plus a read_file call over the job's context dir.
					// Both the content and the tool result it produces live only
					// in the node's transcript.
					fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q,`+
						`"tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"notes.md\"}"}}]},`+
						`"finish_reason":"tool_calls"}]}`, "planning: "+markerTurn)
				default:
					// Turn 2: the final answer. Clean — no marker.
					b, _ := json.Marshal(finalAnswer)
					fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%s},"finish_reason":"stop"}]}`, b)
				}
				return
			}
			if g, _ := body["grammar"].(string); g == "" {
				t.Errorf("chat request with neither tools nor grammar: %v", body)
			}
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{\"answer\":\"42\"}"},"finish_reason":"stop"}],`+
				`"usage":{"prompt_tokens":11,"completion_tokens":5}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// integrationNodeConfig is a real opted-in fleet node pointed at the fake seat.
func integrationNodeConfig(t *testing.T, seatURL string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Home = t.TempDir() // BaseDir() → the materialized job dir lands in a test-owned tree
	cfg.Endpoint = seatURL
	cfg.Model = "workhorse"
	cfg.AgentModel = integrationSeat
	cfg.AgentCtxTokens = 16384
	cfg.FleetNodeID = integrationNode
	cfg.FleetAgentEnabled = true
	cfg.FleetAuthToken = integrationToken
	cfg.Temperature = 0.1
	return cfg
}

// startIntegrationNode boots the REAL fleetnode mux over a REAL pipeline and
// returns its base URL plus the node config (for the on-disk assertions).
func startIntegrationNode(t *testing.T, seatURL string) (base string, nodeCfg config.Config) {
	t.Helper()
	nodeCfg = integrationNodeConfig(t, seatURL)
	nodePipeline := pipeline.New(nodeCfg, llamaclient.New(seatURL, "", nodeCfg.Model, 30*time.Second), nil, nil)

	jobs := fleetnode.NewJobs(time.Hour)
	t.Cleanup(func() { jobs.DrainAndStop(5 * time.Second) })
	node := fleetnode.New(nodePipeline, jobs, fleetnode.Options{
		NodeID:     nodeCfg.FleetNodeID,
		Snapshot:   func() (fleetnode.Snapshot, bool) { return fleetnode.Snapshot{TotalGiB: 16, FreeGiB: 12, At: time.Now()}, true },
		Footprints: func() []fleetnode.FootprintEntry { return nil },
		GpuVendor:  "nvidia",
		GpuArch:    "ampere",
		// httptest binds 127.0.0.1, so this is the truthful resolved answer —
		// and a token is configured anyway, which is what admits the lane.
		LoopbackListener: true,
		Cfg:              nodeCfg,
	})
	// Warm the residency cache deterministically: a cold cache advertises
	// agent_seat_resident:false (fail-closed, by design), which would gate the
	// placement out before any of this test's real subject matter ran. The
	// background trigger itself is covered in fleetnode's own suite.
	node.RefreshAgentResidency()

	srv := httptest.NewServer(node.Handler())
	t.Cleanup(srv.Close)
	return srv.URL, nodeCfg
}

// integrationDelegator is a real MCP Server with the delegation role on and a
// local runner that FAILS the test — this run must land on the fleet node.
func integrationDelegator(t *testing.T, token string) *Server {
	t.Helper()
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	cfg.AgentDelegationEnabled = true
	cfg.FleetAuthToken = token
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.localAgent = func(context.Context, core.AgentContract) (core.AgentWireResult, error) {
		t.Error("local runner ran: route=remote must place this on the fleet node")
		return core.AgentWireResult{}, nil
	}
	return s
}

// advertisedIdentity reads what /fleet/health PUBLISHES about the node — the
// only thing a delegator ever knows about it before dispatching.
func advertisedIdentity(t *testing.T, base string) (nodeID, seat string) {
	t.Helper()
	resp, err := http.Get(base + "/fleet/health")
	if err != nil {
		t.Fatalf("health GET: %v", err)
	}
	defer resp.Body.Close()
	var h struct {
		NodeID    string `json:"node_id"`
		AgentSeat string `json:"agent_seat"`
		Resident  bool   `json:"agent_seat_resident"`
		Enabled   bool   `json:"agent_enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("health decode: %v", err)
	}
	if !h.Enabled || !h.Resident {
		t.Fatalf("node did not advertise a placeable agent lane: %+v", h)
	}
	return h.NodeID, h.AgentSeat
}

func integrationArgs(base string) string {
	return fmt.Sprintf(`{
		"subtasks": [{
			"goal": "read the notes and answer",
			"context": [{"name": "notes.md", "text": %q}],
			"output_schema": {"properties": {"answer": {"type": "string"}}},
			"acceptance": ["nonempty:answer", "contains:42"],
			"timeout_sec": 60
		}],
		"route": "remote",
		"remotes": [%q]
	}`, "the answer is 42 // "+markerDoc, base)
}

// TestDelegationEndToEndAcrossPackages is the whole chain, faking only the seat.
func TestDelegationEndToEndAcrossPackages(t *testing.T) {
	var loopCalls atomic.Int64
	seat := integrationSeatServer(t, &loopCalls)
	base, nodeCfg := startIntegrationNode(t, seat.URL)
	wantNode, wantSeat := advertisedIdentity(t, base)

	s := integrationDelegator(t, integrationToken)
	res, err := s.handleAgentDelegate(context.Background(), callReq(integrationArgs(base)))
	if err != nil {
		t.Fatalf("handleAgentDelegate: %v", err)
	}
	raw := res.Content[0].(*mcp.TextContent).Text

	m := decodeResult(t, res)
	summary, _ := m["summary"].(map[string]any)
	if summary == nil || summary["succeeded"] != float64(1) {
		t.Fatalf("summary = %v, want exactly one success — full response: %s", m["summary"], raw)
	}
	results, _ := m["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %v", m["results"])
	}
	r0, _ := results[0].(map[string]any)

	// (1) The node a result NAMES is the node health ADVERTISED and the node
	// that actually RAN it. Three separate claims that no test could compare
	// before, because no test ever had all three in one process.
	if r0["node"] != wantNode {
		t.Errorf("results[0].node = %v, want %q — the node health advertised", r0["node"], wantNode)
	}
	if r0["seat"] != wantSeat {
		t.Errorf("results[0].seat = %v, want %q — the seat health advertised", r0["seat"], wantSeat)
	}
	if wantSeat != integrationSeat {
		t.Errorf("advertised seat = %q, want the seat the node is configured to RUN (%q)", wantSeat, integrationSeat)
	}
	if n := loopCalls.Load(); n < 2 {
		t.Errorf("agent loop turns on the node = %d, want ≥2 — the result must come from a real loop, not a stub", n)
	}

	// (2) The structured output survived the whole round trip and validates.
	structured, _ := r0["structured"].(map[string]any)
	if structured == nil || structured["answer"] != "42" {
		t.Errorf("structured = %v, want the schema-validated re-pack {\"answer\":\"42\"}", r0["structured"])
	}
	if r0["output"] != finalAnswer {
		t.Errorf("output = %v, want %q", r0["output"], finalAnswer)
	}
	if r0["deferred"] == true || r0["failed"] == true {
		t.Errorf("happy path marked deferred/failed: %v", r0)
	}
	if af, has := r0["acceptance_failures"]; has {
		t.Errorf("acceptance_failures = %v, want none (delegator-side checks passed)", af)
	}

	// (3) QUARANTINE, asserted end to end for the first time: content that
	// existed only inside a tool turn on the remote node must not appear
	// ANYWHERE in the bytes the caller receives.
	if strings.Contains(raw, markerTurn) {
		t.Errorf("the remote transcript leaked into the MCP response: %q found in %s", markerTurn, raw)
	}
	if strings.Contains(raw, markerDoc) {
		t.Errorf("a context doc's bytes came back in the MCP response: %q found in %s", markerDoc, raw)
	}

	// (4) Nothing left on the node's disk. buildAgentRun mints a job dir and
	// writes the context docs into it; the dispatch handler's cleanup closure
	// must remove the whole thing once the run ends.
	jobsRoot := filepath.Join(nodeCfg.BaseDir(), "pipeline-jobs")
	entries, rerr := os.ReadDir(jobsRoot)
	if rerr != nil && !os.IsNotExist(rerr) {
		t.Fatalf("reading %s: %v", jobsRoot, rerr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("job dirs left under %s after the run: %v", jobsRoot, names)
	}
}

// TestDelegationEndToEndRejectsAMissingToken is the same chain with the
// delegator's credential dropped: the node's ack-time guard must refuse, and
// the refusal must arrive as a FAILURE (a human has to act) — never as a quiet
// defer, which is what a caller reads as "the model tried and abstained".
func TestDelegationEndToEndRejectsAMissingToken(t *testing.T) {
	var loopCalls atomic.Int64
	seat := integrationSeatServer(t, &loopCalls)
	base, _ := startIntegrationNode(t, seat.URL)
	advertisedIdentity(t, base) // health stays OPEN: the lane is advertised either way

	s := integrationDelegator(t, "") // no fleet_auth_token on the delegator
	res, err := s.handleAgentDelegate(context.Background(), callReq(integrationArgs(base)))
	if err != nil {
		t.Fatalf("handleAgentDelegate: %v", err)
	}
	m := decodeResult(t, res)
	summary, _ := m["summary"].(map[string]any)
	if summary == nil || summary["failed"] != float64(1) {
		t.Fatalf("summary = %v, want exactly one FAILED subtask (an auth refusal is not a defer)", m["summary"])
	}
	if summary["succeeded"] != float64(0) || summary["deferred"] != float64(0) {
		t.Fatalf("summary = %v, want nothing counted as succeeded or deferred", summary)
	}
	results, _ := m["results"].([]any)
	r0, _ := results[0].(map[string]any)
	reason, _ := r0["reason"].(string)
	if !strings.Contains(reason, "401") {
		t.Errorf("reason = %q, want the node's 401 carried through verbatim", reason)
	}
	if n := loopCalls.Load(); n != 0 {
		t.Errorf("agent loop turns = %d, want 0 — a rejected dispatch must never reach the seat", n)
	}
}
