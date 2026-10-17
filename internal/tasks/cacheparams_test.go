package tasks

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// fromJSON decodes a params document the way the MCP door does: numbers arrive as
// float64, lists as []any, objects as map[string]any.
func fromJSON(t *testing.T, doc string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("bad test document %q: %v", doc, err)
	}
	return m
}

func mustCacheParamsKey(t *testing.T, p map[string]any) string {
	t.Helper()
	k, err := CacheParamsKey(p)
	if err != nil {
		t.Fatalf("CacheParamsKey(%v): %v", p, err)
	}
	return k
}

// TestCacheParamsKeyKeepsTypesAndStructureApart: every pair below rendered ONE string
// through StableParamsKey (its %v is type-erased and space-separated), so a request
// could shape its params to equal another request's result-cache key. The first two
// are the release review's: extract schemas whose "required" lists differ and whose
// "minimum" differs in type. Each pair is checked against the old rendering first, so
// the table cannot go stale.
func TestCacheParamsKeyKeepsTypesAndStructureApart(t *testing.T) {
	const schemaTmpl = `{"schema":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},%s}}`
	pairs := []struct {
		name string
		a, b map[string]any
	}{
		{
			name: "required [a b] against required [a, b] (a name with a space)",
			a:    fromJSON(t, strings.Replace(schemaTmpl, "%s", `"required":["a b"]`, 1)),
			b:    fromJSON(t, strings.Replace(schemaTmpl, "%s", `"required":["a","b"]`, 1)),
		},
		{
			name: "minimum 1 against minimum \"1\"",
			a:    fromJSON(t, `{"schema":{"type":"object","properties":{"n":{"type":"number","minimum":"1"}}}}`),
			b:    fromJSON(t, `{"schema":{"type":"object","properties":{"n":{"type":"number","minimum":1}}}}`),
		},
		{"a string against the number it spells", map[string]any{"seed": "5"}, map[string]any{"seed": 5}},
		{"a string against the bool it spells", map[string]any{"stream": "true"}, map[string]any{"stream": true}},
		{"nil against the string %v prints for it", map[string]any{"k": nil}, map[string]any{"k": "<nil>"}},
		{"a slice element with a space against two elements", map[string]any{"tags": []any{"a b"}}, map[string]any{"tags": []any{"a", "b"}}},
		{
			"a nested map value that spells another entry",
			map[string]any{"o": map[string]any{"k": "v k2:v2"}},
			map[string]any{"o": map[string]any{"k": "v", "k2": "v2"}},
		},
		{"a list against a string that looks like one", map[string]any{"x": []any{"1", "2"}}, map[string]any{"x": "[1 2]"}},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			// Guard on the guard: the pair has to be one the old rendering made equal.
			if StableParamsKey(p.a) != StableParamsKey(p.b) {
				t.Fatalf("StableParamsKey tells %v and %v apart, so this pair no longer shows the defect: %q vs %q",
					p.a, p.b, StableParamsKey(p.a), StableParamsKey(p.b))
			}
			ka, kb := mustCacheParamsKey(t, p.a), mustCacheParamsKey(t, p.b)
			if ka == kb {
				t.Errorf("%v and %v share the result-cache params key %q", p.a, p.b, ka)
			}
		})
	}
}

// TestCacheParamsKeyIsStableAcrossRepresentations: the same logical params give the
// same string whichever Go types carried them, and whatever order a map iterates in, so
// the cache still hits for a repeat that came in by another door.
func TestCacheParamsKeyIsStableAcrossRepresentations(t *testing.T) {
	same := []struct {
		name string
		a, b map[string]any
	}{
		{"an int and the float64 the MCP door decodes", map[string]any{"seed": 5, "width": 1024}, fromJSON(t, `{"seed":5,"width":1024}`)},
		{"a []string and a []any", map[string]any{"labels": []string{"x", "y"}}, map[string]any{"labels": []any{"x", "y"}}},
		{"a map[string]string and a map[string]any", map[string]any{"m": map[string]string{"k": "v"}}, map[string]any{"m": map[string]any{"k": "v"}}},
		{"a pointer and the value it points to", map[string]any{"n": new(int)}, map[string]any{"n": 0}},
	}
	for _, c := range same {
		if ka, kb := mustCacheParamsKey(t, c.a), mustCacheParamsKey(t, c.b); ka != kb {
			t.Errorf("%s: %q != %q", c.name, ka, kb)
		}
	}

	// Map iteration order is random; the rendering must not be.
	big := map[string]any{}
	for _, k := range strings.Fields("q w e r t y u i o p a s d f g h j k l z x c v b n m") {
		big[k] = map[string]any{"z": 1, "y": []any{"b", "a"}, "x": map[string]any{"n": 2, "m": 1}}
	}
	first := mustCacheParamsKey(t, big)
	for i := 0; i < 25; i++ {
		if got := mustCacheParamsKey(t, big); got != first {
			t.Fatalf("the rendering changed between calls:\n%q\n%q", first, got)
		}
	}
	if !strings.Contains(first, `"x":{"m":1,"n":2},"y":["b","a"],"z":1`) {
		t.Errorf("keys are not sorted at depth, and list order is not kept: %q", first)
	}

	for _, empty := range []map[string]any{nil, {}} {
		if got := mustCacheParamsKey(t, empty); got != "" {
			t.Errorf("empty params render %q, want \"\"", got)
		}
	}
}

// TestCacheParamsKeyIsPinned holds the rendering to exact strings. Changing it re-keys
// the whole result cache, so it has to be a visible edit.
func TestCacheParamsKeyIsPinned(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"classify labels", map[string]any{"labels": []string{"animal", "finance"}}, `{"labels":["animal","finance"]}`},
		{"scalars", map[string]any{"b": true, "a": 1.5, "c": nil, "s": "x;y=z"}, `{"a":1.5,"b":true,"c":null,"s":"x;y=z"}`},
		{
			"an extract schema",
			fromJSON(t, `{"schema":{"required":["a","b"],"type":"object","properties":{"b":{"type":"string"},"a":{"type":"string"}}}}`),
			`{"schema":{"properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a","b"],"type":"object"}}`,
		},
		{"a Windows path", map[string]any{"image": `C:\imgs\a.png`}, `{"image":"C:\\imgs\\a.png"}`},
	}
	for _, tc := range cases {
		if got := mustCacheParamsKey(t, tc.in); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

type ownJSON struct{}

func (ownJSON) MarshalJSON() ([]byte, error) { return []byte(`"x"`), nil }

// shout is a string kind whose JSON form is not its own text: without the check for
// marshalers it would look like a plain string and render as something else.
type shout string

func (s shout) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strings.ToUpper(string(s)) + `"`), nil
}

// TestCacheParamsKeyFailsClosed: a value with no faithful typed form returns an error
// instead of a rendering, so the caller skips the cache for that call. A fallback
// rendering would reopen the collision CacheParamsKey exists to close.
func TestCacheParamsKeyFailsClosed(t *testing.T) {
	self := map[string]any{}
	self["me"] = self
	loop := []any{nil}
	loop[0] = loop
	type plain struct{ A int }

	cases := []struct {
		name string
		in   map[string]any
	}{
		{"NaN", map[string]any{"x": math.NaN()}},
		{"positive infinity", map[string]any{"x": math.Inf(1)}},
		{"negative infinity in a float32", map[string]any{"x": float32(math.Inf(-1))}},
		{"NaN inside a nested list", map[string]any{"x": []any{1, map[string]any{"y": math.NaN()}}}},
		{"a function", map[string]any{"x": func() {}}},
		{"a channel", map[string]any{"x": make(chan int)}},
		{"a complex number", map[string]any{"x": complex(1, 2)}},
		{"a struct", map[string]any{"x": plain{A: 1}}},
		{"a pointer to a struct", map[string]any{"x": &plain{A: 1}}},
		{"a byte slice", map[string]any{"x": []byte("abc")}},
		{"a time", map[string]any{"x": time.Unix(0, 0)}},
		{"a type with its own JSON form", map[string]any{"x": ownJSON{}}},
		{"a string type with its own JSON form", map[string]any{"x": shout("a")}},
		{"a map keyed by ints", map[string]any{"x": map[int]string{1: "a"}}},
		{"a string that is not UTF-8", map[string]any{"x": "a\xffb"}},
		{"a nested string that is not UTF-8", map[string]any{"x": []any{"ok", "a\xffb"}}},
		{"a map key that is not UTF-8", map[string]any{"x": map[string]any{"a\xffb": 1}}},
		{"a top-level key that is not UTF-8", map[string]any{"a\xffb": 1}},
		{"a map that holds itself", map[string]any{"x": self}},
		{"a list that holds itself", map[string]any{"x": loop}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CacheParamsKey(tc.in)
			if err == nil {
				t.Fatalf("CacheParamsKey returned %q and no error for a value with no typed form", got)
			}
			if !errors.Is(err, ErrParamsNotKeyable) {
				t.Errorf("the error does not wrap ErrParamsNotKeyable: %v", err)
			}
			if got != "" {
				t.Errorf("a failed key must be empty, got %q", got)
			}
		})
	}

	// The error names kinds, never values: it goes to a log line and the ledger.
	_, err := CacheParamsKey(map[string]any{"question": "the secret text", "x": func() {}})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("the error must name the offender and not echo a value: %v", err)
	}

	// Not vacuous: values next to the failing ones that DO have a typed form.
	for _, ok := range []map[string]any{
		{"x": [2]byte{1, 2}},
		{"x": json.Number("5")},
		{"x": []any{nil, true, 1, "s", 2.5, []any{}, map[string]any{}}},
		{"x": (*int)(nil)},
		{"x": map[string]int{"a": 1}},
	} {
		if _, err := CacheParamsKey(ok); err != nil {
			t.Errorf("CacheParamsKey(%v) failed: %v", ok, err)
		}
	}
}
