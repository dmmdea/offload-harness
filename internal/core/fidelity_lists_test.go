package core

import (
	"fmt"
	"strings"
	"testing"
)

// The second round of review on the fidelity reading: the truthful empty list that the retry prompt
// could turn into a fabricated one, a list nested under an object, an answer that is itself a JSON
// array, and a list declared nullable.

// ListsLost names the lists the refusal is about, and only those: the retry prompt is built from it,
// so it must not tell the seat to copy a list the object already handled.
func TestListsLostNamesOnlyTheListsTheObjectDidNotCarry(t *testing.T) {
	schema := schemaOf(t, repackSchema)
	// No list of the answer is a schema list: the orphan is named, and it is not a same-named list.
	checked := `{"doc_title":"x","up_to_date":true,"checked_files":["a.md","b.md","c.md"]}`
	lost := ListsLost([]byte(`{"doc_title":"x","stale":[],"up_to_date":true}`), schema, checked)
	if len(lost) != 1 || lost[0].Path != "checked_files" || lost[0].Held != 3 || lost[0].Shared {
		t.Fatalf("lost = %+v, want the one orphan list checked_files (3 items), not same-named", lost)
	}
	// The answer has a schema list of the same name that came back empty: that one is named, and the
	// orphan list beside it (not compared when a same-named list exists) is not.
	same := `{"doc_title":"x","stale":["a","b","c"],"checked_files":["a.md"],"up_to_date":false}`
	lost = ListsLost([]byte(`{"doc_title":"x","stale":[],"up_to_date":true}`), schema, same)
	if len(lost) != 1 || lost[0].Path != "stale" || lost[0].Held != 3 || !lost[0].Shared {
		t.Fatalf("lost = %+v, want only the same-named list stale (3 items)", lost)
	}
	// Nothing lost, nothing named.
	if lost := ListsLost([]byte(`{"doc_title":"x","stale":["a","b","c"],"up_to_date":false}`), schema, same); len(lost) != 0 {
		t.Fatalf("lost = %+v for an object that kept the list", lost)
	}
}

// A truthful "nothing is stale" object over an answer whose only list is not a schema list is
// refused: the check cannot tell a list of what was checked from the stale sentences under another
// name. That is the accepted cost of reading by key, pinned here so it is a decision and not a
// surprise; the retry prompt is what keeps the refusal from becoming a fabricated list.
func TestAnAnswerWhoseOnlyListIsNotTheSchemasIsRefused(t *testing.T) {
	schema := schemaOf(t, repackSchema)
	answer := `{"doc_title":"x","up_to_date":true,"checked_files":["a.md","b.md","c.md"]}`
	empty := `{"doc_title":"x","stale":[],"up_to_date":true}`
	if got := ListsEmptied([]byte(empty), schema, answer); !strings.Contains(got, `"checked_files"`) {
		t.Fatalf("finding = %q, want the refusal that names checked_files", got)
	}
}

// A list nested under an object is a list of the answer: {"report":{"outdated":[...]}} is the same
// answer as {"outdated":[...]} one level down.
func TestListsEmptiedReadsListsNestedUnderAnObject(t *testing.T) {
	schema := schemaOf(t, repackSchema)
	nested := `{"doc_title":"Intake guide","report":{"outdated":["s1","s2","s3"],"scanned":"yes"},"meta":{"deeper":{"items":["i1","i2"]}}}`
	got := ListsEmptied([]byte(hollowObject), schema, nested)
	if !strings.Contains(got, `"report.outdated"`) || !strings.Contains(got, `"meta.deeper.items"`) || !strings.Contains(got, "5") {
		t.Fatalf("finding = %q, want the nested lists named by their paths (5 items in all)", got)
	}
	// The object kept some: nothing to refuse, and a note says how many.
	kept := `{"doc_title":"Intake guide","stale":["s1","s2","s3"],"up_to_date":false}`
	if got := ListsEmptied([]byte(kept), schema, nested); got != "" {
		t.Fatalf("an object that kept the nested items: finding %q", got)
	}
	if got := ListsTrimmed([]byte(kept), schema, nested); !strings.Contains(got, "kept 3 of 5") {
		t.Fatalf("note = %q, want kept 3 of 5", got)
	}
	// An object that carries the nested list itself, at the same path, lost nothing.
	carried := `{"doc_title":"Intake guide","stale":[],"up_to_date":true,"report":{"outdated":["s1","s2","s3"]},"meta":{"deeper":{"items":["i1","i2"]}}}`
	if got := ListsEmptied([]byte(carried), schema, nested); got != "" {
		t.Fatalf("an object that carried the nested lists: finding %q", got)
	}
	// Nested lists with no content are no content to lose.
	none := `{"doc_title":"x","report":{"outdated":[],"blanks":["",""]},"up_to_date":true}`
	if got := ListsEmptied([]byte(hollowObject), schema, none); got != "" {
		t.Fatalf("nested lists with no content: finding %q", got)
	}
	// A nested list of the schema's own path is that list, not an orphan.
	nestedSchema := schemaOf(t, `{"properties":{"report":{"type":"object","properties":{"items":{"type":"array","items":{"type":"string"}}}}}}`)
	if got := ListsEmptied([]byte(`{"report":{"items":["a"]}}`), nestedSchema, `{"report":{"items":["a","b"]}}`); got != "" {
		t.Fatalf("a nested list of the schema's own path was read as an orphan: %q", got)
	}
	// A nested orphan under an object the schema DOES define (report.entries beside report.items).
	if got := ListsEmptied([]byte(`{"report":{"items":[]}}`), nestedSchema, `{"report":{"entries":["a","b"]}}`); !strings.Contains(got, `"report.entries"`) {
		t.Fatalf("an orphan list under a defined object was not read: %q", got)
	}
}

// An answer that is itself a JSON array has no object to take keys from, and the whole answer is
// its list: an object that wrote empty lists over it lost every item.
func TestListsEmptiedReadsAnAnswerThatIsAJSONArray(t *testing.T) {
	schema := schemaOf(t, repackSchema)
	var b strings.Builder
	b.WriteString("```json\n[\n")
	for i := 1; i <= 11; i++ {
		sep := ","
		if i == 11 {
			sep = ""
		}
		fmt.Fprintf(&b, "  {\"quote\": \"Sentence %d of the guide.\", \"release_note_says\": \"Release note %d.\"}%s\n", i, i, sep)
	}
	b.WriteString("]\n```")
	answer := b.String()
	got := ListsEmptied([]byte(hollowObject), schema, answer)
	if !strings.Contains(got, "11") || !strings.Contains(got, "list") {
		t.Fatalf("finding = %q, want the 11 items of the answer's own list named", got)
	}
	if lost := ListsLost([]byte(hollowObject), schema, answer); len(lost) != 1 || lost[0].Path != "" || lost[0].Held != 11 {
		t.Fatalf("lost = %+v, want the answer's own list (path empty, 11 items)", lost)
	}
	kept := `{"doc_title":"x","stale":["Sentence 1 of the guide."],"up_to_date":false}`
	if got := ListsEmptied([]byte(kept), schema, answer); got != "" {
		t.Fatalf("an object that kept an item of the array: finding %q", got)
	}
	// What is not an array answer: an empty array, a footnote in prose, an array that is a small
	// part of a long prose answer, an array of numbers.
	for name, text := range map[string]string{
		"empty array":     "[]",
		"blank items":     `["", null, false]`,
		"footnote":        "The guide is current; nothing is stale [1].",
		"aside in prose":  strings.Repeat("The guide was read in full. ", 20) + `Example: ["a","b","c"]. ` + strings.Repeat("Nothing else was found. ", 20),
		"number elements": "[1, 2, 3]",
		"fenced aside":    strings.Repeat("The guide was read in full. ", 20) + "```json\n[\"a\",\"b\",\"c\"]\n```" + strings.Repeat("Nothing else was found. ", 20),
		"prose then list": "The stale sentences are: [\"a\", \"b\"] and nothing else, which I checked twice.",
	} {
		if got := ListsEmptied([]byte(hollowObject), schema, text); got != "" {
			t.Errorf("%s: finding %q, want none", name, got)
		}
	}
}

// A list of the answer at a path the schema defines with another type is that field's value (a
// type error the validator reports), not a list the schema has no word for.
func TestListsEmptiedLeavesAListAtAPathTheSchemaDefinesAlone(t *testing.T) {
	schema := schemaOf(t, `{"properties":{"stale":{"type":"array","items":{"type":"string"}},"tags":{"type":"string"},"report":{"type":"object","properties":{"items":{"type":"array","items":{"type":"string"}},"owner":{"type":"string"}}}}}`)
	if got := ListsEmptied([]byte(`{"stale":[],"tags":"x"}`), schema, `{"tags":["x","y"]}`); got != "" {
		t.Fatalf("a list under a top-level property the schema defines: finding %q", got)
	}
	if got := ListsEmptied([]byte(`{"stale":[],"report":{"items":[],"owner":"x"}}`), schema, `{"report":{"owner":["x","y"]}}`); got != "" {
		t.Fatalf("a list under a nested property the schema defines: finding %q", got)
	}
}

// A property declared nullable (type ["array","null"]) is still a list.
func TestSchemaListSitesSeesANullableArray(t *testing.T) {
	schema := schemaOf(t, `{"properties":{"stale":{"type":["array","null"],"items":{"type":"string"},"maxItems":3},"title":{"type":["string","null"]},"tags":{"type":["null","array"]},"n":{"type":["integer","null"]}}}`)
	sites := SchemaListSites(schema)
	var labels []string
	for _, s := range sites {
		labels = append(labels, s.Label())
	}
	if strings.Join(labels, ",") != "stale,tags" {
		t.Fatalf("sites = %v, want stale and tags", labels)
	}
	if sites[1].MaxItems() != 0 || sites[0].MaxItems() != 3 {
		t.Fatalf("maxItems = %d,%d, want 3 for stale and none for tags", sites[0].MaxItems(), sites[1].MaxItems())
	}
	answer := `{"doc_title":"x","outdated":["a","b"]}`
	if got := ListsEmptied([]byte(`{"title":"x","stale":null}`), schema, answer); got == "" {
		t.Fatalf("a hollow object over a schema whose list is nullable was not refused")
	}
}
