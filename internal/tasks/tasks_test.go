package tasks

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestBuildVQA: a vqa request with a question yields a non-empty system prompt,
// the question verbatim as the user prompt, and NO grammar (free-text VQA).
func TestBuildVQA(t *testing.T) {
	const q = "What number is shown in the image?"
	b, err := Build(core.Request{
		Task:   core.TaskVQA,
		Image:  "/tmp/x.png",
		Params: map[string]any{"question": q},
	})
	if err != nil {
		t.Fatalf("Build(vqa): %v", err)
	}
	if b.System == "" {
		t.Errorf("vqa System must be non-empty")
	}
	if b.User != q {
		t.Errorf("vqa User = %q, want the question %q", b.User, q)
	}
	if b.Grammar != "" {
		t.Errorf("vqa Grammar = %q, want empty (free-text)", b.Grammar)
	}
	if b.MaxTokens <= 0 {
		t.Errorf("vqa MaxTokens = %d, want a positive default", b.MaxTokens)
	}
}

// TestBuildVQAMissingQuestion: a vqa request without a question is an error,
// mirroring triage which requires its question.
func TestBuildVQAMissingQuestion(t *testing.T) {
	if _, err := Build(core.Request{Task: core.TaskVQA, Image: "/tmp/x.png"}); err == nil {
		t.Errorf("expected an error for a vqa request with no question, got nil")
	}
}

// TestBuildOCR: an ocr request needs NO question param, yields a non-empty system
// prompt and a fixed transcription user prompt, NO grammar (free-text OCR), and a
// generous MaxTokens (OCR output can be long).
func TestBuildOCR(t *testing.T) {
	b, err := Build(core.Request{Task: core.TaskOCR, Image: "/tmp/x.png"})
	if err != nil {
		t.Fatalf("Build(ocr): %v", err)
	}
	if b.System == "" {
		t.Errorf("ocr System must be non-empty")
	}
	if b.User == "" {
		t.Errorf("ocr User must be non-empty")
	}
	if b.Grammar != "" {
		t.Errorf("ocr Grammar = %q, want empty (free-text)", b.Grammar)
	}
	if b.MaxTokens < 1024 {
		t.Errorf("ocr MaxTokens = %d, want >= 1024 (OCR output can be long)", b.MaxTokens)
	}
}

func TestBuildVideoDescribe(t *testing.T) {
	if _, err := Build(core.Request{Task: core.TaskVideoDescribe}); err == nil {
		t.Fatal("video_describe with no question must error")
	}
	b, err := Build(core.Request{Task: core.TaskVideoDescribe, Params: map[string]any{"question": "what happens?"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.Grammar != "" {
		t.Error("video_describe is free-text; grammar must be empty")
	}
	if b.User != "what happens?" {
		t.Errorf("User = %q, want the question verbatim", b.User)
	}
}

// TestStableParamsKeyKeepsEntriesApart: the rendering is "<key>=<value>;" per entry,
// so a key or a value that itself held "=" or ";" could stand in for the structure
// around it, and two different params maps rendered the same string. The string is
// an ingredient of the result-cache key, names the render/video/edit output files,
// and is recorded on the persisted exemplar rows, so two maps that shared it shared
// all three. The pairs below rendered the same string before the fix.
func TestStableParamsKeyKeepsEntriesApart(t *testing.T) {
	pairs := []struct {
		name string
		a, b map[string]any
		old  string // what both rendered before the fix
	}{
		{
			name: "a value carries the next entry",
			a:    map[string]any{"a": "1;b=2"},
			b:    map[string]any{"a": "1", "b": "2"},
			old:  "a=1;b=2;",
		},
		{
			name: "a key carries its value and the next key",
			a:    map[string]any{"a=x;b": "y"},
			b:    map[string]any{"a": "x", "b": "y"},
			old:  "a=x;b=y;",
		},
		{
			name: "a value carries a whole entry after an empty one",
			a:    map[string]any{"": ";a="},
			b:    map[string]any{"": "", "a": ""},
			old:  "=;a=;",
		},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			// Guard on the guard: the pair has to be one the unescaped rendering made equal.
			if got := unescapedParamsKey(p.a); got != p.old || unescapedParamsKey(p.b) != p.old {
				t.Fatalf("the unescaped rendering of %v and %v is %q and %q, want both %q: pick a pair that collided",
					p.a, p.b, got, unescapedParamsKey(p.b), p.old)
			}
			ka, kb := StableParamsKey(p.a), StableParamsKey(p.b)
			if ka == kb {
				t.Errorf("%v and %v share the params key %q", p.a, p.b, ka)
			}
		})
	}
}

// unescapedParamsKey is StableParamsKey as it was written before the escaping: the
// reference the colliding pairs above are checked against.
func unescapedParamsKey(p map[string]any) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%v;", k, p[k])
	}
	return b.String()
}

// TestStableParamsKeyOrdinaryParamsAreUnchanged holds the rendering of every params
// map whose keys and rendered values contain none of "%", ";" and "=" to the exact
// bytes it had before the escaping. That is a promise to more than the cache: the
// string names the render-/video-/edit- output files (a stem is a hash of prompt +
// this string, and an existing file is found again by that name) and is recorded on
// the persisted exemplar rows, so a change to it for ordinary params would orphan
// both. Backslashes are ordinary: image, edit and upscale params carry Windows paths,
// and those file names must not move. The expected strings are literals, written from
// the format, not computed by StableParamsKey.
func TestStableParamsKeyOrdinaryParamsAreUnchanged(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"nil map", nil, ""},
		{"empty map", map[string]any{}, ""},
		{"strings, sorted by key", map[string]any{"b": "two", "a": "one"}, "a=one;b=two;"},
		{
			"the image-batch parameter set",
			map[string]any{"seed": 5, "width": 0, "height": 0, "steps": 0, "negative": "people"},
			"height=0;negative=people;seed=5;steps=0;width=0;",
		},
		{"floats", map[string]any{"temperature": 0.7, "top_p": 1.0, "n": float64(3)}, "n=3;temperature=0.7;top_p=1;"},
		{"bools", map[string]any{"stream": true, "refine": false}, "refine=false;stream=true;"},
		{"a nested map", map[string]any{"opts": map[string]any{"b": 2, "a": "x"}, "seed": 7}, "opts=map[a:x b:2];seed=7;"},
		{"slices", map[string]any{"tags": []string{"x", "y"}, "ids": []any{1, "a"}}, "ids=[1 a];tags=[x y];"},
		{"a nil value", map[string]any{"k": nil}, "k=<nil>;"},
		{
			"prose with commas, punctuation and non-ASCII",
			map[string]any{"negative": "blurry, low quality, señor ñ!", "question": "what happens?"},
			"negative=blurry, low quality, señor ñ!;question=what happens?;",
		},
		{"paths and urls without backslashes", map[string]any{"path": "/tmp/a/b.png", "url": "http://host:8080/y"}, "path=/tmp/a/b.png;url=http://host:8080/y;"},
		{"an empty key and an empty value", map[string]any{"": ""}, "=;"},
		// The backslash is not special to the format, so these render as they always did.
		{"Windows paths", map[string]any{"image": `C:\imgs\a.png`, "mask": `C:\imgs\m.png`}, `image=C:\imgs\a.png;mask=C:\imgs\m.png;`},
		{"a UNC path", map[string]any{"share": `\\server\share\a.png`}, `share=\\server\share\a.png;`},
		{"backslashes at the end of a key and of a value", map[string]any{"k": `trailing\`, `key\`: "v"}, `k=trailing\;key\=v;`},
		{"a lone backslash value", map[string]any{"b": `\`}, `b=\;`},
	}
	for _, tc := range cases {
		if got := StableParamsKey(tc.in); got != tc.want {
			t.Errorf("%s: StableParamsKey(%v) = %q, want %q (the bytes ordinary params always had)", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestStableParamsKeyEscapesWhatItMustEscape pins the escaping itself for the values
// that are NOT ordinary: "%", ";" and "=" are written as "%25", "%3B" and "%3D", in
// keys and in the rendered value alike. "%" is the escape character, so it is escaped
// too: that is what keeps a value that spells an escape ("%3B") apart from the
// character it spells (";"). Backslashes are left alone.
func TestStableParamsKeyEscapesWhatItMustEscape(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"a semicolon and an equals sign in a value", map[string]any{"a": "1;b=2"}, `a=1%3Bb%3D2;`},
		{"a semicolon and an equals sign in a key", map[string]any{"a=x;b": "y"}, `a%3Dx%3Bb=y;`},
		{"a percent sign", map[string]any{"p": "100%"}, `p=100%25;`},
		{"a value that spells an escape", map[string]any{"p": "%3B"}, `p=%253B;`},
		{"a trailing percent does not run into the separator", map[string]any{"a": "x%", "b": "y"}, `a=x%25;b=y;`},
		{"a percent sign in a key", map[string]any{"50%": "y"}, `50%25=y;`},
		{"inside a nested map", map[string]any{"o": map[string]any{"k": "v;w"}}, `o=map[k:v%3Bw];`},
		{"a backslash next to a special character", map[string]any{"p": `C:\a;b`}, `p=C:\a%3Bb;`},
	}
	for _, tc := range cases {
		if got := StableParamsKey(tc.in); got != tc.want {
			t.Errorf("%s: StableParamsKey(%v) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
	if StableParamsKey(map[string]any{"p": "%3B"}) == StableParamsKey(map[string]any{"p": ";"}) {
		t.Error(`a value that spells "%3B" shares a params key with the value ";"`)
	}
}

// TestStableParamsKeyIsInjectiveOverASmallAlphabet is the exhaustive form of the
// pairs above: every params map of up to two entries whose keys and values are
// strings of up to two characters from the alphabet {a, %, =, ;}, the three
// characters the format treats specially and one ordinary one. No two different
// maps may render the same string, whatever the pair. (About 93,000 maps; it runs
// in milliseconds.)
func TestStableParamsKeyIsInjectiveOverASmallAlphabet(t *testing.T) {
	alphabet := []string{"a", "%", "=", ";"}
	strs := []string{""}
	for _, c := range alphabet {
		strs = append(strs, c)
		for _, d := range alphabet {
			strs = append(strs, c+d)
		}
	}

	seen := map[string]map[string]any{}
	check := func(m map[string]any) {
		k := StableParamsKey(m)
		if prev, dup := seen[k]; dup {
			t.Fatalf("%v and %v both render %q", prev, m, k)
		}
		seen[k] = m
	}
	for _, k1 := range strs {
		for _, v1 := range strs {
			check(map[string]any{k1: v1})
			for _, k2 := range strs {
				if k2 <= k1 { // keys are distinct, and each unordered pair of entries is built once
					continue
				}
				for _, v2 := range strs {
					check(map[string]any{k1: v1, k2: v2})
				}
			}
		}
	}
	if len(seen) < 90000 {
		t.Fatalf("only %d maps were checked, the enumeration is not what the doc says", len(seen))
	}
}

// TestStableParamsKeyIsInjectiveOverEscapeSpellings closes what the alphabet above
// cannot reach: an escape is three characters ("%3B"), so a format that forgot to
// escape "%" itself only collides on a value that SPELLS an escape, and that needs
// the characters of the spelling in the alphabet. Every single-entry map whose key
// and value are strings of up to three characters from {a, %, ;, =, 3, B, D}
// (160,000 maps) must render differently, so a value "%3B" is kept apart from the
// value ";" and "%3D" from "=".
func TestStableParamsKeyIsInjectiveOverEscapeSpellings(t *testing.T) {
	alphabet := []string{"a", "%", ";", "=", "3", "B", "D"}
	strs := []string{""}
	for _, c := range alphabet {
		strs = append(strs, c)
		for _, d := range alphabet {
			strs = append(strs, c+d)
			for _, e := range alphabet {
				strs = append(strs, c+d+e)
			}
		}
	}

	seen := make(map[string][2]string, len(strs)*len(strs))
	for _, k := range strs {
		for _, v := range strs {
			got := StableParamsKey(map[string]any{k: v})
			if prev, dup := seen[got]; dup {
				t.Fatalf("{%q: %q} and {%q: %q} both render %q", prev[0], prev[1], k, v, got)
			}
			seen[got] = [2]string{k, v}
		}
	}
	if want := 400 * 400; len(seen) != want {
		t.Fatalf("checked %d maps, want %d: the enumeration is not what the doc says", len(seen), want)
	}
}

// TestStableParamsKeyNestedAndNonStringValuesKeepGoRendering PINS A DOCUMENTED LIMIT,
// it does not describe a goal. Only the top-level "<key>=<value>;" structure is
// unambiguous. A value is rendered with %v, which is type-erased and, for slices and
// maps, separates elements with a space, so values that print alike render alike:
// the string "5" and the number 5, a one-element slice holding "a b" and a two-element
// slice holding "a" and "b", a map entry whose value holds "k2:v2" and a second entry.
// The pairs below are exactly such collisions and must keep rendering equal.
//
// Changing that is possible (a typed, delimited rendering of nested values) and is
// deliberate work: the string is hashed into the output file names and recorded on
// the persisted exemplar rows, so every nested or non-string value would get a new
// file name and a new recorded key. If this test goes red because the rendering was
// changed on purpose, update the doc comment on StableParamsKey, the paragraph in
// docs/systems/offload-pipeline.md and the changelog fragment, and say that file
// names move.
func TestStableParamsKeyNestedAndNonStringValuesKeepGoRendering(t *testing.T) {
	pairs := []struct {
		name string
		a, b map[string]any
		want string // what both render
	}{
		{
			name: "a slice element with a space against two elements",
			a:    map[string]any{"tags": []any{"a b"}},
			b:    map[string]any{"tags": []any{"a", "b"}},
			want: "tags=[a b];",
		},
		{
			name: "a nested map value that spells another entry",
			a:    map[string]any{"o": map[string]any{"k": "v k2:v2"}},
			b:    map[string]any{"o": map[string]any{"k": "v", "k2": "v2"}},
			want: "o=map[k:v k2:v2];",
		},
		{
			name: "a string against the number it spells",
			a:    map[string]any{"seed": "5"},
			b:    map[string]any{"seed": 5},
			want: "seed=5;",
		},
	}
	for _, p := range pairs {
		ka, kb := StableParamsKey(p.a), StableParamsKey(p.b)
		if ka != p.want || kb != p.want {
			t.Errorf("%s: %v renders %q and %v renders %q, want both %q (the documented limit: nested and non-string values keep Go's %%v rendering)",
				p.name, p.a, ka, p.b, kb, p.want)
		}
	}
}
