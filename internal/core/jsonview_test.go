package core

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestSelectKeys(t *testing.T) {
	cases := map[string][]string{
		"":                  nil,
		"   ":               nil,
		"a":                 {"a"},
		"a,b,c":             {"a", "b", "c"},
		" gist , segments ": {"gist", "segments"},
		"a,,b,":             {"a", "b"},
	}
	for in, want := range cases {
		if got := SelectKeys(in); !reflect.DeepEqual(got, want) {
			t.Errorf("SelectKeys(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestProjectFields(t *testing.T) {
	data := json.RawMessage(`{"gist":"hi","language":"es","segments":[{"id":0,"text":"a"}],"num_segments":1}`)

	// Empty selection -> unchanged.
	if got := ProjectFields(data, nil); string(got) != string(data) {
		t.Errorf("empty selection should be unchanged, got %s", got)
	}

	// Select a subset of top-level keys.
	got := ProjectFields(data, []string{"gist", "language"})
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("projected output not valid JSON: %v (%s)", err, got)
	}
	if len(m) != 2 || m["gist"] != "hi" || m["language"] != "es" {
		t.Errorf("projection = %s, want only {gist,language}", got)
	}
	if _, has := m["segments"]; has {
		t.Errorf("segments should have been projected out: %s", got)
	}

	// Drop the verbose array, keep everything else (the transcribe use case).
	got = ProjectFields(data, []string{"gist", "language", "num_segments"})
	if _, has := mapOf(t, got)["segments"]; has {
		t.Errorf("segments should be gone: %s", got)
	}

	// Absent key is skipped (no error, no null).
	got = ProjectFields(data, []string{"gist", "nope"})
	mm := mapOf(t, got)
	if _, has := mm["nope"]; has {
		t.Errorf("absent key must not appear: %s", got)
	}
	if mm["gist"] != "hi" {
		t.Errorf("present key must survive: %s", got)
	}
}

func TestProjectFieldsNonObjectUnchanged(t *testing.T) {
	// A JSON array is not an object — projection does not apply, return as-is.
	arr := json.RawMessage(`[1,2,3]`)
	if got := ProjectFields(arr, []string{"a"}); string(got) != string(arr) {
		t.Errorf("array should be unchanged, got %s", got)
	}
	// Invalid JSON returns unchanged.
	bad := json.RawMessage(`not json`)
	if got := ProjectFields(bad, []string{"a"}); string(got) != string(bad) {
		t.Errorf("invalid JSON should be unchanged, got %s", got)
	}
	// Empty data unchanged.
	if got := ProjectFields(nil, []string{"a"}); got != nil {
		t.Errorf("nil data should stay nil, got %s", got)
	}
}

func mapOf(t *testing.T, b json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not an object: %v (%s)", err, b)
	}
	return m
}
