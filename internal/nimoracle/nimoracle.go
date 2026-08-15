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

// Stats counts remote-oracle outcomes per skip cause, so a whole-run systemic
// failure (endpoint dead mid-run, rate limit, model truncating on every item)
// is distinguishable from a genuinely un-judgeable queue at the CLI summary —
// per-item ok=false alone collapses transport errors, truncation, empty
// replies, and malformed answers into one invisible skip. Single-goroutine use
// (shadow.LabelQueue is sequential); FirstErr keeps the first transport error
// verbatim because nimclient's errors carry the actionable HTTP status + body.
type Stats struct {
	Remote       int // oracle calls attempted against the remote endpoint
	ChatErr      int // transport/HTTP errors (retryable operator problems)
	Truncated    int // finish_reason=length (nim_max_tokens too small for this model)
	Empty        int // empty content (e.g. reasoning model burned the budget thinking)
	Unpromptable int // task the oracle does not serve — skipped FREE, before any call
	Unadaptable  int // a PAID reply that Adapt rejected (actionable: the model's answers are unusable)
	FirstErr     error
}

// String renders the counters for the CLI run summary. Unpromptable (free,
// expected for tasks the oracle does not serve) is deliberately separate from
// Unadaptable (paid calls whose answers were rejected — a nim_model problem).
func (s *Stats) String() string {
	msg := fmt.Sprintf("remote=%d chat_err=%d truncated=%d empty=%d unadaptable=%d unpromptable=%d",
		s.Remote, s.ChatErr, s.Truncated, s.Empty, s.Unadaptable, s.Unpromptable)
	if s.FirstErr != nil {
		msg += fmt.Sprintf(" first_err=%q", s.FirstErr.Error())
	}
	return msg
}

// WrapRunTier returns a RunTier that routes calls for oracleAlias (the
// escalation slot) through the NIM endpoint via chat+Adapt, and every other
// model alias (the E2B counterfactual) through local. ok=false on any remote
// failure, truncation, or un-adaptable reply — shadow.LabelQueue then skips
// the item, which is the flywheel's normal un-judgeable path, never an abort;
// stats (optional) records the per-cause counts so the CLI can surface a
// systemic failure. local is REQUIRED: a nil local would silently starve the
// B1 router/kNN feed while confhead labeling looked healthy, so it panics at
// wiring time (a programming error, not a runtime condition).
func WrapRunTier(chat ChatFunc, nimModel string, maxTokens int, oracleAlias string, local RunTierFunc, stats *Stats) RunTierFunc {
	if local == nil {
		panic("nimoracle.WrapRunTier: local RunTier is required (B1 E2B counterfactual must stay local)")
	}
	if stats == nil {
		stats = &Stats{}
	}
	return func(ctx context.Context, req core.Request, model string) (core.Result, bool) {
		if model != oracleAlias {
			return local(ctx, req, model)
		}
		system, user, ok := Prompt(req.Task, req)
		if !ok {
			stats.Unpromptable++
			return core.Result{}, false
		}
		// Temperature 0: the oracle is a judge substrate, not a generator —
		// determinism beats diversity for label stability.
		stats.Remote++
		cr, err := chat(ctx, nimModel, system, user, budgetFor(req.Task, req.Params, maxTokens), 0)
		switch {
		case err != nil:
			stats.ChatErr++
			if stats.FirstErr == nil {
				stats.FirstErr = err
			}
			return core.Result{}, false
		case cr.Truncated:
			stats.Truncated++
			return core.Result{}, false
		case strings.TrimSpace(cr.Content) == "":
			stats.Empty++
			return core.Result{}, false
		}
		res, ok := Adapt(req.Task, req, cr.Content)
		if !ok {
			stats.Unadaptable++
		}
		return res, ok
	}
}

// Prompt builds the per-task system+user prompt instructing the remote model to
// answer in exactly the JSON shape the shadow judges parse. ok=false for task
// types the shadow flywheel does not label (vision/audio tasks never enter the
// shadow queue).
func Prompt(task core.TaskType, req core.Request) (system, user string, ok bool) {
	switch task {
	case core.TaskClassify:
		// A label set is REQUIRED: without one, agreement against the entry
		// output is ill-defined over an open vocabulary, and any free-text
		// "label" (including a refusal) would be laundered into a disagreement
		// row. Shadow-queued classify items always carry labels (capture only
		// enqueues requests that passed tasks.Build, which requires >=2), so
		// rejecting here is the un-judgeable path, not a data gap.
		labels := paramLabels(req.Params)
		if len(labels) == 0 {
			return "", "", false
		}
		system = fmt.Sprintf(
			"You are a precise text classifier. Classify the user's text into exactly one of these labels: %s. "+
				"Reply with ONLY a JSON object of the form {\"label\": \"<label>\"} — no prose, no code fences.",
			strings.Join(labels, ", "))
		return system, req.Input, true

	case core.TaskTriage:
		system = "You are a precise triage judge. Answer the question about the user's text. " +
			"Reply with ONLY a JSON object of the form {\"decision\": \"yes\"}, {\"decision\": \"no\"} or {\"decision\": \"unsure\"} — no prose, no code fences."
		if q := paramString(req.Params, "question"); q != "" {
			return system, "Question: " + q + "\n\nText:\n" + req.Input, true
		}
		return system, req.Input, true

	case core.TaskExtract:
		// The system prompt stays NEUTRAL about groundedness, mirroring the
		// local escalation tier's ("You extract structured data from text.").
		// The extract label IS grounding.Check on this output — instructing
		// "verbatim values only" would coach the oracle toward the very metric
		// being measured and saturate the label toward grounded=true.
		system = "You are a precise information extractor. Extract the requested fields from the user's text. " +
			"Reply with ONLY one JSON object holding the extracted fields — no prose, no code fences."
		if s, sok := req.Params["schema"]; sok {
			if b, err := json.Marshal(s); err == nil {
				system += " The object must follow this JSON Schema: " + string(b)
			}
		}
		return system, req.Input, true

	case core.TaskSummarize:
		// Mirrors tasks.buildSummarize exactly: the B2 judge compares ONLY the
		// "summary" field (a 1-2 sentence abstract) between entry and oracle,
		// so the oracle must be told the same summary/bullets split and the
		// same default point count — otherwise it packs the key points into
		// "summary" and similarity is systematically depressed.
		n := maxPointsFor(req.Params)
		system = "You are a precise summarizer. Be faithful to the source; do not invent facts. " +
			"Reply with ONLY a JSON object of the form {\"summary\": \"<1-2 sentence summary>\", \"bullets\": [\"<key point>\", ...]} — no prose, no code fences."
		user = fmt.Sprintf("Summarize the text below. Provide a 1-2 sentence \"summary\" and up to %d key points in \"bullets\".\n\nTEXT:\n%s", n, req.Input)
		return system, user, true
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
	// Reasoning models on hosted NIM catalogs commonly inline <think>…</think>
	// in content (nimclient only splits the separate reasoning_content field).
	// Strip it BEFORE extraction, or the first balanced JSON object inside the
	// reasoning span would win over the real answer.
	raw = stripReasoning(raw)
	if raw == "" {
		return core.Result{}, false
	}
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
		// The label set is required (Prompt enforces it too): matching against
		// it is what stops a refusal or free-vocab answer being laundered into
		// a disagreement label.
		canon, found := matchLabel(label, paramLabels(req.Params))
		if !found {
			return core.Result{}, false
		}
		return dataResult(map[string]string{"label": canon})

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
		// The instructed {"summary": ...} shape is REQUIRED — no prose
		// fallback. Accepting raw prose would launder a guardrail refusal
		// ("I can't summarize this content.") into a near-zero-similarity
		// "summary" and record a poisoned disagreement label; a model that
		// ignores an explicit JSON-only instruction is un-judgeable here.
		obj, err := parser.Extract(raw)
		if err != nil {
			return core.Result{}, false
		}
		text := strings.TrimSpace(jsonStringField(obj, "summary"))
		if text == "" {
			return core.Result{}, false
		}
		return dataResult(map[string]string{"summary": text})
	}
	return core.Result{}, false
}

// stripReasoning removes inline reasoning from a reply before JSON extraction.
// parser.StripThink only handles a <think> span at the very START of the reply;
// a preamble before the span defeats it, and parser.Extract would then take the
// first balanced object INSIDE the reasoning. So: after StripThink, cut through
// the LAST </think> (the answer follows the final close), and if an unclosed
// <think> still remains the answer never materialized — return "" (un-judgeable).
func stripReasoning(raw string) string {
	s := parser.StripThink(raw)
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	if strings.Contains(s, "<think>") {
		return ""
	}
	return strings.TrimSpace(s)
}

// maxPointsFor resolves the summarize bullet count exactly like
// tasks.buildSummarize: absent key => 5; explicit value clamped to >= 1.
func maxPointsFor(params map[string]any) int {
	if _, present := params["max_points"]; !present {
		return 5
	}
	n := paramInt(params, "max_points")
	if n < 1 {
		n = 1
	}
	return n
}

// budgetFor sizes the completion budget per task, mirroring the local tiers'
// philosophy (tasks.summarizeBudget: an over-large budget costs nothing when
// the model stops early, an under-sized one burns the item). cfgMax alone is
// fine for the tiny label/decision shapes, but summarize is instructed to emit
// a summary PLUS up to N bullets — structurally the largest reply — so its
// budget grows by the same 384+160/bullet the local tier reserves, ON TOP of
// cfgMax (which on reasoning models is largely consumed by thinking).
func budgetFor(task core.TaskType, params map[string]any, cfgMax int) int {
	if task == core.TaskSummarize {
		return cfgMax + 384 + 160*maxPointsFor(params)
	}
	return cfgMax
}

// preflightInput is a fixed, moderately long text for Preflight — long enough
// that a reasoning model does real work and the instructed summary+bullets
// shape is exercised at its real size, so a pass is representative of the
// LARGEST reply the oracle will be asked for, not the smallest.
const preflightInput = "The migration moved the primary database from a single 8-core host to a three-node cluster " +
	"with synchronous replication. Median query latency fell from 41 ms to 12 ms, and the nightly " +
	"backup window shrank from four hours to fifty minutes. Two incidents occurred during cutover: " +
	"a stale DNS cache sent five percent of traffic to the old primary for eleven minutes, and a " +
	"connection-pool ceiling caused brief queueing at peak. Both were resolved without data loss, " +
	"and the rollback plan was never invoked."

// Preflight proves the remote oracle can serve a full-size judged reply BEFORE
// the caller destructively drains the shadow queue: one real chat roundtrip
// with the SUMMARIZE prompt (the structurally largest instructed shape) at its
// real budget, adapted through the same path items will take. A classify-sized
// probe would prove auth but pass at budgets that then truncate every
// summarize item mid-run. Returns nil when the reply arrived un-truncated and
// adapted cleanly.
func Preflight(ctx context.Context, chat ChatFunc, model string, cfgMax int) error {
	req := core.Request{Task: core.TaskSummarize, Input: preflightInput}
	system, user, _ := Prompt(core.TaskSummarize, req)
	cr, err := chat(ctx, model, system, user, budgetFor(core.TaskSummarize, nil, cfgMax), 0)
	switch {
	case err != nil:
		return err
	case cr.Truncated:
		return fmt.Errorf("preflight reply truncated at the summarize budget (nim_max_tokens=%d + bullets headroom) — "+
			"raise nim_max_tokens (reasoning models spend tokens thinking before answering) or pick a non-reasoning nim_model", cfgMax)
	}
	if _, ok := Adapt(core.TaskSummarize, req, cr.Content); !ok {
		return fmt.Errorf("preflight reply was not adaptable to the judge shape — model %s did not follow the JSON-only instruction; pick a different nim_model", model)
	}
	return nil
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
