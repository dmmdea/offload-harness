package pipeline

import (
	"context"
	"testing"
)

// triageToolChat is a planner step that calls offload_triage.
func triageToolChat(n int64) string {
	return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c` +
		jsonNum(n) + `","type":"function","function":{"name":"offload_triage","arguments":"{\"text\":\"the sky is blue today\",\"question\":\"is the sky blue?\"}"}}]},"finish_reason":"tool_calls"}]}`
}

// TestRunAgentTaskInLoopOffloadRidesThePlannerSeat (0.115.18, register D-88):
// a fleet-node agent task whose planner calls offload_triage sends that tier
// request to the PLANNER SEAT without thinking — never to the workhorse, whose
// load would evict the seat (measured 2026-09-10: four 3-minute reloads of the
// 27B inside one 900 s wall).
func TestRunAgentTaskInLoopOffloadRidesThePlannerSeat(t *testing.T) {
	bodies := make(chan map[string]any, 8)
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(n int64) string {
			if n == 1 {
				return triageToolChat(n)
			}
			return doneChat("The answer is 42.")
		},
		repack: func(n int64) string {
			if n == 1 {
				return `{"decision":"yes","reason":"the text says so"}` // the in-loop triage tier
			}
			return `{"answer":"42"}` // the final structured re-pack
		},
		repackBodies: bodies,
	}
	srv := fake.server(t)
	defer srv.Close()
	res := admissionTestPipeline(t, srv.URL, 30).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("deferred: %s", wire.Reason)
	}
	select {
	case b := <-bodies:
		if b["model"] != agentTestSeat || !repackDisablesThinking(b) {
			t.Fatalf("in-loop triage tier hit model=%v kwargs=%v; want the planner seat %q with enable_thinking=false", b["model"], b["chat_template_kwargs"], agentTestSeat)
		}
	default:
		t.Fatal("no grammar completion recorded: the offload_triage call never reached a tier")
	}
}
