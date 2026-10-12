package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The second round of review on the re-pack's reading of its answer.
//
// The retry prompt after a refused attempt said "Write those items into the list fields, one list
// item for each of them" about EVERY list the TEXT held. For an answer whose only list is what was
// checked (a list the schema has no field for, beside a schema list that is truly empty), the
// refusal is a false one, and a seat that obeyed the prompt copied the checked files into "stale":
// a fabricated list contradicting up_to_date: true, published as a success with an empty
// repack_note. The prompt after a refusal is conditional now, and names only the lists that were
// lost.

// checkedFilesAnswer is a truthful "nothing is stale": the answer says the guide is up to date and
// lists the files it checked, under a key of its own. It has no "stale" key, so the object does not
// validate as it stands and goes through the re-pack.
const checkedFilesAnswer = "```json\n" + `{"doc_title": "Intake guide", "up_to_date": true, "checked_files": ["a.md", "b.md", "c.md"]}` + "\n```"

const copiedChecklist = `{"doc_title": "Intake guide", "stale": ["a.md", "b.md", "c.md"], "up_to_date": true, "notes": ""}`

func fidelitySchemaMap(t *testing.T) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal([]byte(fidelityOutputSchema), &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

// The correction for an answer whose list is not a schema list is conditional: it names the list
// and the requested field, tells the seat to copy only if the list is that field's content, and
// says an empty field stays empty. It does not carry the unconditional instruction.
func TestCorrectionForAnOrphanListIsConditional(t *testing.T) {
	schema := fidelitySchemaMap(t)
	hollow := []byte(`{"doc_title":"Intake guide","stale":[],"up_to_date":true}`)
	lost := core.ListsLost(hollow, schema, checkedFilesAnswer)
	got := newRepackSource(checkedFilesAnswer).correction(lost, core.SchemaListSites(schema))
	for _, want := range []string{`"checked_files" (3 items)`, `"stale"`, "If one of those lists is the content of a requested list field", "stays empty"} {
		if !strings.Contains(got, want) {
			t.Errorf("the correction does not say %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Write those items into the list fields") {
		t.Errorf("the correction still instructs an unconditional copy into the list fields:\n%s", got)
	}
}

// A same-named list that came back empty is unconditional (the name matches), and the correction
// names that list alone, not every list the TEXT holds.
func TestCorrectionForASameNamedListNamesOnlyThatList(t *testing.T) {
	schema := fidelitySchemaMap(t)
	answer := `{"doc_title": "Intake guide", "stale": ["s1", "s2", "s3", "s4"], "checked_files": ["a.md", "b.md"], "up_to_date": false}`
	hollow := []byte(`{"doc_title":"Intake guide","stale":[],"up_to_date":true}`)
	lost := core.ListsLost(hollow, schema, answer)
	got := newRepackSource(answer).correction(lost, core.SchemaListSites(schema))
	if !strings.Contains(got, `"stale" (4 items)`) || !strings.Contains(got, "Write those items into the field of that name") {
		t.Errorf("the correction does not send the same-named list into its own field:\n%s", got)
	}
	if strings.Contains(got, "checked_files") {
		t.Errorf("the correction names a list the object was not asked to carry:\n%s", got)
	}
}

// No lost list, no correction: a prose TEXT has none, and neither has an object that kept its lists.
func TestCorrectionIsEmptyWhenNothingWasLost(t *testing.T) {
	schema := fidelitySchemaMap(t)
	if got := newRepackSource("The guide is current.").correction(nil, core.SchemaListSites(schema)); got != "" {
		t.Errorf("a prose TEXT got a correction: %q", got)
	}
	if got := newRepackSource(checkedFilesAnswer).correction(nil, core.SchemaListSites(schema)); got != "" {
		t.Errorf("no lost list, yet a correction: %q", got)
	}
}

// The seat that does what the retry prompt says, end to end. The answer is truthful (nothing is
// stale; it lists the files it checked); the seat returns the empty object until it is told to
// "write those items into the list fields", and then copies the checked files into "stale".
// Refused as it is (the cost, pinned), the call must end as an abstention defer with the finished
// answer attached, never as a success holding a list the answer never called stale.
func TestRunAgentTaskRepackAfterATruthfulEmptyObjectIsNotToldToCopyTheChecklist(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(checkedFilesAnswer) },
		repackStream: func(_ int64, body map[string]any, w http.ResponseWriter, _ *http.Request) {
			if _, user := promptOf(t, body); strings.Contains(user, "Write those items into the list fields") {
				writeCompletion(w, copiedChecklist, "stop", 40)
				return
			}
			writeCompletion(w, hollowObject, "stop", 37)
		},
		chatFallback: func(int64) string { return doneChat(hollowObject) },
		repackBodies: make(chan map[string]any, 8),
		chatBodies:   make(chan map[string]any, 8),
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, fidelityContract())))
	if !wire.Deferred {
		var st struct {
			Stale []string `json:"stale"`
		}
		_ = json.Unmarshal(wire.Structured, &st)
		t.Fatalf("published stale=%v as a success for an answer that lists them as checked files; note %q", st.Stale, wire.RepackNote)
	}
	if wire.DeferClass != core.DeferClassAbstention || !wire.SchemaMiss || wire.Output != checkedFilesAnswer || len(wire.Structured) != 0 {
		t.Fatalf("defer class %q schema_miss=%v output kept=%v structured=%s, want an abstention that carries the finished answer", wire.DeferClass, wire.SchemaMiss, wire.Output == checkedFilesAnswer, wire.Structured)
	}
	if !strings.Contains(wire.RepackNote, `"checked_files"`) {
		t.Fatalf("repack_note = %q, want the refusal to name the list", wire.RepackNote)
	}
	first, second := nextBody(t, fake.repackBodies, "grammar re-pack"), nextBody(t, fake.repackBodies, "grammar re-pack")
	if _, user := promptOf(t, first); strings.Contains(user, "left every list empty") {
		t.Fatalf("the FIRST attempt carries the correction:\n%s", preamble(user))
	}
	_, user := promptOf(t, second)
	if !sentenceBeforeText(user, "If one of those lists is the content of a requested list field") || !strings.Contains(user, "stays empty") {
		t.Fatalf("the second attempt does not carry the conditional correction ahead of the text:\n%s", preamble(user))
	}
}

// An answer that is itself a JSON array: the prompt says so (the empty-values rule would make every
// field absent, which is the inversion this branch set out to remove), lists what the array holds,
// and the object a seat then writes is read against the array.
func arrayAnswer() string {
	var b strings.Builder
	b.WriteString("```json\n[\n")
	for i := 1; i <= 11; i++ {
		sep := ","
		if i == 11 {
			sep = ""
		}
		fmt.Fprintf(&b, "  {\"quote\": \"Sentence %d of the guide, which the release notes now contradict.\", \"release_note_says\": \"Release note %d.\"}%s\n", i, i, sep)
	}
	b.WriteString("]\n```")
	return b.String()
}

func TestRepackOfAnAnswerThatIsAJSONArrayIsToldSoAndReadAgainstIt(t *testing.T) {
	answer := arrayAnswer()
	const emptyRule = "Use empty values when a field is absent"
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { return doneChat(answer) },
		repackStream: func(_ int64, body map[string]any, w http.ResponseWriter, _ *http.Request) {
			// A literal extractor: told that an absent field is empty, it finds every field absent.
			if system, _ := promptOf(t, body); strings.Contains(system, emptyRule) {
				writeCompletion(w, hollowObject, "stop", 37)
				return
			}
			writeCompletion(w, faithfulObject(), "stop", 400)
		},
		chatFallback: func(int64) string { return doneChat(hollowObject) },
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
	system, user := promptOf(t, nextBody(t, fake.repackBodies, "grammar re-pack"))
	if strings.Contains(system, emptyRule) || !strings.Contains(system, "is itself a JSON array") {
		t.Errorf("the system prompt does not say the TEXT is an array:\n%s", system)
	}
	if want := `The TEXT holds: array of 11 objects with fields "quote", "release_note_says".`; !strings.Contains(preamble(user), want) {
		t.Errorf("the instructions do not say %s:\n%s", want, preamble(user))
	}
	if !strings.Contains(wire.RepackNote, "kept 10 of 11") {
		t.Errorf("repack_note = %q, want the note that ten of eleven items were kept", wire.RepackNote)
	}
}

// And a seat that stays hollow over an array answer is refused like any other.
func TestRunAgentTaskRepackDoesNotPublishAnEmptyObjectForAnAnswerThatIsAnArray(t *testing.T) {
	answer := arrayAnswer()
	fake := hollowSeat(answer)
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, fidelityContract())))
	if !wire.Deferred || wire.DeferClass != core.DeferClassAbstention {
		t.Fatalf("deferred=%v class=%q, want an abstention (the object holds none of the array's 11 items); structured = %s", wire.Deferred, wire.DeferClass, wire.Structured)
	}
	if !strings.Contains(wire.RepackNote, "11") || !strings.Contains(wire.RepackNote, "answer's own list") {
		t.Fatalf("repack_note = %q, want the finding to name the answer's own list and its 11 items", wire.RepackNote)
	}
}
