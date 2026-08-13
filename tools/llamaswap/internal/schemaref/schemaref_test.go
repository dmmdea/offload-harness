// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package schemaref

import (
	"encoding/json"
	"strings"
	"testing"
)

type inner struct {
	Label string `json:"label"`
}

type sample struct {
	Version  int               `json:"version"`
	Name     string            `json:"name"`
	Ratio    float64           `json:"ratio"`
	OK       bool              `json:"ok"`
	Optional string            `json:"optional,omitempty"`
	Ptr      *int              `json:"ptr,omitempty"`
	List     []inner           `json:"list"`
	Lookup   map[string]int    `json:"lookup"`
	Free     any               `json:"free"`
	Raw      json.RawMessage   `json:"raw"`
	Hidden   string            `json:"-"`
	unseen   string            //nolint:unused // exercises the unexported skip
	Renamed  map[string]string `json:"renamed,omitempty"`
}

func propType(t *testing.T, s *Schema, name string) string {
	t.Helper()
	p, ok := s.Properties[name]
	if !ok {
		t.Fatalf("property %q missing; have %v", name, s.PropertyOrder)
	}
	return p.Type
}

func TestOfMapsGoKindsOntoJSONTypes(t *testing.T) {
	s := Of[sample]()
	if s.Type != "object" {
		t.Fatalf("root type = %q", s.Type)
	}
	for name, want := range map[string]string{
		"version": "integer",
		"name":    "string",
		"ratio":   "number",
		"ok":      "boolean",
		"ptr":     "integer",
		"list":    "array",
		"lookup":  "object",
	} {
		if got := propType(t, s, name); got != want {
			t.Errorf("%s: type = %q, want %q", name, got, want)
		}
	}
	// `any` and json.RawMessage carry provider-shaped payloads; pinning a
	// type would reject valid results.
	if got := propType(t, s, "free"); got != "" {
		t.Errorf("free: type = %q, want an open schema", got)
	}
	if got := propType(t, s, "raw"); got != "" {
		t.Errorf("raw: type = %q, want an open schema", got)
	}
	if _, present := s.Properties["Hidden"]; present {
		t.Error(`a json:"-" field leaked into the schema`)
	}
	if _, present := s.Properties["unseen"]; present {
		t.Error("an unexported field leaked into the schema")
	}
	// Nested struct shape must survive.
	if item := s.Properties["list"].Items; item == nil || item.Properties["label"] == nil {
		t.Errorf("list items lost their struct shape: %+v", s.Properties["list"])
	}
	if ap := s.Properties["lookup"].AdditionalProperties; ap == nil || ap.Type != "integer" {
		t.Errorf("map value type lost: %+v", s.Properties["lookup"])
	}
}

// omitempty fields and pointers may legitimately be absent, so marking them
// required would make valid output fail validation.
func TestRequiredCoversOnlyAlwaysPresentFields(t *testing.T) {
	s := Of[sample]()
	required := map[string]bool{}
	for _, r := range s.Required {
		required[r] = true
	}
	for _, want := range []string{"version", "name", "ratio", "ok", "list", "lookup", "free", "raw"} {
		if !required[want] {
			t.Errorf("%q is always marshalled but is not required", want)
		}
	}
	for _, notWanted := range []string{"optional", "ptr", "renamed"} {
		if required[notWanted] {
			t.Errorf("%q is omitempty or a pointer and must not be required", notWanted)
		}
	}
}

type base struct {
	ID string `json:"id"`
}

type embedded struct {
	base
	Extra string `json:"extra"`
}

func TestEmbeddedStructFieldsArePromoted(t *testing.T) {
	s := Of[embedded]()
	if _, ok := s.Properties["id"]; !ok {
		t.Errorf("embedded field was not promoted: %v", s.PropertyOrder)
	}
	if _, ok := s.Properties["extra"]; !ok {
		t.Error("own field missing")
	}
	if _, ok := s.Properties["base"]; ok {
		t.Error("the embedded struct was nested instead of promoted")
	}
}

type node struct {
	Name  string  `json:"name"`
	Child *node   `json:"child,omitempty"`
	Peers []*node `json:"peers,omitempty"`
}

// A self-referential struct must terminate with an open object rather than
// recursing until the stack gives out.
func TestRecursiveTypesTerminate(t *testing.T) {
	done := make(chan *Schema, 1)
	go func() { done <- Of[node]() }()
	s := <-done
	if s.Properties["child"].Type != "object" {
		t.Errorf("recursive child = %+v", s.Properties["child"])
	}
	if s.Properties["child"].Properties != nil {
		t.Error("the recursive branch expanded instead of degrading to an open object")
	}
}

// A non-struct root cannot be an MCP outputSchema, which must be an object.
func TestNonStructRootDegradesToAnObject(t *testing.T) {
	if s := Of[[]string](); s.Type != "object" || s.Properties != nil {
		t.Errorf("slice root = %+v, want a bare object", s)
	}
	if s := Of[string](); s.Type != "object" {
		t.Errorf("string root = %+v", s)
	}
}

// The golden files are byte-compared, so rendering must be deterministic and
// must keep declaration order rather than map order.
func TestJSONIsDeterministicAndOrdered(t *testing.T) {
	first, err := JSON(Of[sample]())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := JSON(Of[sample]())
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("render %d differs from the first:\n%s\n---\n%s", i, first, again)
		}
	}
	body := string(first)
	if !strings.HasPrefix(body, "{\n  \"$schema\": \"https://json-schema.org/draft/2020-12/schema\",\n") {
		t.Errorf("missing or misplaced $schema:\n%s", body)
	}
	if !strings.HasSuffix(body, "}\n") {
		t.Error("output is not newline-terminated")
	}
	if i, j := strings.Index(body, `"version"`), strings.Index(body, `"name"`); i < 0 || j < 0 || i > j {
		t.Error("properties are not in declaration order")
	}
	var probe map[string]any
	if err := json.Unmarshal(first, &probe); err != nil {
		t.Fatalf("rendered schema is not valid JSON: %v\n%s", err, body)
	}
	if probe["$schema"] == nil || probe["properties"] == nil {
		t.Errorf("rendered schema lost a top-level member: %v", probe)
	}
}
