// Package tasks builds the per-task prompt, grammar, and validation schema.
package tasks

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dmmdea/local-offload-pp-cli/internal/core"
	"github.com/dmmdea/local-offload-pp-cli/internal/gbnf"
)

// Built is everything the pipeline needs to run one task.
type Built struct {
	System    string
	User      string
	Grammar   string
	Schema    map[string]any // validation schema (extract); nil when grammar suffices
	MaxTokens int
}

// Build produces the request material for req, or an error if params are bad.
func Build(req core.Request) (Built, error) {
	switch req.Task {
	case core.TaskSummarize:
		return buildSummarize(req)
	case core.TaskClassify:
		return buildClassify(req)
	case core.TaskTriage:
		return buildTriage(req)
	case core.TaskExtract:
		return buildExtract(req)
	case core.TaskVQA:
		return buildVQA(req)
	case core.TaskOCR:
		return buildOCR(req)
	case core.TaskAssessImage:
		return buildAssessImage(req)
	default:
		return Built{}, fmt.Errorf("unknown task %q", req.Task)
	}
}

// buildVQA builds a free-text visual-question-answering request: the question is
// the user prompt and the image rides alongside on the vision path (pipeline
// resolves req.Image). No grammar — VQA answers are natural language.
func buildVQA(req core.Request) (Built, error) {
	q := paramString(req.Params, "question")
	if q == "" {
		return Built{}, fmt.Errorf("vqa requires a question")
	}
	return Built{
		System:    "You are a precise visual assistant. Answer ONLY from what is visible in the image. If the image does not contain the answer, say you cannot tell.",
		User:      q,
		Grammar:   "",
		MaxTokens: 256,
	}, nil
}

// buildOCR builds a free-text OCR (image-to-text transcription) request. Unlike
// vqa there is no question param — the task is fixed: transcribe everything. No
// grammar (the output is free-form text), and a generous token budget since a
// dense page transcribes to many tokens.
func buildOCR(req core.Request) (Built, error) {
	return Built{
		System:    "You are an OCR engine. Transcribe ALL text in the image EXACTLY, preserving reading order and line breaks. Output ONLY the transcribed text — no commentary, no markdown fences.",
		User:      "Transcribe the text in this image.",
		Grammar:   "",
		MaxTokens: 1024,
	}, nil
}

// buildAssessImage builds a GBNF-constrained QA over a generated image: it forces
// exactly {has_people:bool, has_text:bool, matches_brief:bool, notes:string} so a
// ComfyUI/RealVisXL render can be checked against hard exclusions (no people / no
// text). The grammar is reused from internal/gbnf (3 booleans + 1 string in a
// fixed-order object). An optional brief (Params["brief"]) is woven into the user
// prompt; with no brief, matches_brief is instructed to true. Grammar+image is
// proven to coexist on this build.
func buildAssessImage(req core.Request) (Built, error) {
	grammar := gbnf.Object([]gbnf.Field{
		{Name: "has_people", Type: gbnf.TBool},
		{Name: "has_text", Type: gbnf.TBool},
		{Name: "matches_brief", Type: gbnf.TBool},
		{Name: "notes", Type: gbnf.TString},
	})
	user := "Assess the image."
	if brief := paramString(req.Params, "brief"); brief != "" {
		user = fmt.Sprintf("Brief: %s. Assess the image.", brief)
	}
	return Built{
		System:    "You are a strict image QA assistant. Report exactly what is VISIBLE. has_people=true if any person/face/body part is visible. has_text=true if any readable letters/words/numbers are rendered in the image. matches_brief: if a brief is given, whether the image matches it; if no brief, set true. notes: one short phrase.",
		User:      user,
		Grammar:   grammar,
		MaxTokens: 128,
	}, nil
}

func buildSummarize(req core.Request) (Built, error) {
	n := paramInt(req.Params, "max_points", 5)
	if n < 1 {
		n = 1
	}
	grammar := gbnf.Object([]gbnf.Field{
		{Name: "summary", Type: gbnf.TString},
		{Name: "bullets", Type: gbnf.TStringArray},
	})
	return Built{
		System:    "You are a precise summarizer. Output ONLY a JSON object. Be faithful to the source; do not invent facts.",
		User:      fmt.Sprintf("Summarize the text below. Provide a 1-2 sentence \"summary\" and up to %d key points in \"bullets\".\n\nTEXT:\n%s", n, req.Input),
		Grammar:   grammar,
		MaxTokens: 512,
	}, nil
}

func buildClassify(req core.Request) (Built, error) {
	labels := paramStrings(req.Params, "labels")
	if len(labels) < 2 {
		return Built{}, fmt.Errorf("classify requires at least 2 labels")
	}
	grammar := gbnf.Object([]gbnf.Field{
		{Name: "label", Type: gbnf.TEnum, Enum: labels},
		{Name: "confidence", Type: gbnf.TNumber},
	})
	return Built{
		System:    "You are a classifier. Choose exactly one label from the allowed set. Output ONLY a JSON object.",
		User:      fmt.Sprintf("Classify the text into exactly one of these labels: %s.\nReturn the chosen \"label\" and a \"confidence\" between 0 and 1.\n\nTEXT:\n%s", strings.Join(labels, ", "), req.Input),
		Grammar:   grammar,
		MaxTokens: 64,
	}, nil
}

func buildTriage(req core.Request) (Built, error) {
	q := paramString(req.Params, "question")
	if q == "" {
		return Built{}, fmt.Errorf("triage requires a question")
	}
	grammar := gbnf.Object([]gbnf.Field{
		{Name: "decision", Type: gbnf.TEnum, Enum: []string{"yes", "no", "unsure"}},
		{Name: "reason", Type: gbnf.TString},
	})
	return Built{
		System:    "You triage yes/no/unsure questions about a piece of text. Output ONLY a JSON object.",
		User:      fmt.Sprintf("Question: %s\nAnswer with \"decision\" (yes, no, or unsure) and a short \"reason\".\n\nTEXT:\n%s", q, req.Input),
		Grammar:   grammar,
		MaxTokens: 256,
	}, nil
}

func buildExtract(req core.Request) (Built, error) {
	schema := paramMap(req.Params, "schema")
	if schema == nil {
		return Built{}, fmt.Errorf("extract requires a json-schema \"schema\" param")
	}
	fields := gbnf.FromJSONSchema(schema)
	if len(fields) == 0 {
		return Built{}, fmt.Errorf("extract schema has no usable properties")
	}
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		names = append(names, f.Name)
	}
	return Built{
		System:    "You extract structured data from text. Output ONLY a JSON object with exactly the requested fields. Use empty values when a field is absent.",
		User:      fmt.Sprintf("Extract these fields from the text: %s.\n\nTEXT:\n%s", strings.Join(names, ", "), req.Input),
		Grammar:   gbnf.Object(fields),
		Schema:    schema,
		MaxTokens: 512,
	}, nil
}

// ---- param helpers ----

func paramString(p map[string]any, k string) string {
	if v, ok := p[k].(string); ok {
		return v
	}
	return ""
}

func paramInt(p map[string]any, k string, def int) int {
	switch v := p[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

func paramStrings(p map[string]any, k string) []string {
	switch v := p[k].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func paramMap(p map[string]any, k string) map[string]any {
	if v, ok := p[k].(map[string]any); ok {
		return v
	}
	return nil
}

// stableParamsKey renders params deterministically for cache keying.
func StableParamsKey(p map[string]any) string {
	if len(p) == 0 {
		return ""
	}
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
