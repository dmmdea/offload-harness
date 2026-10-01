package pipeline

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// A re-pack attempt the time left narrowed and the seat cut at that narrowed budget
// is the clock's verdict, not the seat's (register C-80): the seat was given what the
// wall allowed, and nothing says it would have failed with more. Filed as an
// abstention it is retried on another node, which is charged a failure it did not
// earn and may be spent on a contract the clock could not fit. It is a budget defer,
// the finished answer rides in output flagged schema_miss, and the delegator
// re-packs it (a runaway tail is the exception: that one IS the seat's).
func TestRunAgentTaskAClampCutRepackIsABudgetDeferNotAnAbstention(t *testing.T) {
	proseCut := func(w http.ResponseWriter, body map[string]any) {
		writeCompletion(w, `{"key_facts":["`+proseNoise(3000), "length", maxTokensOf(t, body))
	}
	spaceCut := func(w http.ResponseWriter, body map[string]any) {
		writeCompletion(w, `{"key_facts":["a"],"numbers":[`+strings.Repeat(" \n  ", 300), "length", maxTokensOf(t, body))
	}
	cases := []struct {
		name      string
		answer    string
		tokS      float64
		firstHold time.Duration // how long the first grammar request holds the seat
		grammar   func(w http.ResponseWriter, body map[string]any)
		chat      func(w http.ResponseWriter, body map[string]any)
		want      string
		wantChat  int64 // chat requests that must have been sent
	}{
		{"both lanes cut at the budgets the clock set", answerOfChars(1500), 400, 0, proseCut, proseCut, core.DeferClassBudget, 1},
		{"the clock set the first budget and left no room for the chat lane", "The answer is 42.", 200, 1900 * time.Millisecond, proseCut, nil, core.DeferClassBudget, 0},
		{"a runaway is the seat's fault whatever the clock did", answerOfChars(1500), 400, 0, spaceCut, spaceCut, core.DeferClassAbstention, 1},
		{"the last attempt answered the wrong shape with room to finish", answerOfChars(1500), 400, 0, proseCut,
			func(w http.ResponseWriter, _ map[string]any) { writeCompletion(w, `{"wrong":"shape"}`, "stop", 40) }, core.DeferClassAbstention, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
			fake := &agentFake{
				rosterIDs: []string{agentTestSeat},
				loop:      func(int64) string { return doneChat(tc.answer) },
				repackStream: func(n int64, body map[string]any, w http.ResponseWriter, _ *http.Request) {
					if n == 1 {
						time.Sleep(tc.firstHold)
					}
					tc.grammar(w, body)
				},
			}
			if tc.chat != nil {
				fake.chatStream = func(_ int64, body map[string]any, w http.ResponseWriter, _ *http.Request) { tc.chat(w, body) }
			}
			srv := fake.server(t)
			defer srv.Close()

			wire := decodeWire(t, rateTestPipeline(t, srv.URL, tc.tokS).Run(context.Background(), agentTestRequest(t, composeContract())))
			if got := fake.chatFallbackCNT.Load(); got != tc.wantChat {
				t.Fatalf("chat requests = %d, want %d (deferred %s: %s)", got, tc.wantChat, wire.DeferClass, wire.Reason)
			}
			if !wire.Deferred || wire.DeferClass != tc.want {
				t.Fatalf("deferred=%v class=%q reason=%q, want a %s defer", wire.Deferred, wire.DeferClass, wire.Reason, tc.want)
			}
			if tc.want != core.DeferClassBudget {
				return
			}
			// The shape a delegator that predates the class keys on, the flag the
			// current one rescues on, and the finished answer the rescue re-packs.
			if !strings.HasPrefix(wire.Reason, core.RepackFailedReason) {
				t.Errorf("reason = %q, want the %q prefix", wire.Reason, core.RepackFailedReason)
			}
			if !wire.SchemaMiss || wire.Output != tc.answer {
				t.Errorf("schema_miss=%v output=%q, want the finished answer kept and flagged", wire.SchemaMiss, wire.Output)
			}
			if !delegate.SchemaMissRescuable(wire) {
				t.Errorf("the result is not rescuable, so the delegator would count a finished loop as lost: %+v", wire)
			}
			if !strings.Contains(wire.Reason, "the time left set this budget") {
				t.Errorf("reason = %q, want it to say the clock set the budget", wire.Reason)
			}
		})
	}
}
