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

// TestJudgeCoherenceVerdicts covers the shapes over ONE completion, without a
// server: the judgement rule is what both agent doors share.
func TestJudgeCoherenceVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		content    string
		toolCalls  int
		finish     string
		reasoning  string // the hidden channel this seat answered with
		reasonTok  int    // usage.completion_tokens_details.reasoning_tokens
		wantBroken bool
		wantNote   string
	}{
		{"a parsed tool call passes", "", 1, "tool_calls", "", 0, false, "tool call parsed"},
		{"token-0 spam is broken", "<tool_call>" + strings.Repeat("!", 40), 0, "length", "", 0, true, "repeats"},
		{"an unparsed marker with no call is broken", "<tool_call>{\"name\":\"read_file\"}", 0, "stop", "", 0, true, "no parsed tool call"},
		{"empty at the cap is broken", "", 0, "length", "", 0, true, "empty at the"},
		// A sampler degenerating onto a whitespace token is the NaN shape
		// without the punctuation: DegenerateRun ignores whitespace on purpose,
		// so the cap rule must treat whitespace-only as empty (reviewer finding).
		{"whitespace-only at the cap is broken", strings.Repeat(" \n\t", 32), 0, "length", "", 0, true, "whitespace-only counts as empty"},
		{"whitespace-only but FINISHED is not broken", "  \n", 0, "stop", "", 0, false, "answered in text"},
		{"empty but FINISHED is not broken", "", 0, "stop", "", 0, false, "answered in text"},
		{"plain prose proceeds", "DONE", 0, "stop", "", 0, false, "without a tool call"},
		// A THINKING seat cut inside its think block arrives in exactly the
		// empty-at-the-cap shape and is healthy: the client refuses to fold the
		// reasoning channel into Content at finish "length" on purpose, so
		// content is "" and finish is "length" for a seat that is only slower
		// to the point than 96 tokens allow. Judging it broken deferred a sane
		// seat as `infrastructure` before its wall (reviewer finding, D-118).
		{"a think block cut at the cap is NOT broken", "", 0, "length", "The user wants me to read notes.md, so I should call read_file with", 0, false, "cut inside the think block"},
		{"reasoning TOKENS alone also spare the seat", "", 0, "length", "", 96, false, "cut inside the think block"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comp := agentCompletion(tc.content, tc.toolCalls, tc.finish)
			comp.Reasoning = tc.reasoning
			if tc.reasoning != "" {
				comp.ReasoningKey = "reasoning_content"
			}
			if tc.reasonTok > 0 {
				comp.ReasoningKey = "reasoning"
				comp.Serve = &agent.ServeStats{UsageCompletionTokens: tc.reasonTok, UsageReasoningTokens: tc.reasonTok}
			}
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

// TestAThinkingSeatIsNotJudgedIncoherent is the finding in its live shape: a
// llama.cpp/DeepSeek-style seat that returns `reasoning_content` and nothing
// visible, cut at the probe's 96-token cap. The loop's own classifier calls
// this recoverable reasoning starvation (agent.Completion.Starvation ->
// reasoning_starved); the probe must not call the same completion a broken
// seat, or every thinking seat in the fleet defers `infrastructure` on its
// cold load and burns a re-placement for nothing.
func TestAThinkingSeatIsNotJudgedIncoherent(t *testing.T) {
	comp := agent.Completion{
		Msg:          agent.Msg{Role: "assistant", Content: ""},
		FinishReason: "length",
		Reasoning:    "The user wants me to read notes.md with the read_file tool. Let me think about the path first.",
		ReasoningKey: "reasoning_content",
		Serve:        &agent.ServeStats{UsageCompletionTokens: 96, UsageReasoningTokens: 96},
	}
	// The loop's verdict on this exact completion, for the record: the two
	// rules in this repo must agree about one shape.
	if kind, _, ok := comp.Starvation(); !ok || kind != agent.StopReasoningStarved {
		t.Fatalf("the loop classifies this completion as %q (ok=%v); the test's premise is wrong", kind, ok)
	}
	v := JudgeCoherence(comp, 1200*time.Millisecond)
	if v.Broken {
		t.Fatalf("a thinking seat cut at the cap must not be judged broken: %q", v.Note)
	}
	if !v.Ran {
		t.Fatal("a judged completion always Ran")
	}
	if !strings.Contains(v.Note, "think block") || !strings.Contains(v.Note, "reasoning_content") {
		t.Fatalf("note = %q, want it to name the think block and the channel the seat answered under", v.Note)
	}
}

// TestAWarmRunDefersOnTheRememberedIncoherentSeat (reviewer finding, D-118).
// Under the shipped default only the contract that LOADS a seat is probed, and
// the defer unloads nothing — so before the memo, contract 2 landed on the same
// NaN seat and spent its whole wall on output contract 1 had already proved
// degenerate. The second run here takes the WARM path (no probe at all) and
// still defers, on what the first run learned.
func TestAWarmRunDefersOnTheRememberedIncoherentSeat(t *testing.T) {
	fake := coldLoadFake(func(int64) string { return degenerateChat() })
	srv := fake.server(t)
	defer srv.Close()
	defer forgetCoherence(srv.URL, agentTestSeat)

	first := decodeWire(t, coherenceTestPipeline(t, srv.URL, 30, "").Run(context.Background(), agentTestRequest(t, testContract())))
	if !first.Deferred {
		t.Fatalf("the loading contract must defer: %q", first.Output)
	}
	// The seat is resident now, so this run never reaches the probe.
	second := decodeWire(t, coherenceTestPipeline(t, srv.URL, 30, "").Run(context.Background(), agentTestRequest(t, testContract())))
	if !second.Deferred {
		t.Fatalf("the warm contract must defer on the remembered verdict, got: %q", second.Output)
	}
	if second.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("defer_class = %q, want %q", second.DeferClass, core.DeferClassInfrastructure)
	}
	if !strings.HasPrefix(second.Reason, core.IncoherentSeatReason) {
		t.Fatalf("reason = %q, want the %q prefix the delegator's retry gate matches on", second.Reason, core.IncoherentSeatReason)
	}
	if !strings.Contains(second.Reason, "remembered") {
		t.Fatalf("reason = %q, want it to say the verdict is remembered, not freshly probed", second.Reason)
	}
	if second.CoherenceNote == "" {
		t.Fatal("coherence_note is empty on a run that deferred on the memo")
	}
	if n := fake.probeCNT.Load(); n != 1 {
		t.Fatalf("probe completions = %d, want exactly 1 across both runs — the memo costs no completion", n)
	}
	if n := fake.loopCalls.Load(); n != 0 {
		t.Fatalf("loop completions = %d, want 0 — neither contract may reach the broken seat's loop", n)
	}
}

// TestACoherentProbeClearsTheMemo: the memo must never outlive proof to the
// contrary. A seat that answers a later probe sanely is a seat that works, and
// the next warm contract has to run.
func TestACoherentProbeClearsTheMemo(t *testing.T) {
	const endpoint = "http://127.0.0.1:9/memo-clear"
	defer forgetCoherence(endpoint, agentTestSeat)
	RememberCoherence(endpoint, agentTestSeat, CoherenceVerdict{Ran: true, Broken: true, Note: core.IncoherentSeatReason + "spam"})
	if _, ok := RecallIncoherentSeat(endpoint, agentTestSeat); !ok {
		t.Fatal("a broken verdict must be remembered")
	}
	RememberCoherence(endpoint, agentTestSeat, CoherenceVerdict{Ran: true, Note: "coherence probe: tool call parsed in 1.0s"})
	if note, ok := RecallIncoherentSeat(endpoint, agentTestSeat); ok {
		t.Fatalf("a later coherent probe must clear the memo, still held: %q", note)
	}
}

// TestAnInconclusiveProbeLeavesTheMemoAlone: a probe that never ran, or one
// that could not reach the seat, learned nothing — it must neither create nor
// clear a verdict. (Fail-open is about the DEFER, not about forgetting.)
func TestAnInconclusiveProbeLeavesTheMemoAlone(t *testing.T) {
	const endpoint = "http://127.0.0.1:9/memo-keep"
	defer forgetCoherence(endpoint, agentTestSeat)
	RememberCoherence(endpoint, agentTestSeat, CoherenceVerdict{Ran: true, Broken: true, Note: core.IncoherentSeatReason + "spam"})
	RememberCoherence(endpoint, agentTestSeat, CoherenceVerdict{Note: "coherence probe skipped: 1s of admission budget left"})
	if _, ok := RecallIncoherentSeat(endpoint, agentTestSeat); !ok {
		t.Fatal("a skipped probe must not clear what a real one learned")
	}
}

// TestTheMemoExpires: the escape hatch for a seat fixed out of band. A memo
// older than its TTL is dropped and the next contract probes again — the
// harness never traps a repaired seat in a permanent defer.
func TestTheMemoExpires(t *testing.T) {
	const endpoint = "http://127.0.0.1:9/memo-ttl"
	defer forgetCoherence(endpoint, agentTestSeat)
	RememberCoherence(endpoint, agentTestSeat, CoherenceVerdict{Ran: true, Broken: true, Note: core.IncoherentSeatReason + "spam"})
	key := coherenceMemoKey(endpoint, agentTestSeat)
	coherenceMemo.Lock()
	e := coherenceMemo.broken[key]
	e.at = time.Now().Add(-coherenceMemoTTL - time.Minute)
	coherenceMemo.broken[key] = e
	coherenceMemo.Unlock()
	if note, ok := RecallIncoherentSeat(endpoint, agentTestSeat); ok {
		t.Fatalf("a stale verdict must expire, still held: %q", note)
	}
}
