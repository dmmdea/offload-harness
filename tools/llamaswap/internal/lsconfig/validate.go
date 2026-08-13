// Copyright 2026 Daniel Martinez and contributors. Licensed under Apache-2.0. See LICENSE.

package lsconfig

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// embeddedSchema is the upstream llama-swap draft-07 config schema, vendored
// at schema/config-schema.json. Provenance (retrieval date, llama-swap
// version, sha256) is recorded in schema/README.md. Embedded so validation
// works offline and a given build always validates against a known document.
//
//go:embed schema/config-schema.json
var embeddedSchema []byte

// SchemaBytes returns a copy of the embedded schema document.
func SchemaBytes() []byte {
	out := make([]byte, len(embeddedSchema))
	copy(out, embeddedSchema)
	return out
}

// SchemaSourceURL is where the embedded schema came from.
const SchemaSourceURL = "https://raw.githubusercontent.com/mostlygeek/llama-swap/main/config-schema.json"

// SchemaRetrievedDate and SchemaLlamaSwapVersion mirror schema/README.md so
// `config validate --json` can report which schema it judged against without
// the caller reading a markdown file.
const (
	SchemaRetrievedDate    = "2026-08-13"
	SchemaLlamaSwapVersion = "v249"
)

// ValidationIssue is one schema violation or unknown-key finding.
type ValidationIssue struct {
	// Pointer is a JSON Pointer into the config document ("" for the root).
	Pointer string `json:"pointer"`
	Message string `json:"message"`
	// Suggestion is a nearest-known-key hint for typo'd keys.
	Suggestion string `json:"suggestion,omitempty"`
	// Line is the source line when the issue could be located.
	Line int `json:"line,omitempty"`
}

// ValidationResult is the outcome of `config validate`.
type ValidationResult struct {
	Path             string            `json:"path"`
	Sha256           string            `json:"sha256"`
	Valid            bool              `json:"valid"`
	Issues           []ValidationIssue `json:"issues"`
	ModelCount       int               `json:"model_count"`
	SchemaSource     string            `json:"schema_source"`
	SchemaRetrieved  string            `json:"schema_retrieved"`
	SchemaForVersion string            `json:"schema_for_llamaswap_version"`
}

// knownTopLevelKeys is derived from the embedded schema at init. Used for the
// unknown-key check the schema itself cannot make (upstream leaves
// additionalProperties unset at the root, so a misspelled top-level key
// validates clean and is then ignored at boot).
var knownTopLevelKeys []string

func topLevelKeysFromSchema() []string {
	if len(knownTopLevelKeys) > 0 {
		return knownTopLevelKeys
	}
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(embeddedSchema, &doc); err != nil {
		return nil
	}
	keys := make([]string, 0, len(doc.Properties))
	for k := range doc.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	knownTopLevelKeys = keys
	return keys
}

// KnownTopLevelKeys returns the top-level keys the vendored schema declares.
func KnownTopLevelKeys() []string { return append([]string(nil), topLevelKeysFromSchema()...) }

func compiledSchema() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(embeddedSchema))
	if err != nil {
		return nil, fmt.Errorf("parse embedded schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource("llama-swap-config-schema.json", doc); err != nil {
		return nil, fmt.Errorf("register embedded schema: %w", err)
	}
	s, err := c.Compile("llama-swap-config-schema.json")
	if err != nil {
		return nil, fmt.Errorf("compile embedded schema: %w", err)
	}
	return s, nil
}

// Validate checks a parsed config against the embedded draft-07 schema and
// adds unknown-top-level-key findings with nearest-key suggestions.
func Validate(f *File) (*ValidationResult, error) {
	res := &ValidationResult{
		Path:             f.Path,
		Sha256:           f.Sha256,
		ModelCount:       len(f.Models),
		SchemaSource:     SchemaSourceURL,
		SchemaRetrieved:  SchemaRetrievedDate,
		SchemaForVersion: SchemaLlamaSwapVersion,
	}
	schema, err := compiledSchema()
	if err != nil {
		return nil, err
	}
	if f.Generic == nil {
		return nil, fmt.Errorf("config %s could not be decoded for validation", f.Path)
	}
	if err := schema.Validate(any(f.Generic)); err != nil {
		var ve *jsonschema.ValidationError
		if ok := asValidationError(err, &ve); ok {
			for _, issue := range flattenValidationError(ve) {
				issue.Line = f.lineForPointer(issue.Pointer)
				res.Issues = append(res.Issues, issue)
			}
		} else {
			res.Issues = append(res.Issues, ValidationIssue{Message: err.Error()})
		}
	}
	if err := f.checkDuplicateTopKeys(); err != nil {
		res.Issues = append(res.Issues, ValidationIssue{Message: err.Error()})
	}

	known := topLevelKeysFromSchema()
	for _, k := range f.TopKeys {
		if containsString(known, k) {
			continue
		}
		issue := ValidationIssue{
			Pointer: "/" + k,
			Message: fmt.Sprintf("unknown top-level key %q — llama-swap ignores it silently at boot (the upstream schema does not set additionalProperties:false, so this passes schema validation)", k),
			Line:    f.TopKeyLine[k],
		}
		if s, ok := NearestKey(k, known); ok {
			issue.Suggestion = s
			issue.Message += fmt.Sprintf("; did you mean %q?", s)
		}
		res.Issues = append(res.Issues, issue)
	}

	sort.SliceStable(res.Issues, func(i, j int) bool {
		if res.Issues[i].Line != res.Issues[j].Line {
			return res.Issues[i].Line < res.Issues[j].Line
		}
		return res.Issues[i].Pointer < res.Issues[j].Pointer
	})
	res.Valid = len(res.Issues) == 0
	return res, nil
}

// asValidationError is errors.As specialized to the validator's type, kept in
// one place so the import surface of this file stays small.
func asValidationError(err error, target **jsonschema.ValidationError) bool {
	if ve, ok := err.(*jsonschema.ValidationError); ok {
		*target = ve
		return true
	}
	return false
}

// flattenValidationError renders the validator's Basic output, which is
// already the flat leaf list. The tree root of a draft-07 failure is always a
// useless "doesn't validate with #"; the actual key and reason live in the
// flattened units.
func flattenValidationError(ve *jsonschema.ValidationError) []ValidationIssue {
	if ve == nil {
		return nil
	}
	basic := ve.BasicOutput()
	if basic == nil {
		return []ValidationIssue{{Message: ve.Error()}}
	}
	var out []ValidationIssue
	seen := map[string]bool{}
	for _, unit := range basic.Errors {
		msg := ""
		if unit.Error != nil {
			msg = unit.Error.String()
		}
		if strings.TrimSpace(msg) == "" {
			continue
		}
		// Drop the structural "doesn't validate with ..." wrappers; they add
		// a line per nesting level and name no key an operator can act on.
		if strings.Contains(msg, "doesn't validate with") {
			continue
		}
		key := unit.InstanceLocation + "\x00" + msg
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ValidationIssue{Pointer: unit.InstanceLocation, Message: msg})
	}
	if len(out) == 0 {
		out = append(out, ValidationIssue{Message: ve.Error()})
	}
	return out
}

// checkDuplicateTopKeys catches a repeated top-level key. yaml.v3 keeps both
// in the node tree but last-wins on decode, so a config with two `models:`
// blocks silently loses the first one — invisible to schema validation.
func (f *File) checkDuplicateTopKeys() error {
	seen := map[string]int{}
	var dupes []string
	for _, k := range f.TopKeys {
		seen[k]++
		if seen[k] == 2 {
			dupes = append(dupes, k)
		}
	}
	if len(dupes) == 0 {
		return nil
	}
	sort.Strings(dupes)
	return fmt.Errorf("duplicate top-level key(s) %s — YAML keeps the LAST occurrence and silently discards the earlier one", strings.Join(dupes, ", "))
}

// lineForPointer maps a JSON Pointer back to a source line where we can. Only
// the shapes that matter for operator feedback are resolved: the top-level key
// and /models/<id>.
func (f *File) lineForPointer(ptr string) int {
	if ptr == "" {
		return 0
	}
	parts := strings.Split(strings.TrimPrefix(ptr, "/"), "/")
	if len(parts) == 0 {
		return 0
	}
	if parts[0] == "models" && len(parts) >= 2 {
		if m, ok := f.ModelIndex[unescapePointer(parts[1])]; ok {
			return m.KeyLine
		}
	}
	if l, ok := f.TopKeyLine[unescapePointer(parts[0])]; ok {
		return l
	}
	return 0
}

func unescapePointer(s string) string {
	s = strings.ReplaceAll(s, "~1", "/")
	return strings.ReplaceAll(s, "~0", "~")
}

// NearestKey returns the closest known key to got by Levenshtein distance,
// with a threshold that scales with the length of the input so short keys
// don't match everything.
func NearestKey(got string, known []string) (string, bool) {
	best, bestDist := "", 1<<30
	lowerGot := strings.ToLower(got)
	for _, k := range known {
		d := levenshtein(lowerGot, strings.ToLower(k))
		if d < bestDist {
			best, bestDist = k, d
		}
	}
	if best == "" {
		return "", false
	}
	limit := len(got) / 3
	if limit < 2 {
		limit = 2
	}
	if limit > 5 {
		limit = 5
	}
	if bestDist > limit {
		return "", false
	}
	return best, true
}

func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
