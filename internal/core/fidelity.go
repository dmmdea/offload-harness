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
// second model: the answer is a JSON object (or a JSON array) that holds list items, and the
// object that claims to be its structuring holds none of them in any list the schema asks for. It
// judges lists only, and only an answer that IS JSON: a prose answer has no list to count, and a
// guess at one would reject honest work. So the check has two edges, both documented where the
// result is described. A prose answer whose list was lost is published (nothing can be counted).
// And a refusal can be wrong: an answer that lists what it checked under a key of its own, beside a
// schema list that is legitimately empty, looks exactly like stale sentences under another name.
// That refusal is an abstention defer with the finished answer attached, and the delegator
// re-places an abstention once on another node, which is a second full run. It is never a wrong
// success on its own; what keeps the retry from making it one is the prompt a refused attempt is
// followed by, which says to copy a list only if it is the content of a requested field
// (agentrepackfidelity.go).

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

// AnswerList decodes the JSON array a final answer IS: the whole text, or the one fenced block of
// it when the text around the fence is a short lead-in, as a list. It is stricter than AnswerObject
// on purpose: an array of anything is also how prose writes a footnote or an example, so the
// array must BE the answer (nothing but a fence and a short lead-in around it) before its items
// count, and only items that can hold a finding do (an object, a text, a list), never a number.
func AnswerList(text string) ([]any, bool) {
	t := strings.TrimSpace(text)
	if a := strings.Index(t, "```"); a >= 0 {
		rest := t[a+3:]
		nl := strings.Index(rest, "\n")
		b := -1
		if nl >= 0 {
			b = strings.Index(rest[nl+1:], "```")
		}
		if b < 0 {
			return nil, false
		}
		inner := rest[nl+1 : nl+1+b]
		if outside := len(t) - (3 + nl + 1 + b + 3); outside > AnswerWrapperMax {
			return nil, false
		}
		t = strings.TrimSpace(inner)
	}
	if !strings.HasPrefix(t, "[") || !strings.HasSuffix(t, "]") {
		return nil, false
	}
	var arr []any
	if json.Unmarshal([]byte(t), &arr) != nil {
		return nil, false
	}
	return arr, true
}

// answerItems counts the items of an answer that is itself a list: those that hold content and
// can be a finding (not a bare number or flag).
func answerItems(list []any) int {
	n := 0
	for _, el := range list {
		switch el.(type) {
		case string, map[string]any, []any:
			if !emptyJSON(el) {
				n++
			}
		}
	}
	return n
}

// ListsEmptied says so when a structured object lost the lists its answer held: "" when it did
// not, else one finding per emptied list, each ready to stand alone in a defer reason or an
// acceptance failure. schema is the contract's output schema (only its array properties are
// read, along nested object properties).
//
// The comparison is by key. A list the schema asks for and the answer also has under the same
// path is compared with that list and nothing else: an answer that says `"stale": []` has
// nothing to lose, whatever else it lists. Only when the answer shares no list path with the
// schema do its lists under paths the schema does not define count (at any depth of nested
// objects, and an answer that is itself a list counts as one), and then the object must hold SOME
// item in the lists the schema asks for. A legitimately empty answer that says so with the
// schema's own key therefore passes. One that lists what it checked under a key of its own does
// not, and that false refusal costs an honest defer and a re-placement, not a wrong success (see
// the header for what keeps it so).
func ListsEmptied(structured []byte, schema map[string]any, answer string) string {
	return strings.Join(compareLists(structured, schema, answer).emptied, "; ")
}

// ListsLost returns the lists the refusal of ListsEmptied is about (nil when there is none): the
// same-named list that came back empty, or the lists of the answer no schema list is named for.
// The prompt that follows a refused attempt is built from it, so it names what was lost and not
// every list the answer holds.
func ListsLost(structured []byte, schema map[string]any, answer string) []ListLoss {
	return compareLists(structured, schema, answer).lost
}

// ListsTrimmed says how many list items a structured object kept when it kept fewer than its
// answer held ("" when it kept them all, or when the answer is no object to count): a schema's
// maxItems the answer broke, or a seat that dropped items. It is a note for the result, not a
// reason to reject: the object is faithful as far as it goes, and the answer rides beside it.
func ListsTrimmed(structured []byte, schema map[string]any, answer string) string {
	return strings.Join(compareLists(structured, schema, answer).trimmed, "; ")
}

// ListLoss is a list of the answer that the object did not carry.
type ListLoss struct {
	Path   string // where the answer held it: "stale", "report.outdated"; "" for an answer that is itself a list
	Held   int    // its items that hold content
	Shared bool   // the schema has a list of this very path, which is the one that came back short
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

// SchemaTypeIs reports whether a schema spec declares the type, either as the string form
// ("array") or among a list of types (["array","null"], how a nullable list is written).
func SchemaTypeIs(spec map[string]any, typ string) bool {
	switch t := spec["type"].(type) {
	case string:
		return t == typ
	case []any:
		for _, el := range t {
			if s, ok := el.(string); ok && s == typ {
				return true
			}
		}
	}
	return false
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
			if SchemaTypeIs(sub, "array") {
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

// listReading is what comparing a structured object with its answer found.
type listReading struct {
	emptied []string   // refusals, one sentence each
	trimmed []string   // notes: the object kept fewer items than the answer held
	lost    []ListLoss // the lists the refusals are about
}

// answerList is a list the answer holds: where, and how many of its items hold content.
type answerList struct {
	path []string // key path from the answer's root; nil for an answer that is itself a list
	n    int
}

func (l answerList) label() string { return strings.Join(l.path, ".") }

// collectLists walks an answer object through its nested objects (never into a list: the items of
// a list are the list's content) and appends every list that holds content, in key order.
func collectLists(obj map[string]any, prefix []string, out *[]answerList, depth int) {
	if depth > 8 {
		return
	}
	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := append(append([]string(nil), prefix...), name)
		switch v := obj[name].(type) {
		case []any:
			if n := ContentItems(v); n > 0 {
				*out = append(*out, answerList{path: path, n: n})
			}
		case map[string]any:
			collectLists(v, path, out, depth+1)
		}
	}
}

// schemaDefines reports whether the schema has a property at the path, of any type: a list of the
// answer under a path the schema names is that property's content, wherever it sits, and not a
// list the schema has no word for.
func schemaDefines(schema map[string]any, path []string) bool {
	cur := schema
	for i, name := range path {
		props, _ := cur["properties"].(map[string]any)
		raw, ok := props[name]
		if !ok {
			return false
		}
		if i == len(path)-1 {
			return true
		}
		cur, _ = raw.(map[string]any)
		if cur == nil {
			return false
		}
	}
	return false
}

// compareLists is the one reading behind ListsEmptied, ListsLost and ListsTrimmed.
func compareLists(structured []byte, schema map[string]any, answer string) (r listReading) {
	if schema == nil {
		return r
	}
	var obj map[string]any
	if json.Unmarshal(structured, &obj) != nil || obj == nil {
		return r
	}
	sites := SchemaListSites(schema)
	if len(sites) == 0 {
		return r
	}
	// The delivered count at a site.
	got := func(s ListSite) int {
		l, _ := listAt(obj, s.Path)
		return ContentItems(l)
	}

	// The lists the answer holds. An answer that is a JSON object is read by path; one that is
	// itself a JSON array IS one list, with no key.
	var held []answerList
	src, isObject := AnswerObject(answer)
	if isObject {
		shared := false
		for _, s := range sites {
			srcList, has := listAt(src, s.Path)
			if !has || srcList == nil {
				continue
			}
			shared = true
			n, kept := ContentItems(srcList), got(s)
			switch {
			case n > 0 && kept == 0:
				r.emptied = append(r.emptied, fmt.Sprintf("emptied %q: the answer holds %d list items under %q and the object holds none", s.Label(), n, s.Label()))
				r.lost = append(r.lost, ListLoss{Path: s.Label(), Held: n, Shared: true})
			case kept < n:
				r.trimmed = append(r.trimmed, fmt.Sprintf("kept %d of %d list items under %q%s", kept, n, s.Label(), maxItemsTail(s.Spec, kept)))
			}
		}
		if shared {
			return r
		}
		collectLists(src, nil, &held, 0)
	} else if arr, ok := AnswerList(answer); ok {
		if n := answerItems(arr); n > 0 {
			held = []answerList{{n: n}}
		}
	} else {
		return r
	}

	// The answer shares no list with the schema: the lists it holds at paths the schema does not
	// define are the ones the object was to be written from.
	var orphans []answerList
	heldTotal := 0
	for _, l := range held {
		if len(l.path) > 0 {
			if schemaDefines(schema, l.path) {
				continue
			}
			// An object that carries the list itself, at the path the answer used, lost nothing: a
			// direct answer IS the object, extra keys and all, and the delegator reads every result
			// against its answer.
			if carried, _ := listAt(obj, l.path); ContentItems(carried) >= l.n {
				continue
			}
		}
		orphans = append(orphans, l)
		heldTotal += l.n
	}
	if heldTotal == 0 {
		return r
	}
	labels := make([]string, len(sites))
	keptTotal := 0
	for i, s := range sites {
		labels[i] = fmt.Sprintf("%q", s.Label())
		keptTotal += got(s)
	}
	from := make([]string, len(orphans))
	for i, l := range orphans {
		if len(l.path) == 0 {
			from[i] = "the answer's own list"
			continue
		}
		from[i] = fmt.Sprintf("%q", l.label())
	}
	if keptTotal == 0 {
		r.emptied = []string{fmt.Sprintf("emptied %s: the answer holds %d list items under %s and the object holds none", strings.Join(labels, ", "), heldTotal, strings.Join(from, ", "))}
		for _, l := range orphans {
			r.lost = append(r.lost, ListLoss{Path: l.label(), Held: l.n})
		}
		return r
	}
	if keptTotal < heldTotal {
		tail := ""
		if len(sites) == 1 {
			tail = maxItemsTail(sites[0].Spec, keptTotal)
		}
		r.trimmed = append(r.trimmed, fmt.Sprintf("kept %d of %d list items from %s%s", keptTotal, heldTotal, strings.Join(from, ", "), tail))
	}
	return r
}

// maxItemsTail says " (maxItems N)" when the list kept exactly the bound its schema sets, which is
// the reason the missing items are missing.
func maxItemsTail(spec map[string]any, kept int) string {
	if m, ok := spec["maxItems"].(float64); ok && int(m) == kept {
		return fmt.Sprintf(" (maxItems %d)", kept)
	}
	return ""
}
