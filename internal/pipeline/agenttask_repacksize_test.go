package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
)

// A re-pack is sized from the schema it writes to, not only from the answer it reads. The skip
// decision asks whether the time left can buy the re-pack's output, and the output was sized as if
// the re-pack re-wrote the whole answer: a token per three characters of it. A schema that names a
// title, at most ten strings, a flag and a note bounds what the extraction can emit whatever the
// answer's length. The recorded re-pack of a 7,126-character answer wrote about 200 tokens; it was
// never sent, because its 2,439-token "size" was more than the 2,330 tokens the call's last 476
// seconds buy at 4.9 tokens a second, and a finished, correct answer was lost to a budget defer.

// recordedSizeAnswer is a prose answer of exactly 7,126 characters in lines of about a hundred.
func recordedSizeAnswer(t *testing.T) string {
	t.Helper()
	const line = "Document line 15 says the box has no agent seat, so it is never a placement; the notes now say otherwise.\n"
	var b strings.Builder
	for b.Len()+len(line) <= 7126 {
		b.WriteString(line)
	}
	out := b.String() + strings.Repeat("x", 7126-b.Len())
	if len(out) != 7126 {
		t.Fatalf("answer is %d characters, want 7126", len(out))
	}
	return out
}

func schemaMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The recorded arithmetic, step by step: 476 s of the call's deadline (grace included) buy 2,330
// tokens at 4.9 tok/s; the answer-based figure was 2,439 and skipped; the schema-based one is under
// the 2,330 and sends the attempt clamped to what the time buys.
func TestRepackFitSendsARepackTheSchemaBoundsThatTheDeadlineCannotBuyTheWholeAnswerFor(t *testing.T) {
	now := time.Now()
	o := repackOpts{
		WallEnd: now.Add(-126 * time.Second), OwedEnd: now.Add(445600 * time.Millisecond), OwedCap: "the call's deadline",
		Grace: 30 * time.Second, TokS: 4.9, RateBasis: "the seat-rates store",
	}
	answer := recordedSizeAnswer(t)
	if got, want := expectedRepackTokens(answer), 2439; got != want {
		t.Fatalf("answer-based size = %d, want %d", got, want)
	}
	if got, want := repackBudget(answer), 2887; got != want {
		t.Fatalf("budget = %d, want %d", got, want)
	}

	// Today's arithmetic, which the recorded note quotes.
	f := o.fit(now, time.Time{}, repackBudget(answer), expectedRepackTokens(answer))
	if !f.Skip || !strings.Contains(f.Note, "buys 2330 tokens < the answer's 2439") || !strings.Contains(f.Note, "the call's deadline + 30 s grace") {
		t.Fatalf("answer-sized fit = %+v, want the recorded skip", f)
	}

	schema := schemaMap(t, fidelityOutputSchema)
	least, bySchema := repackLeast(schema, answer)
	if !bySchema || least >= 2330 {
		t.Fatalf("repackLeast = %d (by schema %v), want a schema-based figure under the 2,330 tokens the time buys", least, bySchema)
	}
	f = o.fit(now, time.Time{}, repackBudget(answer), least)
	if f.Skip || !f.Clamped || f.Tokens != 2330 {
		t.Fatalf("schema-sized fit = %+v, want a send clamped to the 2,330 tokens the time buys", f)
	}
}

// repackSchemaTokens reads the bound off the schema, and says nothing when the schema bounds
// nothing: a list without maxItems, an object that names no fields.
func TestRepackSchemaTokensBoundsOnlyWhatTheSchemaBounds(t *testing.T) {
	const unbounded = repackUnbounded
	cases := []struct {
		name     string
		schema   string
		min, max int // inclusive; min == max == unbounded for an open schema
	}{
		{"booleans and numbers", `{"properties":{"ok":{"type":"boolean"},"n":{"type":"number"}}}`, 20, 100},
		{"one free string", `{"properties":{"answer":{"type":"string"}}}`, 130, 200},
		{"a bounded list of strings", `{"properties":{"stale":{"type":"array","items":{"type":"string"},"maxItems":10}}}`, 1300, 1700},
		{"a list with no maxItems", `{"properties":{"stale":{"type":"array","items":{"type":"string"}}}}`, unbounded, unbounded},
		{"a free-form object", `{"properties":{"meta":{"type":"object"}}}`, unbounded, unbounded},
		{"a bounded string", `{"properties":{"code":{"type":"string","maxLength":10}}}`, 20, 80},
		{"an enum", `{"properties":{"label":{"type":"string","enum":["low","medium","high"]}}}`, 20, 80},
		{"a bounded list of objects", `{"properties":{"rows":{"type":"array","maxItems":3,"items":{"type":"object","properties":{"name":{"type":"string"},"n":{"type":"integer"}}}}}}`, 400, 1200},
		{"one unbounded list among bounded fields", `{"properties":{"title":{"type":"string"},"all":{"type":"array","items":{"type":"string"}}}}`, unbounded, unbounded},
		{"a nested object", `{"properties":{"meta":{"type":"object","properties":{"owner":{"type":"string"}}}}}`, 130, 260},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := repackSchemaTokens(schemaMap(t, tc.schema))
			if got < tc.min || got > tc.max {
				t.Fatalf("repackSchemaTokens = %d, want %d..%d", got, tc.min, tc.max)
			}
		})
	}
}

// repackLeast only ever LOWERS the answer-based figure: a short answer is its own bound, and a
// schema that bounds nothing keeps the answer-based one.
func TestRepackLeastNeverRaisesTheAnswerBasedFigure(t *testing.T) {
	schema := schemaMap(t, fidelityOutputSchema)
	short := "Intake guide: nothing is stale."
	if got, by := repackLeast(schema, short); by || got != expectedRepackTokens(short) {
		t.Fatalf("a short answer: least = %d (by schema %v), want the answer-based %d", got, by, expectedRepackTokens(short))
	}
	open := schemaMap(t, `{"properties":{"items":{"type":"array","items":{"type":"string"}}}}`)
	long := recordedSizeAnswer(t)
	if got, by := repackLeast(open, long); by || got != expectedRepackTokens(long) {
		t.Fatalf("an unbounded schema: least = %d (by schema %v), want the answer-based %d", got, by, expectedRepackTokens(long))
	}
	if got, by := repackLeast(nil, long); by || got != expectedRepackTokens(long) {
		t.Fatalf("no schema: least = %d (by schema %v), want the answer-based %d", got, by, expectedRepackTokens(long))
	}
}

// The owed time follows the same figure: 1.5 x the tokens at the seat's rate, the factor the re-pack's
// own stall allowance pads its generation estimate with.
func TestRepackOwedTokensIsTheStallAllowancesGenerationTime(t *testing.T) {
	want := time.Duration(agent.RepackMargin * 200 / 10 * float64(time.Second))
	if got := repackOwedTokens(200, 10); got < want-time.Millisecond || got > want+time.Millisecond {
		t.Fatalf("repackOwedTokens(200, 10) = %v, want %v", got, want)
	}
	if got := repackOwedTokens(200, 0); got != 0 {
		t.Fatalf("with no rate: %v, want 0", got)
	}
}

// answerInLines is a prose loop answer of about n characters in short lines, never JSON, so it
// always goes to the re-pack. The lines differ: the loop reads the same line repeated as a loop.
func answerInLines(n int) string {
	var b strings.Builder
	for i := 1; b.Len() < n; i++ {
		fmt.Fprintf(&b, "Decision %d recorded: arm C approved by operator %d.\n", i, i*7)
	}
	return b.String()
}

// The same shape end to end. A 2 s wall, a call deadline 4 s after the wall began, a seat at 100 tok/s
// and a 3,000-character answer that is already written: the answer-sized re-pack (1,064 tokens, owed
// 16 s) is capped by the call's deadline to the 410 tokens its last 4.1 s buy, and skipped. The
// schema, `{answer: string}`, bounds the object near 150 tokens: it is sent, clamped to the 410.
func TestRunAgentTaskRepackWhoseSchemaBoundsItsOutputIsSentWhenTheDeadlineCannotBuyTheWholeAnswer(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	answer := answerInLines(3000)
	fake := okRepackFake()
	fake.loop = func(int64) string { return doneChat(answer) }
	srv := fake.server(t)
	defer srv.Close()

	p := rateTestPipeline(t, srv.URL, 100)
	cfg := p.Cfg()
	cfg.AgentCallDeadlineSec = 4
	p.cfg = cfg
	contract := testContract()
	contract.TimeoutSec = 2
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s\nrepack_note: %s", wire.DeferClass, wire.Reason, wire.RepackNote)
	}
	if got := fake.grammarCNT.Load(); got != 1 {
		t.Fatalf("grammar re-pack requests = %d, want 1: the schema bounds the object well under what the deadline buys", got)
	}
	var st map[string]string
	if err := json.Unmarshal(wire.Structured, &st); err != nil || st["answer"] != "42" {
		t.Fatalf("structured = %s (%v), want the re-packed object", wire.Structured, err)
	}
	mt := maxTokensOf(t, <-fake.repackBodies)
	if mt < 150 || mt > 450 {
		t.Fatalf("max_tokens = %d, want what the last seconds of the call's deadline buy (about 410), not the %d budget", mt, repackBudget(answer))
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 1 || d[0].Skipped || d[0].ClampedFrom != repackBudget(answer) {
		t.Fatalf("detail = %+v, want one attempt sent, clamped from %d", d, repackBudget(answer))
	}
}

// A schema that bounds nothing keeps the answer-based size, so the same call is still skipped: the
// re-pack could be as long as the answer, and the time cannot buy that.
func TestRunAgentTaskRepackWhoseSchemaBoundsNothingIsStillSkippedAtTheCallsDeadline(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	answer := answerInLines(3000)
	fake := okRepackFake()
	fake.loop = func(int64) string { return doneChat(answer) }
	srv := fake.server(t)
	defer srv.Close()

	p := rateTestPipeline(t, srv.URL, 100)
	cfg := p.Cfg()
	cfg.AgentCallDeadlineSec = 4
	p.cfg = cfg
	contract := testContract()
	contract.TimeoutSec = 2
	contract.OutputSchema = json.RawMessage(`{"properties":{"items":{"type":"array","items":{"type":"string"}}},"required":["items"]}`)
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
	if got := fake.grammarCNT.Load() + fake.chatFallbackCNT.Load(); got != 0 {
		t.Fatalf("re-pack requests = %d, want 0: an unbounded list may be as long as the answer", got)
	}
	if !wire.Deferred || wire.DeferClass != core.DeferClassBudget || !wire.SchemaMiss || !strings.Contains(wire.RepackNote, "re-pack skipped:") {
		t.Fatalf("deferred=%v class=%q schema_miss=%v note=%q, want a budget defer carrying the finished answer", wire.Deferred, wire.DeferClass, wire.SchemaMiss, wire.RepackNote)
	}
}

// What the schema bound cannot do: raise a figure. A re-pack a short answer owes is the short
// answer's, and a skip that the schema did not decide still names the answer.
func TestRepackSkipNoteNamesWhatSizedIt(t *testing.T) {
	now := time.Now()
	o := repackOpts{WallEnd: now.Add(-time.Hour), Grace: 30 * time.Second, TokS: 1, RateBasis: "agent_seat_tok_s"}
	if f := o.fit(now, time.Time{}, 1024, 500); !f.Skip || !strings.Contains(f.Note, "the answer's 500") {
		t.Fatalf("fit = %+v, want the skip to name the answer's size", f)
	}
	o.NeedWhat = "the object's"
	if f := o.fit(now, time.Time{}, 1024, 500); !f.Skip || !strings.Contains(f.Note, "the object's 500") {
		t.Fatalf("fit = %+v, want the skip to name the object's size once the schema sized it", f)
	}
}

// A slow loop leaves the wall spent, and the re-pack is then owed the time it needs (register F44/C13).
// What it needs is what it WRITES: a 3,000-character answer re-packed into {answer: string} owes about
// 150 tokens at 100 tok/s (2.3 s), not the 1,064 tokens the answer weighs (16 s). The request carries
// what that time buys, not the whole 1,512-token budget a 16-second allowance would have let through.
func TestRunAgentTaskRepackIsOwedWhatItWritesNotWhatTheAnswerWeighs(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	answer := answerInLines(3000)
	fake := okRepackFake()
	fake.loopStream = func(_ int64, _ map[string]any, w http.ResponseWriter, r *http.Request) {
		select { // a loop that ends 0.4 s past its 2 s wall
		case <-r.Context().Done():
			return
		case <-time.After(2400 * time.Millisecond):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(doneChat(answer)))
	}
	srv := fake.server(t)
	defer srv.Close()

	contract := testContract()
	contract.TimeoutSec = 2
	wire := decodeWire(t, rateTestPipeline(t, srv.URL, 100).Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s\nrepack_note: %s", wire.DeferClass, wire.Reason, wire.RepackNote)
	}
	mt := maxTokensOf(t, <-fake.repackBodies)
	if mt < 150 || mt > 320 {
		t.Fatalf("max_tokens = %d, want what the 2.3 s owed for the object (plus the 0.1 s of grace) buy, not the %d budget a 16 s allowance would let through", mt, repackBudget(answer))
	}
	if d := wire.RepackAttemptsDetail; len(d) != 1 || d[0].ClampedFrom != repackBudget(answer) {
		t.Fatalf("detail = %+v, want one attempt clamped from the %d budget", d, repackBudget(answer))
	}
}

// A schema-sized re-pack that the time still cannot buy is skipped, and the note says it was the
// object's size that was compared, not the answer's.
func TestRunAgentTaskRepackSkipNamesTheObjectWhenTheSchemaSizedIt(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	answer := answerInLines(3000)
	fake := okRepackFake()
	fake.loop = func(int64) string { return doneChat(answer) }
	srv := fake.server(t)
	defer srv.Close()

	p := rateTestPipeline(t, srv.URL, 100)
	cfg := p.Cfg()
	cfg.AgentCallDeadlineSec = 1 // the call's last 1.1 s buy 110 tokens at 100 tok/s: under the object's 154
	p.cfg = cfg
	contract := testContract()
	contract.TimeoutSec = 1
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
	if got := fake.grammarCNT.Load() + fake.chatFallbackCNT.Load(); got != 0 {
		t.Fatalf("re-pack requests = %d, want 0", got)
	}
	if !wire.Deferred || wire.DeferClass != core.DeferClassBudget || !strings.Contains(wire.RepackNote, "< the object's") {
		t.Fatalf("deferred=%v class=%q note=%q, want a budget defer whose note names the object's size", wire.Deferred, wire.DeferClass, wire.RepackNote)
	}
}
