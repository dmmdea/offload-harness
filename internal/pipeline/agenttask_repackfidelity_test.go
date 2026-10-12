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

// A re-pack that returns a schema-valid object with every list empty, over an answer that holds
// those lists under keys of its own, was published as a success. The seat's final answer was a
// JSON object ({"doc_title", "outdated_sentences": [{quote, release_note_says} x 11]}, because
// the cut-final re-issue asks for "the same JSON object that was asked for" and says which keys
// to use nowhere), the schema asks for {doc_title, stale: [string], up_to_date, notes}, and the
// re-pack prompt said "use empty values when a field is absent": no key of the TEXT is called
// "stale", so the extractor wrote [] for it, true for up_to_date and a note that nothing was
// stale. The object validated, `contains:` read the answer, `nonempty:doc_title` read the title,
// and eleven stale sentences were delivered as none.

const fidelityOutputSchema = `{"type":"object","properties":{"doc_title":{"type":"string"},"stale":{"type":"array","items":{"type":"string"},"maxItems":10},"up_to_date":{"type":"boolean"},"notes":{"type":"string"}},"required":["doc_title","stale","up_to_date"]}`

const hollowObject = `{"doc_title": "Intake guide", "stale": [], "up_to_date": true, "notes": "No stale sentences were identified in the provided text."}`

func fidelityContract() core.AgentContract {
	c := testContract()
	c.OutputSchema = json.RawMessage(fidelityOutputSchema)
	c.Acceptance = []string{"contains:Intake guide", "nonempty:doc_title"}
	return c
}

// invertedKeysAnswer is the seat's final answer: a fenced JSON object with eleven items under a
// key the schema does not have, padded to exactly size characters so the re-pack is sized from
// the same figure a real run was (5,411 characters re-pack at a 2,315-token budget).
func invertedKeysAnswer(t *testing.T, size int) string {
	t.Helper()
	build := func(pad string) string {
		var b strings.Builder
		b.WriteString("```json\n{\n  \"doc_title\": \"Intake guide\",\n  \"outdated_sentences\": [\n")
		for i := 1; i <= 11; i++ {
			sep := ","
			note := fmt.Sprintf("Release note %d.", i)
			if i == 11 {
				sep, note = "", note+pad
			}
			fmt.Fprintf(&b, "    {\n      \"quote\": \"Sentence %d of the guide, which the release notes now contradict.\",\n      \"release_note_says\": \"%s\"\n    }%s\n", i, note, sep)
		}
		b.WriteString("  ]\n}\n```")
		return b.String()
	}
	base := build("")
	if len(base) > size {
		t.Fatalf("the unpadded answer is already %d characters, over %d", len(base), size)
	}
	out := build(strings.Repeat("x", size-len(base)))
	if len(out) != size {
		t.Fatalf("answer is %d characters, want %d", len(out), size)
	}
	return out
}

// nextBody is the next recorded request of a lane, or a failure when the lane was never asked: a
// bare receive would hang a test whose lane the code under test no longer reaches.
func nextBody(t *testing.T, ch chan map[string]any, what string) map[string]any {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(2 * time.Second):
		t.Fatalf("no %s request was recorded", what)
		return nil
	}
}

// preamble is the instructions of a prompt, up to the text it carries: a failure prints these and
// never the answer.
func preamble(user string) string {
	if i := strings.Index(user, "TEXT:"); i >= 0 {
		return user[:i]
	}
	return user
}

// faithfulObject is what an extractor that read the keys of the TEXT writes: the ten
// sentences the schema's maxItems allows, taken from the "quote" of each item.
func faithfulObject() string {
	items := make([]string, 10)
	for i := range items {
		items[i] = fmt.Sprintf("Sentence %d of the guide, which the release notes now contradict.", i+1)
	}
	b, _ := json.Marshal(map[string]any{"doc_title": "Intake guide", "stale": items, "up_to_date": false, "notes": "Eleven sentences are contradicted; the first ten are listed."})
	return string(b)
}

// hollowSeat is a seat that inverts the re-pack on every request it is sent.
func hollowSeat(answer string) *agentFake {
	return &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(answer) },
		repackStream: func(_ int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
			writeCompletion(w, hollowObject, "stop", 37)
		},
		chatFallback: func(int64) string { return doneChat(hollowObject) },
		repackBodies: make(chan map[string]any, 8),
		chatBodies:   make(chan map[string]any, 8),
	}
}

// The wrong success, end to end: every attempt writes the hollow object, and none is published.
// The re-pack is sized from the whole answer (5,411 characters, a 2,315-token budget), so this is
// not a cut or a trimmed answer: the seat saw all of it and wrote an empty list.
func TestRunAgentTaskRepackDoesNotPublishAnObjectThatEmptiedTheListsOfItsAnswer(t *testing.T) {
	answer := invertedKeysAnswer(t, 5411)
	fake := hollowSeat(answer)
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, fidelityContract())))
	if mt := maxTokensOf(t, nextBody(t, fake.repackBodies, "grammar re-pack")); mt != 2315 || mt != repackBudget(answer) {
		t.Fatalf("first re-pack max_tokens = %d, want 2315 (the whole 5,411-character answer's budget %d)", mt, repackBudget(answer))
	}
	if !wire.Deferred {
		var st struct {
			Stale []string `json:"stale"`
		}
		_ = json.Unmarshal(wire.Structured, &st)
		t.Fatalf("published stale=%v as a success while the answer holds 11 list items; structured = %s", st.Stale, wire.Structured)
	}
	if wire.DeferClass != core.DeferClassAbstention || !wire.SchemaMiss {
		t.Fatalf("defer class %q schema_miss=%v, want an abstention that carries the finished answer", wire.DeferClass, wire.SchemaMiss)
	}
	if !strings.Contains(wire.RepackNote, "emptied") || !strings.Contains(wire.RepackNote, `"outdated_sentences"`) {
		t.Fatalf("repack_note = %q, want the finding to name the emptied list and where the answer held it", wire.RepackNote)
	}
	if wire.Output != answer || len(wire.Structured) != 0 {
		t.Fatalf("the finished answer must ride the defer with no object: output kept=%v structured=%s", wire.Output == answer, wire.Structured)
	}
	// Every lane was tried and every lane's object was refused: two grammar attempts, then the chat lane.
	if fake.grammarCNT.Load() != 2 || fake.chatFallbackCNT.Load() != 1 {
		t.Fatalf("requests: grammar=%d chat=%d, want 2 and 1", fake.grammarCNT.Load(), fake.chatFallbackCNT.Load())
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 3 || !strings.Contains(d[0].Why, "emptied") || !strings.Contains(d[2].Why, "emptied") {
		t.Fatalf("detail = %+v, want three attempts each refused for the emptied list", d)
	}
}

// When the TEXT is itself a JSON object, the prompt says so and lists its keys, and does not tell
// the extractor to leave a field empty when it finds no key of that name. A field with a bound on
// its list says the bound, on the grammar lane too (the grammar projection drops maxItems, so the
// prompt reads it from the contract's schema).
func TestRepackPromptListsTheKeysOfAnAnswerThatIsAnObject(t *testing.T) {
	answer := invertedKeysAnswer(t, 5411)
	const emptyRule = "Use empty values when a field is absent"
	check := func(t *testing.T, lane string, body map[string]any) {
		t.Helper()
		system, user := promptOf(t, body)
		if strings.Contains(system, emptyRule) {
			t.Errorf("%s: the system prompt still tells the extractor to leave a field empty when it finds no key of that name:\n%s", lane, system)
		}
		for _, want := range []string{`"outdated_sentences"`, "11", `"quote"`, `"release_note_says"`, "at most 10"} {
			if !strings.Contains(user, want) {
				t.Errorf("%s: the user prompt does not say %s:\n%s", lane, want, preamble(user))
			}
		}
		if !strings.Contains(system+user, "empty value only") {
			t.Errorf("%s: the prompt does not say when a field may be left empty:\n%s\n%s", lane, system, preamble(user))
		}
		// The names and the counts, never the content: the sentences are in TEXT, once.
		if keys := user[:strings.Index(user, "TEXT:")]; strings.Contains(keys, "Sentence 1 of the guide") {
			t.Errorf("%s: the key listing quotes the answer's content:\n%s", lane, keys)
		}
	}
	t.Run("grammar lane", func(t *testing.T) {
		fake := hollowSeat(answer)
		srv := fake.server(t)
		defer srv.Close()
		decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, fidelityContract())))
		check(t, "grammar lane", nextBody(t, fake.repackBodies, "grammar re-pack"))
	})
	t.Run("chat lane", func(t *testing.T) {
		fake := hollowSeat(answer)
		srv := fake.server(t)
		defer srv.Close()
		decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, fidelityContract())))
		check(t, "chat lane", nextBody(t, fake.chatBodies, "chat re-pack"))
	})
	// A prose answer is unchanged: nothing to list, and an absent field is still empty.
	t.Run("prose answer", func(t *testing.T) {
		fake := &agentFake{
			rosterIDs: []string{agentTestSeat}, loop: func(int64) string { return doneChat("The guide is current; nothing is stale.") },
			repack: func(int64) string { return `{"doc_title":"Intake guide","stale":[],"up_to_date":true}` }, repackBodies: make(chan map[string]any, 4),
		}
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, fidelityContract())))
		if wire.Deferred {
			t.Fatalf("a prose answer that says nothing is stale was refused: %s / %s", wire.Reason, wire.RepackNote)
		}
		system, user := promptOf(t, nextBody(t, fake.repackBodies, "grammar re-pack"))
		if !strings.Contains(system, emptyRule) || strings.Contains(user, "TEXT is itself") {
			t.Errorf("the prose prompt changed:\n%s\n%s", system, preamble(user))
		}
	})
}

// The refusal is not the end of the re-pack: the attempt after it is a different request, told what
// the last one left out, so a seat that decodes greedy is not asked the same question twice. Here
// the first attempt inverts and the second, which carries the correction, copies the ten sentences
// the schema's bound allows, and the result says it kept ten of eleven.
func TestRunAgentTaskRepackAfterAnEmptiedObjectIsAskedAgainWithTheFinding(t *testing.T) {
	answer := invertedKeysAnswer(t, 5411)
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(answer) },
		repackStream: func(n int64, body map[string]any, w http.ResponseWriter, _ *http.Request) {
			if n == 1 {
				writeCompletion(w, hollowObject, "stop", 37)
				return
			}
			writeCompletion(w, faithfulObject(), "stop", 400)
		},
		repackBodies: make(chan map[string]any, 8),
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, fidelityContract())))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s\nrepack_note: %s", wire.DeferClass, wire.Reason, wire.RepackNote)
	}
	var st struct {
		Stale []string `json:"stale"`
	}
	if err := json.Unmarshal(wire.Structured, &st); err != nil || len(st.Stale) != 10 {
		t.Fatalf("structured = %s (%v), want the ten sentences", wire.Structured, err)
	}
	first, second := nextBody(t, fake.repackBodies, "grammar re-pack"), nextBody(t, fake.repackBodies, "grammar re-pack")
	if _, user := promptOf(t, first); strings.Contains(user, "left every list empty") {
		t.Fatalf("the FIRST attempt carries the correction:\n%s", preamble(user))
	}
	if _, user := promptOf(t, second); !sentenceBeforeText(user, "left every list empty") || !strings.Contains(user, `"outdated_sentences"`) {
		t.Fatalf("the second attempt does not carry the finding ahead of the text:\n%s", preamble(user))
	}
	if wire.RepackAttempts != 2 {
		t.Fatalf("repack_attempts = %d, want 2", wire.RepackAttempts)
	}
	if !strings.Contains(wire.RepackNote, "kept 10 of 11") || !strings.Contains(wire.RepackNote, "maxItems 10") {
		t.Fatalf("repack_note = %q, want the note that ten of eleven items were kept under the schema's maxItems", wire.RepackNote)
	}
}

// The chat lane is held to the same reading, and a faithful chat object is delivered.
func TestRunAgentTaskRepackChatLaneIsHeldToTheListsOfItsAnswer(t *testing.T) {
	answer := invertedKeysAnswer(t, 5411)
	fake := hollowSeat(answer)
	fake.chatFallback = func(int64) string { return doneChat(faithfulObject()) }
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, fidelityContract())))
	if wire.Deferred {
		t.Fatalf("deferred (%s): %s\nrepack_note: %s", wire.DeferClass, wire.Reason, wire.RepackNote)
	}
	d := wire.RepackAttemptsDetail
	if len(d) != 3 || !strings.Contains(d[0].Why, "emptied") || !strings.Contains(d[1].Why, "emptied") || d[2].Lane != "chat" || d[2].Why != "" {
		t.Fatalf("detail = %+v, want two refused grammar attempts and a chat attempt that produced the object", d)
	}
	// The chat lane is told what the refused grammar attempts left out, ahead of the text.
	if _, user := promptOf(t, nextBody(t, fake.chatBodies, "chat re-pack")); !sentenceBeforeText(user, "left every list empty") {
		t.Fatalf("the chat attempt after two refused objects does not carry the finding ahead of the text:\n%s", preamble(user))
	}
	// The chat lane's object carries ten of the answer's eleven items, and says so.
	if !strings.Contains(wire.RepackNote, "kept 10 of 11") {
		t.Fatalf("repack_note = %q, want the note that ten of eleven items were kept", wire.RepackNote)
	}
}

// The control that keeps the guard honest: an answer that says its list is empty, and lists what
// it checked under another key, is re-packed into an empty list and delivered.
func TestRunAgentTaskRepackDeliversAnEmptyListTheAnswerItselfHolds(t *testing.T) {
	// No up_to_date key, so the object does not validate as it stands and goes through the re-pack.
	answer := "```json\n" + `{"doc_title": "Intake guide", "stale": [], "checked_files": ["a.md", "b.md", "c.md"]}` + "\n```"
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat}, loop: func(int64) string { return doneChat(answer) },
		repack: func(int64) string { return hollowObject }, repackBodies: make(chan map[string]any, 4),
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, fidelityContract())))
	if wire.Deferred {
		t.Fatalf("a legitimately empty list was refused: %s / %s", wire.Reason, wire.RepackNote)
	}
}

// A hollow object that needed its types repaired first is held to the same reading, on either lane:
// the coercion mends "true" into true and must not launder the loss of the lists.
func TestRunAgentTaskRepackRefusesAnEmptiedObjectThatNeededCoercion(t *testing.T) {
	answer := invertedKeysAnswer(t, 5411)
	const coercibleHollow = `{"doc_title": "Intake guide", "stale": [], "up_to_date": "true"}`

	t.Run("grammar lane refused, chat lane delivers", func(t *testing.T) {
		fake := hollowSeat(answer)
		fake.repackStream = func(_ int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
			writeCompletion(w, coercibleHollow, "stop", 30)
		}
		fake.chatFallback = func(int64) string { return doneChat(faithfulObject()) }
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, fidelityContract())))
		if wire.Deferred {
			t.Fatalf("deferred (%s): %s / %s", wire.DeferClass, wire.Reason, wire.RepackNote)
		}
		var st struct {
			Stale []string `json:"stale"`
		}
		if err := json.Unmarshal(wire.Structured, &st); err != nil || len(st.Stale) != 10 {
			t.Fatalf("structured = %s (%v), want the chat lane's ten sentences, not the coerced hollow object", wire.Structured, err)
		}
	})

	t.Run("chat lane refused too", func(t *testing.T) {
		fake := hollowSeat(answer)
		fake.repackStream = func(_ int64, _ map[string]any, w http.ResponseWriter, _ *http.Request) {
			writeCompletion(w, coercibleHollow, "stop", 30)
		}
		fake.chatFallback = func(int64) string { return doneChat(coercibleHollow) }
		srv := fake.server(t)
		defer srv.Close()
		wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, fidelityContract())))
		if !wire.Deferred || wire.DeferClass != core.DeferClassAbstention || !strings.Contains(wire.RepackNote, "emptied") {
			t.Fatalf("deferred=%v class=%q note=%q, want an abstention that names the emptied list", wire.Deferred, wire.DeferClass, wire.RepackNote)
		}
		d := wire.RepackAttemptsDetail
		if len(d) != 3 || !strings.Contains(d[2].Why, "emptied") {
			t.Fatalf("detail = %+v, want the chat attempt refused for the emptied list", d)
		}
	})
}
