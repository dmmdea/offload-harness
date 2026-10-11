package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// A re-pack that loops must not be asked again the same way (register C14, 28
// jobs in 48 h). The seat decodes greedy at temperature 0 and nothing on the wire
// discouraged repetition, so a list it began to repeat ("WRONG", "WRONG", ...) was
// repeated to max_tokens; the next attempt, the chat lane, carried the same
// sampling and the same prompt and looped on the same text (the ledger shows both
// attempts degenerate in 5 rows), and a loop that ended before the cap on a
// complete, schema-valid object was published as the answer.
//
// loopSeat models that seat. A request that carries no frequency_penalty gets the
// loop, cut at the budget it was given; a request that carries one gets the object.
// Which lane asks makes no difference to it, which is the point: only a different
// request can end the loop.

// asked reports whether a recorded request carries the no-repeat conditioning: a
// positive frequency_penalty.
func askedNoRepeat(body map[string]any) bool {
	fp, _ := body["frequency_penalty"].(float64)
	return fp > 0
}

// userText is the last user message of a recorded chat request.
func userText(body map[string]any) string {
	msgs, _ := body["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]any)
		if m["role"] == "user" {
			s, _ := m["content"].(string)
			return s
		}
	}
	return ""
}

// loopSeat answers every re-pack request (grammar and chat lane alike) the way a
// greedy small seat does: the loop, cut at the request's max_tokens, unless the
// request is conditioned against repeating, and then `clean`.
func loopSeat(answer, loopContent, clean string) *agentFake {
	respond := func(_ int64, body map[string]any, w http.ResponseWriter, _ *http.Request) {
		if askedNoRepeat(body) {
			writeCompletion(w, clean, "stop", 12)
			return
		}
		mt, _ := body["max_tokens"].(float64)
		writeCompletion(w, loopContent, "length", int(mt))
	}
	return &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat(answer) },
		repackStream: respond,
		chatStream:   respond,
		repackBodies: make(chan map[string]any, 8),
		chatBodies:   make(chan map[string]any, 8),
	}
}

// A degenerate first attempt is followed by a request that is NOT the same one: the
// next grammar attempt carries the frequency penalty and says not to repeat, and
// that is the attempt that finishes. The first request carries neither, so a
// re-pack that never loops is byte-for-byte what it was.
func TestRunAgentTaskRepackAfterADegenerateCutIsAskedNotToRepeat(t *testing.T) {
	answer := answerOfChars(2000)
	fake := loopSeat(answer, `{"answer":"`+strings.Repeat(`WRONG\", \"`, 40), `{"answer":"42"}`)
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s\nrepack_note: %s", wire.DeferClass, wire.Reason, wire.RepackNote)
	}
	var st map[string]string
	if err := json.Unmarshal(wire.Structured, &st); err != nil || st["answer"] != "42" {
		t.Fatalf("structured = %s (%v), want the object the conditioned attempt wrote", wire.Structured, err)
	}
	first, second := <-fake.repackBodies, <-fake.repackBodies
	if askedNoRepeat(first) || strings.Contains(userText(first), repackNoRepeatNote) {
		t.Fatalf("the FIRST attempt carries the no-repeat conditioning: %v", first)
	}
	if fp, _ := second["frequency_penalty"].(float64); fp != repackLoopPenalty {
		t.Fatalf("second attempt frequency_penalty = %v, want %v", second["frequency_penalty"], repackLoopPenalty)
	}
	if !strings.Contains(userText(second), repackNoRepeatNote) {
		t.Fatalf("second attempt's prompt does not say not to repeat: %q", userText(second))
	}
	if fake.chatFallbackCNT.Load() != 0 {
		t.Fatalf("chat requests = %d, want 0: the conditioned grammar attempt finished it", fake.chatFallbackCNT.Load())
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 2 || !strings.Contains(d[0].Why, "degenerate") || !strings.Contains(d[1].Lane, "no-repeat") {
		t.Fatalf("detail = %+v, want a degenerate first attempt, then an attempt whose lane says it was told not to repeat", d)
	}
}

// "The output ran on past it": the answer needs fewer tokens than the budget held
// and the seat wrote on regardless, in words no byte-level test calls periodic
// (37 of 73 truncations in the ledger). It is the same signal: the budget was not
// the problem, so asking again the same way cannot help.
func TestRunAgentTaskRepackAfterARunOnCutIsAskedNotToRepeat(t *testing.T) {
	answer := answerOfChars(2000)
	budget := repackBudget(answer)
	fake := loopSeat(answer, `{"answer":"`+proseNoise(budget*4), `{"answer":"42"}`)
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s", wire.DeferClass, wire.Reason)
	}
	if got := fake.grammarCNT.Load(); got != 2 {
		t.Fatalf("grammar requests = %d, want 2 (the cut one and the conditioned one)", got)
	}
	<-fake.repackBodies
	if second := <-fake.repackBodies; !askedNoRepeat(second) {
		t.Fatalf("second attempt is not conditioned against repeating: %v", second)
	}
}

// When the conditioned grammar attempt loops too, the chat lane is conditioned the
// same way: it was the lane that looped on identical sampling in the ledger.
func TestRunAgentTaskRepackChatLaneAfterALoopIsAskedNotToRepeat(t *testing.T) {
	answer := answerOfChars(2000)
	loop := `{"answer":"` + strings.Repeat(`WRONG\", \"`, 40)
	respondChat := func(_ int64, body map[string]any, w http.ResponseWriter, _ *http.Request) {
		if askedNoRepeat(body) {
			writeCompletion(w, `{"answer":"42"}`, "stop", 12)
			return
		}
		mt, _ := body["max_tokens"].(float64)
		writeCompletion(w, loop, "length", int(mt))
	}
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(answer) },
		// Both grammar attempts loop whatever they carry: only the chat lane can finish.
		repackStream: func(_ int64, body map[string]any, w http.ResponseWriter, _ *http.Request) {
			mt, _ := body["max_tokens"].(float64)
			writeCompletion(w, loop, "length", int(mt))
		},
		chatStream:   respondChat,
		repackBodies: make(chan map[string]any, 8),
		chatBodies:   make(chan map[string]any, 8),
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s\nrepack_note: %s", wire.DeferClass, wire.Reason, wire.RepackNote)
	}
	chat := <-fake.chatBodies
	if !askedNoRepeat(chat) || !strings.Contains(userText(chat), repackNoRepeatNote) {
		t.Fatalf("the chat lane after a loop is not conditioned against repeating: %v", chat)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 3 || d[2].Lane != "chat+no-repeat" {
		t.Fatalf("detail = %+v, want three attempts ending on the conditioned chat lane", d)
	}
}

// The conditioning is for a LOOP, not for a cut that more tokens would cure: a
// budget that was too small is retried at the cap exactly as before, with neither
// the penalty nor the sentence, because a penalty on an honest long copy costs
// fidelity and buys nothing.
func TestRunAgentTaskRepackEscalationAfterATooSmallBudgetIsNotConditioned(t *testing.T) {
	answer := answerOfChars(1500)
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(answer) },
		repackStream: func(n int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
			writeCompletion(w, `{"answer":"`+digitNoise(1400), "length", agentRepackMaxTokens)
		},
		chatFallback: func(int64) string { return doneChat(`{"answer":"42"}`) },
		repackBodies: make(chan map[string]any, 8),
	}
	srv := fake.server(t)
	defer srv.Close()

	decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	first, second := <-fake.repackBodies, <-fake.repackBodies
	if maxTokensOf(t, second) != agentRepackMaxTokensCap {
		t.Fatalf("second attempt max_tokens = %d, want the cap: the budget was the problem", maxTokensOf(t, second))
	}
	for i, b := range []map[string]any{first, second} {
		if askedNoRepeat(b) || strings.Contains(userText(b), repackNoRepeatNote) {
			t.Fatalf("attempt %d is conditioned against repeating, but the cut was a budget cut: %v", i+1, b)
		}
	}
}

// A repetition that ENDS before the cap is a complete, schema-valid object, and was
// published. The seat wrote the item the answer holds once, forty times; it must
// not be handed back as the structured result.
func TestRunAgentTaskRepackDoesNotPublishARepeatedListItem(t *testing.T) {
	schema := json.RawMessage(`{"properties":{"items":{"type":"array","items":{"type":"string"}}},"required":["items"]}`)
	answer := "The review found alpha, beta and gamma. " + answerOfChars(300)
	loop := `{"items":[` + strings.TrimSuffix(strings.Repeat(`"alpha",`, 40), ",") + `]}`
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(answer) },
		repackStream: func(_ int64, body map[string]any, w http.ResponseWriter, _ *http.Request) {
			if askedNoRepeat(body) {
				writeCompletion(w, `{"items":["alpha","beta","gamma"]}`, "stop", 20)
				return
			}
			writeCompletion(w, loop, "stop", 120)
		},
		repackBodies: make(chan map[string]any, 8),
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.OutputSchema = schema
	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s", wire.DeferClass, wire.Reason)
	}
	var got struct{ Items []string }
	if err := json.Unmarshal(wire.Structured, &got); err != nil || fmt.Sprint(got.Items) != "[alpha beta gamma]" {
		t.Fatalf("structured = %s (%v), want the conditioned attempt's list, not the 40 repeats", wire.Structured, err)
	}
	if d := wire.RepackAttemptsDetail; len(d) != 2 || !strings.Contains(d[0].Why, "repeated") {
		t.Fatalf("detail = %+v, want the repeated list refused as the first attempt's verdict", d)
	}
}

// And a seat that repeats on every attempt is not published either: the defer is an
// abstention naming the repetition, with the finished answer kept for the caller.
func TestRunAgentTaskRepackThatAlwaysRepeatsDefersNamingTheRepetition(t *testing.T) {
	schema := json.RawMessage(`{"properties":{"items":{"type":"array","items":{"type":"string"}}},"required":["items"]}`)
	loop := `{"items":[` + strings.TrimSuffix(strings.Repeat(`"alpha",`, 40), ",") + `]}`
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat("The review found alpha. Nothing else.") },
		repack:       func(int64) string { return loop },
		chatFallback: func(int64) string { return doneChat(loop) },
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.OutputSchema = schema
	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract)))
	if !wire.Deferred || wire.DeferClass != core.DeferClassAbstention || len(wire.Structured) != 0 {
		t.Fatalf("deferred=%v class=%q structured=%s, want an abstention with nothing published", wire.Deferred, wire.DeferClass, wire.Structured)
	}
	if !strings.Contains(wire.Reason, "repeated") || !strings.Contains(wire.Reason, `"alpha"`) {
		t.Fatalf("reason = %q, want the repeated item named", wire.Reason)
	}
	if wire.Output != "The review found alpha. Nothing else." {
		t.Fatalf("output = %q, want the finished answer kept for the caller", wire.Output)
	}
}

// The check also reads an object the node had to re-type (the chat lane writes
// bare JSON numbers where the schema wants strings, and the coercion turns them
// into the strings): a loop of sevens is a loop of "7" once coerced.
func TestRunAgentTaskRepackDoesNotPublishARepeatedItemThatWasCoerced(t *testing.T) {
	schema := json.RawMessage(`{"properties":{"items":{"type":"array","items":{"type":"string"}}},"required":["items"]}`)
	loop := `{"items":[` + strings.TrimSuffix(strings.Repeat(`7,`, 30), ",") + `]}`
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat("The review found one seven.") },
		repack:       func(int64) string { return loop },
		chatFallback: func(int64) string { return doneChat(loop) },
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.OutputSchema = schema
	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, contract)))
	if !wire.Deferred || wire.DeferClass != core.DeferClassAbstention || len(wire.Structured) != 0 {
		t.Fatalf("deferred=%v class=%q structured=%s, want an abstention with nothing published", wire.Deferred, wire.DeferClass, wire.Structured)
	}
	if !strings.Contains(wire.Reason, "repeated the list item") {
		t.Fatalf("reason = %q, want the repetition named", wire.Reason)
	}
	if fake.chatFallbackCNT.Load() != 1 {
		t.Fatalf("chat requests = %d, want the chat lane tried (and refused) as well", fake.chatFallbackCNT.Load())
	}
}

// A cut the CLOCK set is the clock's verdict and no loop: the attempt was clamped to
// what the time left buys, ran on past the answer's size (prose, no period), and the
// next attempt is the chat lane as before, not a conditioned retry.
func TestRunAgentTaskRepackClampedRunOnIsTheClocksVerdictNotALoop(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	answer := answerOfChars(1500) // the answer's own size is 564 tokens, the budget 1,024
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(answer) },
		repackStream: func(n int64, body map[string]any, w http.ResponseWriter, _ *http.Request) {
			mt := int(body["max_tokens"].(float64))
			writeCompletion(w, `{"answer":"`+proseNoise(mt*4), "length", mt)
		},
		chatFallback: func(int64) string { return doneChat(`{"answer":"42"}`) },
		repackBodies: make(chan map[string]any, 8),
		chatBodies:   make(chan map[string]any, 8),
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, rateTestPipeline(t, srv.URL, 400).Run(context.Background(), agentTestRequest(t, composeContract())))
	if got := fake.grammarCNT.Load(); got != 1 {
		t.Fatalf("grammar requests = %d, want 1: a cut the clock set is not answered by a conditioned retry", got)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 2 || d[0].ClampedFrom != agentRepackMaxTokens || d[1].Lane != "chat" {
		t.Fatalf("detail = %+v, want the clamped grammar attempt, then the plain chat lane", d)
	}
	if chat := <-fake.chatBodies; askedNoRepeat(chat) {
		t.Fatalf("the chat lane after a clamped cut is conditioned against repeating: %v", chat)
	}
}

// degenerateList is the check behind both: a run of one item (or a short block of
// items) repeated at least repackLoopRun times in a list, that the answer the
// object was written from does not itself hold that many times.
func TestDegenerateListFlagsALoopAndSparesAnswersThatHoldTheRepeat(t *testing.T) {
	list := func(items ...string) []byte {
		b, _ := json.Marshal(map[string]any{"items": items})
		return b
	}
	rep := func(s string, n int) []string { return strings.Fields(strings.TrimSpace(strings.Repeat(s+" ", n))) }
	cases := []struct {
		name    string
		content []byte
		source  string
		flagged bool
	}{
		{"a run of one item the answer holds once", list(rep("alpha", 20)...), "alpha is the finding", true},
		{"a run the answer holds as often", list(rep("n/a", 8)...), strings.Repeat("n/a ", 8), false},
		// The threshold itself is pinned by literals, not by the constant: a test written
		// in terms of repackLoopRun passes for any value of it.
		{"a run of 5, just under the threshold of 6", list(rep("alpha", 5)...), "alpha", false},
		{"a run of 6, at the threshold", list(rep("alpha", 6)...), "alpha", true},
		{"an alternating pair, six times", list(rep("alpha beta", 6)...), "alpha beta", true},
		{"an alternating pair, five times", list(rep("alpha beta", 5)...), "alpha beta", false},
		{"distinct items", list("alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta"), "x", false},
		{"empty items are not a loop", list(make([]string, 10)...), "", false},
		{"no list at all", []byte(`{"answer":"42"}`), "42", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := degenerateList(tc.content, tc.source)
			if (got != "") != tc.flagged {
				t.Fatalf("degenerateList = %q, flagged want %v", got, tc.flagged)
			}
		})
	}
}

// The detector reads loops whose block is longer than the 64 bytes the first
// version looked for: two alternating list items of 80 bytes are a 160-byte
// period, invisible in the 256-byte window, and an undetected loop was ESCALATED to
// the 8,192-token cap (the C-80 waste) and filed "ran on past it".
func TestDegenerateTailReadsALongerLoop(t *testing.T) {
	a := `"the ledger shows pass four measured against the baseline while latency stayed",`
	b := `"flat across every run and the operator approved arm C after reviewing numbers",`
	loop := `{"key_facts":[` + strings.Repeat(a+b, 7)
	if got := degenerateTail(loop); got == "" {
		t.Fatalf("a 160-byte block repeated 7 times was not read as a loop")
	}
	// Ordinary long output is not a loop: distinct sentences, and a block repeated
	// only three times.
	prose := `{"key_facts":["` + proseNoise(1500) + `"]}`
	if got := degenerateTail(prose); got != "" {
		t.Fatalf("distinct prose read as a loop: %q", got)
	}
	if got := degenerateTail(`{"key_facts":[` + strings.Repeat(a+b, 3)); got != "" {
		t.Fatalf("a block repeated three times read as a loop: %q", got)
	}
}
