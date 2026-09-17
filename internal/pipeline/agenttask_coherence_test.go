// agenttask_coherence_test.go — the post-warm seat coherence probe (register
// D-118), through the SAME faked endpoint runAgentTask's other admission tests
// use (agentFake). The probe is recognised by its shape, so the loop call
// indexing every older test pins is untouched.
//
// The live defect these pin: a vLLM seat that passed /health, /v1/models, the
// speed probe and the READY smoke and then answered every contract with
// `<tool_call>!!!!!!!!!!!!!!!!!!!!…` for 126–336 s (2026-09-16/17).
package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// coherenceTestPipeline is admissionTestPipeline plus the probe policy under
// test: the key is a per-box config, so a test that does not set it exercises
// the default ("cold").
func coherenceTestPipeline(t *testing.T, base string, admissionSec int, policy string) *Pipeline {
	t.Helper()
	cfg := config.Config{
		Endpoint:              base,
		Model:                 "workhorse",
		AgentModel:            agentTestSeat,
		FleetNodeID:           "node-t",
		Temperature:           0.1,
		AgentAdmissionWaitSec: admissionSec,
		AgentCoherenceProbe:   policy,
	}
	return New(cfg, llamaclient.New(base, "", cfg.Model, 30*time.Second), nil, nil)
}

// coldLoadFake is the shape the warm-up treats as a cold load: /running lists
// nothing until the passthrough GET has answered, then lists the seat ready.
func coldLoadFake(probe func(int64) string) *agentFake {
	return &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		probe:     probe,
		running: func(n int64) string {
			if n <= 2 { // the admission poll and the warm-up's own check: absent
				return `{"running":[]}`
			}
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}`
		},
		upstreamModels: func(int64) string {
			return `{"object":"list","data":[{"id":"` + agentTestSeat + `","max_model_len":131072}]}`
		},
	}
}

// degenerateChat is the NaN completion the live seat produced: the tool-call
// marker followed by token-0 spam, no parsed tool calls.
func degenerateChat() string {
	return `{"choices":[{"message":{"role":"assistant","content":"<tool_call>` + strings.Repeat("!", 40) +
		`"},"finish_reason":"length"}],"usage":{"prompt_tokens":40,"completion_tokens":96}}`
}

// TestRunAgentTaskDefersAnIncoherentSeatAfterColdLoad: the seat cold-loads,
// the probe asks its one question, the seat answers with the NaN shape — the
// contract defers as INFRASTRUCTURE before the loop ever starts. The load-bearing
// assertion is the call count: the loop must not have run at all.
func TestRunAgentTaskDefersAnIncoherentSeatAfterColdLoad(t *testing.T) {
	fake := coldLoadFake(func(int64) string { return degenerateChat() })
	srv := fake.server(t)
	defer srv.Close()
	res := coherenceTestPipeline(t, srv.URL, 30, "").Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if !wire.Deferred {
		t.Fatalf("an incoherent seat must defer, got a result: %q", wire.Output)
	}
	if wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("defer_class = %q, want %q — the stack under the seat is what is broken", wire.DeferClass, core.DeferClassInfrastructure)
	}
	if !strings.Contains(wire.Reason, "incoherent") {
		t.Fatalf("reason = %q, want it to name the incoherent seat", wire.Reason)
	}
	if !strings.HasPrefix(wire.Reason, core.IncoherentSeatReason) {
		t.Fatalf("reason = %q, want the %q prefix the delegator's retry gate matches on", wire.Reason, core.IncoherentSeatReason)
	}
	if wire.CoherenceNote == "" {
		t.Fatal("coherence_note is empty on a run whose probe returned a verdict")
	}
	if n := fake.probeCNT.Load(); n != 1 {
		t.Fatalf("probe completions = %d, want exactly 1", n)
	}
	if n := fake.loopCalls.Load(); n != 0 {
		t.Fatalf("loop completions = %d, want 0 — the loop must never start on a broken seat", n)
	}
}

// TestRunAgentTaskCoherenceProbeAcceptsAParsedToolCall: the same cold load, the
// seat parses the read_file call, the run proceeds and says so.
func TestRunAgentTaskCoherenceProbeAcceptsAParsedToolCall(t *testing.T) {
	fake := coldLoadFake(func(int64) string { return probeToolCall() })
	srv := fake.server(t)
	defer srv.Close()
	res := coherenceTestPipeline(t, srv.URL, 30, "").Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("a coherent seat must run: %s", wire.Reason)
	}
	if !strings.Contains(wire.CoherenceNote, "tool call parsed") {
		t.Fatalf("coherence_note = %q, want the parsed-tool-call verdict", wire.CoherenceNote)
	}
	if n := fake.probeCNT.Load(); n != 1 {
		t.Fatalf("probe completions = %d, want exactly 1", n)
	}
	if n := fake.loopCalls.Load(); n == 0 {
		t.Fatal("the loop never ran after a coherent probe")
	}
}

// TestRunAgentTaskSkipsTheCoherenceProbeOnAWarmSeat: the default policy is
// "cold" — a seat that was already resident is one the previous run already
// proved, and probing it would spend a completion per contract on every warm
// run in the fleet.
func TestRunAgentTaskSkipsTheCoherenceProbeOnAWarmSeat(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		running:   func(int64) string { return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}` },
		probe:     func(int64) string { return degenerateChat() }, // would defer, if it were ever asked
	}
	srv := fake.server(t)
	defer srv.Close()
	res := coherenceTestPipeline(t, srv.URL, 30, "").Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if n := fake.probeCNT.Load(); n != 0 {
		t.Fatalf("probe completions = %d, want 0 on a warm seat under the default policy", n)
	}
	if wire.CoherenceNote != "" {
		t.Fatalf("coherence_note = %q, want empty when the probe did not run", wire.CoherenceNote)
	}
}

// TestRunAgentTaskCoherenceProbeOff: agent_coherence_probe "off" is the
// pre-D-118 behaviour exactly — a cold load, no probe, and a broken seat is
// back to being the loop's problem.
func TestRunAgentTaskCoherenceProbeOff(t *testing.T) {
	fake := coldLoadFake(func(int64) string { return degenerateChat() })
	srv := fake.server(t)
	defer srv.Close()
	res := coherenceTestPipeline(t, srv.URL, 30, "off").Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("with the probe off the run proceeds: %s", wire.Reason)
	}
	if n := fake.probeCNT.Load(); n != 0 {
		t.Fatalf("probe completions = %d, want 0 with agent_coherence_probe off", n)
	}
	if wire.CoherenceNote != "" {
		t.Fatalf("coherence_note = %q, want empty", wire.CoherenceNote)
	}
}

// TestRunAgentTaskCoherenceProbeAlways: "always" probes a WARM seat too — the
// setting for a box whose seat has gone NaN under it before.
func TestRunAgentTaskCoherenceProbeAlways(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		running:   func(int64) string { return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	res := coherenceTestPipeline(t, srv.URL, 30, "always").Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	if n := fake.probeCNT.Load(); n != 1 {
		t.Fatalf("probe completions = %d, want exactly 1 on a warm seat under \"always\"", n)
	}
	if !strings.Contains(wire.CoherenceNote, "tool call parsed") {
		t.Fatalf("coherence_note = %q, want the parsed-tool-call verdict", wire.CoherenceNote)
	}
}

// TestRunAgentTaskCoherenceProbeIsInconclusiveOnTransportError: the probe never
// converts SILENCE into a defer. A seat that answers 500 is the loop's first
// chat call to diagnose, with far more detail than one completion carries —
// exactly how warmSeat and the admission pre-flight fail open.
func TestRunAgentTaskCoherenceProbeIsInconclusiveOnTransportError(t *testing.T) {
	fake := coldLoadFake(nil)
	fake.probeStatus = func(int64) int { return 500 }
	srv := fake.server(t)
	defer srv.Close()
	res := coherenceTestPipeline(t, srv.URL, 30, "").Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("an unreachable probe must fail open, got a defer: %s", wire.Reason)
	}
	if !strings.Contains(wire.CoherenceNote, "inconclusive") {
		t.Fatalf("coherence_note = %q, want the inconclusive verdict", wire.CoherenceNote)
	}
	if n := fake.probeCNT.Load(); n != 1 {
		t.Fatalf("probe completions = %d, want exactly 1", n)
	}
}

// TestCoherenceProbeNeverRunsInsideTheWall is the whole point of running the
// probe where it runs: a 1 s wall with a ~2 s probe after a cold load must NOT
// become a wall timeout. The probe is charged to admission, and
// admission_wait_sec says so.
func TestCoherenceProbeNeverRunsInsideTheWall(t *testing.T) {
	fake := coldLoadFake(func(int64) string {
		time.Sleep(2 * time.Second)
		return probeToolCall()
	})
	srv := fake.server(t)
	defer srv.Close()
	contract := testContract()
	contract.TimeoutSec = 1
	res := coherenceTestPipeline(t, srv.URL, 30, "").Run(context.Background(), agentTestRequest(t, contract))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("the probe must not consume the wall; got: %s", wire.Reason)
	}
	if wire.AdmissionWaitSec < 1.9 {
		t.Fatalf("admission_wait_sec = %v, want the probe's ~2 s charged to admission", wire.AdmissionWaitSec)
	}
	if !strings.Contains(wire.CoherenceNote, "tool call parsed") {
		t.Fatalf("coherence_note = %q, want the parsed-tool-call verdict", wire.CoherenceNote)
	}
}

// TestCoherenceProbeSkipsWithoutAdmissionBudget: a run whose admission budget
// is already spent gets no probe and no note — the probe is never allowed to
// invent budget, and a probe given two seconds proves nothing.
func TestCoherenceProbeSkipsWithoutAdmissionBudget(t *testing.T) {
	cfg := config.Config{Endpoint: "http://127.0.0.1:1", Model: "workhorse", AgentModel: agentTestSeat}
	v := ProbeSeatCoherence(context.Background(), cfg, agentTestSeat, time.Second)
	if v.Ran || v.Broken {
		t.Fatalf("verdict = %+v, want a skip under the budget floor", v)
	}
	if !strings.Contains(v.Note, "skipped") {
		t.Fatalf("note = %q, want it to say the probe was skipped", v.Note)
	}
}

// TestJudgeCoherenceVerdicts covers the four shapes over ONE completion,
// without a server: the judgement rule is what both agent doors share.
func TestJudgeCoherenceVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		content    string
		toolCalls  int
		finish     string
		wantBroken bool
		wantNote   string
	}{
		{"a parsed tool call passes", "", 1, "tool_calls", false, "tool call parsed"},
		{"token-0 spam is broken", "<tool_call>" + strings.Repeat("!", 40), 0, "length", true, "repeats"},
		{"an unparsed marker with no call is broken", "<tool_call>{\"name\":\"read_file\"}", 0, "stop", true, "no parsed tool call"},
		{"empty at the cap is broken", "", 0, "length", true, "empty at the"},
		{"empty but FINISHED is not broken", "", 0, "stop", false, "answered in text"},
		{"plain prose proceeds", "DONE", 0, "stop", false, "without a tool call"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comp := agentCompletion(tc.content, tc.toolCalls, tc.finish)
			v := JudgeCoherence(comp, 1500*time.Millisecond)
			if !v.Ran {
				t.Fatal("a judged completion always Ran")
			}
			if v.Broken != tc.wantBroken {
				t.Fatalf("Broken = %v, want %v (note %q)", v.Broken, tc.wantBroken, v.Note)
			}
			if !strings.Contains(v.Note, tc.wantNote) {
				t.Fatalf("note = %q, want it to contain %q", v.Note, tc.wantNote)
			}
			if tc.wantBroken && !strings.HasPrefix(v.Note, core.IncoherentSeatReason) {
				t.Fatalf("note = %q, want the %q prefix", v.Note, core.IncoherentSeatReason)
			}
		})
	}
}

// agentCompletion builds the completion shape the client hands JudgeCoherence.
func agentCompletion(content string, toolCalls int, finish string) agent.Completion {
	msg := agent.Msg{Role: "assistant", Content: content}
	for i := 0; i < toolCalls; i++ {
		msg.ToolCalls = append(msg.ToolCalls, agent.ToolCall{ID: "c", Name: "read_file", Args: `{"path":"notes.md"}`})
	}
	return agent.Completion{Msg: msg, FinishReason: finish}
}
