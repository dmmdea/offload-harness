package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A re-pack that returns a schema-valid object with every list empty, over an answer that
// holds those lists under other keys, is a wrong success: the object validates, the
// acceptance checks on its scalar fields pass, and the caller gets "nothing is stale" for a
// document with eleven stale sentences. ListsEmptied is the check that reads the object
// against the answer it was written from.

// repackSchema is a contract's output schema: a title, a bounded list of strings, a flag.
const repackSchema = `{"type":"object","properties":{"doc_title":{"type":"string"},"stale":{"type":"array","items":{"type":"string"},"maxItems":10},"up_to_date":{"type":"boolean"},"notes":{"type":"string"}},"required":["doc_title","stale","up_to_date"]}`

func schemaOf(t *testing.T, raw string) map[string]any {
	t.Helper()
	var s map[string]any
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// invertedAnswer is a final answer in the shape that was seen: a fenced JSON object whose
// list lives under a key the schema does not have, as objects where the schema wants strings.
func invertedAnswer(items int) string {
	var b strings.Builder
	b.WriteString("```json\n{\n  \"doc_title\": \"Intake guide\",\n  \"outdated_sentences\": [\n")
	for i := 1; i <= items; i++ {
		sep := ","
		if i == items {
			sep = ""
		}
		fmt.Fprintf(&b, "    {\"quote\": \"Sentence %d of the guide, which the release notes now contradict.\", \"release_note_says\": \"Release note %d.\"}%s\n", i, i, sep)
	}
	b.WriteString("  ]\n}\n```")
	return b.String()
}

const hollowObject = `{"doc_title":"Intake guide","stale":[],"up_to_date":true,"notes":"No stale sentences were identified in the provided text."}`

func TestListsEmptiedCatchesAnObjectThatLostTheListsItsAnswerHeld(t *testing.T) {
	schema := schemaOf(t, repackSchema)
	got := ListsEmptied([]byte(hollowObject), schema, invertedAnswer(11))
	if got == "" {
		t.Fatalf("an all-empty object over an answer holding 11 list items under an orphan key was not rejected")
	}
	for _, want := range []string{`"stale"`, `"outdated_sentences"`, "11"} {
		if !strings.Contains(got, want) {
			t.Fatalf("finding %q does not name %s", got, want)
		}
	}
}

func TestListsEmptiedCatchesASameNamedListEmptied(t *testing.T) {
	schema := schemaOf(t, repackSchema)
	answer := `{"doc_title":"Intake guide","stale":["one","two","three"],"up_to_date":false}`
	got := ListsEmptied([]byte(hollowObject), schema, answer)
	if !strings.Contains(got, `"stale"`) || !strings.Contains(got, "3") {
		t.Fatalf("finding = %q, want the emptied same-named list and its 3 items named", got)
	}
}

func TestListsEmptiedLeavesAFaithfulObjectAlone(t *testing.T) {
	schema := schemaOf(t, repackSchema)
	// The control: the answer was a numbered list in prose, the object kept six items.
	prose := "1. The display card is closed.\n2. b\n3. c\n4. d\n5. e\n6. f\n"
	kept := `{"doc_title":"Composite tier","stale":["a","b","c","d","e","f"],"up_to_date":false}`
	if got := ListsEmptied([]byte(kept), schema, prose); got != "" {
		t.Fatalf("prose answer: finding %q, want none", got)
	}
	// Same-named keys: the answer's own list is the comparison, and it is kept.
	same := `{"doc_title":"x","stale":["a","b"],"up_to_date":false}`
	if got := ListsEmptied([]byte(kept), schema, same); got != "" {
		t.Fatalf("same-named list kept: finding %q, want none", got)
	}
	// An object that kept the items from the orphan key (as strings) is not empty.
	fromOrphan := `{"doc_title":"Intake guide","stale":["Sentence 1 of the guide","Sentence 2 of the guide"],"up_to_date":false}`
	if got := ListsEmptied([]byte(fromOrphan), schema, invertedAnswer(2)); got != "" {
		t.Fatalf("items kept from the orphan key: finding %q, want none", got)
	}
}

// Legitimately empty must still pass: the answer itself says the list is empty, and a list
// it holds under another key (a list of what was checked, say) is not the schema's list.
func TestListsEmptiedLetsALegitimatelyEmptyListThrough(t *testing.T) {
	schema := schemaOf(t, repackSchema)
	answer := `{"doc_title":"x","stale":[],"checked_files":["a.go","b.go","c.go"],"up_to_date":true}`
	empty := `{"doc_title":"x","stale":[],"up_to_date":true}`
	if got := ListsEmptied([]byte(empty), schema, answer); got != "" {
		t.Fatalf("the answer's own empty list was rejected: %q", got)
	}
	// An orphan list that holds nothing (empty strings, an empty list) is no content to lose.
	none := `{"doc_title":"x","errors":[],"blanks":["",""],"up_to_date":true}`
	if got := ListsEmptied([]byte(empty), schema, none); got != "" {
		t.Fatalf("orphan lists with no content: finding %q, want none", got)
	}
	// An answer with no lists at all.
	if got := ListsEmptied([]byte(empty), schema, `{"doc_title":"x","up_to_date":true}`); got != "" {
		t.Fatalf("an answer with no lists: finding %q, want none", got)
	}
}

func TestListsEmptiedOnlyJudgesAnAnswerThatIsAnObject(t *testing.T) {
	schema := schemaOf(t, repackSchema)
	// A code sample inside a long prose answer is an aside, not the answer.
	aside := strings.Repeat("The guide was read in full and compared with the notes. ", 20) +
		`For example {"things":["a","b","c"]} shows the shape. ` + strings.Repeat("Nothing else was found. ", 20)
	if got := ListsEmptied([]byte(hollowObject), schema, aside); got != "" {
		t.Fatalf("an object inside prose was read as the answer: %q", got)
	}
	// Garbage in either argument is not a finding.
	if got := ListsEmptied([]byte("not json"), schema, invertedAnswer(3)); got != "" {
		t.Fatalf("an object that is not JSON: %q", got)
	}
	if got := ListsEmptied([]byte(hollowObject), nil, invertedAnswer(3)); got != "" {
		t.Fatalf("no schema: %q", got)
	}
	if got := ListsEmptied([]byte(hollowObject), schemaOf(t, `{"properties":{"a":{"type":"string"}}}`), invertedAnswer(3)); got != "" {
		t.Fatalf("a schema with no list: %q", got)
	}
}

func TestListsEmptiedFollowsNestedObjectsByPath(t *testing.T) {
	schema := schemaOf(t, `{"properties":{"report":{"type":"object","properties":{"items":{"type":"array","items":{"type":"string"}},"title":{"type":"string"}}}}}`)
	answer := `{"report":{"items":["a","b"],"title":"t"}}`
	got := ListsEmptied([]byte(`{"report":{"items":[],"title":"t"}}`), schema, answer)
	if !strings.Contains(got, `"report.items"`) {
		t.Fatalf("finding = %q, want the nested list named by its path", got)
	}
	if got := ListsEmptied([]byte(`{"report":{"items":["a","b"],"title":"t"}}`), schema, answer); got != "" {
		t.Fatalf("a kept nested list: finding %q, want none", got)
	}
}

// The object may keep fewer items than the answer held (a maxItems the answer broke): that is
// not a loss to reject, and it is a loss to SAY.
func TestListsTrimmedSaysWhenTheObjectKeptFewerItemsThanTheAnswerHeld(t *testing.T) {
	schema := schemaOf(t, repackSchema)
	ten := make([]string, 10)
	for i := range ten {
		ten[i] = fmt.Sprintf("Sentence %d", i+1)
	}
	b, _ := json.Marshal(map[string]any{"doc_title": "Intake guide", "stale": ten, "up_to_date": false})
	got := ListsTrimmed(b, schema, invertedAnswer(11))
	for _, want := range []string{"kept 10 of 11", `"outdated_sentences"`, "maxItems 10"} {
		if !strings.Contains(got, want) {
			t.Fatalf("note %q does not say %q", got, want)
		}
	}
	// Same-named lists are compared with each other.
	sameAnswer := `{"stale":["a","b","c","d"]}`
	got = ListsTrimmed([]byte(`{"doc_title":"x","stale":["a","b"],"up_to_date":false}`), schema, sameAnswer)
	if !strings.Contains(got, "kept 2 of 4") || !strings.Contains(got, `"stale"`) {
		t.Fatalf("note = %q, want kept 2 of 4 under \"stale\"", got)
	}
	// Nothing lost, nothing said.
	if got := ListsTrimmed([]byte(`{"doc_title":"x","stale":["a","b","c","d"],"up_to_date":false}`), schema, sameAnswer); got != "" {
		t.Fatalf("a complete copy: note %q, want none", got)
	}
	if got := ListsTrimmed([]byte(`{"doc_title":"x","stale":["a"],"up_to_date":false}`), schema, "1. a\n2. b\n"); got != "" {
		t.Fatalf("a prose answer: note %q, want none", got)
	}
}
