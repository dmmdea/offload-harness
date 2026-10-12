package core

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Fidelity of a structured object to the final answer it was written from.
//
// A structured re-pack is a transformation of text a seat has already finished. The schema
// validator says the result has the right SHAPE; it cannot say the result kept the CONTENT. A
// seat whose final answer was itself a JSON object with keys of its own ("outdated_sentences",
// a list of {quote, release_note_says}) was re-packed into the schema's keys by an extractor
// told to "use empty values when a field is absent": no key of the answer is called "stale",
// so the extractor wrote [] for it, "up_to_date": true and a note that nothing was stale. The
// object validated, the acceptance checks on its scalar fields passed, and eleven stale
// sentences were published as none. Nothing compared the object with its source.
//
// The comparison here is the one cheap, deterministic fact that exposes that failure without a
// second model: the answer is a JSON object that holds list items, and the object that claims to
// be its structuring holds none of them in any list the schema asks for. It judges lists only,
// and only an answer that IS a JSON object: a prose answer has no list to count, and a guess at
// one would reject honest work.

// AnswerWrapperMax is the most prose (fences, "Here is the result:", a closing line) that may
// surround a JSON object for it to count as the answer when the object is under half of the text.
const AnswerWrapperMax = 200

// AnswerObject decodes the JSON object a final answer IS: the text trimmed to its outermost
// {...} span, with fences and a short preamble or tail around it allowed. The object must BE the
// answer and not an aside inside it, so a code sample in a long prose answer is not one (the same
// rule a direct structured answer is taken by). ok is false for anything else.
func AnswerObject(text string) (map[string]any, bool) {
	t := strings.TrimSpace(text)
	i, j := strings.Index(t, "{"), strings.LastIndex(t, "}")
	if i < 0 || j <= i {
		return nil, false
	}
	if span, outside := j+1-i, len(t)-(j+1-i); span*2 < len(t) && outside > AnswerWrapperMax {
		return nil, false
	}
	var obj map[string]any
	if json.Unmarshal([]byte(t[i:j+1]), &obj) != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// ListsEmptied says so when a structured object lost the lists its answer held: "" when it did
// not, else one finding per emptied list, each ready to stand alone in a defer reason or an
// acceptance failure. schema is the contract's output schema (only its array properties are
// read, along nested object properties).
//
// The comparison is by key. A list the schema asks for and the answer also has under the same
// path is compared with that list and nothing else: an answer that says `"stale": []` has
// nothing to lose, whatever else it lists. Only when the answer shares no list path with the
// schema do its lists under keys the schema does not define count, and then the object must hold
// SOME item in the lists the schema asks for. A legitimately empty answer therefore passes, and a
// false reject costs an honest defer with the finished answer attached, never a wrong success.
func ListsEmptied(structured []byte, schema map[string]any, answer string) string {
	emptied, _ := compareLists(structured, schema, answer)
	return strings.Join(emptied, "; ")
}

// ListsTrimmed says how many list items a structured object kept when it kept fewer than its
// answer held ("" when it kept them all, or when the answer is no object to count): a schema's
// maxItems the answer broke, or a seat that dropped items. It is a note for the result, not a
// reason to reject: the object is faithful as far as it goes, and the answer rides beside it.
func ListsTrimmed(structured []byte, schema map[string]any, answer string) string {
	_, trimmed := compareLists(structured, schema, answer)
	return strings.Join(trimmed, "; ")
}

// ListSite is one array property of the schema: its path ("report.items") and its spec.
type ListSite struct {
	Path []string
	Spec map[string]any
}

func (s ListSite) Label() string { return strings.Join(s.Path, ".") }

// MaxItems is the schema's bound on the list, 0 when it sets none.
func (s ListSite) MaxItems() int {
	if m, ok := s.Spec["maxItems"].(float64); ok && m > 0 {
		return int(m)
	}
	return 0
}

// SchemaListSites returns the array properties of a schema, through the properties of nested
// object properties, sorted by path so a finding reads the same on every run.
func SchemaListSites(schema map[string]any) []ListSite {
	var sites []ListSite
	var walk func(spec map[string]any, prefix []string)
	walk = func(spec map[string]any, prefix []string) {
		props, _ := spec["properties"].(map[string]any)
		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			sub, _ := props[name].(map[string]any)
			if sub == nil {
				continue
			}
			path := append(append([]string(nil), prefix...), name)
			if typ, _ := sub["type"].(string); typ == "array" {
				sites = append(sites, ListSite{Path: path, Spec: sub})
				continue
			}
			walk(sub, path)
		}
	}
	walk(schema, nil)
	return sites
}

// listAt reads the list at a path of a decoded object. has is true when something sits there;
// list is nil unless it is a list.
func listAt(obj map[string]any, path []string) (list []any, has bool) {
	var cur any = obj
	for _, name := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[name]
		if !ok {
			return nil, false
		}
	}
	list, _ = cur.([]any)
	return list, true
}

// ContentItems counts the items of a list that hold something a reader could call content: an
// empty string, null, false, zero, or an empty list or object is the schema's empty value.
func ContentItems(list []any) int {
	n := 0
	for _, el := range list {
		if !emptyJSON(el) {
			n++
		}
	}
	return n
}

// emptyJSON reports whether a decoded value holds nothing: null, an empty string, false, zero,
// and a list or object whose every element is one of those, at any depth.
func emptyJSON(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case bool:
		return !t
	case float64:
		return t == 0
	case []any:
		for _, el := range t {
			if !emptyJSON(el) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, el := range t {
			if !emptyJSON(el) {
				return false
			}
		}
		return true
	}
	return false
}

// compareLists is the one reading behind ListsEmptied and ListsTrimmed.
func compareLists(structured []byte, schema map[string]any, answer string) (emptied, trimmed []string) {
	if schema == nil {
		return nil, nil
	}
	var obj map[string]any
	if json.Unmarshal(structured, &obj) != nil || obj == nil {
		return nil, nil
	}
	src, ok := AnswerObject(answer)
	if !ok {
		return nil, nil
	}
	sites := SchemaListSites(schema)
	if len(sites) == 0 {
		return nil, nil
	}
	// The delivered count at a site.
	got := func(s ListSite) int {
		l, _ := listAt(obj, s.Path)
		return ContentItems(l)
	}

	shared := false
	for _, s := range sites {
		srcList, has := listAt(src, s.Path)
		if !has || srcList == nil {
			continue
		}
		shared = true
		held, kept := ContentItems(srcList), got(s)
		switch {
		case held > 0 && kept == 0:
			emptied = append(emptied, fmt.Sprintf("emptied %q: the answer holds %d list items under %q and the object holds none", s.Label(), held, s.Label()))
		case kept < held:
			trimmed = append(trimmed, fmt.Sprintf("kept %d of %d list items under %q%s", kept, held, s.Label(), maxItemsTail(s.Spec, kept)))
		}
	}
	if shared {
		return emptied, trimmed
	}

	// The answer shares no list with the schema: the lists it holds under keys the schema does
	// not define are the ones the object was to be written from.
	props, _ := schema["properties"].(map[string]any)
	var orphans []string
	heldTotal := 0
	names := make([]string, 0, len(src))
	for name := range src {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, defined := props[name]; defined {
			continue
		}
		l, _ := src[name].([]any)
		n := ContentItems(l)
		if n == 0 {
			continue
		}
		// An object that carries the list itself, under the key the answer used, lost nothing: a
		// direct answer IS the object, extra keys and all, and the delegator reads every result
		// against its answer.
		if carried, _ := obj[name].([]any); ContentItems(carried) >= n {
			continue
		}
		orphans = append(orphans, name)
		heldTotal += n
	}
	if heldTotal == 0 {
		return nil, nil
	}
	labels := make([]string, len(sites))
	keptTotal := 0
	for i, s := range sites {
		labels[i] = fmt.Sprintf("%q", s.Label())
		keptTotal += got(s)
	}
	from := make([]string, len(orphans))
	for i, name := range orphans {
		from[i] = fmt.Sprintf("%q", name)
	}
	if keptTotal == 0 {
		return []string{fmt.Sprintf("emptied %s: the answer holds %d list items under %s and the object holds none", strings.Join(labels, ", "), heldTotal, strings.Join(from, ", "))}, nil
	}
	if keptTotal < heldTotal {
		tail := ""
		if len(sites) == 1 {
			tail = maxItemsTail(sites[0].Spec, keptTotal)
		}
		trimmed = append(trimmed, fmt.Sprintf("kept %d of %d list items from %s%s", keptTotal, heldTotal, strings.Join(from, ", "), tail))
	}
	return nil, trimmed
}

// maxItemsTail says " (maxItems N)" when the list kept exactly the bound its schema sets, which is
// the reason the missing items are missing.
func maxItemsTail(spec map[string]any, kept int) string {
	if m, ok := spec["maxItems"].(float64); ok && int(m) == kept {
		return fmt.Sprintf(" (maxItems %d)", kept)
	}
	return ""
}
