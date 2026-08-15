// Package nimoracle adapts a remote NIM chat model into the structured
// counterfactual-oracle contract the shadow-labeling flywheel expects
// (shadow.LabelDeps.RunTier). The local cascade returns grammar-constrained
// core.Result values; a NIM endpoint returns free text. This package closes
// that gap per task type: it builds a JSON-only instruction prompt, calls the
// endpoint, and parser-extracts + validates the reply into exactly the Data
// shape the shadow judges (pipeline.AnswersAgree, grounding.Check, the B2
// summarize judge) consume.
//
// Scope guard: only the ESCALATION (oracle) call is routed remotely. The B1
// router-label path inside shadow.LabelQueue reruns the E2B tier through the
// same RunTier hook — WrapRunTier dispatches any non-oracle model alias to the
// local RunTier untouched, so router/kNN labels keep their local provenance.
// NIM calls never enter the savings ledger (deliberate experiment, not
// defer-avoidance), matching the nimclient package contract.
package nimoracle

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/nimclient"
	"github.com/dmmdea/offload-harness/internal/parser"
)

// ChatFunc matches (*nimclient.Client).Chat so the client plugs in directly and
// tests fake the network with a closure.
type ChatFunc func(ctx context.Context, model, system, user string, maxTokens int, temperature float64) (nimclient.ChatResult, error)

// RunTierFunc matches shadow.LabelDeps.RunTier / (*pipeline.Pipeline).RunTier.
type RunTierFunc func(ctx context.Context, req core.Request, model string) (core.Result, bool)

// WrapRunTier returns a RunTier that routes calls for oracleAlias (the
// escalation slot) through the NIM endpoint via chat+Adapt, and every other
// model alias (the E2B counterfactual) through local. ok=false on any remote
// failure, truncation, or un-adaptable reply — shadow.LabelQueue then skips
// the item, which is the flywheel's normal un-judgeable path, never an abort.
func WrapRunTier(chat ChatFunc, nimModel string, maxTokens int, oracleAlias string, local RunTierFunc) RunTierFunc {
	return func(ctx context.Context, req core.Request, model string) (core.Result, bool) {
		if model != oracleAlias {
			if local == nil {
				return core.Result{}, false
			}
			return local(ctx, req, model)
		}
		system, user, ok := Prompt(req.Task, req)
		if !ok {
			return core.Result{}, false
		}
		// Temperature 0: the oracle is a judge substrate, not a generator —
		// determinism beats diversity for label stability.
		cr, err := chat(ctx, nimModel, system, user, maxTokens, 0)
		if err != nil || cr.Truncated || strings.TrimSpace(cr.Content) == "" {
			return core.Result{}, false
		}
		return Adapt(req.Task, req, cr.Content)
	}
}

// Prompt builds the per-task system+user prompt instructing the remote model to
// answer in exactly the JSON shape the shadow judges parse. ok=false for task
// types the shadow flywheel does not label (vision/audio tasks never enter the
// shadow queue).
func Prompt(task core.TaskType, req core.Request) (system, user string, ok bool) {
	switch task {
	case core.TaskClassify:
		labels := paramLabels(req.Params)
		if len(labels) > 0 {
			system = fmt.Sprintf(
				"You are a precise text classifier. Classify the user's text into exactly one of these labels: %s. "+
					"Reply with ONLY a JSON object of the form {\"label\": \"<label>\"} — no prose, no code fences.",
				strings.Join(labels, ", "))
		} else {
			system = "You are a precise text classifier. Reply with ONLY a JSON object of the form {\"label\": \"<label>\"} — no prose, no code fences."
		}
		return system, req.Input, true

	case core.TaskTriage:
		system = "You are a precise triage judge. Answer the question about the user's text. " +
			"Reply with ONLY a JSON object of the form {\"decision\": \"yes\"}, {\"decision\": \"no\"} or {\"decision\": \"unsure\"} — no prose, no code fences."
		if q := paramString(req.Params, "question"); q != "" {
			return system, "Question: " + q + "\n\nText:\n" + req.Input, true
		}
		return system, req.Input, true

	case core.TaskExtract:
		system = "You are a precise information extractor. Extract the requested fields from the user's text. " +
			"Every extracted value must appear VERBATIM in the text — never infer, reformat, or invent. " +
			"Reply with ONLY one JSON object holding the extracted fields — no prose, no code fences."
		if s, sok := req.Params["schema"]; sok {
			if b, err := json.Marshal(s); err == nil {
				system += " The object must follow this JSON Schema: " + string(b)
			}
		}
		return system, req.Input, true

	case core.TaskSummarize:
		system = "You are a precise summarizer. Summarize the user's text faithfully. " +
			"Reply with ONLY a JSON object of the form {\"summary\": \"<summary>\"} — no prose, no code fences."
		if mp := paramInt(req.Params, "max_points"); mp > 0 {
			system = fmt.Sprintf("You are a precise summarizer. Summarize the user's text faithfully in at most %d key points. "+
				"Reply with ONLY a JSON object of the form {\"summary\": \"<summary>\"} — no prose, no code fences.", mp)
		}
		return system, req.Input, true
	}
	return "", "", false
}

// Adapt converts the remote model's free-text reply into the core.Result shape
// the shadow judges expect for task. ok=false means un-adaptable — the caller
// skips the item (the flywheel's normal un-judgeable path). Validation is
// strict on the fields the judges compare: a classify label outside the
// request's label set, or a triage decision outside yes/no/unsure, is a
// malformed oracle answer, not a disagreement — reporting it as Data would
// poison the agreement label, so it is rejected here instead.
func Adapt(task core.TaskType, req core.Request, raw string) (core.Result, bool) {
	switch task {
	case core.TaskClassify:
		obj, err := parser.Extract(raw)
		if err != nil {
			return core.Result{}, false
		}
		label := jsonStringField(obj, "label")
		if label == "" {
			return core.Result{}, false
		}
		if labels := paramLabels(req.Params); len(labels) > 0 {
			canon, found := matchLabel(label, labels)
			if !found {
				return core.Result{}, false
			}
			label = canon
		}
		return dataResult(map[string]string{"label": label})

	case core.TaskTriage:
		obj, err := parser.Extract(raw)
		if err != nil {
			return core.Result{}, false
		}
		decision := strings.ToLower(strings.TrimSpace(jsonStringField(obj, "decision")))
		if decision != "yes" && decision != "no" && decision != "unsure" {
			return core.Result{}, false
		}
		return dataResult(map[string]string{"decision": decision})

	case core.TaskExtract:
		obj, err := parser.Extract(raw)
		if err != nil {
			return core.Result{}, false
		}
		// grounding.Check needs a JSON OBJECT; a bare array/scalar reply is a
		// malformed extraction.
		var m map[string]any
		if json.Unmarshal(obj, &m) != nil {
			return core.Result{}, false
		}
		return core.Result{OK: true, Data: obj}, true

	case core.TaskSummarize:
		// Prefer the instructed {"summary": ...} shape; a model that answered in
		// plain prose still produced a usable summary — fall back to the raw text
		// (shadow.summaryText applies the same JSON-else-raw reading on its side).
		text := raw
		if obj, err := parser.Extract(raw); err == nil {
			if s := jsonStringField(obj, "summary"); s != "" {
				text = s
			}
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return core.Result{}, false
		}
		return dataResult(map[string]string{"summary": text})
	}
	return core.Result{}, false
}

// dataResult marshals v as the Result Data payload.
func dataResult(v any) (core.Result, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return core.Result{}, false
	}
	return core.Result{OK: true, Data: b}, true
}

// matchLabel finds candidate in labels case-insensitively and returns the label
// set's canonical casing, so sidecar rows stay uniform with local-oracle rows.
func matchLabel(candidate string, labels []string) (string, bool) {
	c := strings.TrimSpace(candidate)
	for _, l := range labels {
		if strings.EqualFold(c, strings.TrimSpace(l)) {
			return l, true
		}
	}
	return "", false
}

// paramLabels reads the classify label set, accepting []string or []any
// (JSON-decoded shadow-queue items carry []any). Sorted copy is NOT taken —
// prompt order mirrors the caller's order.
func paramLabels(params map[string]any) []string {
	v, ok := params["labels"]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func paramString(params map[string]any, key string) string {
	if v, ok := params[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// paramInt reads an int-ish param (JSON decoding yields float64).
func paramInt(params map[string]any, key string) int {
	switch v := params[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

// jsonStringField returns the string value of field in a JSON object, or ""
// when absent/unparseable/non-string. Mirrors pipeline.jsonStringField (which
// is unexported there; duplicating three lines beats exporting a helper across
// package boundaries for this).
func jsonStringField(raw []byte, field string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	if s, ok := m[field].(string); ok {
		return s
	}
	return ""
}

// SupportedTasks is the closed set of shadow-labelable tasks this oracle
// serves, for the CLI's usage text so the flag help never drifts from the code.
func SupportedTasks() []string {
	ts := []string{string(core.TaskClassify), string(core.TaskTriage), string(core.TaskExtract), string(core.TaskSummarize)}
	sort.Strings(ts)
	return ts
}
