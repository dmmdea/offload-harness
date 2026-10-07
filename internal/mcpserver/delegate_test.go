// Task 6 (multi-node agent delegation): the agent_delegate MCP surface.
// Two load-bearing pins: (1) roast delta 13 — the tool registers ONLY when
// agent_delegation_enabled, and the flag adds EXACTLY that one tool (tools/list
// byte-identical otherwise); (2) roast delta 14 — the response leads with the
// summary (the raw JSON literally starts with "summary"), so eight quiet
// defers read as a loud outcome.

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// listTools connects a real MCP client to the built server over an in-memory
// transport and returns the advertised tool list — the same view Claude gets.
func listTools(t *testing.T, cfg config.Config) []*mcp.Tool {
	t.Helper()
	srv := New(pipeline.New(cfg, nil, nil, nil)).buildServer("test")
	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "pin", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	return res.Tools
}

func TestAgentDelegateRegistrationGated(t *testing.T) {
	off := listTools(t, config.Default()) // agent_delegation_enabled defaults false
	for _, tool := range off {
		if tool.Name == "agent_delegate" {
			t.Fatal("agent_delegate advertised with agent_delegation_enabled OFF")
		}
	}

	cfgOn := config.Default()
	cfgOn.AgentDelegationEnabled = true
	on := listTools(t, cfgOn)

	// The flag must add EXACTLY agent_delegate and change nothing else:
	// strip it from the on-list and byte-compare the remainder to the
	// off-list (the delta-13 "tools/list byte-identical when off" pin).
	var stripped []*mcp.Tool
	found := map[string]bool{}
	for _, tool := range on {
		// offload_research rides the same gate (it IS a delegation, fetched
		// delegator-side), so the flag adds exactly these two and nothing else.
		if tool.Name == "agent_delegate" || tool.Name == "offload_research" {
			found[tool.Name] = true
			continue
		}
		stripped = append(stripped, tool)
	}
	if !found["agent_delegate"] || !found["offload_research"] {
		t.Fatalf("agent_delegate/offload_research not both advertised with agent_delegation_enabled ON: %v", found)
	}
	offJSON, err := json.Marshal(off)
	if err != nil {
		t.Fatal(err)
	}
	strippedJSON, err := json.Marshal(stripped)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(offJSON, strippedJSON) {
		t.Fatal("enabling agent_delegation changed the tool list beyond adding agent_delegate")
	}
}

// delegateTestServer builds a Server with the delegation flag on, every side
// effect rooted in a temp dir, and a fake local runner injected through the
// localAgent seam.
func delegateTestServer(t *testing.T, local func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error), roster ...string) *Server {
	t.Helper()
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	cfg.AgentDelegationEnabled = true
	// roster is the configured fleet (delegate_remotes): a call's own remotes list may only narrow it.
	cfg.DelegateRemotes = roster
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.localAgent = local
	return s
}

func TestAgentDelegateHandlerLocalHappyPath(t *testing.T) {
	var gotContract core.AgentContract
	s := delegateTestServer(t, func(ctx context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		gotContract = c
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			NodeID:        "this-box",
			Seat:          "fake-seat",
			Output:        "done on zorblax",
			Structured:    json.RawMessage(`{"answer":"42"}`),
			Steps:         1,
			StopReason:    "done",
		}, nil
	})

	res, err := s.handleAgentDelegate(context.Background(), callReq(
		`{"subtasks":[{"goal":"answer it","output_schema":{"properties":{"answer":{"type":"string"}}},"acceptance":["contains:zorblax","nonempty:answer"],"max_steps":99}],"route":"local"}`))
	if err != nil {
		t.Fatalf("handleAgentDelegate: %v", err)
	}

	// Summary-first pin (roast delta 14): the raw JSON leads with the tally.
	raw := res.Content[0].(*mcp.TextContent).Text
	if !strings.HasPrefix(raw, `{"summary"`) {
		t.Fatalf("response must lead with the summary, got %q…", raw[:min(len(raw), 40)])
	}

	m := decodeResult(t, res)
	summary, _ := m["summary"].(map[string]any)
	if summary == nil || summary["succeeded"] != float64(1) {
		t.Fatalf("summary = %v, want one success", m["summary"])
	}
	results, _ := m["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %v", m["results"])
	}
	r0, _ := results[0].(map[string]any)
	if r0["node"] != "this-box" || r0["seat"] != "fake-seat" || r0["output"] != "done on zorblax" {
		t.Errorf("result = %v", r0)
	}
	if r0["deferred"] == true || r0["failed"] == true {
		t.Errorf("happy path marked deferred/failed: %v", r0)
	}

	// The handler's intake must have minted version/depth and clamped steps.
	if gotContract.SchemaVersion != core.AgentWireSchemaVersion || gotContract.Depth != 0 {
		t.Errorf("contract version/depth = %d/%d", gotContract.SchemaVersion, gotContract.Depth)
	}
	if gotContract.MaxSteps != core.AgentMaxStepsCap {
		t.Errorf("max_steps = %d, want clamped to %d", gotContract.MaxSteps, core.AgentMaxStepsCap)
	}
}

// TestAgentDelegateHandlerAcceptanceFailure: a schema-valid local result that
// fails acceptance surfaces as failed_verification with the check named —
// through the FULL handler path, not just the engine.
func TestAgentDelegateHandlerAcceptanceFailure(t *testing.T) {
	s := delegateTestServer(t, func(ctx context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: 1, NodeID: "this-box", Seat: "fake-seat",
			Output: "wrong content", Structured: json.RawMessage(`{"answer":"x"}`), StopReason: "done"}, nil
	})
	res, err := s.handleAgentDelegate(context.Background(), callReq(
		`{"subtasks":[{"goal":"answer it","acceptance":["contains:zorblax"]}],"route":"local"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := decodeResult(t, res)
	summary, _ := m["summary"].(map[string]any)
	if summary["failed_verification"] != float64(1) || summary["succeeded"] != float64(0) {
		t.Fatalf("summary = %v, want the acceptance failure counted", summary)
	}
}

// TestAgentDelegateHandlerLoudOnFailureAndInfrastructure (C-I): the loud-exit
// contract existed only on the CLI (delegateExitErr). handleAgentDelegate
// never set IsError, so both summary.failed > 0 and summary.infrastructure > 0
// came back as SUCCESSFUL tool calls — and the MCP caller is this lane's
// primary consumer. The JSON body must stay intact either way: the summary and
// the per-subtask reasons are the whole diagnosis.
func TestAgentDelegateHandlerLoudOnFailureAndInfrastructure(t *testing.T) {
	cases := []struct {
		name        string
		local       func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error)
		wantIsError bool
		wantKey     string
	}{
		{
			name: "a transport/config failure",
			local: func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
				return core.AgentWireResult{}, errors.New("planner endpoint refused")
			},
			wantIsError: true,
			wantKey:     "failed",
		},
		{
			name: "a defer that blames the stack",
			local: func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
				return core.AgentWireResult{SchemaVersion: 1, NodeID: "this-box", Seat: "fake-seat",
					Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "agent loop: llama-server 500"}, nil
			},
			wantIsError: true,
			wantKey:     "infrastructure",
		},
		{
			name: "an honest abstention (the control: still a success)",
			local: func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
				return core.AgentWireResult{SchemaVersion: 1, NodeID: "this-box", Seat: "fake-seat",
					Deferred: true, DeferClass: core.DeferClassAbstention, Reason: "output failed schema: missing answer"}, nil
			},
			wantIsError: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := delegateTestServer(t, tc.local)
			res, err := s.handleAgentDelegate(context.Background(), callReq(
				`{"subtasks":[{"goal":"answer it"}],"route":"local"}`))
			if err != nil {
				t.Fatalf("handleAgentDelegate: %v", err)
			}
			if res.IsError != tc.wantIsError {
				t.Fatalf("IsError = %v, want %v — a broken stack must not return as a successful tool call", res.IsError, tc.wantIsError)
			}
			m := decodeResult(t, res)
			summary, _ := m["summary"].(map[string]any)
			if summary == nil {
				t.Fatalf("the JSON body must survive the error flag: %v", m)
			}
			if tc.wantKey != "" && summary[tc.wantKey] != float64(1) {
				t.Fatalf("summary[%q] = %v, want 1 (%v)", tc.wantKey, summary[tc.wantKey], summary)
			}
			if results, _ := m["results"].([]any); len(results) != 1 {
				t.Fatalf("results = %v, want the per-subtask diagnosis intact", m["results"])
			}
		})
	}
}

func TestAgentDelegateHandlerBadInputsDefer(t *testing.T) {
	s := delegateTestServer(t, func(ctx context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		t.Error("local runner must not run on a refused request")
		return core.AgentWireResult{}, nil
	})
	nine := strings.Repeat(`{"goal":"g"},`, 8) + `{"goal":"g"}`
	cases := []struct{ name, args, want string }{
		{"no subtasks", `{"subtasks":[]}`, "subtask"},
		{"nine subtasks", `{"subtasks":[` + nine + `]}`, "8"},
		{"bad route", `{"subtasks":[{"goal":"g"}],"route":"sideways"}`, "route"},
		{"public remote", `{"subtasks":[{"goal":"g"}],"remotes":["https://api.evil.com"]}`, "api.evil.com"},
		{"invalid acceptance", `{"subtasks":[{"goal":"g","acceptance":["contains:"]}]}`, "acceptance"},
		{"missing goal", `{"subtasks":[{"context":[{"name":"a","text":"b"}]}]}`, "goal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.handleAgentDelegate(context.Background(), callReq(tc.args))
			if err != nil {
				t.Fatalf("MCP error (want deferred-shape): %v", err)
			}
			m := decodeResult(t, res)
			if m["deferred"] != true {
				t.Fatalf("want deferred:true, got %v", m)
			}
			if reason, _ := m["reason"].(string); !strings.Contains(reason, tc.want) {
				t.Fatalf("reason %q must mention %q", reason, tc.want)
			}
		})
	}
}

// TestDelegateIsErrorRequiresNothingUsableCameBack (R4-8, amended by C-75): IsError fired on
// `Infrastructure > 0` alone, and Infrastructure counts a SUCCESSFUL local
// placement taken while the fleet was down (delegate.Summary's
// remotesUnreachable). So a run whose every subtask completed, validated against
// its schema, and passed acceptance returned IsError:true — and in MCP semantics
// that says THE CALL FAILED, which most models answer by discarding or redoing
// correct work.
//
// This is the one place where "default to loud" cuts the other way: the
// fleet-down signal must stay fully visible (it is in the body, and the CLI still
// exits non-zero on it), but a boolean that means "your call failed" must not be
// set on work that succeeded. A CLI exit code can carry that nuance beside the
// printed results; a boolean cannot.
//
// R5-2 — the round-4 fix expressed that motivation as `Succeeded == 0`, which
// silenced far more than it meant to: a subtask eaten by a box with a dead
// llama-server came back as a clean tool call the moment any sibling succeeded,
// while `local-offload delegate` exited NON-ZERO on the identical run. R5-2
// therefore flagged the whole call whenever ANY subtask failed or was lost to
// the stack.
//
// C-75 reverses that sibling half, for the reason R4-8 already gave: the flag
// that means "your call failed" was being set on a call that DELIVERED. The cost
// was concrete — the MCP client keeps only the head and tail of an error-flagged
// body, so every partial research reply the workers saw had its middle (the
// digests that DID succeed) cut out. A partial result is a successful call whose
// body says which subtasks failed: the loss stays fully visible
// (summary.failed / summary.lost_to_stack and each subtask's own `failed` /
// `defer_class` / `reason`), and the flag is kept for the two cases it is true
// of — NOTHING succeeded, or the run skipped work outright. The CLI keeps its
// wider exit-code rule; only the boolean narrowed.
func TestDelegateIsErrorRequiresNothingUsableCameBack(t *testing.T) {
	cases := []struct {
		name string
		sum  delegate.Summary
		want bool
	}{
		{"a local success taken while the whole fleet was down", delegate.Summary{Succeeded: 1, Infrastructure: 1}, false},
		{"nothing came back and the stack is why", delegate.Summary{Deferred: 1, Infrastructure: 1, LostToStack: 1}, true},
		// C-75: a sibling succeeding makes the result PARTIAL, and a partial result
		// is not a failed call. The lost subtask is still named in the body.
		{"a sibling succeeded, but a subtask was lost to a broken box (partial)", delegate.Summary{Succeeded: 1, Deferred: 1, Infrastructure: 1, LostToStack: 1}, false},
		// Why `Deferred > 0 && Infrastructure > 0` is NOT a safe proxy for a lost
		// subtask: this run's Infrastructure comes from a fleet-down LOCAL
		// SUCCESS and its defer is contract-classed (the caller has a contract to
		// fix, the fleet is fine). Nothing was lost to the stack, so the proxy
		// would fire on finished work — the exact defect R4-8 removed.
		{"a contract-classed defer beside a fleet-down local success", delegate.Summary{Succeeded: 1, Deferred: 1, Infrastructure: 1}, false},
		{"seven subtasks succeeded and one failed (partial)", delegate.Summary{Succeeded: 7, Failed: 1}, false},
		{"a subtask failed and nothing succeeded", delegate.Summary{Failed: 1}, true},
		{"every subtask failed", delegate.Summary{Failed: 3}, true},
		{"a failure and a stack loss and nothing succeeded", delegate.Summary{Failed: 1, Deferred: 1, Infrastructure: 1, LostToStack: 1}, true},
		{"chunks were never attempted and nothing succeeded", delegate.Summary{Skipped: 4}, true},
		{"chunks were never attempted after eight subtasks succeeded (partial)", delegate.Summary{Succeeded: 8, Skipped: 4}, false},
		{"honest abstentions only", delegate.Summary{Succeeded: 1, Deferred: 1}, false},
		{"failed verification is a RESULT shape", delegate.Summary{FailedVerification: 2}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := delegateIsError(tc.sum); got != tc.want {
				t.Fatalf("delegateIsError(%+v) = %v, want %v", tc.sum, got, tc.want)
			}
		})
	}
}

// TestAgentDelegateHandlerAPartialResultIsNotAToolError (C-75, end to end; it
// replaces R5-2's "a subtask lost to the stack is always loud"): one subtask
// completes, another defers blaming the stack. R5-2 flagged the whole call for
// that, and the MCP client answers an error-flagged body by keeping only its head
// and tail — the completed subtask's digest sat in the middle and was cut.
//
// The half that is lost stays loud IN THE BODY, which is where a caller reads it:
// summary.lost_to_stack, summary.infrastructure and that subtask's own
// defer_class / reason. The flag that means "the call failed" is not set on a
// call that delivered a usable result. The two-row control below pins the other
// direction: with NOTHING succeeded the flag stays.
func TestAgentDelegateHandlerAPartialResultIsNotAToolError(t *testing.T) {
	cases := []struct {
		name string
		// second is what the subtask that does NOT succeed returns.
		second func() (core.AgentWireResult, error)
		want   map[string]float64 // summary keys the body must still report
	}{
		{
			name: "a sibling lost to a broken box",
			second: func() (core.AgentWireResult, error) {
				return core.AgentWireResult{SchemaVersion: 1, NodeID: "this-box", Seat: "fake-seat",
					Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "agent loop: llama-server 500"}, nil
			},
			want: map[string]float64{"succeeded": 1, "infrastructure": 1, "lost_to_stack": 1},
		},
		{
			name: "a sibling that failed outright",
			second: func() (core.AgentWireResult, error) {
				return core.AgentWireResult{}, errors.New("planner endpoint refused")
			},
			want: map[string]float64{"succeeded": 1, "failed": 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			s := delegateTestServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
				if calls.Add(1) == 1 {
					return core.AgentWireResult{SchemaVersion: 1, NodeID: "this-box", Seat: "fake-seat",
						Output: "done on the seat", StopReason: "done"}, nil
				}
				return tc.second()
			})
			res, err := s.handleAgentDelegate(context.Background(), callReq(
				`{"subtasks":[{"goal":"answer it"},{"goal":"answer it too"}],"route":"local"}`))
			if err != nil {
				t.Fatalf("handleAgentDelegate: %v", err)
			}
			if res.IsError {
				t.Fatalf("IsError = true on a PARTIAL result: one subtask delivered, and the client cuts the middle out of an error-flagged body")
			}
			// The loss is still fully reported: the counts that describe it and one
			// result per subtask, the failed one carrying its own reason.
			m := decodeResult(t, res)
			summary, _ := m["summary"].(map[string]any)
			for k, want := range tc.want {
				if summary[k] != want {
					t.Fatalf("summary[%q] = %v, want %v — a partial result must still say what was lost (%v)", k, summary[k], want, summary)
				}
			}
			results, _ := m["results"].([]any)
			if len(results) != 2 {
				t.Fatalf("results = %v, want both subtasks' outcomes in the body", m["results"])
			}
			var lostOne int
			for _, r := range results {
				rm, _ := r.(map[string]any)
				if rm["failed"] == true || rm["defer_class"] == core.DeferClassInfrastructure {
					lostOne++
					if reason, _ := rm["reason"].(string); reason == "" {
						t.Fatalf("the lost subtask carries no reason: %v", rm)
					}
				}
			}
			if lostOne != 1 {
				t.Fatalf("results = %v, want exactly one subtask marked failed or infrastructure-deferred", results)
			}
		})
	}
}

// TestAgentDelegateHandlerNothingSucceededStaysAnError is the control for the
// test above and for C-75's other half: when NOTHING succeeded and something
// failed or was lost to the stack, the call did fail, so the flag stays.
func TestAgentDelegateHandlerNothingSucceededStaysAnError(t *testing.T) {
	cases := []struct {
		name  string
		local func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error)
	}{
		{"every subtask lost to the stack", func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
			return core.AgentWireResult{SchemaVersion: 1, NodeID: "this-box", Seat: "fake-seat",
				Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: "agent loop: llama-server 500"}, nil
		}},
		{"every subtask failed", func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
			return core.AgentWireResult{}, errors.New("planner endpoint refused")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := delegateTestServer(t, tc.local)
			res, err := s.handleAgentDelegate(context.Background(), callReq(
				`{"subtasks":[{"goal":"answer it"},{"goal":"answer it too"}],"route":"local"}`))
			if err != nil {
				t.Fatalf("handleAgentDelegate: %v", err)
			}
			if !res.IsError {
				t.Fatalf("IsError = false although nothing succeeded and every subtask was lost or failed")
			}
			if m := decodeResult(t, res); m["summary"] == nil {
				t.Fatalf("the JSON body must survive the error flag: %v", m)
			}
		})
	}
}

// TestAgentDelegateHandlerAcceptanceLint: the intake lint's warnings ride the
// response per subtask, through the FULL handler path — a parrot-passable
// acceptance (its only content check also matches the goal) is flagged, and a
// discriminating one on the same call is not. Position must line up with
// submission order, because that is the only key the caller has.
func TestAgentDelegateHandlerAcceptanceLint(t *testing.T) {
	s := delegateTestServer(t, func(ctx context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "this-box",
			Seat: "fake-seat", Output: "mentions zorblax and 412", StopReason: "done"}, nil
	})
	res, err := s.handleAgentDelegate(context.Background(), callReq(
		`{"subtasks":[`+
			`{"goal":"does the doc mention zorblax?","context":[{"name":"d.md","text":"zorblax is the workstation"}],"acceptance":["contains:zorblax"]},`+
			`{"goal":"how many pallets?","context":[{"name":"d.md","text":"Rotterdam holds 412 pallets"}],"acceptance":["contains:412"]}`+
			`],"route":"local"}`))
	if err != nil {
		t.Fatalf("handleAgentDelegate: %v", err)
	}
	m := decodeResult(t, res)
	results, _ := m["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %v", m["results"])
	}
	r0, _ := results[0].(map[string]any)
	lint0, _ := r0["acceptance_lint"].([]any)
	if len(lint0) != 1 || !strings.Contains(lint0[0].(string), "PARROT-PASSABLE") {
		t.Errorf("subtask 0 (echoable contains:zorblax) lint = %v, want one PARROT-PASSABLE warning", r0["acceptance_lint"])
	}
	r1, _ := results[1].(map[string]any)
	if _, present := r1["acceptance_lint"]; present {
		t.Errorf("subtask 1 (grounded, non-echoable) carries a lint: %v", r1["acceptance_lint"])
	}
}
