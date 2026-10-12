package pipeline

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
)

// What a structured re-pack tells the seat about the text it re-packs, and how it checks the
// object it gets back against that text.
//
// The re-pack is an extraction over the loop's final answer, and its system prompt said "use
// empty values when a field is absent". For a prose answer that is the right rule. For an answer
// that is itself a JSON object, with keys of its own, it was the trap: no key of the TEXT was
// called "stale", so a literal extractor wrote [] for the schema's "stale", true for
// "up_to_date" and a note that nothing was stale, over a text that listed eleven stale
// sentences under another name. The object validated and was published as the structured
// result. Two changes, both here:
//
//   - the prompt says the TEXT is a JSON object, lists its keys (names and counts, never the
//     content), says to take each field from the key whose content matches and to leave a field
//     empty only when no key holds content for it, and states the bound the schema puts on a
//     list (the grammar's projection of the schema drops maxItems, so the prompt is where the
//     seat learns it);
//   - an object that comes back with every list the schema asks for empty, over a text that
//     holds list items, is refused (core.ListsEmptied), and the next attempt is told what the
//     refused one left out.

// repackSource is the text a re-pack reads, as far as its prompts and its checks care: whether it
// is itself a JSON object, and which lists it holds.
type repackSource struct {
	obj map[string]any // the TEXT decoded when it is a JSON object, else nil
}

// newRepackSource reads the final answer once per re-pack call.
func newRepackSource(output string) repackSource {
	obj, _ := core.AnswerObject(output)
	return repackSource{obj: obj}
}

// emptyValuesRule is the sentence both prompts carried before: right for a prose TEXT.
const emptyValuesRule = "Use empty values when a field is absent."

// objectTextRule replaces it when the TEXT is a JSON object. A seat that reads "use empty values
// when a field is absent" beside a JSON text whose keys are not the requested names finds every
// field absent.
const objectTextRule = "The TEXT is itself a JSON object whose keys need not be the requested field names: take each requested field from the key whose content matches it (a list of strings from the main text of each item of the matching list), and use an empty value only for a field that no key of the TEXT holds content for."

// absentRule is the system prompt's sentence about fields the TEXT does not hold.
func (s repackSource) absentRule() string {
	if s.obj == nil {
		return emptyValuesRule
	}
	return objectTextRule
}

// repackKeysCap bounds how many keys of the TEXT the listing names.
const repackKeysCap = 40

// keysNote lists the keys of a TEXT that is a JSON object, each with what it holds: a name and a
// shape, never the content, which is in the TEXT once. "" for a prose TEXT.
func (s repackSource) keysNote() string {
	if s.obj == nil {
		return ""
	}
	names := sortedKeys(s.obj)
	parts := make([]string, 0, len(names))
	for i, name := range names {
		if i == repackKeysCap {
			parts = append(parts, fmt.Sprintf("and %d more", len(names)-repackKeysCap))
			break
		}
		parts = append(parts, fmt.Sprintf("%q (%s)", name, valueShape(s.obj[name])))
	}
	// Worded so that "TEXT:" appears once in the prompt, as the marker the text follows.
	return "The TEXT has these keys, each with what it holds: " + strings.Join(parts, ", ") + "."
}

// valueShape names what a decoded value is, with the counts and field names a reader needs to
// map it onto a schema: "string", "array of 11 objects with fields "quote", "release_note_says"".
func valueShape(v any) string {
	switch t := v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	case map[string]any:
		if f := fieldNames(t); f != "" {
			return "object with fields " + f
		}
		return "object"
	case []any:
		return listShape(t)
	}
	return "value"
}

// listShape is "array of N items" with the kind of item when they are all one kind.
func listShape(list []any) string {
	if len(list) == 0 {
		return "empty array"
	}
	kind := ""
	fields := map[string]bool{}
	for _, el := range list {
		k := ""
		switch e := el.(type) {
		case string:
			k = "strings"
		case float64:
			k = "numbers"
		case bool:
			k = "booleans"
		case map[string]any:
			k = "objects"
			for name := range e {
				fields[name] = true
			}
		case []any:
			k = "arrays"
		default:
			k = "items"
		}
		if kind == "" {
			kind = k
		} else if kind != k {
			kind = "items"
		}
	}
	out := fmt.Sprintf("array of %d %s", len(list), kind)
	if kind == "objects" && len(fields) > 0 {
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		sort.Strings(names)
		out += " with fields " + quoteList(names)
	}
	return out
}

// repackFieldsCap bounds how many field names a shape names.
const repackFieldsCap = 8

func fieldNames(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	return quoteList(sortedKeys(m))
}

func quoteList(names []string) string {
	if len(names) > repackFieldsCap {
		names = append(append([]string(nil), names[:repackFieldsCap]...), "…")
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		if n == "…" {
			quoted[i] = n
			continue
		}
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(quoted, ", ")
}

func sortedKeys(m map[string]any) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// correction is what the attempt after a refused one is told: the previous object left every
// list empty, and where the TEXT holds list items. "" when the TEXT holds none (and then a
// refusal could not have been for this reason).
func (s repackSource) correction() string {
	if s.obj == nil {
		return ""
	}
	var where []string
	for _, name := range sortedKeys(s.obj) {
		list, _ := s.obj[name].([]any)
		if n := core.ContentItems(list); n > 0 {
			where = append(where, fmt.Sprintf("%q (%d items)", name, n))
		}
	}
	if len(where) == 0 {
		return ""
	}
	return "A previous attempt to write this object left every list empty, although the TEXT holds list items under " + strings.Join(where, ", ") + ". Write those items into the list fields, one list item for each of them, in the order the TEXT gives them."
}

// limitsNote states the bound the schema puts on each of its lists, from the contract's own
// schema: the grammar lane's projection of it drops maxItems, and a seat that is not told the
// bound writes the whole list and fails validation (a list of eleven against maxItems 10 failed
// every attempt). "" when no list is bounded.
func limitsNote(schema map[string]any) string {
	var parts []string
	for _, site := range core.SchemaListSites(schema) {
		if n := site.MaxItems(); n > 0 {
			unit := "items"
			if n == 1 {
				unit = "item"
			}
			parts = append(parts, fmt.Sprintf("%q holds at most %d %s", site.Label(), n, unit))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "List limits: " + strings.Join(parts, "; ") + " (keep the first ones the TEXT gives)."
}

// repackUserPrompt is the user turn of a re-pack attempt: the lead sentence and the fields, then
// each non-empty note as a paragraph of its own AHEAD of the text (a seat doing an extraction
// reads what follows TEXT: as more text), then the text. With no note it is the prompt the
// re-pack always sent.
func repackUserPrompt(lead, fields string, notes []string, output string) string {
	var b strings.Builder
	b.WriteString(lead)
	b.WriteString(": ")
	b.WriteString(fields)
	b.WriteString(".\n\n")
	for _, n := range notes {
		if strings.TrimSpace(n) == "" {
			continue
		}
		b.WriteString(n)
		b.WriteString("\n\n")
	}
	b.WriteString("TEXT:\n")
	b.WriteString(output)
	return b.String()
}
