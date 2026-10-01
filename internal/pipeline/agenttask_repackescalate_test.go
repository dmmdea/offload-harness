package pipeline

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// lcg is a tiny deterministic generator: the escalation gate's tests need text
// that is neither periodic nor a repeated block, and no seed that can drift.
type lcg uint32

func (g *lcg) next() uint32 {
	*g = *g*1664525 + 1013904223
	return uint32(*g >> 8)
}

// digitNoise is n bytes of digits and spaces with no period: about 1.4 bytes per
// token on a tokenizer that reads a digit at a time, the dense text whose token
// count len/3 under-estimates.
func digitNoise(n int) string {
	g := lcg(7)
	var b strings.Builder
	for b.Len() < n {
		b.WriteByte(byte('0' + g.next()%10))
		if g.next()%3 == 0 {
			b.WriteByte(' ')
		}
	}
	return b.String()[:n]
}

var proseWords = strings.Fields("the ledger shows pass four measured against the baseline while latency stayed flat across every run and the operator approved arm C after reviewing throughput numbers from the second week of the trial")

// proseNoise is n bytes of ordinary words in a random order: about four bytes
// per token, and no period.
func proseNoise(n int) string {
	g := lcg(11)
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(proseWords[g.next()%uint32(len(proseWords))])
		b.WriteByte(' ')
	}
	return b.String()[:n]
}

// answerOfChars is a prose loop answer of about n characters (never JSON, so it
// always goes to the re-pack).
func answerOfChars(n int) string {
	return strings.Repeat("Decision recorded: arm C approved by the operator. ", n/52+1)
}

// cutRepackFake is a seat whose grammar completions are always cut at
// finish_reason "length" with the given content, reporting `tokens` generated;
// the chat lane answers the object. It records every grammar and chat request.
func cutRepackFake(answer, cutContent string, tokens int) *agentFake {
	return &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(answer) },
		repackStream: func(n int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
			writeCompletion(w, cutContent, "length", tokens)
		},
		chatFallback: func(int64) string { return doneChat(`{"answer":"42"}`) },
		repackBodies: make(chan map[string]any, 8),
		chatBodies:   make(chan map[string]any, 8),
	}
}

func maxTokensOf(t *testing.T, body map[string]any) int {
	t.Helper()
	mt, ok := body["max_tokens"].(float64)
	if !ok {
		t.Fatalf("request carries no max_tokens: %v", body)
	}
	return int(mt)
}

// The grammar lane is cut at max_tokens and the retry used to go to the CAP
// unconditionally: an identical greedy request that ran to the cap again
// (register C-80: 8,192 tokens at 5.6 tok/s is 24 minutes). The second request
// now goes out only when the truncated attempt shows the budget was the problem;
// otherwise the loop goes straight to the chat lane.
func TestRunAgentTaskRepackDoesNotEscalateWhenMoreTokensCannotHelp(t *testing.T) {
	answer := answerOfChars(2800) // ~1,445-token budget
	budget := repackBudget(answer)
	cases := []struct {
		name    string
		content string // what the cut completion wrote
		tokens  int
		answer  string
		why     string // the attempt's recorded reason must say this
	}{
		{"a whitespace tail", `{"key_facts":["a"],"numbers":[` + strings.Repeat(" \n  ", 200), budget, answer, "degenerate"},
		{"a repeating tail", `{"key_facts":["a"],"numbers":[` + proseNoise(500) + strings.Repeat(`"", `, 60), budget, answer, "degenerate"},
		{"a single repeated byte", `{"key_facts":["a"],"numbers":[` + proseNoise(500) + strings.Repeat("!", 120), budget, answer, "degenerate"},
		{"prose that already fits the budget", `{"key_facts":["` + proseNoise(budget*4), budget, answer, "inside"},
		{"a budget that is already the cap", `{"key_facts":["` + digitNoise(8192), agentRepackMaxTokensCap, answerOfChars(30000), "cap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := cutRepackFake(tc.answer, tc.content, tc.tokens)
			srv := fake.server(t)
			defer srv.Close()
			wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
			if wire.Deferred {
				t.Fatalf("deferred (%s): %s", wire.DeferClass, wire.Reason)
			}
			if got := fake.grammarCNT.Load(); got != 1 {
				t.Fatalf("grammar requests = %d, want 1: a second request could not have finished what the first could not", got)
			}
			if got := fake.chatFallbackCNT.Load(); got != 1 {
				t.Fatalf("chat requests = %d, want 1: the loop goes straight to the chat lane", got)
			}
			if wire.RepackAttempts != 2 {
				t.Fatalf("repack_attempts = %d, want 2 (the cut grammar request and the chat lane)", wire.RepackAttempts)
			}
			d := wire.RepackAttemptsDetail
			if len(d) != 2 || d[0].FinishReason != "length" || !strings.Contains(d[0].Why, tc.why) || d[1].Lane != "chat" {
				t.Fatalf("detail = %+v, want a cut grammar attempt whose reason says %q, then the chat lane", d, tc.why)
			}
			if strings.Contains(d[0].Why, "cannot hold it") {
				t.Fatalf("the truncation note still says what it cannot know: %q", d[0].Why)
			}
			t.Logf("attempt 1 note: %s", d[0].Why)
		})
	}
}

// And it still escalates when the budget WAS the problem: the truncated attempt
// was writing dense, non-repeating text, and at the bytes-per-token the seat
// actually wrote the answer needs more tokens than the budget held. Both
// attempts are cut, the chat lane is down, and the defer names the truncation
// (a cut prefix is never judged as invalid JSON). This is the old escalation
// test's own shape (two grammar attempts, the first at the floor, the second at
// the cap), now pinned for the case that earns it.
func TestRunAgentTaskRepackTruncationIsNamedAndEscalatesWhenTheBudgetWasTheProblem(t *testing.T) {
	answer := answerOfChars(1500) // 1,024-token floor
	if repackBudget(answer) != agentRepackMaxTokens {
		t.Fatalf("fixture: budget = %d, want the floor", repackBudget(answer))
	}
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(answer) },
		repackStream: func(n int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
			// ~1.4 bytes per token over the 1,024-token floor: the 1,500-char answer
			// needs ~1,160 tokens at that density, so 1,024 was too few.
			writeCompletion(w, `{"key_facts":["`+digitNoise(1400), "length", agentRepackMaxTokens)
		},
		repackBodies: make(chan map[string]any, 4),
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || wire.DeferClass != core.DeferClassAbstention {
		t.Fatalf("deferred/class = %v/%q, want an abstention", wire.Deferred, wire.DeferClass)
	}
	if !strings.Contains(wire.Reason, "re-pack truncated at") || strings.Contains(wire.Reason, "unexpected end of JSON") {
		t.Fatalf("reason = %q, want the truncation named rather than an invalid-json verdict", wire.Reason)
	}
	if fake.grammarCNT.Load() != 2 {
		t.Fatalf("grammar attempts = %d, want 2", fake.grammarCNT.Load())
	}
	first, second := <-fake.repackBodies, <-fake.repackBodies
	if got := maxTokensOf(t, first); got != agentRepackMaxTokens {
		t.Fatalf("first attempt max_tokens = %d, want the floor %d", got, agentRepackMaxTokens)
	}
	if got := maxTokensOf(t, second); got != agentRepackMaxTokensCap {
		t.Fatalf("escalated attempt max_tokens = %d, want the cap %d", got, agentRepackMaxTokensCap)
	}
	d := wire.RepackAttemptsDetail
	if len(d) < 2 || !strings.Contains(d[0].Why, "too small") {
		t.Fatalf("detail = %+v, want the first cut attempt's reason to say the budget was too small", d)
	}
	t.Logf("attempt 1 note: %s", d[0].Why)
}

// The chat lane's own truncation says what it observed too: it used to end with
// the same "the structured budget cannot hold it", a claim no one had measured.
func TestRunAgentTaskChatLaneTruncationNoteSaysWhatWasObserved(t *testing.T) {
	answer := answerOfChars(2800)
	budget := repackBudget(answer)
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(answer) },
		repack:    func(int64) string { return `{"wrong":"shape"}` }, // both grammar attempts answer the wrong shape
		chatStream: func(n int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
			writeCompletion(w, `{"answer":"`+strings.Repeat(" ", 400), "length", budget)
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || wire.DeferClass != core.DeferClassAbstention {
		t.Fatalf("deferred/class = %v/%q, want an abstention (the chat lane was cut)", wire.Deferred, wire.DeferClass)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 3 || d[2].Lane != "chat" || d[2].FinishReason != "length" {
		t.Fatalf("detail = %+v, want two grammar attempts then a chat attempt cut at its budget", d)
	}
	if strings.Contains(d[2].Why, "cannot hold it") || !strings.Contains(d[2].Why, "degenerate") {
		t.Fatalf("chat attempt note = %q, want what was observed (a degenerate tail), not what cannot be known", d[2].Why)
	}
	if !strings.Contains(wire.Reason, "chat re-pack truncated at") || strings.Contains(wire.Reason, "cannot hold it") {
		t.Fatalf("reason = %q, want the chat truncation named with what was seen", wire.Reason)
	}
}
